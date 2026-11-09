package codingagent

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestNeutraliseText(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"empty", "", ""},
		{"plain", "hello, wörld ☃ 日本\n\tok", "hello, wörld ☃ 日本\n\tok"},
		{"esc", "a\x1b[31mred", `a\x1b[31mred`},
		{"osc and bell", "\x1b]0;t\x07", `\x1b]0;t\x07`},
		{"all controls", "\x00\x01\x08\x0d\x1b\x7f\u0080\u009f", `\x00\x01\x08\x0d\x1b\x7f\x80\x9f`},
		{"bidi", "a\u202ab\u202ec\u2066d\u2069e\u200ef\u200fg\u061ch", `a\u{202A}b\u{202E}c\u{2066}d\u{2069}e\u{200E}f\u{200F}g\u{061C}h`},
		{"invisible", "a\u2028b\u2029c\ufeffd\u00ade\u180ef\u2060g\u2064h\u206ai\u206fj\ufff9k\ufffbl", `a\u{2028}b\u{2029}c\u{FEFF}d\u{00AD}e\u{180E}f\u{2060}g\u{2064}h\u{206A}i\u{206F}j\u{FFF9}k\u{FFFB}l`},
		{"tag block", "a\U000E0001\U000E0041\U000E007Fb", `a\u{E0001}\u{E0041}\u{E007F}b`},
		{"backslash is escaped", `C:\dir \x1b \u{202E} <U+202E>`, `C:\\dir \\x1b \\u{202E} <U+202E>`},
		{"escape beside literal escape", "\x1b" + `\x1b`, `\x1b\\x1b`},
		{"keeps newline tab and a real replacement char", "a\n\tb�c", "a\n\tb�c"},
		{"invalid utf8", "a\xffb\xc3", `a\xffb\xc3`},
		{"truncated multibyte then control", "\xe2\x82\x1b", `\xe2\x82\x1b`},
		{"valid multibyte beside controls", "€\x1b€", `€\x1b€`},
		{"crlf kept", "a\r\nb\r\n", "a\r\nb\r\n"},
		{"lone cr escaped", "x\ry", `x\x0dy`},
		{"cr cr lf", "\r\r\n", `\x0d` + "\r\n"},
		{"trailing cr escaped", "ab\r", `ab\x0d`},
		{"cr then control then lf", "\r\x1b\n", `\x0d\x1b` + "\n"},
		{"lone surrogate bytes", "\xed\xa0\x80", `\xed\xa0\x80`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := neutraliseText(tc.in)
			if got != tc.want {
				t.Fatalf("neutraliseText(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("output is not valid UTF-8: %q", got)
			}
			// Not idempotent by design (a second pass escapes the first pass's
			// backslashes); instead every backslash must start exactly one escape.
			if !wellFormedEscapes(got) {
				t.Fatalf("output %q holds a backslash that starts no escape", got)
			}
		})
	}
}

// Text with nothing to neutralise is returned as it came, without a copy.
func TestNeutraliseTextCleanInputAllocatesNothing(t *testing.T) {
	s := strings.Repeat("clean text ", 1000)
	if n := testing.AllocsPerRun(20, func() { _ = neutraliseText(s) }); n != 0 {
		t.Fatalf("clean text allocated %v times", n)
	}
}

// benchText is about 1 MiB of unit repeated, cut at a rune boundary.
func benchText(unit string) string {
	text := strings.Repeat(unit, (1<<20)/len(unit)+1)[:1<<20]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}

// Clean CRLF text (Windows tools, Codex output) is returned without a copy.
func TestNeutraliseTextCleanCRLFAllocatesNothing(t *testing.T) {
	s := strings.Repeat("a line of output\r\n", 1000)
	if n := testing.AllocsPerRun(20, func() { _ = neutraliseText(s) }); n != 0 {
		t.Fatalf("clean CRLF text allocated %v times", n)
	}
}

func BenchmarkNeutraliseText1MiB(b *testing.B) {
	for _, tc := range []struct{ name, text string }{
		{"clean", benchText("The quick brown fox, jumps over the lazy dog \u2603.\n")},
		{"controls", benchText("ab\x1b[31mcd\u202eef gh ij kl mn op qr st uv\n")},
		{"backslashes", benchText(`C:\Users\agent\src\main.go: printf("%d\n")` + "\n")},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(tc.text)))
			b.ReportAllocs()
			for b.Loop() {
				_ = neutraliseText(tc.text)
			}
		})
	}
}

// A completed Claude turn accepts a follow-up whatever permission requests went
// unanswered: each is withdrawn at turn end, ahead of the status.
func TestClaudeUnansweredPermissionDoesNotBlockContinuationAfterCompletion(t *testing.T) {
	s := openFixture(t, "perm-then-result", "ask")
	if err := s.StartTurn(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	var seen []string
	var reqs []Request
	for _, e := range drainUntilStatus(t, s.Next) {
		switch {
		case e.Request != nil:
			reqs = append(reqs, *e.Request)
			seen = append(seen, "request:"+e.Request.ID)
		case e.CancelledID != "":
			seen = append(seen, "cancel:"+e.CancelledID)
		case e.Status != "":
			seen = append(seen, "status:"+e.Status)
		}
	}
	if len(reqs) != 2 {
		t.Fatalf("captured %d permission requests, want 2: %v", len(reqs), seen)
	}
	want := []string{"request:" + reqs[0].ID, "request:" + reqs[1].ID, "cancel:" + reqs[0].ID, "cancel:" + reqs[1].ID, "status:completed"}
	if !slices.Equal(seen, want) {
		t.Fatalf("events = %v, want both withdrawals ahead of the completed status: %v", seen, want)
	}
	// A late answer to a withdrawn request is a callback gone, not a stale one.
	if err := s.Respond(t.Context(), reqs[0], Response{Decision: "allow_once"}); !errors.Is(err, ErrCallbackWithdrawn) {
		t.Fatalf("answer after turn end = %v, want ErrCallbackWithdrawn", err)
	}
	if err := s.ContinueTurn(t.Context(), "next"); err != nil {
		t.Fatalf("ContinueTurn after a completed turn with unanswered permissions = %v", err)
	}
}

// A turn that ended in failure is terminal: a follow-up is still refused.
func TestClaudeContinueTurnAfterTerminalStatusIsStillRefused(t *testing.T) {
	s := openFixture(t, "denied", "ask")
	if err := s.StartTurn(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	drainUntilStatus(t, s.Next)
	if err := s.ContinueTurn(t.Context(), "again"); !ContinueRefused(err) {
		t.Fatalf("ContinueTurn on a failed session = %v, want a refusal", err)
	}
}

// A session-cap breach is the cap error for every reader, including one that
// wakes on the cancellation first, and after the frame was already consumed.
func TestConnectionFailRecordsTheCauseBeforeTheCancel(t *testing.T) {
	ctx, fail := context.WithCancelCause(t.Context())
	pr, pw := io.Pipe() // nothing reads it, so a send can only end through the connection's context
	t.Cleanup(func() { _ = pr.Close() })
	c := &connection{ctx: ctx, fail: fail, stdin: pw, frames: make(chan wireRead, 1)}
	cause := errors.New("managed provider output exceeded 32 MiB")
	c.frames <- wireRead{err: cause}
	c.fail(cause)
	for i := 0; i < 50; i++ {
		rctx, stop := context.WithTimeout(t.Context(), time.Second)
		_, _, err := c.receiveFrame(rctx)
		stop()
		if !errors.Is(err, cause) || errors.Is(err, context.Canceled) {
			t.Fatalf("read %d = %v, want the recorded cause", i, err)
		}
	}
	if err := c.sendRaw(t.Context(), []byte("{}")); !errors.Is(err, cause) {
		t.Fatalf("send after the failure = %v, want the recorded cause", err)
	}
}

// A stderr flood ends the connection with its own cap error, which serve reads as
// a failure; a bare cancel would read as an interruption.
func TestLimitedDiscardRecordsTheStderrCapAsTheCause(t *testing.T) {
	ctx, fail := context.WithCancelCause(t.Context())
	c := &connection{ctx: ctx, fail: fail}
	w := &limitedDiscard{remaining: 4, fail: c.fail}
	if _, err := w.Write(make([]byte, 10)); !errors.Is(err, errStderrCap) {
		t.Fatalf("over-cap stderr write = %v, want errStderrCap", err)
	}
	if err := c.err(); !errors.Is(err, errStderrCap) || errors.Is(err, context.Canceled) {
		t.Fatalf("connection ended with %v, want the stderr cap", err)
	}
	rctx, stop := context.WithCancel(t.Context())
	stop()
	if _, err := c.receive(rctx); !errors.Is(err, errStderrCap) {
		t.Fatalf("a reader with its own ctx also cancelled got %v, want the stderr cap", err)
	}
}

// A caller cancel with no recorded failure is still the caller's cancel.
func TestCallerCancelWithoutAFailureIsReportedAsTheCallersCancel(t *testing.T) {
	ctx, fail := context.WithCancelCause(t.Context())
	defer fail(nil)
	c := &connection{ctx: ctx, fail: fail, frames: make(chan wireRead)}
	rctx, stop := context.WithCancel(t.Context())
	stop()
	if _, _, err := c.receiveFrame(rctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("receiveFrame = %v, want the caller's context.Canceled", err)
	}
	fail(nil) // a plain Close is also not a failure
	if c.failure() != nil {
		t.Fatalf("failure() = %v after a plain cancel, want nil", c.failure())
	}
}
