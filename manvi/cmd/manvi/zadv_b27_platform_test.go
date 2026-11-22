package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
)

// Adversarial tests for b27-platform (41e38d29): what
// TestTheTUIsDefaultBoundIgnoresTheOperatorsEnvironment actually proves.

// operatorEnvironment sets what the guard test sets: provider and cap in the
// environment, and a config file at every location it writes, in its own text.
func operatorEnvironment(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/xdg")
	t.Setenv(flags.EnvKey(flags.LLMDefaultProvider), "local")
	t.Setenv(flags.EnvKey(flags.LLMLocalMaxOutputTokens), "1234")
	for _, dir := range []string{home + "/.manvi", home + "/xdg/manvi", home + "/.config/manvi"} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"config.yaml", "config.yml", "config.json"} {
			body := "llm.provider.default: local\nllm.local.max_output_tokens: 1234\n"
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return home
}

// The registry the guard test's host is built on (registryWith, which tuiTurnHost
// calls) never reads the environment (no LoadEnv) and never opens a config file:
// the guard's setup is inert, so it would pass without any of it. This asserts
// that: every flag still resolves from its default.
func TestAdvB27TheGuardsRegistryReadsNeitherTheEnvironmentNorAnyFile(t *testing.T) {
	operatorEnvironment(t)
	reg := registryWith(t, nil)
	for _, key := range []string{flags.LLMDefaultProvider, flags.LLMLocalMaxOutputTokens} {
		v, err := reg.Lookup(key)
		if err != nil {
			t.Fatal(err)
		}
		if v.Origin != flags.OriginDefault {
			t.Fatalf("%s resolved from %v (%q) in the guard's registry; the guard would be exercising a real override", key, v.Origin, v.Raw)
		}
	}
}

// A VALID configuration (provider local, cap 1234), in the one form the guard's
// registry accepts (LoadConfig's settings map), is honoured by the TUI's default
// path: 1234, not 8192. Expected behaviour; it shows the guard's 8192 comes
// from the absence of settings, not from the TUI ignoring them.
func TestAdvB27AValidLocalConfigIsHonouredByTheTUIDefaultPath(t *testing.T) {
	reqs := tuiTurn(t, truncatedThenDone(), map[string]string{
		flags.LLMDefaultProvider:      "local",
		flags.LLMLocalMaxOutputTokens: "1234",
	})
	if len(reqs) == 0 {
		t.Fatal("the model was never asked")
	}
	if got := reqs[0].MaxTokens; got != 1234 {
		t.Fatalf("MaxTokens = %d, want the operator's local cap 1234", got)
	}
}

// The same through the registry production builds (newRegistry), with the
// environment the guard sets and a valid user config at the one path it reads
// (MANVI_USER_CONFIG, else <UserConfigDir>/manvi/config.yaml): the TUI default
// path honours it, so the guard's HOME, XDG and config.json lines cannot be
// what holds the guard at 8192.
func TestAdvB27ProductionRegistryHonoursTheGuardsEnvironment(t *testing.T) {
	home := operatorEnvironment(t)
	user := filepath.Join(home, "user-config.yaml")
	if err := os.WriteFile(user, []byte("llm.provider.default: local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envUserConfig, user)
	t.Chdir(t.TempDir())
	reg, err := newRegistry("")
	if err != nil {
		t.Fatalf("newRegistry: %v", err)
	}
	if got := tuiMaxTokens(reg); got != 1234 {
		t.Fatalf("tuiMaxTokens = %d, want 1234 from the environment the guard sets", got)
	}
}

// config.json holds YAML text in the guard test. The user-config loader parses
// whatever file it is pointed at as the flat YAML dialect, so a config.json at
// MANVI_USER_CONFIG parses (the dotted keys are understood) rather than failing:
// it is the location, not the format, that keeps the guard's config.json unread.
func TestAdvB27TheGuardsDottedKeysParseButItsCapKeyIsNotAUserConfigKey(t *testing.T) {
	home := operatorEnvironment(t)
	t.Chdir(t.TempDir())
	t.Setenv(envUserConfig, filepath.Join(home, "xdg", "manvi", "config.yaml"))
	if _, err := newRegistry(""); err == nil {
		t.Fatal("the guard's own config.yaml (provider and cap together) loaded as a user config; the cap key is operator-environment-only and must be refused")
	}
}
