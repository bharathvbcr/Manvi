package main

import (
	"bytes"
	"strings"
	"testing"
)

// An operator who set MANVI_LLM_PROVIDER_LOCAL_ENABLED believed they had
// switched something. They never had — nothing read the key — and removing it
// from the catalogue makes LoadEnv stop looking at the variable entirely, which
// is the same silence wearing a different hat. Startup refuses instead, the way
// it already refuses the retired DEVHARNESS_ prefix.
func TestARetiredEnvironmentVariableIsRefusedAtStartup(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("MANVI_LLM_PROVIDER_LOCAL_ENABLED", "false")

	var out, notes bytes.Buffer
	err := run(&out, &notes, []string{"flags"})
	if err == nil {
		t.Fatal("the harness started with a variable naming a setting it no longer has, " +
			"and printed the flag table as though nothing had been asked for")
	}
	for _, want := range []string{"MANVI_LLM_PROVIDER_LOCAL_ENABLED", "no longer has", "llm.provider.default"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}
