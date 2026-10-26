package budget

import (
	"errors"
	"testing"
)

// The Windows liveness rule, run on any OS. The syscall path that fills an
// exitProbe only compiles under GOOS=windows; this is the part that can be
// checked here.
func TestExitProbeGone(t *testing.T) {
	denied := errors.New("Access is denied.")
	failed := errors.New("wait failed")
	waited := func(event uint32, err error) func() (uint32, error) {
		return func() (uint32, error) { return event, err }
	}
	neverWaited := func() (uint32, error) {
		t.Error("the wait ran although the exit code was not the ambiguous one")
		return waitTimeout, nil
	}

	for _, c := range []struct {
		name  string
		probe exitProbe
		want  bool
	}{
		{"running: 259 and the process object unsignaled",
			exitProbe{ExitCode: stillActive, Wait: waited(waitTimeout, nil)}, false},
		{"exited with 0",
			exitProbe{ExitCode: 0, Wait: neverWaited}, true},
		{"exited with 1",
			exitProbe{ExitCode: 1, Wait: neverWaited}, true},
		{"exited with 259: the code says active, the object is signaled",
			exitProbe{ExitCode: stillActive, Wait: waited(waitObject0, nil)}, true},
		{"259 and the wait failed",
			exitProbe{ExitCode: stillActive, Wait: waited(0xFFFFFFFF, failed)}, false},
		{"259 and the wait reported something unexpected",
			exitProbe{ExitCode: stillActive, Wait: waited(0x80, nil)}, false},
		{"259 with no way to wait",
			exitProbe{ExitCode: stillActive}, false},
		{"exit code unreadable",
			exitProbe{ExitCodeErr: failed, Wait: neverWaited}, false},
		{"access denied opening the process",
			exitProbe{OpenErr: denied}, false},
		{"no such pid (invalid parameter)",
			exitProbe{OpenErr: errors.New("The parameter is incorrect."), NoSuchProcess: true}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.probe.gone(); got != c.want {
				t.Fatalf("gone() = %v, want %v", got, c.want)
			}
		})
	}
}
