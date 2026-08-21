// Package fx is the procedural-effects kit.
//
// Every effect here is a small computation over an integer tick — the frame
// counter the TUI already advances ten times a second — rather than a
// pre-rendered animation. Nothing is stored between frames except what the
// caller keeps (a sparkline's ring, a reveal's start tick), nothing runs on a
// goroutine, and a frame that animates is drawn into the same buffer and
// diffed by the same painter as one that does not. The cost of motion is
// therefore exactly the cells that moved, which is the only cost a terminal
// animation is allowed to have: a full-screen repaint over ssh is how a
// decoration becomes the reason the UI feels broken.
//
// The vocabulary is deliberately small — a backdrop rain, a border sweep, a
// breathing pulse, a typewriter reveal, a ticker, a sample ring — because
// these are accents on a working screen, not the screen. An effect that
// competes with the transcript for attention is a bug, and the gates below
// (theme, colour profile, MANVI_FX) exist so the operator can turn all of it
// off with one variable.
package fx

import (
	"os"
	"strings"
)

// Enabled reports whether ambient animation is permitted at all.
//
// The operator override is MANVI_FX=off (or 0/false/no). Colour depth and the
// plain theme are checked by the caller, which is the code that knows both —
// this package decides shapes, not policy.
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MANVI_FX"))) {
	case "0", "off", "false", "no", "disabled":
		return false
	}
	return true
}

// Pulse is a breathing wave: 0 at rest, rising to 1 at the period's midpoint,
// back to 0 at its end.
//
// A triangle rather than a sine. At ten ticks a second the two are
// indistinguishable, and the triangle is integer-indexable arithmetic with no
// transcendental call — which is also what keeps it exactly reproducible in a
// test at any tick.
func Pulse(tick, period int) float64 {
	if period < 2 {
		return 0
	}
	t := tick % period
	if t < 0 {
		t += period
	}
	half := float64(period) / 2
	v := float64(t) / half
	if v > 1 {
		v = 2 - v
	}
	return v
}

// maxRevealTicks bounds how long a typewriter reveal may take. A longer text
// types faster rather than keeping the operator waiting: the reveal is a
// courtesy, not a gate.
const maxRevealTicks = 24

// Reveal reports how many runes of an n-rune text are visible at tick, the
// reveal having begun at start. cps is the base rate in runes per tick.
//
// tick 0 shows nothing: a reveal that begins complete is not a reveal, and
// the first animation tick arrives within a tenth of a second of the frame.
func Reveal(n, tick, start, cps int) int {
	if n <= 0 {
		return 0
	}
	elapsed := tick - start
	if elapsed < 0 {
		return 0
	}
	if cps < 1 {
		cps = 1
	}
	if need := (n + maxRevealTicks - 1) / maxRevealTicks; need > cps {
		cps = need
	}
	if shown := elapsed * cps; shown < n {
		return shown
	}
	return n
}

// RevealText is Reveal applied to a string, by runes so a multi-byte
// character never appears half-formed.
func RevealText(s string, tick, start, cps int) string {
	rs := []rune(s)
	n := Reveal(len(rs), tick, start, cps)
	if n >= len(rs) {
		return s
	}
	return string(rs[:n])
}

// Marquee renders text into width columns, scrolling when it does not fit.
//
// The window dwells at both ends of the travel, because a ticker that never
// rests is read by nobody: the first word and the last are the ones that say
// what the label is. Movement is one column per two ticks — fast enough to
// read as motion, slow enough to read as text.
func Marquee(text string, width, tick int) string {
	if width < 1 {
		return ""
	}
	rs := []rune(text)
	if len(rs) <= width {
		return text
	}
	span := len(rs) - width
	const dwell = 6
	pos := tick/2 + 1 - dwell
	if pos < 0 {
		pos = 0
	}
	if pos > span {
		pos = span
	}
	return string(rs[pos : pos+width])
}

// Series is a fixed-capacity ring of samples, feeding sparkline strips.
//
// Push is O(1) and allocates nothing after construction; Values copies the
// window out oldest-first. A series is how a status bar shows the recent past
// of a number — tokens per tick, steps per turn — without the bar keeping a
// history it has to bound itself.
type Series struct {
	vals []float64
	next int
	n    int
}

// NewSeries builds a ring of the given capacity, with a floor of two so a
// sparkline always has a shape rather than a point.
func NewSeries(capacity int) *Series {
	if capacity < 2 {
		capacity = 2
	}
	return &Series{vals: make([]float64, capacity)}
}

// Push appends a sample, retiring the oldest when full.
func (s *Series) Push(v float64) {
	s.vals[s.next] = v
	s.next = (s.next + 1) % len(s.vals)
	if s.n < len(s.vals) {
		s.n++
	}
}

// Len is how many samples the ring currently holds.
func (s *Series) Len() int { return s.n }

// Values returns the samples oldest-first. A fresh slice each call: the ring
// keeps moving under the caller otherwise, and a sparkline drawn from a
// shifting window flickers for reasons nobody can see.
func (s *Series) Values() []float64 {
	out := make([]float64, s.n)
	for i := 0; i < s.n; i++ {
		out[i] = s.vals[(s.next-s.n+i+len(s.vals))%len(s.vals)]
	}
	return out
}
