package term

import (
	"io"
	"os"
	"strconv"
	"strings"
)

// This file is the terminal's own chrome: the tab or window title, and the
// progress indicator a terminal draws on its tab, its window or the taskbar.
// They are how a run says what it is doing to someone who is not looking at it,
// which for an agent that works for minutes at a time is most of the time.
//
// Two rules shape everything here.
//
// A sequence is written only when what it says has changed. The caller may ask
// on every frame; the terminal remembers what it last wrote and stays silent
// until the answer differs. A title rewritten every frame is a redraw loop that
// never rests, and inside a multiplexer it can be a bell per frame.
//
// And what is written is taken back. Stop and a suspend both leave the plain
// title and no progress behind, so a shell prompt is never left under a tab
// that still says an agent is running.

// Progress is what the terminal's progress indicator shows.
//
// There is no percentage. A turn has no known length, and a bar that fills at a
// guessed rate is a claim about the future the harness cannot back.
type Progress int

const (
	// ProgressNone clears the indicator.
	ProgressNone Progress = iota
	// ProgressBusy is the indeterminate state: work is running.
	ProgressBusy
	// ProgressPaused is work that is waiting, on the operator or on nothing
	// happening. Terminals draw it as a warning.
	ProgressPaused
	// ProgressError is work that failed.
	ProgressError
)

// status is what this terminal has been told about titles and progress.
type status struct {
	// base is the title Start set, already sanitised. It is what a live title
	// is replaced with when the terminal is handed back.
	base string
	// liveTitle and liveProgress say whether SetTitle and SetProgress may
	// write at all, decided once at Start.
	liveTitle    bool
	liveProgress bool
	// title and progress are what was last written.
	title    string
	progress Progress
}

// titleSequence sets the window and icon title (OSC 0). The text must already
// be sanitised: the string ends at the first BEL.
func titleSequence(title string) string {
	return "\x1b]0;" + title + "\x07"
}

// progressSequence is the ConEmu progress sequence (OSC 9;4), with the state
// and the value both always present. Paused and error carry a full bar so the
// state has something to colour; the other two states ignore the value.
func progressSequence(p Progress) string {
	state, value := "0", "0"
	switch p {
	case ProgressBusy:
		state = "3"
	case ProgressPaused:
		state, value = "4", "100"
	case ProgressError:
		state, value = "2", "100"
	}
	return "\x1b]9;4;" + state + ";" + value + "\x07"
}

// SetTitle makes title the tab's title, or puts back the one Start set when
// title is empty. It writes nothing when the title is already that.
//
// The text is content: it names a session and a tool. It is cut down by the
// same rule as the starting title, so it cannot end the sequence early or
// start another.
func (t *Terminal) SetTitle(title string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.started || t.out == nil || !t.status.liveTitle {
		return
	}
	title = sanitizeTitle(title)
	if title == "" {
		title = t.status.base
	}
	if title == t.status.title {
		return
	}
	if _, err := io.WriteString(t.out, titleSequence(title)); err == nil {
		t.status.title = title
	}
}

// SetProgress sets the terminal's progress indicator. It writes nothing when
// the indicator already shows p, and nothing at all on a terminal that is not
// known to understand the sequence (see progressAllowed).
func (t *Terminal) SetProgress(p Progress) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.started || t.out == nil || !t.status.liveProgress || p == t.status.progress {
		return
	}
	if _, err := io.WriteString(t.out, progressSequence(p)); err == nil {
		t.status.progress = p
	}
}

// settleStatusLocked returns what must be written to hand the terminal back
// with no progress showing and the plain title on its tab, and records that it
// was. The next SetTitle and SetProgress therefore write again, which is what
// a resume after a suspend needs.
//
// The title is put back explicitly as well as popped by the caller, because not
// every terminal keeps a title stack, and one that does not would otherwise be
// left with whatever the last live title said.
func (t *Terminal) settleStatusLocked() string {
	var b strings.Builder
	if t.status.progress != ProgressNone {
		b.WriteString(progressSequence(ProgressNone))
		t.status.progress = ProgressNone
	}
	if t.status.title != t.status.base {
		b.WriteString(titleSequence(t.status.base))
		t.status.title = t.status.base
	}
	return b.String()
}

// newStatus decides, once, what this run may write.
func newStatus(title string, getenv func(string) string) status {
	base := sanitizeTitle(title)
	return status{
		base:  base,
		title: base,
		// With no starting title there is nothing to put back, so the title is
		// left alone for the whole run.
		liveTitle:    base != "" && liveTitleAllowed(getenv),
		liveProgress: progressAllowed(getenv),
	}
}

func envSwitch(v string) (on, set bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "on", "true", "yes", "enabled":
		return true, true
	case "0", "off", "false", "no", "disabled":
		return false, true
	}
	return false, false
}

// liveTitleAllowed reports whether the title may change during a run.
//
// MANVI_TERM_TITLE=off is the operator's switch and leaves the one static
// title Start writes. Without it, the title follows the run everywhere a
// title sequence is known to be read as one, which leaves out three places:
// GNU screen, where a title sequence can reach the outer terminal as a bell;
// the Linux console, whose handling of these strings depends on the kernel;
// and a terminal that says it is dumb or says nothing.
func liveTitleAllowed(getenv func(string) string) bool {
	if on, set := envSwitch(getenv("MANVI_TERM_TITLE")); set {
		return on
	}
	if getenv("STY") != "" {
		return false
	}
	switch getenv("TERM") {
	case "", "dumb", "linux":
		return false
	}
	return true
}

// progressAllowed reports whether this terminal is known to draw OSC 9;4 as a
// progress indicator.
//
// It is a list of terminals, not a capability probe, and the list is short on
// purpose. OSC 9 began as "post a notification", and a terminal that still
// reads it that way turns every state change into a popup that says "4;3;0".
// Sending the sequence to a terminal that might do that is worse than not
// sending it, so only terminals whose own documentation describes the progress
// form are named, each by the variable it sets:
//
//   - Windows Terminal (WT_SESSION)
//   - ConEmu, where the sequence comes from (ConEmuANSI=ON)
//   - Ghostty from 1.2.0 (TERM_PROGRAM=ghostty and its version)
//
// The first two variables and their values are in those terminals' own
// documentation. Ghostty's names TERM_PROGRAM and TERM_PROGRAM_VERSION as how
// a program detects it without printing the value, and "ghostty" has not been
// checked against a running one. If it is wrong the indicator stays off there,
// which is the safe way for this to be wrong.
//
// Others draw it too, and MANVI_TERM_PROGRESS=on is how an operator on one of
// them says so; =off turns it off everywhere. Inside tmux or GNU screen the
// answer is no unless the operator says otherwise: the variables above are
// inherited from the terminal the multiplexer was started in, which is not
// necessarily the one attached now.
func progressAllowed(getenv func(string) string) bool {
	if on, set := envSwitch(getenv("MANVI_TERM_PROGRESS")); set {
		return on
	}
	if getenv("TMUX") != "" || getenv("STY") != "" {
		return false
	}
	switch {
	case getenv("WT_SESSION") != "":
		return true
	case strings.EqualFold(getenv("ConEmuANSI"), "ON"):
		return true
	case getenv("TERM_PROGRAM") == "ghostty":
		return versionAtLeast(getenv("TERM_PROGRAM_VERSION"), 1, 2)
	}
	return false
}

// versionAtLeast reports whether v, read as major.minor with anything after
// ignored, is at least the given version. A version that cannot be read is
// not at least anything: an unknown build is not a confirmed one.
func versionAtLeast(v string, major, minor int) bool {
	parts := strings.SplitN(strings.TrimSpace(v), ".", 3)
	if len(parts) < 2 {
		return false
	}
	gotMajor, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	// The minor field may carry a suffix on a development build ("2-dev").
	digits := parts[1]
	for i, r := range digits {
		if r < '0' || r > '9' {
			digits = digits[:i]
			break
		}
	}
	gotMinor, err := strconv.Atoi(digits)
	if err != nil {
		return false
	}
	return gotMajor > major || (gotMajor == major && gotMinor >= minor)
}

// osGetenv is the environment newStatus reads in production.
var osGetenv = os.Getenv
