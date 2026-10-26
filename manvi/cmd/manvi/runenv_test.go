package main

import (
	"os"
	"strings"
	"testing"
)

// The provider, tier, model and effort setup has one owner. `manvi reflect`
// once carried its own copy that omitted the effort validation `manvi run`
// had; this pins that neither command, nor the serve verification critic,
// spells the setup out again, so a check added to buildRunEnv reaches all
// three.
func TestRunAndReflectShareOneRunEnvSetup(t *testing.T) {
	owned := []string{"agent.PlanEffort(", "checkTiers(", "registerSessionLLM(", "resolveModelFor(", "mainEffort(", "buildProvider("}
	for _, file := range []string{"run.go", "reflect.go", "verifycritic.go"} {
		// #nosec G304 -- fixed names in this package's own directory.
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		src := string(raw)
		if !strings.Contains(src, "buildRunEnv(") {
			t.Errorf("%s does not build its run environment through buildRunEnv", file)
		}
		for _, call := range owned {
			if strings.Contains(src, call) {
				t.Errorf("%s calls %s itself; that setup belongs to buildRunEnv", file, call)
			}
		}
	}
	// The effort validation is in the shared owner, not dropped.
	raw, err := os.ReadFile("runenv.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"agent.PlanEffort(", "capability.Validate("} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("runenv.go lacks %s", want)
		}
	}
}
