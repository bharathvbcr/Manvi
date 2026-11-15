package term

import (
	"strings"
	"testing"
)

func envOf(pairs ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return func(k string) string { return m[k] }
}

// The progress sequence began life as "post a notification". A terminal that
// still reads it that way shows a popup saying "4;3;0" for every state change,
// so the sequence goes only to terminals named here, and an unknown terminal
// gets nothing. Each row is one terminal as its environment describes it.
func TestProgressGoesOnlyToTerminalsKnownToDrawIt(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		want bool
	}{
		{"nothing known", []string{"TERM", "xterm-256color"}, false},
		{"Windows Terminal", []string{"WT_SESSION", "0c1d"}, true},
		{"ConEmu with ANSI on", []string{"ConEmuANSI", "ON"}, true},
		{"ConEmu with ANSI off", []string{"ConEmuANSI", "OFF"}, false},
		{"Ghostty 1.2.0", []string{"TERM_PROGRAM", "ghostty", "TERM_PROGRAM_VERSION", "1.2.0"}, true},
		{"Ghostty 1.3.1", []string{"TERM_PROGRAM", "ghostty", "TERM_PROGRAM_VERSION", "1.3.1"}, true},
		{"Ghostty 2.0", []string{"TERM_PROGRAM", "ghostty", "TERM_PROGRAM_VERSION", "2.0.0"}, true},
		{"Ghostty development build", []string{"TERM_PROGRAM", "ghostty", "TERM_PROGRAM_VERSION", "1.2-dev+abc"}, true},
		{"Ghostty before progress", []string{"TERM_PROGRAM", "ghostty", "TERM_PROGRAM_VERSION", "1.1.3"}, false},
		{"Ghostty with no version", []string{"TERM_PROGRAM", "ghostty"}, false},
		{"Ghostty with an unreadable version", []string{"TERM_PROGRAM", "ghostty", "TERM_PROGRAM_VERSION", "tip"}, false},
		// These read OSC 9 as a notification in at least some released
		// versions, and nothing here can tell which version is attached.
		{"iTerm2", []string{"TERM_PROGRAM", "iTerm.app", "TERM_PROGRAM_VERSION", "3.5.0"}, false},
		{"WezTerm", []string{"TERM_PROGRAM", "WezTerm", "TERM_PROGRAM_VERSION", "20240203-110809-5046fc22"}, false},
		{"kitty", []string{"TERM", "xterm-kitty", "KITTY_WINDOW_ID", "1"}, false},
		{"Apple Terminal", []string{"TERM_PROGRAM", "Apple_Terminal"}, false},
		// A multiplexer inherits the variables of the terminal it was started
		// in, which is not necessarily the one attached now.
		{"Ghostty inside tmux", []string{"TERM_PROGRAM", "ghostty", "TERM_PROGRAM_VERSION", "1.2.0", "TMUX", "/tmp/tmux-501/default,1,0"}, false},
		{"Windows Terminal inside screen", []string{"WT_SESSION", "0c1d", "STY", "1234.pts-0.host"}, false},
		// The operator outranks both the list and the multiplexer rule.
		{"operator turns it on", []string{"TERM_PROGRAM", "WezTerm", "MANVI_TERM_PROGRESS", "on"}, true},
		{"operator turns it on inside tmux", []string{"TMUX", "x", "MANVI_TERM_PROGRESS", "1"}, true},
		{"operator turns it off", []string{"WT_SESSION", "0c1d", "MANVI_TERM_PROGRESS", "off"}, false},
		{"an unreadable switch is not a switch", []string{"WT_SESSION", "0c1d", "MANVI_TERM_PROGRESS", "maybe"}, true},
	}
	for _, c := range cases {
		if got := progressAllowed(envOf(c.env...)); got != c.want {
			t.Errorf("%s: progress allowed = %v, want %v", c.name, got, c.want)
		}
	}
}

// The title follows the run wherever a title sequence is read as a title, and
// stays the one static title where it might not be.
func TestLiveTitleIsHeldBackWhereATitleSequenceIsNotSafe(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		want bool
	}{
		{"an ordinary terminal", []string{"TERM", "xterm-256color"}, true},
		{"inside tmux", []string{"TERM", "tmux-256color", "TMUX", "x"}, true},
		{"inside GNU screen", []string{"TERM", "screen-256color", "STY", "1234.pts-0.host"}, false},
		{"the Linux console", []string{"TERM", "linux"}, false},
		{"a dumb terminal", []string{"TERM", "dumb"}, false},
		{"no TERM at all", nil, false},
		{"operator turns it off", []string{"TERM", "xterm-256color", "MANVI_TERM_TITLE", "off"}, false},
		{"operator turns it on inside screen", []string{"TERM", "screen", "STY", "1", "MANVI_TERM_TITLE", "on"}, true},
	}
	for _, c := range cases {
		if got := liveTitleAllowed(envOf(c.env...)); got != c.want {
			t.Errorf("%s: live title allowed = %v, want %v", c.name, got, c.want)
		}
	}
	// With no starting title there is nothing to put back afterwards, so the
	// title is never touched, whatever the terminal.
	if newStatus("", envOf("TERM", "xterm-256color")).liveTitle {
		t.Error("a run that set no title may still rewrite it")
	}
}

// Every state is written with both fields, and never as a percentage of
// anything: the only values are the two a state needs to have a bar to colour.
func TestProgressSequencesAreTheFourStatesAndNoPercentage(t *testing.T) {
	want := map[Progress]string{
		ProgressNone:   "\x1b]9;4;0;0\x07",
		ProgressBusy:   "\x1b]9;4;3;0\x07",
		ProgressPaused: "\x1b]9;4;4;100\x07",
		ProgressError:  "\x1b]9;4;2;100\x07",
	}
	for p, seq := range want {
		if got := progressSequence(p); got != seq {
			t.Errorf("progress %d is written %q, want %q", p, got, seq)
		}
	}
	for p := range want {
		if strings.Contains(progressSequence(p), "]9;4;1;") {
			t.Errorf("progress %d is written as a percentage", p)
		}
	}
}

// The one-character forms of the string controls are stripped along with the
// ordinary control characters. U+009C ends a string and U+009D starts one, and
// a terminal that honours them would read a title carrying them as two
// sequences.
func TestSanitizeTitleStripsTheSecondControlRange(t *testing.T) {
	got := sanitizeTitle("run\u009c\u009d0;evil\u0080 one\u009f\u007f")
	for _, r := range got {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			t.Fatalf("control character %U survived: %q", r, got)
		}
	}
	if got != "run0;evil one" {
		t.Fatalf("the printable part was changed: %q", got)
	}
	// U+00A0 and up are ordinary characters, and so is the dot a title is
	// joined with.
	if got := sanitizeTitle("thinking · café — manvi"); got != "thinking · café — manvi" {
		t.Fatalf("printable text was changed: %q", got)
	}
}
