package computer

import "testing"

// The runner derives the reuse invariant itself, from what it did to the
// desktop. These cases drive that rule with sequences the reducer is never asked
// about, so a reducer fault cannot make the runner keep a frame that was taken
// right after something that can change the screen.
func TestCaptureKeeperKeepsOnlyAFrameThatFollowsAnotherSinceTheLastScreenChange(t *testing.T) {
	type step string
	const (
		change step = "screen change" // an act, a control, a window activation
		frame  step = "frame"         // a usable capture
		failed step = "failed"        // a capture that produced no frame
	)
	cases := []struct {
		name  string
		steps []step
		kept  bool // is the last frame kept
	}{
		{"the first frame of a run", []step{frame}, false},
		{"a second frame", []step{frame, frame}, true},
		{"the first frame after a screen change", []step{frame, frame, change, frame}, false},
		{"the second frame after a screen change", []step{frame, frame, change, frame, frame}, true},
		{"a failed capture between a change and a frame", []step{frame, frame, change, failed, frame}, false},
		{"a failed capture does not settle the screen", []step{change, failed, failed, frame}, false},
		{"a failed capture between two frames", []step{change, frame, failed, frame}, true},
		{"back to back changes", []step{frame, frame, change, change, frame}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var k captureKeeper
			n := 0
			for _, s := range c.steps {
				switch s {
				case change:
					k.screenChanged()
					if k.kept != nil {
						t.Fatal("a screen change left a capture kept")
					}
				case failed:
					k.captured(nil)
				case frame:
					n++
					k.captured(&retainedObservation{safe: Observation{ID: string(rune('a' + n))}})
				}
			}
			if got := k.kept != nil; got != c.kept {
				t.Fatalf("kept = %v, want %v", got, c.kept)
			}
		})
	}
}
