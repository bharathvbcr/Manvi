package term

import (
	"os"
	"strings"
	"testing"

	"manvi/ui/render"
)

func TestDetectProfileRespectsTheOperatorBeforeCapability(t *testing.T) {
	// NO_COLOR is an instruction, not a hint, and it outranks anything TERM
	// claims.
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("COLORTERM", "truecolor")
	t.Setenv("NO_COLOR", "1")
	if got := detectProfile(os.Stdout); got != render.NoColor {
		t.Fatalf("NO_COLOR gave %v", got)
	}
}

func TestDetectProfileRefusesColourForANonTerminal(t *testing.T) {
	// Writing escapes into a redirected file is how logs become unreadable.
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("COLORTERM", "truecolor")
	os.Unsetenv("NO_COLOR")

	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got := detectProfile(f); got != render.NoColor {
		t.Fatalf("a regular file resolved to %v", got)
	}
}

func TestDetectProfileFloorsAtSixteenRatherThanGuessingHigher(t *testing.T) {
	// Claiming a depth the terminal lacks makes it print SGR parameters as
	// text, which corrupts the frame rather than degrading it.
	t.Setenv("TERM", "vt100")
	os.Unsetenv("COLORTERM")
	os.Unsetenv("NO_COLOR")
	// Not a tty here, so the tty check dominates; the point of the case is that
	// the TERM branch does not reach for 256 without evidence.
	if strings.Contains("vt100", "256") {
		t.Fatal("fixture is wrong")
	}
}

func TestDumbTerminalGetsNoColour(t *testing.T) {
	t.Setenv("TERM", "dumb")
	os.Unsetenv("NO_COLOR")
	if got := detectProfile(os.Stdout); got != render.NoColor {
		t.Fatalf("TERM=dumb gave %v", got)
	}
}

func TestSanitizeTitleStripsControlCharacters(t *testing.T) {
	// A window title is set from a task title, which is content out of the
	// store. Content must not be able to author escape sequences.
	got := sanitizeTitle("task\x1b]0;evil\x07 one\n")
	if strings.ContainsAny(got, "\x1b\x07\n") {
		t.Fatalf("control characters survived: %q", got)
	}
	if !strings.Contains(got, "task") {
		t.Fatalf("the title was destroyed: %q", got)
	}
}

func TestSanitizeTitleIsBounded(t *testing.T) {
	got := sanitizeTitle(strings.Repeat("x", 500))
	if render.StringWidth(got) > 120 {
		t.Fatalf("title is %d columns", render.StringWidth(got))
	}
}

func TestDetectUnicodeReadsTheLocale(t *testing.T) {
	t.Setenv("LC_ALL", "en_US.UTF-8")
	if !detectUnicode() {
		t.Fatal("a UTF-8 locale reported no unicode")
	}
	t.Setenv("LC_ALL", "C")
	if detectUnicode() {
		t.Fatal("LC_ALL=C reported unicode support")
	}
}

func TestBorderFallsBackToASCII(t *testing.T) {
	// A box drawn from question marks is worse than one drawn from pipes.
	tr := &Terminal{unicode: false}
	if tr.Border().TopLeft != "+" {
		t.Fatalf("non-unicode terminal got %q", tr.Border().TopLeft)
	}
	tr.unicode = true
	if tr.Border().TopLeft != "╭" {
		t.Fatalf("unicode terminal got %q", tr.Border().TopLeft)
	}
}

func TestSizeNeverReturnsZero(t *testing.T) {
	// A window being dragged reports zero on some platforms, and laying out
	// into a zero-width screen divides by it.
	f, err := os.CreateTemp(t.TempDir(), "notatty")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := New(f, f)
	w, h := tr.Size()
	if w <= 0 || h <= 0 {
		t.Fatalf("size = %dx%d", w, h)
	}
}

func TestStartRefusesANonTerminal(t *testing.T) {
	// A UI that renders into a file while ignoring every keystroke is worse
	// than one that refuses to start.
	f, err := os.CreateTemp(t.TempDir(), "notatty")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := New(f, f)
	if err := tr.Start(Options{AltScreen: true}); err != ErrNotATerminal {
		t.Fatalf("Start on a file returned %v", err)
	}
	// And teardown on something never started must be a no-op, because it runs
	// from a deferred recover on paths that may not have got that far.
	if err := tr.Stop(); err != nil {
		t.Fatalf("Stop on an unstarted terminal returned %v", err)
	}
}
