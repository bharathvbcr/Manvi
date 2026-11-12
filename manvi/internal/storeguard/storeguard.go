// Package storeguard keeps a test binary from leaking dcstore processes.
//
// DevCouncil's store client keeps a pool of persistent `dcstore serve`
// children that only (*store.Client).Close ends. A test that builds a client
// and forgets to close it leaves one process per use behind, and a long
// -count run accumulates thousands of them.
//
// NewClient registers the Close with the test's cleanup, so a fixture cannot
// forget it. TestEveryTestStoreClientIsBuiltThroughNewClient fails on any test
// in the module that calls store.New directly, which is what holds that
// routing. Run, called from TestMain, fails the binary when a dcstore child of
// the test process is still alive after the last test.
//
// Run is the weaker half. DevCouncil's client ends an idle child a minute after
// its last use (serveIdleTTL), so Run sees only a leak made in roughly the last
// minute of the package's run; the static test sees every one.
package storeguard

import (
	"fmt"
	"os"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
)

// NewClient returns a store client that is closed when the test ends.
func NewClient(t testing.TB, binary, db string) *store.Client {
	t.Helper()
	c := store.New(binary, db)
	t.Cleanup(c.Close)
	return c
}

// Run runs the package's tests and returns the exit code for os.Exit. It
// returns non-zero when the tests passed but dcstore children of this process
// outlived them; the offenders are listed and ended.
func Run(m *testing.M) int {
	code := m.Run()
	if leaked := reapLeaked(); len(leaked) > 0 {
		fmt.Fprintf(os.Stderr, "storeguard: %d dcstore child process(es) outlived the tests; every store client must be closed (use storeguard.NewClient):\n", len(leaked))
		for _, p := range leaked {
			fmt.Fprintf(os.Stderr, "  pid %d  %s\n", p.pid, p.command)
		}
		if code == 0 {
			code = 1
		}
	}
	return code
}

type child struct {
	pid     int
	command string
}
