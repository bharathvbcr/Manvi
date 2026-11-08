package codingagent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Seat routing is for operators who run seats. An operator who authenticates
// managed Claude another way (a custom config directory, a long-lived OAuth
// token) and has no `.claude-account` marker and no ~/.claude-accounts must see
// the launch behave as it did before seat routing existed: only the
// nested-session markers are dropped.

func envHas(dump, key string) bool {
	return strings.Contains("\n"+dump, "\n"+key+"=")
}

func TestClaudeLaunchWithoutSeatsKeepsAnOperatorsConfigDirAndOnlyDropsNestedSessionMarkers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	custom := filepath.Join(home, ".claude-custom")
	if err := os.MkdirAll(custom, 0o700); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(home, "Code", "Plain")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", custom)
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", custom)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "operator-oauth-token")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")

	dump := openClaudeSeatFixture(t, repo)
	for key, want := range map[string]string{
		"CLAUDE_CONFIG_DIR":               custom,
		"CLAUDE_SECURESTORAGE_CONFIG_DIR": custom,
	} {
		if !strings.Contains("\n"+dump, "\n"+key+"="+want+"\n") {
			t.Errorf("no seats configured, but %s=%s did not reach the child:\n%s", key, want, dump)
		}
	}
	for _, key := range []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT"} {
		if envHas(dump, key) {
			t.Errorf("nested-session marker %s reached the child:\n%s", key, dump)
		}
	}
}

func TestClaudeSeatOverridesWithoutSeatsUnsetOnlyTheNestedSessionMarkers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	repo := filepath.Join(home, "Code", "Plain")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := claudeSeatOverrides(repo)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT"}
	if !slices.Equal(slices.Sorted(slices.Values(got.unsetKeys)), slices.Sorted(slices.Values(want))) || len(got.set) != 0 {
		t.Fatalf("overrides without seats = %+v, want unset %v and nothing set", got, want)
	}
}

func TestClaudeSeatOverridesStripCredentialsOnceSeatRoutingIsActive(t *testing.T) {
	credentialKeys := []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN"}
	for name, setup := range map[string]func(t *testing.T, home, repo string){
		"marker only": func(t *testing.T, _, repo string) {
			if err := os.WriteFile(filepath.Join(repo, ".claude-account"), []byte("personal\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"accounts file only": func(t *testing.T, home, _ string) {
			if err := os.WriteFile(filepath.Join(home, ".claude-accounts"), []byte("# no seats yet\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			repo := filepath.Join(home, "Code", "Seated")
			if err := os.MkdirAll(repo, 0o755); err != nil {
				t.Fatal(err)
			}
			setup(t, home, repo)
			got, err := claudeSeatOverrides(repo)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range append(credentialKeys, "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT") {
				if !slices.Contains(got.unsetKeys, key) {
					t.Errorf("seat routing active but %s is not unset: %v", key, got.unsetKeys)
				}
			}
		})
	}
}
