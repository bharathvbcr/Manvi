package render

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestOSC8EmitsAndClosesLinksBeforeReset(t *testing.T) {
	p := NewPainter(40, 2, TrueColor)
	linkStyle := Plain.Foreground(Hex("#58a6ff")).With(Underline)
	line := Styled("see ", Plain).
		AppendLink("inspector.go", "file:///repo/inspector.go", linkStyle).
		Append(" and ", Plain).
		AppendLink("docs", "https://example.com/docs", linkStyle)
	line.Draw(p.Buffer(), 0, 0)

	var out bytes.Buffer
	if err := p.Flush(&out, Cursor{}); err != nil {
		t.Fatalf("flush: %v", err)
	}
	got := out.String()

	wantOpen1 := "\x1b]8;;file:///repo/inspector.go\x1b\\"
	wantOpen2 := "\x1b]8;;https://example.com/docs\x1b\\"
	wantClose := "\x1b]8;;\x1b\\"

	if !strings.Contains(got, wantOpen1) {
		t.Fatalf("frame missing first OSC 8 open sequence %q:\n%q", wantOpen1, got)
	}
	if !strings.Contains(got, wantOpen2) {
		t.Fatalf("frame missing second OSC 8 open sequence %q:\n%q", wantOpen2, got)
	}
	if count := strings.Count(got, wantClose); count < 2 {
		t.Fatalf("expected at least 2 OSC 8 close sequences, got %d in %q", count, got)
	}
	if !strings.HasSuffix(got, "\x1b[0m") {
		t.Fatalf("frame must still end with SGR reset \\x1b[0m, got %q", got)
	}

	// Specifically test a link sitting on the very last painted cell of the frame:
	// OSC 8 close MUST precede the final \x1b[0m reset.
	p2 := NewPainter(4, 1, TrueColor)
	Styled("a", Plain).AppendLink("bcd", "https://example.com/end", linkStyle).Draw(p2.Buffer(), 0, 0)
	out.Reset()
	if err := p2.Flush(&out, Cursor{}); err != nil {
		t.Fatalf("flush tail link: %v", err)
	}
	tail := out.String()
	if !strings.HasSuffix(tail, wantClose+"\x1b[0m") {
		t.Fatalf("tail link did not close OSC 8 immediately before \\x1b[0m: %q", tail)
	}
}

func TestOSC8IdleFrameEmitsZeroBytesAndDetectsLinkChange(t *testing.T) {
	p := NewPainter(30, 2, TrueColor)
	draw := func(url string) {
		p.Buffer().Clear(Plain)
		Styled("open ", Plain).AppendLink("file.go", url, Plain).Draw(p.Buffer(), 0, 0)
	}

	draw("file:///repo/a.go")
	var out bytes.Buffer
	if err := p.Flush(&out, Cursor{X: 0, Y: 0, Visible: true}); err != nil {
		t.Fatalf("frame 1: %v", err)
	}

	// Frame 2: Clear + identical link re-registered -> 0 bytes on the wire.
	draw("file:///repo/a.go")
	out.Reset()
	if err := p.Flush(&out, Cursor{X: 0, Y: 0, Visible: true}); err != nil {
		t.Fatalf("frame 2: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("idle linked frame emitted %d bytes: %q", out.Len(), out.String())
	}

	// Frame 3: same visible text and style, different URL in same LinkID slot -> must repaint.
	draw("file:///repo/b.go")
	out.Reset()
	if err := p.Flush(&out, Cursor{X: 0, Y: 0, Visible: true}); err != nil {
		t.Fatalf("frame 3: %v", err)
	}
	if !strings.Contains(out.String(), "\x1b]8;;file:///repo/b.go\x1b\\") {
		t.Fatalf("URL change with identical text/LinkID did not repaint: %q", out.String())
	}

	// Frame 4: link removed, same text -> same(x,y) detects LinkID 0 != 1 and closes link.
	p.Buffer().Clear(Plain)
	Styled("open file.go", Plain).Draw(p.Buffer(), 0, 0)
	out.Reset()
	if err := p.Flush(&out, Cursor{X: 0, Y: 0, Visible: true}); err != nil {
		t.Fatalf("frame 4: %v", err)
	}
	if out.Len() == 0 {
		t.Fatal("removing link from cell emitted 0 bytes")
	}
}

func TestOSC8SanitizesControlAndEscapeInjection(t *testing.T) {
	attacks := []struct {
		name string
		url  string
		bad  []string
	}{
		{
			name: "esc st osc52 injection",
			url:  "https://example.com\x1b\\ \x1b]52;c;bWFsaWNl\x07",
			bad:  []string{"\x1b]52;", "\x07"},
		},
		{
			name: "bel termination injection",
			url:  "https://example.com\x07\x1b[2J",
			bad:  []string{"\x07", "\x1b[2J"},
		},
		{
			name: "c1 st and osc injection",
			url:  "https://example.com\u009c\u009d52;c;YQ==\u009c",
			bad:  []string{"\u009c", "\u009d"},
		},
		{
			name: "c1 csi injection",
			url:  "https://example.com\u009b31m",
			bad:  []string{"\u009b"},
		},
		{
			name: "nul cr lf del",
			url:  "https://example.com/\x00a\rb\nc\x7fd",
			bad:  []string{"\x00", "\r", "\n", "\x7f"},
		},
		{
			name: "only control chars drops link",
			url:  "\x1b\x07\u009c",
			bad:  []string{"\x07", "\u009c"},
		},
		{
			name: "disallowed javascript scheme",
			url:  "javascript:alert(1)",
			bad:  []string{"\x1b]8;;", "javascript:"},
		},
		{
			name: "disallowed data scheme",
			url:  "data:text/html,<script>alert(1)</script>",
			bad:  []string{"\x1b]8;;", "data:"},
		},
		{
			name: "disallowed ftp scheme",
			url:  "ftp://example.com/file",
			bad:  []string{"\x1b]8;;", "ftp://"},
		},
	}

	for _, tc := range attacks {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPainter(40, 1, TrueColor)
			if id := p.Buffer().RegisterLink(tc.url); id != 0 {
				t.Fatalf("RegisterLink(%q) = %d, want 0 (refuse unsafe/disallowed URL)", tc.url, id)
			}
			Styled("", Plain).AppendLink("click", tc.url, Plain).Draw(p.Buffer(), 0, 0)
			var out bytes.Buffer
			if err := p.Flush(&out, Cursor{}); err != nil {
				t.Fatalf("flush: %v", err)
			}
			got := out.String()
			for _, seq := range tc.bad {
				if strings.Contains(got, seq) {
					t.Fatalf("injected/disallowed sequence %q survived in %q", seq, got)
				}
			}
			if strings.Contains(got, "\x1b]8;;") {
				t.Fatalf("unsafe URL must not emit any OSC 8 sequence, got %q", got)
			}
		})
	}
}

func TestOSC8WideRuneContinuationAndEraseRestCloseLink(t *testing.T) {
	// Wide rune continuation cell must carry the same LinkID.
	p := NewPainter(30, 2, TrueColor)
	Styled("", Plain).AppendLink("漢字", "https://example.com/cjk", Plain).Draw(p.Buffer(), 0, 0)
	if id0, id1 := p.Buffer().Cell(0, 0).LinkID, p.Buffer().Cell(1, 0).LinkID; id0 == 0 || id0 != id1 {
		t.Fatalf("wide rune continuation LinkID = %d, head LinkID = %d", id1, id0)
	}

	// Row 0 ends with an open link at the right margin (no trailing blanks), and
	// row 1 changes from non-blank to uniform blanks (triggering eraseRest \x1b[K).
	// The open link from row 0 must close BEFORE \x1b[K.
	p.Buffer().Clear(Plain)
	Styled("", Plain).AppendLink(strings.Repeat("a", 30), "https://example.com/row0", Plain).Draw(p.Buffer(), 0, 0)
	p.Buffer().SetString(0, 1, strings.Repeat("b", 30), Plain)
	var out bytes.Buffer
	if err := p.Flush(&out, Cursor{}); err != nil {
		t.Fatalf("frame 1: %v", err)
	}

	p.Buffer().Clear(Plain)
	Styled("", Plain).AppendLink(strings.Repeat("c", 30), "https://example.com/row0b", Plain).Draw(p.Buffer(), 0, 0)
	out.Reset()
	if err := p.Flush(&out, Cursor{}); err != nil {
		t.Fatalf("frame 2: %v", err)
	}
	got := out.String()
	closeIdx := strings.Index(got, "\x1b]8;;\x1b\\")
	elIdx := strings.Index(got, "\x1b[K")
	if closeIdx < 0 || elIdx < 0 || closeIdx > elIdx {
		t.Fatalf("open link must close before \\x1b[K: closeIdx=%d elIdx=%d out=%q", closeIdx, elIdx, got)
	}
}

func TestOSC8InterningCapAndZeroAllocReuse(t *testing.T) {
	b := NewBuffer(20, 2)

	// Interning returns same ID for identical clean URLs.
	id1 := b.RegisterLink("file:///a.go")
	id2 := b.RegisterLink("file:///a.go")
	if id1 == 0 || id1 != id2 {
		t.Fatalf("expected identical non-zero LinkID, got %d and %d", id1, id2)
	}
	if b.RegisterLink("") != 0 {
		t.Fatal("empty URL must return LinkID 0")
	}

	// Fill up to MaxLinksPerFrame.
	b.Clear(Plain)
	for i := 0; i < MaxLinksPerFrame; i++ {
		id := b.RegisterLink(fmt.Sprintf("file:///f%d.go", i))
		if id != uint16(i+1) {
			t.Fatalf("link %d got id %d, want %d", i, id, i+1)
		}
	}
	// Existing link still resolves its ID even at capacity.
	if got := b.RegisterLink("file:///f0.go"); got != 1 {
		t.Fatalf("existing link at cap returned %d, want 1", got)
	}
	// New link past MaxLinksPerFrame fails closed to 0.
	if got := b.RegisterLink("file:///overflow.go"); got != 0 {
		t.Fatalf("overflow link returned %d, want 0", got)
	}

	// Clear resets table; Resize also resets table.
	b.Clear(Plain)
	if b.LinkURL(1) != "" {
		t.Fatalf("Clear did not clear link table: %q", b.LinkURL(1))
	}
	b.RegisterLink("file:///pre-resize.go")
	b.Resize(25, 3)
	if b.LinkURL(1) != "" {
		t.Fatalf("Resize did not clear link table: %q", b.LinkURL(1))
	}

	// Warmed-up per-frame RegisterLink for clean URLs must not allocate.
	urls := []string{"file:///one.go", "file:///two.go", "https://example.com"}
	b.Clear(Plain)
	for _, u := range urls {
		b.RegisterLink(u)
	}
	allocs := testing.AllocsPerRun(20, func() {
		b.Clear(Plain)
		for _, u := range urls {
			_ = b.RegisterLink(u)
		}
	})
	if allocs != 0 {
		t.Fatalf("RegisterLink allocated %.1f times per frame after warmup, want 0", allocs)
	}
}

func TestOSC8SpanTruncateAndWrapPreserveLink(t *testing.T) {
	url := "file:///long/path/file.go"
	line := Styled("prefix ", Plain).AppendLink("verylongfilename.go", url, Plain)

	trunc := line.Truncate(14)
	if len(trunc) < 2 || trunc[len(trunc)-1].Link != url {
		t.Fatalf("Truncate dropped link on truncated span: %+v", trunc)
	}

	wrapped := line.Wrap(12)
	if len(wrapped) < 2 {
		t.Fatalf("expected wrapped lines, got %d", len(wrapped))
	}
	for i, wl := range wrapped {
		for _, sp := range wl {
			if strings.Contains(sp.Text, "very") || strings.Contains(sp.Text, "file") || strings.Contains(sp.Text, ".go") {
				if sp.Link != url {
					t.Fatalf("wrapped row %d span %q lost Link %q: got %q", i, sp.Text, url, sp.Link)
				}
			}
		}
	}
}
