package input

import (
	"io"
	"strings"
	"testing"
	"time"
)

// decodeAll runs the decoder over a complete input, as the pump would once
// every byte has arrived.
func decodeAll(t *testing.T, in string) []Event {
	t.Helper()
	buf := []byte(in)
	var out []Event
	for len(buf) > 0 {
		ev, n := decode(buf, true)
		if n == 0 {
			t.Fatalf("decode consumed nothing under flush on %q; the pump would spin", buf)
		}
		buf = buf[n:]
		if ev != nil {
			out = append(out, ev)
		}
	}
	return out
}

func single(t *testing.T, in string) Event {
	t.Helper()
	got := decodeAll(t, in)
	if len(got) != 1 {
		t.Fatalf("decoding %q produced %d events, want 1: %#v", in, len(got), got)
	}
	return got[0]
}

func TestPrintableAndControlBytes(t *testing.T) {
	cases := []struct {
		in   string
		want string
		why  string
	}{
		{"a", "a", "plain letter"},
		{"A", "A", "shift is not reported for printable characters"},
		{"\x01", "ctrl+a", "control byte maps to ctrl plus its letter"},
		{"\x03", "ctrl+c", "the interrupt the application must handle itself"},
		{"\r", "enter", "with ICRNL cleared, Enter is carriage return"},
		{"\n", "ctrl+j", "line feed is Ctrl+J, which is why ICRNL must be cleared"},
		{"\t", "tab", ""},
		{"\x7f", "backspace", "DEL is what most terminals send for Backspace"},
		{"\x08", "backspace", "some send BS instead, and both must delete"},
		{" ", "space", ""},
		{"\x00", "ctrl+space", ""},
	}
	for _, c := range cases {
		if got := single(t, c.in).(Key).String(); got != c.want {
			t.Errorf("decode(%q) = %q, want %q (%s)", c.in, got, c.want, c.why)
		}
	}
}

func TestArrowsInBothEncodings(t *testing.T) {
	// Terminals switch into application-cursor mode without asking. An
	// application that decodes only CSI loses its arrow keys when they do.
	for _, in := range []string{"\x1b[A", "\x1bOA"} {
		if got := single(t, in).(Key).String(); got != "up" {
			t.Errorf("decode(%q) = %q, want up", in, got)
		}
	}
	for _, in := range []string{"\x1b[D", "\x1bOD"} {
		if got := single(t, in).(Key).String(); got != "left" {
			t.Errorf("decode(%q) = %q, want left", in, got)
		}
	}
}

func TestModifiedKeys(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\x1b[1;5A", "ctrl+up"},
		{"\x1b[1;2A", "shift+up"},
		{"\x1b[1;3A", "alt+up"},
		{"\x1b[1;6A", "ctrl+shift+up"},
		{"\x1b[1;5H", "ctrl+home"},
		{"\x1b[3;5~", "ctrl+delete"},
		{"\x1b[Z", "shift+tab"},
	}
	for _, c := range cases {
		if got := single(t, c.in).(Key).String(); got != c.want {
			t.Errorf("decode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFunctionAndNavigationKeys(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\x1b[2~", "insert"}, {"\x1b[3~", "delete"},
		{"\x1b[5~", "pgup"}, {"\x1b[6~", "pgdn"},
		{"\x1b[1~", "home"}, {"\x1b[4~", "end"},
		{"\x1b[H", "home"}, {"\x1b[F", "end"},
		{"\x1bOP", "f1"}, {"\x1b[11~", "f1"}, {"\x1b[P", "f1"},
		{"\x1b[15~", "f5"}, {"\x1b[24~", "f12"},
	}
	for _, c := range cases {
		if got := single(t, c.in).(Key).String(); got != c.want {
			t.Errorf("decode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAltPrefix(t *testing.T) {
	if got := single(t, "\x1bb").(Key).String(); got != "alt+b" {
		t.Errorf("ESC b = %q, want alt+b", got)
	}
	if got := single(t, "\x1b\x7f").(Key).String(); got != "alt+backspace" {
		t.Errorf("ESC DEL = %q, want alt+backspace", got)
	}
}

func TestLoneEscapeAndDoubleEscape(t *testing.T) {
	if got := single(t, "\x1b").(Key).Type; got != KeyEscape {
		t.Errorf("a flushed lone ESC = %v, want KeyEscape", got)
	}
	// Double Escape is a binding — clear the draft, open the rewind picker —
	// and both presses can land in one read.
	got := decodeAll(t, "\x1b\x1b")
	if len(got) != 2 {
		t.Fatalf("ESC ESC produced %d events, want 2: %#v", len(got), got)
	}
	for i, ev := range got {
		if k, ok := ev.(Key); !ok || k.Type != KeyEscape {
			t.Fatalf("event %d = %#v, want Escape", i, ev)
		}
	}
}

func TestIncompleteSequenceAsksForMoreBytes(t *testing.T) {
	// The pump must wait rather than emitting "[" into the user's prompt.
	for _, partial := range []string{"\x1b", "\x1b[", "\x1b[1", "\x1b[1;", "\x1b[1;5", "\x1bO"} {
		if ev, n := decode([]byte(partial), false); n != 0 || ev != nil {
			t.Errorf("decode(%q, more-coming) = %#v/%d, want a request for more bytes", partial, ev, n)
		}
	}
}

func TestSplitSequenceReassembles(t *testing.T) {
	// An arrow key over a slow link arrives in pieces. Splitting it at every
	// possible point must still yield one arrow key.
	full := "\x1b[1;5C"
	for split := 1; split < len(full); split++ {
		buf := []byte(full[:split])
		if ev, n := decode(buf, false); n != 0 {
			t.Fatalf("prefix %q decoded to %#v before it was complete", full[:split], ev)
		}
		buf = append(buf, full[split:]...)
		ev, n := decode(buf, false)
		if n != len(full) {
			t.Fatalf("reassembled %q consumed %d bytes, want %d", full, n, len(full))
		}
		if got := ev.(Key).String(); got != "ctrl+right" {
			t.Fatalf("reassembled %q = %q, want ctrl+right", full, got)
		}
	}
}

func TestSplitUTF8RuneIsNotCorrupted(t *testing.T) {
	// A multi-byte character typed fast, or pasted, can straddle two reads.
	// Answering early replaces it with U+FFFD permanently.
	full := []byte("漢")
	if ev, n := decode(full[:2], false); n != 0 {
		t.Fatalf("a partial rune decoded to %#v; it must ask for more bytes", ev)
	}
	ev, n := decode(full, false)
	if n != len(full) || ev.(Key).Rune() != '漢' {
		t.Fatalf("complete rune decoded to %#v consuming %d", ev, n)
	}
}

func TestBracketedPaste(t *testing.T) {
	ev := single(t, "\x1b[200~hello\nworld\x1b[201~")
	p, ok := ev.(Paste)
	if !ok {
		t.Fatalf("got %#v, want a Paste", ev)
	}
	if p.Text != "hello\nworld" {
		t.Fatalf("paste text = %q", p.Text)
	}
}

func TestPasteIsNotSplitIntoKeystrokes(t *testing.T) {
	// This is the whole reason bracketed paste is enabled: without it, a
	// newline inside pasted text is indistinguishable from pressing Enter, and
	// half a paste gets sent to the model.
	got := decodeAll(t, "\x1b[200~line one\nline two\x1b[201~")
	if len(got) != 1 {
		t.Fatalf("paste produced %d events, want exactly 1: %#v", len(got), got)
	}
}

func TestPasteStripsEscapesAndNormalisesCRLF(t *testing.T) {
	// Pasted content is the most direct route untrusted bytes have into the
	// process. Escapes in it must never reach the terminal.
	ev := single(t, "\x1b[200~a\x1b[31mred\x07\r\nb\x1b[201~")
	p := ev.(Paste)
	if strings.ContainsRune(p.Text, 0x1b) || strings.ContainsRune(p.Text, 0x07) {
		t.Fatalf("paste retained control characters: %q", p.Text)
	}
	if !strings.Contains(p.Text, "\n") || strings.Contains(p.Text, "\r") {
		t.Fatalf("CRLF was not normalised: %q", p.Text)
	}
}

func TestUnterminatedPasteIsDeliveredOnFlush(t *testing.T) {
	if ev, n := decode([]byte("\x1b[200~abc"), false); n != 0 {
		t.Fatalf("an open paste decoded early: %#v", ev)
	}
	ev, n := decode([]byte("\x1b[200~abc"), true)
	p, ok := ev.(Paste)
	if !ok || p.Text != "abc" || n != len("\x1b[200~abc") {
		t.Fatalf("flushed open paste = %#v consuming %d", ev, n)
	}
}

func TestMouseSGR(t *testing.T) {
	cases := []struct {
		in     string
		x, y   int
		button MouseButton
		action MouseAction
	}{
		{"\x1b[<0;10;5M", 9, 4, MouseLeft, MousePress},
		{"\x1b[<0;10;5m", 9, 4, MouseLeft, MouseRelease},
		{"\x1b[<2;1;1M", 0, 0, MouseRight, MousePress},
		{"\x1b[<64;3;7M", 2, 6, MouseNone, MouseWheelUp},
		{"\x1b[<65;3;7M", 2, 6, MouseNone, MouseWheelDown},
		{"\x1b[<32;4;4M", 3, 3, MouseLeft, MouseMotion},
	}
	for _, c := range cases {
		m, ok := single(t, c.in).(Mouse)
		if !ok {
			t.Fatalf("decode(%q) is not a Mouse", c.in)
		}
		if m.X != c.x || m.Y != c.y {
			t.Errorf("decode(%q) at %d,%d, want %d,%d — the wire is 1-based and rects are 0-based",
				c.in, m.X, m.Y, c.x, c.y)
		}
		if m.Action != c.action {
			t.Errorf("decode(%q) action = %v, want %v", c.in, m.Action, c.action)
		}
		if m.Action != MouseWheelUp && m.Action != MouseWheelDown && m.Button != c.button {
			t.Errorf("decode(%q) button = %v, want %v", c.in, m.Button, c.button)
		}
	}
}

func TestMouseBeyondColumn223(t *testing.T) {
	// The pre-SGR encoding packs coordinates into single bytes and cannot
	// address a column past 223, which any wide terminal has. This is why SGR
	// mode is the one enabled.
	m := single(t, "\x1b[<0;250;80M").(Mouse)
	if m.X != 249 || m.Y != 79 {
		t.Fatalf("wide-terminal click landed at %d,%d, want 249,79", m.X, m.Y)
	}
}

func TestFocusEvents(t *testing.T) {
	if got := single(t, "\x1b[I").(Focus); !got.In {
		t.Error("ESC [ I should report focus gained")
	}
	if got := single(t, "\x1b[O").(Focus); got.In {
		t.Error("ESC [ O should report focus lost")
	}
}

func TestUnknownCSIIsSwallowedNotTyped(t *testing.T) {
	// A device status report or capability response is not a keystroke, and
	// letting its bytes through puts them in the user's prompt.
	got := decodeAll(t, "\x1b[?1;2c")
	if len(got) != 0 {
		t.Fatalf("an unknown CSI produced %#v; it should be consumed silently", got)
	}
	// And it must not swallow what follows it.
	got = decodeAll(t, "\x1b[?1;2cx")
	if len(got) != 1 || got[0].(Key).Rune() != 'x' {
		t.Fatalf("the key after an unknown CSI was lost: %#v", got)
	}
}

func TestMalformedCSIDoesNotConsumeUnboundedInput(t *testing.T) {
	// Garbage after ESC [ must not eat the rest of the buffer.
	got := decodeAll(t, "\x1b[\x01abc")
	if len(got) == 0 {
		t.Fatal("malformed CSI swallowed everything")
	}
	if k, ok := got[0].(Key); !ok || k.Type != KeyEscape {
		t.Fatalf("first event = %#v, want a lone Escape", got[0])
	}
}

func TestKittyProtocolKeys(t *testing.T) {
	// Terminals that negotiate the kitty protocol send Ctrl+letter here instead
	// of as a control byte; ignoring it silently loses those bindings.
	if got := single(t, "\x1b[99;5u").(Key).String(); got != "ctrl+c" {
		t.Errorf("kitty ctrl+c = %q", got)
	}
	if got := single(t, "\x1b[13;5u").(Key).String(); got != "ctrl+enter" {
		t.Errorf("kitty ctrl+enter = %q", got)
	}
	if got := single(t, "\x1b[9;2u").(Key).String(); got != "shift+tab" {
		t.Errorf("kitty shift+tab = %q", got)
	}
}

func TestReaderDeliversEventsAndClosesOnEOF(t *testing.T) {
	r := NewReader(strings.NewReader("ab\x1b[A"))
	r.SetEscapeDelay(200 * time.Millisecond)
	go r.Run()

	var got []string
	for ev := range r.Events() {
		if k, ok := ev.(Key); ok {
			got = append(got, k.String())
			continue
		}
		if e, ok := ev.(Error); ok && e.Err == io.EOF {
			break
		}
	}
	want := []string{"a", "b", "up"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// slowReader delivers one byte at a time with a gap, standing in for a terminal
// on a slow link.
type slowReader struct {
	data []byte
	gap  time.Duration
	i    int
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.i >= len(s.data) {
		return 0, io.EOF
	}
	time.Sleep(s.gap)
	p[0] = s.data[s.i]
	s.i++
	return 1, nil
}

func TestReaderReassemblesAcrossSlowReads(t *testing.T) {
	// An escape sequence dribbling in one byte at a time must still decode as
	// one key, provided the gaps are inside the grace period.
	r := NewReader(&slowReader{data: []byte("\x1b[1;5C"), gap: time.Millisecond})
	r.SetEscapeDelay(200 * time.Millisecond)
	go r.Run()

	for ev := range r.Events() {
		if k, ok := ev.(Key); ok {
			if k.String() != "ctrl+right" {
				t.Fatalf("slow-link sequence decoded as %q, want ctrl+right", k.String())
			}
			return
		}
	}
	t.Fatal("no key was decoded")
}

func TestReaderResolvesLoneEscapeAfterTheGracePeriod(t *testing.T) {
	// Escape with nothing following must reach the application, or Escape can
	// never cancel a running turn.
	pr, pw := io.Pipe()
	r := NewReader(pr)
	r.SetEscapeDelay(10 * time.Millisecond)
	go r.Run()
	defer r.Close()

	go func() {
		_, _ = pw.Write([]byte{0x1b})
	}()

	select {
	case ev := <-r.Events():
		k, ok := ev.(Key)
		if !ok || k.Type != KeyEscape {
			t.Errorf("got %#v, want a lone Escape", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a lone Escape never arrived; cancel would be unreachable")
	}
}

func TestInjectDeliversResize(t *testing.T) {
	r := NewReader(strings.NewReader(""))
	if !r.Inject(Resize{W: 100, H: 40}) {
		t.Fatal("Inject reported failure on an open reader")
	}
	ev := <-r.Events()
	if got, ok := ev.(Resize); !ok || got.W != 100 {
		t.Fatalf("got %#v, want the injected resize", ev)
	}
	r.Close()
	if r.Inject(Resize{}) {
		t.Fatal("Inject must report failure once closed")
	}
}
