package main

import (
	"os"
	"strings"
	"sync"
	"testing"

	"manvi/core/bus"
	"manvi/flags"
	"manvi/tools"
)

// The verification command is the one string in this harness that is taken from
// a human and executed with the harness's own authority at the end of every
// mutating turn. Where it comes from is a security property, so it gets tests
// of its own.
//
// The attack it is shaped against is short. Settings load from
// .devcouncil/config.yaml. The restricted-path rung that keeps an agent out of
// that file lives inside the hard-rules block (policy/file.go), and a relaxed
// posture switches that block off. So under `--yolo` an agent — or a prompt
// injected into anything the agent read — can write the settings file. If the
// command were a setting, that write would be arbitrary code execution on the
// next turn, running outside the command gate the agent's own shell calls pass,
// and it would look like verification in every log.

// TestVerifyCommandIsNotASetting is the guard. A key of this name in the
// settings catalogue would be the vulnerability, so its absence is asserted
// directly rather than left to a convention nobody re-reads.
func TestVerifyCommandIsNotASetting(t *testing.T) {
	reg := flags.New()
	if err := flags.DefineHarnessFlags(reg); err != nil {
		t.Fatal(err)
	}
	for _, key := range reg.Keys() {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "verify") && strings.Contains(lower, "command") {
			t.Fatalf("%q is a settings key. Settings load from a file inside the repository, and "+
				"the rung protecting that file is disabled under a relaxed posture — so a "+
				"verification command sourced from here is a command the agent can write and "+
				"this harness then executes with its own authority", key)
		}
	}
}

// TestVerifyCommandComesFromTheEnvironmentOnly pins the reader. If this ever
// starts consulting the registry, the test above stops being sufficient.
func TestVerifyCommandComesFromTheEnvironmentOnly(t *testing.T) {
	t.Setenv(verifyCommandEnv, "  go test ./...  ")
	if got := operatorVerifyCommand(); got != "go test ./..." {
		t.Fatalf("operatorVerifyCommand() = %q, want the environment value trimmed", got)
	}

	t.Setenv(verifyCommandEnv, "   ")
	if got := operatorVerifyCommand(); got != "" {
		t.Fatalf("operatorVerifyCommand() = %q, want empty: whitespace is not a command", got)
	}

	os.Unsetenv(verifyCommandEnv)
	if got := operatorVerifyCommand(); got != "" {
		t.Fatalf("operatorVerifyCommand() = %q with nothing set, want empty", got)
	}
}

// The per-surface harness state is written when a session's tool surface is
// built and read when its turn starts. The TUI builds surfaces outside the lock
// it takes to number a tab, so two tabs opening at once is two writers — and an
// unsynchronised map write in Go ends the process rather than losing an entry.
func TestHarnessStateIsSafeUnderConcurrentSessions(t *testing.T) {
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pipeline := tools.NewRegistry(bus.New())
			rememberHarness(pipeline, &subAgentRunner{}, harnessCapability{
				CodeMapConfigured: i%2 == 0,
			})
			runner, caps := harnessFor(pipeline)
			if runner == nil {
				t.Error("what was just recorded did not come back")
			}
			if caps.CodeMapConfigured != (i%2 == 0) {
				t.Error("a surface read back another surface's capabilities")
			}
		}()
	}
	wg.Wait()
}

// A surface nothing recorded yields zero values rather than panicking, which is
// what every caller that never resolved a provider relies on.
func TestHarnessForAnUnknownSurfaceIsZero(t *testing.T) {
	runner, caps := harnessFor(tools.NewRegistry(bus.New()))
	if runner != nil {
		t.Fatal("an unrecorded surface returned a runner")
	}
	if caps.CodeMapConfigured || caps.DocLookup || len(caps.Areas) != 0 {
		t.Fatalf("an unrecorded surface claimed capabilities: %+v", caps)
	}
}
