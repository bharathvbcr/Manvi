package main

import (
	"os"
	"path/filepath"
	"strings"
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
	// The tier profile is operator scope: it is read from the user config and
	// from MANVI_LLM_TIER_*. Both would otherwise answer every tier test with
	// the profile of whoever ran the suite, so the variables are cleared and the
	// user config is pointed at a file that does not exist. A test that wants
	// either sets it with t.Setenv.
	for _, kv := range os.Environ() {
		if key, _, _ := strings.Cut(kv, "="); strings.HasPrefix(key, "MANVI_LLM_TIER_") {
			if err := os.Unsetenv(key); err != nil {
				panic("clearing " + key + ": " + err.Error())
			}
		}
	}
	isolated, err := os.MkdirTemp("", "manvi-user-config-")
	if err != nil {
		panic("creating an isolated user config directory: " + err.Error())
	}
	if err := os.Setenv(envUserConfig, filepath.Join(isolated, "config.yaml")); err != nil {
		panic("isolating the user config: " + err.Error())
	}
	code := m.Run()
	_ = os.RemoveAll(isolated)
	os.Exit(code)
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
