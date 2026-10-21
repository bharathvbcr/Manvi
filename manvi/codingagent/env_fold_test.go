package codingagent

import (
	"slices"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/childenv"
)

// On Windows `Git_Dir` is GIT_DIR: a routing override that differs only in case
// must be scrubbed too. On unix it is a different, harmless variable.
func TestAgentEnvScrubsRoutingNamesAsTheHostOSDoes(t *testing.T) {
	environ := []string{"HOME=/h", "Git_Dir=/elsewhere", "git_config_key_0=core.x", "Pwd=/old", "devcouncil_root=/old"}

	got := agentEnvKeyed(environ, "/repo", childenv.KeyFor(true))
	want := []string{"HOME=/h", "DEVCOUNCIL_ROOT=/repo", "PWD=/repo"}
	if !slices.Equal(got, want) {
		t.Fatalf("fold=true: got %q, want %q", got, want)
	}

	got = agentEnvKeyed(environ, "/repo", childenv.KeyFor(false))
	want = append(slices.Clone(environ), "DEVCOUNCIL_ROOT=/repo", "PWD=/repo")
	if !slices.Equal(got, want) {
		t.Fatalf("fold=false: got %q, want %q", got, want)
	}
}
