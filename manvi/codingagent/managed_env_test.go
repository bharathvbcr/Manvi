package codingagent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/childenv/childenvtest"
)

// envDumpVar names the file prefix a fixture child writes its environment to.
// The parent sets it; it passes through to the child because it is not a
// provider credential.
const envDumpVar = "MANVI_CODINGAGENT_ENV_DUMP"

// dumpFixtureEnv records what a fixture child was started with, so the parent
// can assert on the environment the real spawn path built.
func dumpFixtureEnv(tag string) {
	prefix := os.Getenv(envDumpVar)
	if prefix == "" {
		return
	}
	_ = os.WriteFile(prefix+"."+tag, []byte(strings.Join(os.Environ(), "\n")), 0o600) // #nosec G703 -- the parent test names this t.TempDir prefix; the fixture child writes nowhere else
}

// A managed Claude agent authenticates with its own login. Handed the host's
// ANTHROPIC_API_KEY it would move to API billing; handed the workspace it
// would bill that one. Neither the session nor the `--version` probe that
// precedes it may see them, whatever the budget settings say.
func TestManagedClaudeDoesNotSeeProviderCredentials(t *testing.T) {
	childenvtest.SetParent(t)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "the-cli-own-login")
	prefix := filepath.Join(t.TempDir(), "env")
	t.Setenv(envDumpVar, prefix)

	openFixture(t, "normal", "ask")

	for _, tag := range []string{"version", "session"} {
		seen, err := os.ReadFile(prefix + "." + tag) // #nosec G304 -- a file under this test's own t.TempDir
		if err != nil {
			t.Fatalf("the %s child did not record its environment: %v", tag, err)
		}
		dump := string(seen)
		childenvtest.Check(t, dump)
		if !strings.Contains(dump, "CLAUDE_CODE_OAUTH_TOKEN=the-cli-own-login") {
			t.Errorf("the %s child lost the CLI's own login:\n%s", tag, dump)
		}
	}
}

// The routing scrub agentEnv adds on top of childenv: a parent's git
// repository overrides and Manvi root never steer the agent elsewhere.
func TestAgentEnvScrubsRepositoryRouting(t *testing.T) {
	environ := []string{
		"CLAUDE_CODE_OAUTH_TOKEN=login", "HOME=/home/u",
		"GIT_DIR=/elsewhere", "GIT_CONFIG_KEY_0=core.x", "PWD=/old", "DEVCOUNCIL_ROOT=/old",
	}
	got := agentEnv(environ, "/repo")
	want := []string{"CLAUDE_CODE_OAUTH_TOKEN=login", "HOME=/home/u", "DEVCOUNCIL_ROOT=/repo", "PWD=/repo"}
	if !slices.Equal(got, want) {
		t.Fatalf("agentEnv = %q, want %q", got, want)
	}
}
