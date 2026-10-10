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

func BenchmarkSanitize(b *testing.B) {
	inputs := map[string]string{
		"ascii":   strings.Repeat("wrote src/calc.go (42 bytes) and ran the tests\n", 40),
		"harness": strings.Repeat("✓ go test ./... — 42 passed, 0 failed · 1.2s\n", 40),
		"cjk":     strings.Repeat("日本語のテキストと café ☕ — emoji \U0001f389\n", 40),
		"hostile": strings.Repeat("ok\x1b[2J — \u202eevil\n", 40),
	}
	for name, in := range inputs {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(in)))
			for i := 0; i < b.N; i++ {
				Sanitize(in)
			}
		})
	}
}

// TestFastPathNeverSkipsWork: isPlain lets Sanitize return its input without
// looking further, so any string it accepts that the rewriting pass would have
// changed is a control sequence reaching the terminal. Checked against the
// slow path itself rather than a list of expectations, because the property
// that matters is the agreement, not any one output.
func TestFastPathNeverSkipsWork(t *testing.T) {
	check := func(in string) {
		t.Helper()
		slow := sanitizeSlow(in)
		if isPlain(in) && slow != in {
			t.Fatalf("isPlain accepted %q, which the slow path rewrites to %q", in, slow)
		}
		if got := Sanitize(in); got != slow {
			t.Fatalf("Sanitize(%q) = %q, slow path gives %q", in, got, slow)
		}
	}

	for _, in := range []string{
		"", "plain ascii", "tab\tand\nnewline", "crlf\r\n", "bare\rreturn",
		"\x00", "\x1b[31mred\x1b[0m", "\x7f", "\x07bell",
		"\u0080", "\u0085next line", "\u009b2J", "\u009f", "\u00a0nbsp", // C1 and just past it
		"zwj \U0001f469\u200d\U0001f4bb family", "\u200b zero width space", "\u200c\u200d",
		"\u061c", "\u200e\u200f", "\u202a\u202b\u202c\u202d\u202e", "\u2066\u2067\u2068\u2069",
		"literal \ufffd replacement", "\ufeffbom", "\u2028line sep\u2029",
		"\u2713 passed \u2014 3 files \u00b7 1.2s", "\u65e5\u672c\u8a9e", "\U0001f389", "\U0010ffff",
		string([]byte{0xff}), string([]byte{0xc3}), string([]byte{0xe2, 0x82}),
		string([]byte{0xc0, 0x80}),             // overlong NUL
		string([]byte{0xed, 0xa0, 0x80}),       // encoded surrogate
		string([]byte{0xf4, 0x90, 0x80, 0x80}), // past U+10FFFF
		"ok" + string([]byte{0xe2, 0x80}) + "\u202e",
	} {
		check(in)
	}

	// Every rune, alone and embedded in text either side of it.
	for r := rune(0); r <= utf8.MaxRune; r++ {
		s := string(r)
		check(s)
		if r%97 == 0 {
			check("a—" + s + "✓b")
		}
	}
	// Every one- and two-byte sequence, which covers the truncated and invalid
	// leads a rune loop never produces.
	for i := 0; i < 256; i++ {
		check(string([]byte{byte(i)}))
		for j := 0; j < 256; j++ {
			check(string([]byte{byte(i), byte(j)}))
		}
	}
}

// FuzzSanitizeFastPath extends the agreement check to arbitrary input.
func FuzzSanitizeFastPath(f *testing.F) {
	for _, seed := range []string{"✓ ok — done", "\x1b[2J", "\u202e", "\xff\xfe", "a\u0085b", "\ufffd"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		slow := sanitizeSlow(in)
		if isPlain(in) && slow != in {
			t.Fatalf("isPlain accepted %q, which the slow path rewrites to %q", in, slow)
		}
	})
}

// TestCommonUnicodeTakesTheFastPath pins the reason the fast path decodes at
// all: the harness's own punctuation must not cost an allocation per string.
func TestCommonUnicodeTakesTheFastPath(t *testing.T) {
	for _, in := range []string{
		"✓ go test ./... — 42 passed · 1.2s",
		"café ☕ 日本語 \U0001f389",
		"zwj 👩\u200d💻",
	} {
		if !isPlain(in) {
			t.Errorf("isPlain(%q) = false; ordinary text is taking the slow path", in)
		}
		if n := testing.AllocsPerRun(10, func() { Sanitize(in) }); n != 0 {
			t.Errorf("Sanitize(%q) allocated %v times", in, n)
		}
	}
}
