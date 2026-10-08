package main

import (
	"os"
	"testing"
)

// projectRootOverrides are the variables findProjectRoot honours ahead of any
// discovery. An operator — or an agent session — that exports one of them
// would otherwise answer every root-discovery test in this package with its
// own checkout, and every child binary the end-to-end tests start would run
// against that checkout instead of the repository the test built.
var projectRootOverrides = []string{"MANVI_PROJECT_ROOT", "MANVI_ROOT", "DEVCOUNCIL_ROOT"}

// TestMain clears the overrides once for the whole package, before any test
// runs, so the result of `go test` does not depend on the shell it was run
// from. A test that wants an override sets it with t.Setenv, which restores
// the cleared state afterwards.
func TestMain(m *testing.M) {
	for _, key := range projectRootOverrides {
		if err := os.Unsetenv(key); err != nil {
			panic("clearing " + key + ": " + err.Error())
		}
	}
	os.Exit(m.Run())
}

// TestTheOperatorsRootOverrideDoesNotReachTests pins the isolation: an
// override exported by whoever ran the suite is gone by the time a test, or a
// child process started from os.Environ(), could read it.
func TestTheOperatorsRootOverrideDoesNotReachTests(t *testing.T) {
	for _, key := range projectRootOverrides {
		if val, ok := os.LookupEnv(key); ok {
			t.Errorf("%s=%q reached the test process; root discovery would answer with it", key, val)
		}
	}
}
