package main

import (
	"os"
	"path/filepath"
	"strings"
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

// unsetEnv removes a variable for the rest of the test (t.Setenv restores it).
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func advWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The registry production builds (newRegistry), file only: a valid user config
// holding provider local, dotted keys as the guard writes them, no MANVI_LLM_*
// variable. The provider resolves from the config layer, and the TUI default
// path honours it: the cap it returns is the registry's own, not the face's 8192
// fallback.
func TestAdvB27ProductionRegistryHonoursAUserConfigFileAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	unsetEnv(t, flags.EnvKey(flags.LLMDefaultProvider))
	unsetEnv(t, flags.EnvKey(flags.LLMLocalMaxOutputTokens))
	user := filepath.Join(home, "user-config.yaml")
	advWriteFile(t, user, "llm.provider.default: local\n")
	t.Setenv(envUserConfig, user)
	t.Chdir(t.TempDir())
	reg, err := newRegistry("")
	if err != nil {
		t.Fatalf("newRegistry: %v", err)
	}
	v, err := reg.Lookup(flags.LLMDefaultProvider)
	if err != nil {
		t.Fatal(err)
	}
	if v.Raw != "local" || v.Origin != flags.OriginConfig {
		t.Fatalf("provider = %q from %v, want local from the config file", v.Raw, v.Origin)
	}
	want, _, err := reg.Int(flags.LLMLocalMaxOutputTokens)
	if err != nil {
		t.Fatal(err)
	}
	if got := tuiMaxTokens(reg); got != want {
		t.Fatalf("tuiMaxTokens = %d, want the local cap %d the registry resolves", got, want)
	}
}

// Where the cap is allowed: the repository's config.yaml. A valid pair (provider
// local from the user config, cap 1234 from the repository file) reaches the TUI
// default path as 1234.
func TestAdvB27ProductionRegistryHonoursACapInTheRepositoryConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	unsetEnv(t, flags.EnvKey(flags.LLMDefaultProvider))
	unsetEnv(t, flags.EnvKey(flags.LLMLocalMaxOutputTokens))
	user := filepath.Join(home, "user-config.yaml")
	advWriteFile(t, user, "llm.provider.default: local\n")
	t.Setenv(envUserConfig, user)
	t.Chdir(t.TempDir())
	repo := filepath.Join(t.TempDir(), "config.yaml")
	advWriteFile(t, repo, "llm.local.max_output_tokens: 1234\n")
	reg, err := newRegistry(repo)
	if err != nil {
		t.Fatalf("newRegistry: %v", err)
	}
	if v, _ := reg.Lookup(flags.LLMLocalMaxOutputTokens); v.Origin != flags.OriginConfig || v.Raw != "1234" {
		t.Fatalf("cap = %q from %v, want 1234 from the config layer", v.Raw, v.Origin)
	}
	if got := tuiMaxTokens(reg); got != 1234 {
		t.Fatalf("tuiMaxTokens = %d, want 1234", got)
	}
}

// Environment only, through newRegistry: the pair the guard sets is read (origin
// env) and honoured: 1234. So the guard's 8192 is the registryWith helper not
// reading the environment, not the TUI ignoring it.
func TestAdvB27ProductionRegistryHonoursTheEnvironmentAlone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(envUserConfig, filepath.Join(t.TempDir(), "absent.yaml"))
	t.Setenv(flags.EnvKey(flags.LLMDefaultProvider), "local")
	t.Setenv(flags.EnvKey(flags.LLMLocalMaxOutputTokens), "1234")
	t.Chdir(t.TempDir())
	reg, err := newRegistry("")
	if err != nil {
		t.Fatalf("newRegistry: %v", err)
	}
	if v, _ := reg.Lookup(flags.LLMDefaultProvider); v.Origin != flags.OriginEnv {
		t.Fatalf("provider origin = %v, want env", v.Origin)
	}
	if got := tuiMaxTokens(reg); got != 1234 {
		t.Fatalf("tuiMaxTokens = %d, want 1234", got)
	}
}

// The guard writes config.json with YAML text. Nothing in production opens a file
// of that name, but the user-config loader parses whatever path
// MANVI_USER_CONFIG names with the flat YAML dialect, so pointed at a config.json
// holding the provider line it parses (dotted keys included) and is honoured:
// the guard's config.json is unread because of its name, not because it would
// fail to parse.
func TestAdvB27AConfigJSONHoldingYAMLParsesWhenItIsTheUserConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	unsetEnv(t, flags.EnvKey(flags.LLMDefaultProvider))
	unsetEnv(t, flags.EnvKey(flags.LLMLocalMaxOutputTokens))
	user := filepath.Join(home, "config.json")
	advWriteFile(t, user, "llm.provider.default: local\n")
	t.Setenv(envUserConfig, user)
	t.Chdir(t.TempDir())
	reg, err := newRegistry("")
	if err != nil {
		t.Fatalf("newRegistry: %v", err)
	}
	if v, _ := reg.Lookup(flags.LLMDefaultProvider); v.Raw != "local" || v.Origin != flags.OriginConfig {
		t.Fatalf("provider = %q from %v, want local from config", v.Raw, v.Origin)
	}
}

// The guard's own config.yaml (provider and cap together) is refused as a user
// config, and the refusal names the cap key: that key is not one the user config
// may set. So at the one location Linux would read it (XDG_CONFIG_HOME/manvi),
// production would error, not apply it.
func TestAdvB27TheGuardsConfigYAMLIsRefusedNamingTheCapKey(t *testing.T) {
	home := operatorEnvironment(t)
	t.Chdir(t.TempDir())
	t.Setenv(envUserConfig, filepath.Join(home, "xdg", "manvi", "config.yaml"))
	_, err := newRegistry("")
	if err == nil {
		t.Fatal("the guard's config.yaml loaded as a user config")
	}
	if !strings.Contains(err.Error(), flags.LLMLocalMaxOutputTokens) {
		t.Fatalf("the refusal does not name %s: %v", flags.LLMLocalMaxOutputTokens, err)
	}
}
