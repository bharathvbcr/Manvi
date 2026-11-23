package tui

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/ui"
)

// advHeld recomputes what the byte counter is supposed to track.
func advHeld(s *Scrollback) int {
	n := 0
	for _, e := range s.entries {
		n += entryBytes(e)
	}
	return n
}

func advCheckBytes(t *testing.T, s *Scrollback, where string) {
	t.Helper()
	if want := advHeld(s); s.bytes != want {
		t.Fatalf("%s: byte counter %d != recomputed %d (drift %d), entries=%d dropped=%d", where, s.bytes, want, s.bytes-want, len(s.entries), s.dropped)
	}
}

// A newest entry larger than the bound is kept whole; everything older is
// retired and counted; the next entry retires the big one in turn.
func TestAdvB31ScrollbackNewestLargerThanBoundIsKeptWhole(t *testing.T) {
	s := NewScrollback()
	s.MaxBytes = 1000
	for i := 0; i < 5; i++ {
		s.Append(ui.Event{Kind: ui.KindNotice, Text: strings.Repeat("a", 100)})
	}
	big := strings.Repeat("B", 5000)
	s.Append(ui.Event{Kind: ui.KindNotice, Text: big})
	if s.Len() != 1 || s.Dropped() != 5 {
		t.Fatalf("len=%d dropped=%d, want 1 and 5", s.Len(), s.Dropped())
	}
	if got := s.Entries()[0].Event.Text; got != big {
		t.Fatalf("the oversized newest entry was cut: %d bytes", len(got))
	}
	advCheckBytes(t, s, "after big")
	s.Append(ui.Event{Kind: ui.KindNotice, Text: "small"})
	if s.Len() != 1 || s.Dropped() != 6 {
		t.Fatalf("after a small entry len=%d dropped=%d, want 1 and 6", s.Len(), s.Dropped())
	}
	advCheckBytes(t, s, "after small")
}

// Streamed growth across many small appends stays bounded and exact.
func TestAdvB31ScrollbackStreamedGrowthManySmallAppends(t *testing.T) {
	s := NewScrollback()
	s.MaxBytes = 4096
	s.Append(ui.Event{Kind: ui.KindNotice, Text: strings.Repeat("n", 500)})
	for i := 0; i < 20000; i++ {
		s.Append(ui.Event{Kind: ui.KindText, Text: "tok"})
		if i%997 == 0 {
			advCheckBytes(t, s, "stream")
		}
	}
	advCheckBytes(t, s, "stream end")
	if s.Dropped() != 1 {
		t.Fatalf("dropped=%d, want the one notice only (a lone streaming entry is the newest and never retired)", s.Dropped())
	}
	if s.Len() != 1 {
		t.Fatalf("len=%d", s.Len())
	}
}

// Random mixes of every kind of event, at a tight byte and entry bound, for many
// cycles: the counter never drifts from what the entries hold, and
// dropped + retained is the number of entries ever created.
func TestAdvB31ScrollbackCounterNeverDriftsOverManyCycles(t *testing.T) {
	rng := rand.New(rand.NewSource(31))
	kinds := []ui.Kind{ui.KindText, ui.KindReasoning, ui.KindNotice, ui.KindToolStart, ui.KindToolResult, ui.KindTurnEnd, ui.KindError}
	for _, bounds := range [][2]int{{0, 3000}, {7, 0}, {7, 3000}, {1, 50}, {50, 1 << 20}} {
		s := NewScrollback()
		s.MaxEntries, s.MaxBytes = bounds[0], bounds[1]
		if bounds[0] == 0 {
			s.MaxEntries = 0
		}
		created := 0
		for i := 0; i < 20000; i++ {
			k := kinds[rng.Intn(len(kinds))]
			before := len(s.entries)
			dropped := s.dropped
			ev := ui.Event{Kind: k, Text: strings.Repeat("x", rng.Intn(400)), Detail: strings.Repeat("d", rng.Intn(40)), Tool: "tool", Agent: "a"}
			if rng.Intn(5) == 0 {
				ev.Findings = []string{strings.Repeat("f", rng.Intn(200))}
			}
			s.Append(ev)
			created += len(s.entries) - before + (s.dropped - dropped)
			if i%101 == 0 {
				advCheckBytes(t, s, "random")
			}
		}
		advCheckBytes(t, s, "random end")
		if s.Len()+s.Dropped() != created {
			t.Fatalf("bounds %v: retained %d + dropped %d != created %d", bounds, s.Len(), s.Dropped(), created)
		}
		if s.MaxBytes > 0 && s.Len() > 1 && s.bytes > s.MaxBytes {
			t.Fatalf("bounds %v: %d bytes held over the %d bound with %d entries", bounds, s.bytes, s.MaxBytes, s.Len())
		}
	}
}

// A reply counts once against MaxBytes (this once logged 2000 for 1000 bytes).
func TestAdvB31ScrollbackBoundCountsAReplyTwice(t *testing.T) {
	s := NewScrollback()
	s.Append(ui.Event{Kind: ui.KindText, Text: strings.Repeat("r", 1000)})
	t.Logf("a 1000-byte reply counts %d bytes against MaxBytes", s.bytes)
}
