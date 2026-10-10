package tui

import (
	"strconv"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/ui"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

// BenchmarkScrollbackStream streams 20-byte text deltas into one reply and
// draws a 120x40 frame every tenth delta — the steady state of a long turn.
// One op is the whole stream, so ns/op divided by the frame count is the
// per-frame cost to hold against minFrame. The unbroken variants are one
// 160KB paragraph, the worst case for a layout that keeps finished lines; the
// "lines" variants break every fourth delta, as prose does. The "prior" variants put 4998
// finished entries (the retention bound less the streaming one) ahead of it.
func BenchmarkScrollbackStream(b *testing.B) {
	for _, tc := range []struct {
		deltas, prior int
		lines         bool
	}{{2000, 0, false}, {8000, 0, false}, {2000, 4998, false}, {8000, 4998, false}, {8000, 0, true}, {8000, 4998, true}} {
		name := "deltas=" + strconv.Itoa(tc.deltas) + "/prior=" + strconv.Itoa(tc.prior)
		if tc.lines {
			name += "/lines"
		}
		b.Run(name, func(b *testing.B) {
			th := Dark()
			buf := render.NewBuffer(120, 40)
			r := buf.Bounds()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				s := NewScrollback()
				for p := 0; p < tc.prior; p++ {
					s.Append(ui.Event{Kind: ui.KindToolStart, Tool: "t" + strconv.Itoa(p)})
				}
				s.Append(ui.Event{Kind: ui.KindTurnStart, Text: "turn"})
				b.StartTimer()
				for d := 0; d < tc.deltas; d++ {
					text := "delta-text-20bytes! "
					if tc.lines && d%4 == 3 {
						text = "delta-text-19bytes!\n"
					}
					s.Append(ui.Event{Kind: ui.KindText, Text: text})
					if d%10 == 9 {
						s.Draw(buf, r, th, true)
					}
				}
			}
		})
	}
}

// BenchmarkScrollbackFrameAt160KB is one op of the steady state at the end of
// a long unbroken reply: ten deltas and the frame they cost, on a 160KB
// paragraph. ns/op is the per-frame figure the 8ms minFrame bounds.
func BenchmarkScrollbackFrameAt160KB(b *testing.B) {
	th := Dark()
	buf := render.NewBuffer(120, 40)
	r := buf.Bounds()
	s := NewScrollback()
	s.Append(ui.Event{Kind: ui.KindTurnStart, Text: "turn"})
	for d := 0; d < 8000; d++ {
		s.Append(ui.Event{Kind: ui.KindText, Text: "delta-text-20bytes! "})
	}
	s.Draw(buf, r, th, true)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for d := 0; d < 10; d++ {
			s.Append(ui.Event{Kind: ui.KindText, Text: "delta-text-20bytes! "})
		}
		s.Draw(buf, r, th, true)
	}
}
