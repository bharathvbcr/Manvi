package tui

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"manvi/ui"
	"manvi/ui/input"
	"manvi/ui/render"
	"manvi/ui/term"
)

// Config wires the TUI to a harness.
type Config struct {
	// Host is what the UI can ask the harness to do. Required.
	Host Host
	// In and Out default to stdin and stdout.
	In, Out *os.File
	// Title is the window title.
	Title string
	// FirstSession, when set, is created before the first frame.
	FirstSession bool
}

// Runner owns the terminal, the painter, and the loop.
type Runner struct {
	cfg     Config
	term    *term.Terminal
	painter *render.Painter
	reader  *input.Reader
	app     *App

	actions chan Action

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	// savedStderr and stderrWriter hold the capture installed for the run.
	savedStderr  *os.File
	stderrWriter *os.File
	// done closes when the runner stops, retiring the stderr reader.
	done chan struct{}
}

// New builds a runner.
func New(cfg Config) (*Runner, error) {
	if cfg.Host == nil {
		return nil, errors.New("tui: a host is required; the UI performs nothing on its own")
	}
	if cfg.In == nil {
		cfg.In = os.Stdin
	}
	if cfg.Out == nil {
		cfg.Out = os.Stdout
	}
	t := term.New(cfg.In, cfg.Out)
	if !t.IsTerminal() {
		return nil, term.ErrNotATerminal
	}
	th := PickTheme(t.Profile(), t.Unicode())
	return &Runner{
		cfg:     cfg,
		term:    t,
		app:     newAppFor(th, cfg.Host, t),
		actions: make(chan Action, 512),
		cancels: map[string]context.CancelFunc{},
		done:    make(chan struct{}),
	}, nil
}

// Sink returns an event sink that feeds a session's transcript.
//
// It is a ui.Sink like any other, which is the point: the harness writes to it
// exactly as it writes to the line renderer or the JSON face, and knows nothing
// about the terminal.
func (r *Runner) Sink(sessionID string) ui.Sink {
	return ui.SinkFunc(func(e ui.Event) {
		if e.At.IsZero() {
			e.At = time.Now().UTC()
		}
		// A blocking send is deliberate. Dropping an event would leave a
		// transcript that is missing something that happened, and the loop
		// never waits on the harness — every effect runs on its own goroutine —
		// so this cannot deadlock.
		r.actions <- ActionEvent{SessionID: sessionID, Event: e}
	})
}

// Approver returns an approval seam bound to a session.
func (r *Runner) Approver(sessionID string) ui.Approver {
	return &Approver{SessionID: sessionID, Actions: r.actions}
}

// Send injects an action from outside the loop.
func (r *Runner) Send(a Action) { r.actions <- a }

// Busy lists the sessions with a turn in flight, excluding one.
//
// The exclusion is not a convenience: a slash command is itself run as a turn,
// so a command that asked "is anything running" without discounting its own
// caller would always answer yes. What the caller wants to know is whether it
// is alone — which is the question behind "a safety flag may only be moved
// outside a running turn". The registry is one object shared by every session,
// so a setting moved while another session is mid-turn changes the rules that
// turn is being judged by, halfway through.
func (r *Runner) Busy(exclude string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for id := range r.cancels {
		if id != exclude {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// frame bounds are the two ends of the redraw budget. Below minFrame a burst of
// streamed deltas would repaint faster than a terminal can show, which costs
// bandwidth and produces tearing over ssh; above maxIdle an animation stalls.
const (
	minFrame = 8 * time.Millisecond
	tickRate = 100 * time.Millisecond
)

// Run drives the UI until the operator quits or ctx is cancelled.
func (r *Runner) Run(ctx context.Context) (err error) {
	if err := r.term.Start(term.Options{
		AltScreen: true, Mouse: true, Motion: true, BracketedPaste: true, Title: r.cfg.Title,
	}); err != nil {
		return err
	}
	// Three registrations for one teardown. A panic here leaves the terminal in
	// raw mode on the alternate screen, where the shell that regains it shows
	// nothing the user types.
	defer func() {
		if rec := recover(); rec != nil {
			// Order matters: the terminal is restored and stderr is put back
			// before the panic resumes, or the trace is written in raw mode
			// onto an alternate screen that is about to be discarded.
			r.restoreStderr()
			_ = r.term.Stop()
			panic(rec)
		}
		if stopErr := r.term.Stop(); err == nil {
			err = stopErr
		}
	}()

	// Anything written to stderr while the UI owns the screen lands on top of
	// the frame, and the painter — which believes it knows what every cell
	// holds — will not repaint over it. A stray log line from a dependency, or
	// a diagnostic from the harness itself, corrupts the display until
	// something forces a full redraw.
	//
	// So stderr is captured and turned into events. Nothing is lost; it is
	// shown inside the UI instead of over it, and the real stderr is restored
	// before any panic can need it.
	restoreStderr := r.captureStderr()
	defer restoreStderr()

	w, h := r.term.Size()
	r.painter = render.NewPainter(w, h, r.term.Profile())
	r.app.Dispatch(ActionResize{W: w, H: h})

	r.reader = input.NewReader(r.cfg.In)
	go r.reader.Run()
	defer r.reader.Close()
	defer close(r.done)

	if r.cfg.FirstSession {
		r.runEffect(ctx, EffectNewSession{})
	}

	ticker := time.NewTicker(tickRate)
	defer ticker.Stop()

	var (
		dirty     = true
		lastDraw  time.Time
		frameWait <-chan time.Time
		frameT    *time.Timer
		// lastMotion coalesces the pointer stream: any-motion tracking can
		// deliver the same cell repeatedly, and a motion that crossed nothing
		// is not worth a dispatch or a frame.
		lastMotionX, lastMotionY, lastMotionB = -1, -1, -1
	)
	armFrame := func(d time.Duration) {
		if frameT != nil {
			frameT.Stop()
		}
		frameT = time.NewTimer(d)
		frameWait = frameT.C
	}
	defer func() {
		if frameT != nil {
			frameT.Stop()
		}
	}()

	for !r.app.Quitting() {
		select {
		case ev, ok := <-r.reader.Events():
			if !ok {
				return nil
			}
			if e, isErr := ev.(input.Error); isErr {
				if errors.Is(e.Err, io.EOF) {
					return nil
				}
				return e.Err
			}
			moved := false
			for _, act := range actionsFor(ev) {
				if m, ok := act.(ActionMotion); ok {
					if m.X == lastMotionX && m.Y == lastMotionY && m.Button == lastMotionB {
						continue
					}
					lastMotionX, lastMotionY, lastMotionB = m.X, m.Y, m.Button
				}
				r.apply(ctx, act)
				moved = true
			}
			if moved {
				dirty = true
			}

		case act := <-r.actions:
			r.apply(ctx, act)
			dirty = true

		case <-ticker.C:
			r.apply(ctx, ActionTick{})
			dirty = true

		case <-r.term.Resized():
			w, h := r.term.Size()
			r.painter.Resize(w, h)
			r.apply(ctx, ActionResize{W: w, H: h})
			dirty = true

		case <-r.term.Resumed():
			r.painter.Invalidate()
			dirty = true

		case <-frameWait:
			frameWait = nil

		case <-ctx.Done():
			return ctx.Err()
		}

		if !dirty {
			continue
		}
		// Coalesce: drain whatever else is already queued before painting, so a
		// burst of streamed text becomes one frame rather than one per delta.
		drained := true
		for drained {
			select {
			case act := <-r.actions:
				r.apply(ctx, act)
			default:
				drained = false
			}
		}

		if since := time.Since(lastDraw); since < minFrame {
			armFrame(minFrame - since)
			continue
		}
		if err := r.draw(); err != nil {
			return err
		}
		lastDraw = time.Now()
		dirty = false
	}
	return nil
}

// apply dispatches an action and runs whatever effects it asked for.
func (r *Runner) apply(ctx context.Context, act Action) {
	for _, e := range r.app.Dispatch(act) {
		r.runEffect(ctx, e)
	}
}

func (r *Runner) draw() error {
	caret := r.app.Draw(r.painter.Buffer())
	cursor := render.Cursor{}
	if caret.W > 0 {
		cursor = render.Cursor{X: caret.X, Y: caret.Y, Visible: true}
	}
	return r.painter.Flush(r.term.Out(), cursor)
}

// maxNoticeBytes bounds one captured stderr line. A diagnostic longer than this
// is truncated and marked rather than dropped: the drain must never stop, and a
// line too long to show is still evidence that something went wrong.
const maxNoticeBytes = 8 << 10

// captureStderr redirects os.Stderr into the event stream and returns the
// restore function.
func (r *Runner) captureStderr() func() {
	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		// Not fatal. Without the capture a stray write corrupts a frame, which
		// a redraw fixes; refusing to start the UI over it would not be a
		// trade worth making.
		return func() {}
	}
	r.mu.Lock()
	r.savedStderr, r.stderrWriter = os.Stderr, pipeW
	r.mu.Unlock()
	os.Stderr = pipeW

	go func() {
		// Closing the read end when the drain ends is what turns a stalled
		// consumer into a failed write instead of a blocked one. It also
		// returns the descriptor, which the restore path cannot do: it only
		// holds the writer.
		defer func() { _ = pipeR.Close() }()

		reader := bufio.NewReaderSize(pipeR, 64<<10)
		for {
			line, truncated, err := readNotice(reader)
			if err != nil {
				// EOF is the writer being closed by restore, which is the
				// ordinary way this ends.
				return
			}
			text := strings.TrimSpace(line)
			if text == "" {
				continue
			}
			if truncated {
				text = fmt.Sprintf("%s … [truncated at %d bytes]", text, maxNoticeBytes)
			}
			select {
			case r.actions <- ActionNotice{Text: text, Status: StatusBlock}:
			case <-r.done:
				return
			}
		}
	}()
	return r.restoreStderr
}

// readNotice reads one captured stderr line, bounded.
//
// The bound is why this is not a bufio.Scanner. A Scanner stops permanently on
// the first token past its limit, and a drain that stops is not merely a lost
// diagnostic: nothing empties the pipe after that, its buffer fills, and the
// next write to os.Stderr blocks forever — wedging whichever goroutine was
// trying to report a problem. So an oversized line is truncated and its tail
// discarded to the next boundary, and reading continues. The caller is told it
// was truncated so the notice can say so; a diagnostic too long to show is
// still evidence, and dropping it silently is the failure this avoids.
func readNotice(r *bufio.Reader) (line string, truncated bool, err error) {
	var buf []byte
	for {
		chunk, readErr := r.ReadSlice('\n')
		if room := maxNoticeBytes - len(buf); room > 0 {
			if room > len(chunk) {
				room = len(chunk)
			}
			buf = append(buf, chunk[:room]...)
			if room < len(chunk) {
				truncated = true
			}
		} else if len(chunk) > 0 {
			truncated = true
		}
		switch {
		case readErr == bufio.ErrBufferFull:
			// More of this same line is waiting. Keep going: the tail is
			// counted and discarded above, never accumulated.
			continue
		case readErr == io.EOF && len(buf) > 0:
			// A final line with no terminator is still a line.
			return string(buf), truncated, nil
		case readErr != nil:
			return "", false, readErr
		default:
			return string(buf), truncated, nil
		}
	}
}

// restoreStderr puts the real stderr back. It is idempotent.
func (r *Runner) restoreStderr() {
	r.mu.Lock()
	saved, writer := r.savedStderr, r.stderrWriter
	r.savedStderr, r.stderrWriter = nil, nil
	r.mu.Unlock()
	if saved == nil {
		return
	}
	os.Stderr = saved
	// Closing ends the reader goroutine.
	_ = writer.Close()
}

// actionsFor maps a terminal event onto the action vocabulary.
func actionsFor(ev input.Event) []Action {
	switch t := ev.(type) {
	case input.Key:
		// A plain printable keystroke is text; everything else is a binding.
		// The distinction is made here rather than in the dispatcher so the
		// dispatcher never has to know about modifiers.
		if t.Mod == 0 && t.Type == input.KeyRunes && len(t.Runes) > 0 {
			return []Action{ActionRune{Runes: t.Runes}}
		}
		if t.Mod == 0 && t.Type == input.KeySpace && len(t.Runes) > 0 {
			return []Action{ActionRune{Runes: []rune{' '}}}
		}
		return []Action{ActionKey{Binding: t.String()}}

	case input.Paste:
		return []Action{ActionPaste{Text: t.Text}}

	case input.Resize:
		return []Action{ActionResize{W: t.W, H: t.H}}

	case input.Mouse:
		switch t.Action {
		case input.MouseWheelUp:
			return []Action{ActionScroll{X: t.X, Y: t.Y, Delta: -3}}
		case input.MouseWheelDown:
			return []Action{ActionScroll{X: t.X, Y: t.Y, Delta: 3}}
		case input.MousePress:
			return []Action{ActionClick{X: t.X, Y: t.Y, Button: int(t.Button)}}
		case input.MouseRelease:
			return []Action{ActionRelease{X: t.X, Y: t.Y, Button: int(t.Button)}}
		case input.MouseMotion:
			// A drag and a bare hover arrive on the same action; the button
			// is what tells the dispatcher which it is.
			return []Action{ActionMotion{X: t.X, Y: t.Y, Button: int(t.Button)}}
		}
		return nil
	}
	return nil
}

// runEffect performs one effect. Anything that can block runs on its own
// goroutine and reports back as an Action.
func (r *Runner) runEffect(ctx context.Context, e Effect) {
	switch t := e.(type) {
	case EffectSubmit:
		r.startTurn(ctx, t.SessionID, func(c context.Context) error {
			return r.cfg.Host.Submit(c, t.SessionID, t.Text)
		})

	case EffectCommand:
		r.startTurn(ctx, t.SessionID, func(c context.Context) error {
			return r.cfg.Host.Command(c, t.SessionID, t.Name, t.Args)
		})

	case EffectCancel:
		r.mu.Lock()
		cancel := r.cancels[t.SessionID]
		r.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		r.cfg.Host.Cancel(t.SessionID)

	case EffectDecide:
		// The channel is buffered by the sender, so this never blocks the loop.
		select {
		case t.Reply <- t.Decision:
		default:
		}

	case EffectNewSession:
		go func() {
			id, title, err := r.cfg.Host.NewSession(ctx)
			if err != nil {
				r.actions <- ActionNotice{Text: "could not start a session: " + err.Error(), Status: StatusBlock}
				return
			}
			r.actions <- ActionSessionAdded{ID: id, Title: title}
		}()

	case EffectCloseSession:
		go func() {
			// A session's teardown runs on a fresh context. Cancelling the work
			// is exactly what strands the lease it holds, and the release is the
			// call that undoes that damage — it cannot be made on the context
			// that was just cancelled.
			releaseCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := r.cfg.Host.CloseSession(releaseCtx, t.SessionID); err != nil {
				r.actions <- ActionNotice{
					SessionID: t.SessionID,
					Text:      "session closed, but its cleanup failed: " + err.Error(),
					Status:    StatusBlock,
				}
			}
			r.actions <- actionRemoveSession{ID: t.SessionID}
		}()

	case EffectCopy:
		r.copyToClipboard(t.Text)

	case EffectSuspend:
		if err := r.term.Suspend(); err != nil {
			r.actions <- ActionNotice{Text: "suspend failed: " + err.Error(), Status: StatusWarn}
		}
		r.painter.Invalidate()

	case EffectRedraw:
		r.painter.Invalidate()

	case EffectQuit:
		// Handled by the loop condition.
	}
}

// actionRemoveSession retires a session once the host has released it.
type actionRemoveSession struct{ ID string }

func (actionRemoveSession) action() {}

// startTurn runs work for a session, reporting start and end as actions.
func (r *Runner) startTurn(parent context.Context, sessionID string, work func(context.Context) error) {
	ctx, cancel := context.WithCancel(parent)
	r.mu.Lock()
	if prev := r.cancels[sessionID]; prev != nil {
		// One turn per session. Two would interleave their writes into one
		// session log, and that log is the only thing the model's history is
		// projected from.
		r.mu.Unlock()
		cancel()
		// Reported rather than dropped in silence. The composer queues a
		// prompt typed during a turn and says so; a slash command took a
		// different path, arrived here, and vanished — the operator saw the
		// command echo into the transcript and no result ever follow, which
		// reads as the harness having hung rather than having refused.
		r.actions <- ActionNotice{
			SessionID: sessionID,
			Text:      "a turn is already running in this session — ctrl+c cancels it",
			Status:    StatusWarn,
		}
		return
	}
	r.cancels[sessionID] = cancel
	r.mu.Unlock()

	r.actions <- ActionTurnStarted{SessionID: sessionID}
	go func() {
		defer func() {
			r.mu.Lock()
			delete(r.cancels, sessionID)
			r.mu.Unlock()
			cancel()
		}()
		err := work(ctx)
		if errors.Is(err, context.Canceled) {
			// A cancel the operator asked for is not a failure to report as one.
			err = nil
		}
		r.actions <- ActionTurnEnded{SessionID: sessionID, Err: err}
	}()
}

// osc52Limit bounds a clipboard write. Terminals cap the sequence and several
// truncate silently, so the cap is applied here where it can be reported.
const osc52Limit = 96 * 1024

// copyToClipboard writes the selection with OSC 52.
//
// OSC 52 rather than a platform clipboard binary because the harness is
// routinely run over ssh, where pbcopy and xclip put text on the wrong machine's
// clipboard — the one the operator is not looking at.
func (r *Runner) copyToClipboard(text string) {
	truncated := false
	if len(text) > osc52Limit {
		text = text[:osc52Limit]
		truncated = true
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(text))
	fmt.Fprintf(r.term.Out(), "\x1b]52;c;%s\x07", encoded)
	// The painter believes it owns the screen, and this wrote past it.
	r.painter.Invalidate()
	if truncated {
		r.actions <- ActionNotice{
			Text:   "copied the first 96KB — the selection was larger",
			Status: StatusWarn,
		}
	}
}

// newAppFor builds the App and tells it what the terminal can do.
//
// The capability is asked of the terminal rather than read back off the theme,
// because the plain theme reports NoColor whatever the terminal supports — so a
// user who tried plain once would be stuck with a colourless dark theme after
// switching back.
func newAppFor(th Theme, host Host, t *term.Terminal) *App {
	a := NewApp(th, host)
	a.Profile = t.Profile()
	a.Unicode = t.Unicode()
	return a
}
