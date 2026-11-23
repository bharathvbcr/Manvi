package tui

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/ui"
)

// scrollbackByteBound is the contract the default transcript keeps: a count of
// entries does not bound memory, since one tool result can be megabytes and
// five thousand of them is gigabytes. 32 MiB is what a default Scrollback
// retains, however few entries that is.
const scrollbackByteBound = 32 << 20

// heldBytes is the text the transcript retains. A streamed entry holds its
// text in Body, and Event.Text is the first delta of the same text, so it is
// counted once.
func heldBytes(s *Scrollback) int {
	n := 0
	for _, e := range s.Entries() {
		if e.Body.Len() > 0 {
			n += e.Body.Len()
		} else {
			n += len(e.Event.Text)
		}
	}
	return n
}

func TestScrollbackIsBoundedByBytesAsWellAsEntries(t *testing.T) {
	s := NewScrollback()
	// One shared backing string: the test holds 1 MiB however many entries
	// point at it.
	chunk := strings.Repeat("x", 1<<20)
	const appended = 100
	for i := 0; i < appended; i++ {
		s.Append(ui.Event{Kind: ui.KindNotice, Text: chunk})
	}
	if got := heldBytes(s); got > scrollbackByteBound {
		t.Fatalf("%d bytes retained across %d entries, bound is %d", got, s.Len(), scrollbackByteBound)
	}
	if s.Dropped()+s.Len() != appended {
		t.Fatalf("dropped %d + retained %d != %d: an entry was lost without being counted",
			s.Dropped(), s.Len(), appended)
	}
}

// Streamed text grows an entry after it was appended; the bound has to follow
// it, and the newest entry is never the one retired.
func TestScrollbackByteBoundCountsStreamedGrowth(t *testing.T) {
	s := NewScrollback()
	big := strings.Repeat("y", 10<<20)
	for i := 0; i < 3; i++ {
		s.Append(ui.Event{Kind: ui.KindToolStart, Tool: "t", Text: big})
	}
	piece := strings.Repeat("z", 1<<20)
	for i := 0; i < 40; i++ {
		s.Append(ui.Event{Kind: ui.KindText, Text: piece})
	}
	if s.Len() != 1 {
		t.Fatalf("%d entries retained while a %d byte reply streams against a %d byte bound; the earlier ones should be gone",
			s.Len(), heldBytes(s), scrollbackByteBound)
	}
	if s.Dropped() != 3 {
		t.Fatalf("dropped = %d, want 3", s.Dropped())
	}
}

// Each retained byte of reply text counts once against MaxBytes. A streamed
// entry keeps its text in the body and the event's own text is the first piece
// of that same text, so counting both made the bound about half of what
// MaxBytes says. No fixed per-entry overhead is counted: the counter is the
// text held and nothing else.
func TestScrollbackCountsAReplyOnce(t *testing.T) {
	s := NewScrollback()
	s.Append(ui.Event{Kind: ui.KindText, Text: strings.Repeat("r", 1000)})
	if s.bytes != 1000 {
		t.Fatalf("a 1000-byte reply counts %d bytes against MaxBytes, want 1000", s.bytes)
	}
	s.Append(ui.Event{Kind: ui.KindText, Text: strings.Repeat("r", 500)})
	if s.bytes != 1500 {
		t.Fatalf("after 500 more streamed bytes the counter is %d, want 1500", s.bytes)
	}
	r := NewScrollback()
	r.Append(ui.Event{Kind: ui.KindReasoning, Text: strings.Repeat("t", 700)})
	r.Append(ui.Event{Kind: ui.KindNotice, Text: strings.Repeat("n", 300)})
	if r.bytes != 1000 {
		t.Fatalf("a thought and a notice count %d, want 1000", r.bytes)
	}
}

// Replies that together fit under MaxBytes are all retained.
func TestScrollbackKeepsRepliesThatFitTheBound(t *testing.T) {
	s := NewScrollback()
	s.MaxBytes = 10_000
	const n, size = 9, 1100 // 9900 bytes of text, just under the bound
	for i := 0; i < n; i++ {
		// A different tool between replies keeps each reply its own entry.
		s.Append(ui.Event{Kind: ui.KindText, Text: strings.Repeat("x", size)})
		if i < n-1 {
			s.Append(ui.Event{Kind: ui.KindTurnStart})
		}
	}
	if s.Dropped() != 0 || heldBytes(s) != n*size {
		t.Fatalf("dropped %d, holding %d of %d bytes under a %d byte bound", s.Dropped(), heldBytes(s), n*size, s.MaxBytes)
	}
}
