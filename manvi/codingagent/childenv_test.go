package codingagent

import (
	"slices"
	"testing"
)

func TestChildEnvWithholdsWhatTheHostNames(t *testing.T) {
	environ := []string{
		"ANTHROPIC_API_KEY=sk-ant-metered", "ANTHROPIC_AUTH_TOKEN=oauth", "HOME=/home/u",
		"GIT_DIR=/elsewhere", "GIT_CONFIG_KEY_0=core.x", "PWD=/old", "DEVCOUNCIL_ROOT=/old",
	}
	got := childEnv(environ, "/repo", []string{"ANTHROPIC_API_KEY"})
	want := []string{"ANTHROPIC_AUTH_TOKEN=oauth", "HOME=/home/u", "DEVCOUNCIL_ROOT=/repo", "PWD=/repo"}
	if !slices.Equal(got, want) {
		t.Fatalf("childEnv = %q, want %q", got, want)
	}
	// Nothing withheld keeps authentication intact: withholding is opt-in.
	if got := childEnv(environ, "/repo", nil); !slices.Contains(got, "ANTHROPIC_API_KEY=sk-ant-metered") {
		t.Fatalf("childEnv without a withheld list dropped the credential: %q", got)
	}
}
