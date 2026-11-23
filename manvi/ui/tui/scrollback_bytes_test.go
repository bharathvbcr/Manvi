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

// heldBytes is the text an entry's retention is measured on: its event text
// and the streamed body.
func heldBytes(s *Scrollback) int {
	n := 0
	for _, e := range s.Entries() {
		n += len(e.Event.Text) + e.Body.Len()
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
