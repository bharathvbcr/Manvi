// Package term owns the tty: raw mode, the alternate screen, size, and the
// signals that change any of them.
//
// Everything here is about one property — the terminal must be returned to the
// state it was found in, on every exit path there is. Raw mode with ISIG
// cleared means the shell's own Ctrl+C cannot rescue the user, and the
// alternate screen means their scrollback is hidden. A harness that exits
// through a panic, a SIGTERM, or a Ctrl+Z without undoing both leaves an
// operator with a terminal that shows nothing they type.
//
// So restoration is registered three times over: as a deferred call, as a
// signal handler, and as an explicit Stop. All three land on the same
// idempotent teardown.
package term

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"

	"manvi/ui/render"
)

// Terminal is an attached tty in a known state.
type Terminal struct {
	in  *os.File
	out *os.File

	mu      sync.Mutex
	saved   *state
	started bool

	// resize carries a notification, never a size. The size is read at the
	// moment of use: several resizes can arrive during one frame, and acting on
	// a stale one paints a frame for a window that no longer exists.
	resize chan struct{}
	// suspended fires after a Ctrl+Z round trip, so the app can force a repaint.
	suspended chan struct{}
	// stop closes to retire the signal goroutine. It is replaced on each Start
	// rather than guarded by a sync.Once, so a terminal that is stopped and
	// started again is not left deaf to resizes.
	stop chan struct{}

	profile render.Profile
	unicode bool
	// mouse, motion, and paste record what was enabled, so teardown disables
	// exactly what setup turned on.
	mouse  bool
	motion bool
	paste  bool
}

// New builds a terminal over the given files, normally os.Stdin and os.Stdout.
func New(in, out *os.File) *Terminal {
	t := &Terminal{
		in:        in,
		out:       out,
		resize:    make(chan struct{}, 1),
		suspended: make(chan struct{}, 1),
	}
	t.profile = detectProfile(out)
	t.unicode = detectUnicode()
	return t
}

// ErrNotATerminal reports that the destination cannot host a full-screen UI.
var ErrNotATerminal = errors.New("term: not a terminal")

// IsTerminal reports whether both ends are a tty.
//
// Both, not either: a UI whose output is a pipe cannot be seen, and one whose
// input is a pipe cannot be driven. Checking only the output is the common
// mistake, and it produces a UI that renders beautifully into a file while
// ignoring every keystroke.
func (t *Terminal) IsTerminal() bool {
	return isTTY(t.in) && isTTY(t.out)
}

func isTTY(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// Profile reports the colour depth resolved for this terminal.
func (t *Terminal) Profile() render.Profile { return t.profile }

// Unicode reports whether box-drawing and block glyphs are safe to use.
func (t *Terminal) Unicode() bool { return t.unicode }

// Border returns the border set appropriate to this terminal.
func (t *Terminal) Border() render.Border {
	if t.unicode {
		return render.Rounded
	}
	return render.ASCII
}

// Out is the writer frames are painted to.
func (t *Terminal) Out() io.Writer { return t.out }

// In is the reader keystrokes arrive on.
func (t *Terminal) In() io.Reader { return t.in }

// Options configures what Start turns on.
type Options struct {
	// AltScreen switches to the alternate buffer, leaving the user's scrollback
	// untouched and restoring it on exit.
	AltScreen bool
	// Mouse enables SGR mouse reporting: clicks, wheel, and drag motion.
	Mouse bool
	// Motion extends Mouse to any-motion tracking (mode 1003 on top of 1002),
	// so a bare hover is reported too — what lets a list highlight follow the
	// pointer before any click. It costs a stream of motion events; the loop
	// coalesces repeats within a cell.
	Motion bool
	// BracketedPaste makes a paste arrive as one delimited event rather than as
	// a burst of keystrokes. Without it, pasted text containing a newline is
	// indistinguishable from the user pressing Enter — which sends a
	// half-pasted prompt to a model.
	BracketedPaste bool
	// Title, when set, is written to the window title and restored on exit.
	Title string
}

// Start puts the terminal into raw mode and applies the options.
func (t *Terminal) Start(opts Options) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started {
		return nil
	}
	if !t.IsTerminal() {
		return ErrNotATerminal
	}

	t.stop = make(chan struct{})
	saved, err := makeRaw(t.in.Fd())
	if err != nil {
		return err
	}
	t.saved = saved
	t.started = true

	var b strings.Builder
	if opts.AltScreen {
		// 1049 saves the cursor, switches to the alternate buffer, and clears
		// it, all in one sequence the terminal undoes on the matching reset.
		b.WriteString("\x1b[?1049h")
	}
	if opts.Title != "" {
		b.WriteString("\x1b[22;0t") // push the current title so it can be restored
		b.WriteString("\x1b]0;" + sanitizeTitle(opts.Title) + "\x07")
	}
	if opts.Mouse {
		// 1000 button events, 1002 drag, 1006 SGR encoding.
		// SGR matters: the original encoding packs coordinates into single
		// bytes and simply cannot address a column past 223, which any
		// full-width terminal has.
		b.WriteString("\x1b[?1000h\x1b[?1002h\x1b[?1006h")
		t.mouse = true
	}
	if opts.Mouse && opts.Motion {
		// 1003 any-motion, layered on 1002's drag-only reporting.
		b.WriteString("\x1b[?1003h")
		t.motion = true
	}
	if opts.BracketedPaste {
		b.WriteString("\x1b[?2004h")
		t.paste = true
	}
	b.WriteString("\x1b[?25l") // hide the cursor; the painter places it
	if _, err := io.WriteString(t.out, b.String()); err != nil {
		// A failure here leaves the terminal raw with the UI never drawn, which
		// is the worst of both states. Undo before returning.
		_ = t.restoreLocked()
		return err
	}

	t.watchSignals()
	return nil
}

// Stop restores everything Start changed. It is idempotent and safe to call
// from a deferred recover.
func (t *Terminal) Stop() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.restoreLocked()
}

func (t *Terminal) restoreLocked() error {
	if !t.started {
		return nil
	}
	t.started = false
	if t.stop != nil {
		close(t.stop)
		t.stop = nil
	}

	var b strings.Builder
	if t.paste {
		b.WriteString("\x1b[?2004l")
	}
	if t.motion {
		b.WriteString("\x1b[?1003l")
		t.motion = false
	}
	if t.mouse {
		b.WriteString("\x1b[?1006l\x1b[?1002l\x1b[?1000l")
		t.mouse = false
	}
	b.WriteString("\x1b[0m")     // drop any style the last frame left set
	b.WriteString("\x1b[?25h")   // show the cursor again
	b.WriteString("\x1b[?1049l") // leave the alternate screen
	b.WriteString("\x1b[23;0t")  // pop the window title
	_, writeErr := io.WriteString(t.out, b.String())

	var restoreErr error
	if t.saved != nil {
		restoreErr = setState(t.in.Fd(), t.saved)
		t.saved = nil
	}
	// The termios restore is the one that matters — a terminal left in raw mode
	// is unusable, whereas a stray escape sequence is cosmetic — so its error
	// is the one reported.
	if restoreErr != nil {
		return restoreErr
	}
	return writeErr
}

// Size reports the terminal's current dimensions.
//
// A terminal that reports zero in either dimension — which happens when the
// window is being dragged, and on some multiplexers at startup — is reported as
// a small usable size rather than as zero. Laying out into a zero-width screen
// divides by it.
func (t *Terminal) Size() (int, int) {
	w, h, err := size(t.out.Fd())
	if err != nil || w <= 0 || h <= 0 {
		if env := os.Getenv("COLUMNS"); env != "" {
			fmt.Sscanf(env, "%d", &w)
		}
		if env := os.Getenv("LINES"); env != "" {
			fmt.Sscanf(env, "%d", &h)
		}
	}
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	return w, h
}

// Resized fires when the window changes size.
func (t *Terminal) Resized() <-chan struct{} { return t.resize }

// Resumed fires after the process returns from a suspend, when the terminal has
// been put back into raw mode and the screen must be repainted from scratch.
func (t *Terminal) Resumed() <-chan struct{} { return t.suspended }

// Suspend performs a Ctrl+Z: restore the terminal, stop this process, and on
// resume put everything back and ask for a repaint.
func (t *Terminal) Suspend() error {
	stopSig, contSig := suspendSignals()
	if stopSig == nil {
		return nil
	}

	t.mu.Lock()
	saved, mouse, paste := t.saved, t.mouse, t.paste
	t.mu.Unlock()
	if saved == nil {
		return nil
	}

	// Hand the terminal back before stopping. A process suspended while the tty
	// is raw leaves the shell with no echo and no line editing.
	_, _ = io.WriteString(t.out, "\x1b[?2004l\x1b[?1006l\x1b[?1002l\x1b[?1000l\x1b[0m\x1b[?25h\x1b[?1049l")
	if err := setState(t.in.Fd(), saved); err != nil {
		return err
	}

	cont := make(chan os.Signal, 1)
	signal.Notify(cont, contSig)
	defer signal.Stop(cont)

	if err := raiseSelf(stopSig); err != nil {
		return err
	}
	<-cont

	// Resumed: re-establish everything, then ask for a full repaint. The screen
	// contents are gone and the painter's diff would otherwise skip every cell
	// it believes is already correct.
	if _, err := makeRaw(t.in.Fd()); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("\x1b[?1049h")
	if mouse {
		b.WriteString("\x1b[?1000h\x1b[?1002h\x1b[?1006h")
	}
	if paste {
		b.WriteString("\x1b[?2004h")
	}
	b.WriteString("\x1b[?25l")
	if _, err := io.WriteString(t.out, b.String()); err != nil {
		return err
	}
	// Signals are deliberately not re-armed here: the notification registered
	// by Start was never cancelled, so re-arming would leak a goroutine and
	// deliver every subsequent resize twice.
	select {
	case t.suspended <- struct{}{}:
	default:
	}
	return nil
}

// watchSignals delivers resize notifications and guarantees restoration on a
// terminating signal.
func (t *Terminal) watchSignals() {
	if sig := resizeSignal(); sig != nil {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, sig)
		// The stop channel is captured here rather than read from the struct
		// inside the loop. Reading t.stop from the goroutine races with
		// restoreLocked setting it to nil — the race detector reports it, and
		// the consequence in a real run is a watcher that reads a nil channel
		// and blocks forever instead of retiring, leaking a goroutine and a
		// signal registration on every Start/Stop cycle. A local copy also
		// gives each watcher its own channel, which is what lets a terminal be
		// stopped and started again without the old watcher outliving it.
		stop := t.stop
		go func() {
			for {
				select {
				case <-ch:
					// Non-blocking: a burst of resizes during a window drag
					// collapses to one pending notification, and the size is
					// read fresh when it is handled.
					select {
					case t.resize <- struct{}{}:
					default:
					}
				case <-stop:
					signal.Stop(ch)
					return
				}
			}
		}()
	}
}

// sanitizeTitle strips anything that could terminate the OSC string early or
// smuggle a further sequence. A window title is set from a task title, which is
// content out of the store, and content must not be able to author escapes.
func sanitizeTitle(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return render.TruncateWidth(b.String(), 120, "")
}

// detectProfile resolves colour depth from the environment.
//
// The order is deliberate. NO_COLOR is an operator instruction and outranks
// capability. A non-tty gets no colour regardless of what TERM claims, because
// the destination is a file or a pipe. Only then is capability consulted.
func detectProfile(out *os.File) render.Profile {
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return render.NoColor
	}
	term := os.Getenv("TERM")
	if term == "" || term == "dumb" {
		return render.NoColor
	}
	if !isTTY(out) {
		return render.NoColor
	}
	switch strings.ToLower(os.Getenv("COLORTERM")) {
	case "truecolor", "24bit":
		return render.TrueColor
	}
	if strings.Contains(term, "truecolor") || strings.Contains(term, "direct") {
		return render.TrueColor
	}
	if strings.Contains(term, "256") {
		return render.ANSI256
	}
	// Terminals that support 256 colours without advertising it in TERM are
	// common inside multiplexers, but claiming a depth the terminal lacks
	// prints SGR parameters as text. Sixteen is the safe floor.
	return render.ANSI16
}

// detectUnicode reports whether the locale suggests the terminal can render box
// drawing and block elements.
func detectUnicode() bool {
	for _, key := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		v := strings.ToUpper(os.Getenv(key))
		if v == "" {
			continue
		}
		return strings.Contains(v, "UTF-8") || strings.Contains(v, "UTF8")
	}
	// No locale set at all. Terminal emulators default to UTF-8 and have for
	// years; a bare `docker exec` is the usual reason the variables are absent,
	// and its terminal handles UTF-8 fine.
	return true
}

// DetectProfile reports what colour a stream can carry, for callers that print
// to one without taking over the terminal.
//
// Exported so the line-oriented commands answer the question the same way the
// full-screen face does. A second copy of this logic is how `manvi logo` ends
// up emitting truecolor into a terminal that prints the parameters as text.
func DetectProfile(out *os.File) render.Profile { return detectProfile(out) }

// DetectUnicode reports whether block elements are safe, for the same reason.
func DetectUnicode() bool { return detectUnicode() }
