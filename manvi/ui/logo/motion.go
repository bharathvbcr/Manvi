package logo

import "github.com/bharathvbcr/Manvi/manvi/ui/render"

// Motion is where the mark is in its animation.
//
// The zero Motion is the mark at rest, and it is what Lines and Draw render:
// the CLI, a terminal with animation turned off, and every test that pins the
// mark's characters all get the same still drawing. A caller that animates
// passes its own frame counter and the mark computes the rest, so nothing is
// stored between frames and a frame at a given tick is always the same frame.
//
// Motion only ever does two things to the still drawing. While the mark
// unfolds it withholds cells, writing the mark's background where they will
// be; and when light crosses the ribbon it changes colours. It never moves a
// cell, changes a character that is drawn, or changes a line's width, so the
// lockup occupies the same cells at every tick and nothing around it shifts.
type Motion struct {
	// Live turns motion on.
	Live bool
	// Tick is the caller's frame counter.
	Tick int
	// Start is the tick at which the mark came on screen. It unfolds from
	// there, tracing the ribbon from the foot of its first leg.
	Start int
	// Nudge is the tick of the last thing the mark should answer, which it
	// does by sending a glint along the ribbon at once instead of waiting for
	// the next one. Zero means it has not been nudged.
	Nudge int
	// Working makes the mark stand for work in progress: light runs the
	// ribbon round and round with no rest, so there is no tick at which a
	// working mark looks like a still one. It replaces the rest of the
	// choreography. A mark that says "busy" is already on screen, so it does
	// not unfold, and Start and Nudge are not read.
	Working bool
}

// The choreography, in ticks. The TUI advances ten a second.
const (
	// unfoldTicks is how long the ribbon takes to draw itself. It is under the
	// effects kit's cap on a reveal: an entrance is a courtesy, not a wait.
	unfoldTicks = 12
	// glintTicks is how long one glint takes to run the ribbon, and glintEvery
	// how often one sets off unprompted. Most of the time the mark is still.
	glintTicks = 18
	glintEvery = 90
	// workPace is how long the light spends on each step of a working mark.
	// On the one-line ribbon that is a lap every eight ticks.
	workPace = 2
)

// NudgeDue reports whether a nudge at tick should be passed on, given the tick
// of the last one that was.
//
// A nudge restarts its glint from the foot of the ribbon, so passing every one
// on would hold the light there for as long as the nudges kept coming: someone
// typing would see the first leg flicker and nothing travel. A glint that has
// set off is allowed to finish, and the nudges that arrive while it runs are
// the ones it is already answering.
func NudgeDue(tick, last int) bool {
	return last <= 0 || tick-last >= glintTicks
}

// glintTail is how many steps of the ribbon trail a glint's head, for a
// ribbon of n steps.
func glintTail(n int) int { return max(2, n/6) }

// shown reports how many steps of an n-step ribbon have been drawn.
func (m Motion) shown(n int) int {
	if !m.Live || m.Working {
		return n
	}
	elapsed := m.Tick - m.Start
	switch {
	case elapsed <= 0:
		return 0
	case elapsed >= unfoldTicks:
		return n
	}
	return n * elapsed / unfoldTicks
}

// glow reports how brightly the step at is lit on an n-step ribbon, from 0
// (not at all) to tail (the head of a glint), and tail itself.
//
// There are up to three heads on the ribbon at once and a step takes the
// brightest: the one leading the unfold, the one that sets off every
// glintEvery ticks once the mark has settled, and the one a nudge sent.
func (m Motion) glow(at, n int) (level, tail int) {
	tail = glintTail(n)
	if !m.Live {
		return 0, tail
	}
	if m.Working {
		// The head laps the ribbon and its tail wraps with it, so the foot
		// of the first leg is lit behind a head on the last. Without the
		// wrap there would be a dark frame every lap, and a dark frame is
		// the still mark: the one thing a busy indicator must never show.
		head := (m.Tick / workPace) % n
		if head < 0 {
			head += n
		}
		if d := (head - at + n) % n; d < tail {
			return tail - d, tail
		}
		return 0, tail
	}
	behind := func(head int) int {
		d := head - at
		if d < 0 || d >= tail {
			return 0
		}
		return tail - d
	}
	// run is a head that takes glintTicks to travel the ribbon and its own
	// tail, so the light has left the last step before the glint is over.
	run := func(elapsed int) int {
		if elapsed < 0 || elapsed >= glintTicks {
			return 0
		}
		return behind(elapsed * (n + tail) / glintTicks)
	}

	elapsed := m.Tick - m.Start
	switch {
	case elapsed <= 0:
		return 0, tail
	case elapsed < unfoldTicks:
		// The step just drawn is the bright one.
		return behind(m.shown(n) - 1), tail
	}
	level = run((elapsed - unfoldTicks) % glintEvery)
	if m.Nudge > m.Start {
		level = max(level, run(m.Tick-m.Nudge))
	}
	return level, tail
}

// lift moves a face's colour toward the glint by level out of tail.
//
// The shaded face is lifted half as far as the lit one. The two therefore keep
// their order at every level, which matters in the cells that hold both: a
// fold must still read as a fold while the light is on it.
func lift(face, glint render.Color, level, tail int, shaded bool) render.Color {
	if level <= 0 || glint.IsDefault() {
		return face
	}
	t := 0.8 * float64(level) / float64(tail)
	if shaded {
		t /= 2
	}
	return face.Blend(glint, t)
}
