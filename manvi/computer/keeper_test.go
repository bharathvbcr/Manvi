package computer

import "testing"

// The runner derives the reuse invariant itself, from the acts it launched. This
// test drives that rule with sequences the reducer is never asked about, so a
// reducer fault cannot make the runner keep a first-after-dispatch frame.
func TestCaptureKeeperNeverKeepsTheFirstFrameAfterADispatch(t *testing.T) {
	frame := func(id string) *retainedObservation { return &retainedObservation{safe: Observation{ID: id}} }
	var k captureKeeper

	k.captured(frame("start-1"))
	if k.kept != nil {
		t.Fatal("the first frame of a run was kept")
	}
	k.captured(frame("start-2"))
	if k.kept == nil || k.kept.safe.ID != "start-2" {
		t.Fatal("a frame that follows another since the last dispatch should be kept")
	}

	for name, dispatch := range map[string]func(){
		"a step's act":        k.dispatched,
		"a recovery click":    k.dispatched,
		"a human action":      k.dispatched,
		"a control or resume": k.dispatched,
	} {
		dispatch()
		if k.kept != nil {
			t.Fatalf("%s left a capture kept", name)
		}
		k.captured(frame(name + "-first"))
		if k.kept != nil {
			t.Fatalf("the first frame after %s was kept", name)
		}
		k.captured(nil) // a capture that produced no frame is not a frame
		if k.kept != nil {
			t.Fatalf("a failed capture after %s made a frame look settled", name)
		}
		k.captured(frame(name + "-second"))
		if k.kept == nil || k.kept.safe.ID != name+"-second" {
			t.Fatalf("the second frame after %s should be kept", name)
		}
	}
}
