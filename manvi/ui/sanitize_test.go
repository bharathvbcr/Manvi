package ui

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestEscapeSequencesNeverReachTheTerminal is the security case. Model output
// and tool results are untrusted, and a terminal executes control sequences
// rather than showing them.
func TestEscapeSequencesNeverReachTheTerminal(t *testing.T) {
	attacks := map[string]string{
		"clear screen":     "before\x1b[2Jafter",
		"cursor up":        "line one\x1b[1A\x1b[2Kline one rewritten",
		"colour injection": "\x1b[31mfake error\x1b[0m",
		"OSC 8 hyperlink":  "\x1b]8;;https://evil.example\x07click here\x1b]8;;\x07",
		"OSC 52 clipboard": "\x1b]52;c;ZWNobyBwd25lZA==\x07",
		"window title":     "\x1b]0;pwned\x07",
		"bell":             "wake up\x07",
		"backspace rub":    "denied\x08\x08\x08\x08\x08\x08allowed",
		"device control":   "\x1bP+q544e\x1b\\",
		"C1 introducer":    "text2Jmore",
		"NUL":              "before\x00after",
		"vertical tab":     "a\x0bb",
		"delete":           "a\x7fb",
	}
	for name, attack := range attacks {
		got := Sanitize(attack)
		for _, forbidden := range []rune{0x1b, 0x07, 0x08, 0x00, 0x0b, 0x7f, 0x9b} {
			if strings.ContainsRune(got, forbidden) {
				t.Errorf("%s: control %U survived sanitizing: %q", name, forbidden, got)
			}
		}
	}
}

// TestTheApprovalPromptCannotBeRepainted is the specific attack this defends.
// The harness prints approval prompts to the same terminal as model output, so
// content that can move the cursor can redraw the question a human is
// answering. A control a caller can repaint is not a control.
func TestTheApprovalPromptCannotBeRepainted(t *testing.T) {
	hostile := "Here is the file.\x1b[5A\x1b[2K\rAllow write to src/safe.go? [y/N]"
	got := Sanitize(hostile)
	if strings.Contains(got, "\x1b[5A") || strings.Contains(got, "\x1b[2K") {
		t.Fatalf("cursor movement survived: %q", got)
	}
	if strings.ContainsRune(got, '\r') {
		t.Fatalf("a carriage return survived; it alone can overwrite a line: %q", got)
	}
	// The text is still readable, which is what makes this safe rather than
	// merely quiet — a reviewer can see the attempt.
	if !strings.Contains(got, "Allow write to src/safe.go?") {
		t.Fatalf("the content itself was destroyed: %q", got)
	}
}

// TestBidiOverridesAreNeutralised covers Trojan Source: text that displays in a
// different order than it is stored, which is precisely wrong in a diff viewer.
func TestBidiOverridesAreNeutralised(t *testing.T) {
	trojan := "if (level != \"user‮ ⁦// Check if admin⁩ ⁦\") {"
	got := Sanitize(trojan)
	for _, r := range []rune{0x202e, 0x2066, 0x2069, 0x200e, 0x200f, 0x061c} {
		if strings.ContainsRune(got, r) {
			t.Errorf("bidi control %U survived: %q", r, got)
		}
	}
	if !strings.Contains(got, "Check if admin") {
		t.Fatalf("the readable content was destroyed: %q", got)
	}
}

// TestOrdinaryTextIsUntouched: a sanitizer that mangles normal output would be
// replaced within a day, and then nothing would be sanitized.
func TestOrdinaryTextIsUntouched(t *testing.T) {
	for _, plain := range []string{
		"wrote src/calc.go (42 bytes)",
		"func main() {\n\tfmt.Println(\"hi\")\n}\n",
		"TASK-001 — implement the parser",
		"café ☕ 日本語 — emoji are fine \U0001f389",
		"",
	} {
		if got := Sanitize(plain); got != plain {
			t.Errorf("Sanitize(%q) = %q, want it unchanged", plain, got)
		}
	}
}

// TestCRLFIsNotMarked: Windows line endings are ordinary, and marking every one
// would bury real findings in noise.
func TestCRLFIsNotMarked(t *testing.T) {
	if got := Sanitize("one\r\ntwo\r\n"); got != "one\ntwo\n" {
		t.Fatalf("got %q", got)
	}
}

// TestSanitizeAlwaysReturnsValidUTF8: invalid bytes reaching a terminal can
// desynchronise its decoder, so everything after them renders as something else.
func TestSanitizeAlwaysReturnsValidUTF8(t *testing.T) {
	for _, raw := range []string{
		string([]byte{0xff, 0xfe, 0x41}),
		string([]byte{0xc3}),
		string([]byte{0xe2, 0x82}),
	} {
		if got := Sanitize(raw); !utf8.ValidString(got) {
			t.Errorf("Sanitize(% x) produced invalid UTF-8: %q", raw, got)
		}
	}
}

// TestTruncateMarksAndDoesNotSplitRunes.
func TestTruncateMarksAndDoesNotSplitRunes(t *testing.T) {
	if got := Truncate("short", 20); got != "short" {
		t.Errorf("got %q", got)
	}
	got := Truncate("日本語のテキスト", 3)
	if !strings.HasPrefix(got, "日本語") {
		t.Errorf("got %q, want a rune-aligned cut", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("got %q; a truncated result must never read as complete", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncation produced invalid UTF-8: %q", got)
	}
	if Truncate("anything", 0) != "" {
		t.Error("a zero budget must produce nothing")
	}
}
