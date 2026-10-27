package tui

import (
	"strconv"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/ui"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

// openLinePayload is n bytes of one unbroken reply paragraph, in the shape a
// layout of the open line has to survive:
//
//	prose  — 19-byte words, one space each
//	table  — a markdown table row, far wider than the pane, words and pipes
//	base64 — one token with no break opportunity at all
//	json   — minified JSON, no spaces
func openLinePayload(shape string, n int) string {
	const b64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var b strings.Builder
	b.Grow(n + 64)
	for i := 0; b.Len() < n; i++ {
		switch shape {
		case "prose":
			b.WriteString("delta-text-20bytes! ")
		case "table":
			b.WriteString(" cell-" + string(rune('a'+i%26)) + " |")
		case "base64":
			b.WriteByte(b64[(i*7+i/64)%64])
		case "json":
			b.WriteString(`{"id":` + strconv.Itoa(i%10) + `,"name":"item","tags":["a","b"],"v":1.5},`)
		default:
			panic("unknown payload shape " + shape)
		}
	}
	return b.String()[:n]
}

var openLineShapes = []string{"prose", "table", "base64", "json"}

// BenchmarkScrollbackStream streams 20-byte text deltas into one reply and
// draws a 120x40 frame every tenth delta — the steady state of a long turn.
// One op is the whole stream, so ns/op divided by the frame count is the
// per-frame cost to hold against minFrame. The unbroken variants are one
// 160KB paragraph, the worst case for a layout that keeps finished lines: the
// default shape is spaced prose, "base64" and "json" have no space for a wrap
// to break at. The "lines" variants break every fourth delta, as prose does.
// The "prior" variants put 4998 finished entries (the retention bound less the
// streaming one) ahead of it.
func BenchmarkScrollbackStream(b *testing.B) {
	for _, tc := range []struct {
		deltas, prior int
		lines         bool
		shape         string
	}{
		{2000, 0, false, ""}, {8000, 0, false, ""}, {2000, 4998, false, ""}, {8000, 4998, false, ""},
		{8000, 0, true, ""}, {8000, 4998, true, ""},
		{8000, 0, false, "base64"}, {8000, 0, false, "json"},
	} {
		name := "deltas=" + strconv.Itoa(tc.deltas) + "/prior=" + strconv.Itoa(tc.prior)
		if tc.lines {
			name += "/lines"
		}
		if tc.shape != "" {
			name += "/" + tc.shape
		}
		b.Run(name, func(b *testing.B) {
			th := Dark()
			buf := render.NewBuffer(120, 40)
			r := buf.Bounds()
			var payload string
			if tc.shape != "" {
				payload = openLinePayload(tc.shape, 20*tc.deltas)
			}
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
					if tc.shape != "" {
						text = payload[20*d : 20*d+20]
					} else if tc.lines && d%4 == 3 {
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
// a long unbroken reply: ten 20-byte deltas and the frame they cost, on a
// 160KB paragraph, for each payload shape. ns/op is the per-frame figure the
// 8ms minFrame bounds. The reply is rebuilt, off the clock, when the payload
// runs out, so a fast layout is measured at 160KB and not at whatever size the
// run happened to grow to.
func BenchmarkScrollbackFrameAt160KB(b *testing.B) {
	const base, frames = 160_000, 2000
	for _, shape := range openLineShapes {
		b.Run(shape, func(b *testing.B) {
			th := Dark()
			buf := render.NewBuffer(120, 40)
			r := buf.Bounds()
			text := openLinePayload(shape, base+frames*10*20)
			var s *Scrollback
			pos := 0
			reset := func() {
				s = NewScrollback()
				s.Append(ui.Event{Kind: ui.KindTurnStart, Text: "turn"})
				for pos = 0; pos < base; pos += 20 {
					s.Append(ui.Event{Kind: ui.KindText, Text: text[pos : pos+20]})
				}
				s.Draw(buf, r, th, true)
			}
			reset()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if pos+10*20 > len(text) {
					b.StopTimer()
					reset()
					b.StartTimer()
				}
				for d := 0; d < 10; d++ {
					s.Append(ui.Event{Kind: ui.KindText, Text: text[pos : pos+20]})
					pos += 20
				}
				s.Draw(buf, r, th, true)
			}
		})
	}
}
