package transport

import (
	"sync"
	"time"
)

// manualClock is the stall watchdog's clock, driven by the test rather than by
// the machine.
//
// The tests it serves assert that a stream which keeps producing output is
// *not* abandoned, and paced with real sleeps that assertion is really about
// how busy the test runner is: a gap whose sleep overran and a watchdog that
// fired early both reach the reader as "no output for longer than the limit".
// The openaicompat case with the same shape failed six times in eight
// full-suite runs while passing five of five on its own. Widening the limit
// only moves the load at which that recurs, because the ambiguity is in what
// is being measured. With the gaps stated, tripping the watchdog requires it
// to have moved its deadline wrongly, and nothing else.
//
// See StallClock for the seam, and openaicompat's copy of this type: that
// package builds streams through this one, and this package's own tests cannot
// import anything that imports it, so the double is scaffolding in both places
// rather than a behaviour with two owners.
type manualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*manualTimer
}

// newManualClock starts at a real instant rather than the zero time, so a
// deadline computed before the first advance is an ordinary time.Time.
func newManualClock() *manualClock {
	return &manualClock{now: time.Unix(1<<30, 0)}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) AfterFunc(d time.Duration, f func()) StallTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &manualTimer{clock: c, fn: f, deadline: c.now.Add(d), active: true}
	c.timers = append(c.timers, t)
	return t
}

// scheduled reports how many timers this clock has been asked to run, which is
// what tells a test that the watchdog is on this clock and not on the wall.
// Without the check the driven tests pass whether or not the seam is wired:
// with the frames sent as fast as the socket takes them there is no real gap
// for a real watchdog to trip on, so a stated gap that never reached the
// watchdog would read exactly like one it tolerated.
func (c *manualClock) scheduled() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// Advance moves the clock and runs whatever that makes due.
//
// The callbacks run with this lock released: the watchdog's callback takes the
// watchdog's own mutex and may re-arm the timer from under it, which comes
// back here for the lock.
func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []*manualTimer
	for _, t := range c.timers {
		if t.active && !t.deadline.After(c.now) {
			t.active = false
			due = append(due, t)
		}
	}
	c.mu.Unlock()
	for _, t := range due {
		t.fn()
	}
}

// manualTimer is one scheduled callback. Its fields are guarded by the clock's
// mutex, because Advance walks them while the watchdog re-arms them.
type manualTimer struct {
	clock    *manualClock
	fn       func()
	deadline time.Time
	active   bool
}

func (t *manualTimer) Reset(d time.Duration) bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := t.active
	t.deadline = t.clock.now.Add(d)
	t.active = true
	return was
}

func (t *manualTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := t.active
	t.active = false
	return was
}
