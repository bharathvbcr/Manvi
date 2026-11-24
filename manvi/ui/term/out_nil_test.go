package term

import "testing"

// A Terminal with no files must report a nil writer and reader, not an
// interface holding a nil *os.File: emitOSC52 and its siblings guard with
// `out != nil`, and a typed nil passes that guard and is then written through.
func TestOutAndInAreNilInterfacesWhenTheTerminalHasNoFiles(t *testing.T) {
	var tm Terminal
	if w := tm.Out(); w != nil {
		t.Fatalf("Out() = %#v, want a nil interface", w)
	}
	if r := tm.In(); r != nil {
		t.Fatalf("In() = %#v, want a nil interface", r)
	}
}
