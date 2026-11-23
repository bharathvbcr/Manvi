package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/agents"
)

// useHarness isolates one `manvi use` run: a repository root, a user config
// outside it, and none of the variables that would shadow what it saves.
type useHarness struct {
	root, config string
}

func newUseHarness(t *testing.T) *useHarness {
	t.Helper()
	dir := t.TempDir()
	h := &useHarness{root: filepath.Join(dir, "repo"), config: filepath.Join(dir, "config", "manvi", "config.yaml")}
	if err := os.MkdirAll(h.root, 0o700); err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(h.root); err == nil {
		h.root = resolved
	}
	t.Chdir(h.root)
	t.Cleanup(setProjectRootForTest(h.root))
	t.Setenv(envUserConfig, h.config)
	for _, name := range []string{
		flags.EnvKey(flags.LLMDefaultProvider), flags.EnvKey(flags.LLMLocalBaseURL), flags.EnvKey(flags.LLMLocalModel),
		flags.EnvKey(tierModelKey(agents.TierMain)), "MANVI_MODEL",
	} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func (h *useHarness) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	reg, err := newRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = runUse(&out, reg, args, func() (*flags.Registry, error) { return newRegistry("") })
	return out.String(), err
}

func (h *useHarness) read(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(h.config)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (h *useHarness) status(t *testing.T, args ...string) useStatus {
	t.Helper()
	out, err := h.run(t, append(args, "--json")...)
	if err != nil {
		t.Fatalf("manvi use %v: %v", args, err)
	}
	var s useStatus
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("not JSON: %q: %v", out, err)
	}
	return s
}

func TestUseCloudWritesTheProviderAndTheMainTierAndTheLoaderReadsThemBack(t *testing.T) {
	h := newUseHarness(t)
	s := h.status(t, "cloud", "--model", "claude-sonnet-5-5")
	if s.Mode != "cloud" || s.Provider != "anthropic" || s.ProviderFrom != "config" || s.Model != "claude-sonnet-5-5" || s.ModelFrom != "config" || s.Cloud.Model != "claude-sonnet-5-5" {
		t.Fatalf("status = %+v", s)
	}
	if s.UserConfig != h.config || len(s.ShadowedBy) != 0 {
		t.Fatalf("user_config %q, shadowed_by %v", s.UserConfig, s.ShadowedBy)
	}
	reg, err := newRegistry("")
	if err != nil {
		t.Fatalf("the next run cannot load what was written: %v", err)
	}
	if got, origin, _ := reg.String(flags.LLMDefaultProvider); got != "anthropic" || origin != flags.OriginConfig {
		t.Fatalf("provider = %q from %s", got, origin)
	}
	if got, _, _ := reg.String(tierModelKey(agents.TierMain)); got != "claude-sonnet-5-5" {
		t.Fatalf("main tier = %q", got)
	}
	info, err := os.Stat(h.config)
	if err != nil {
		t.Fatalf("config = %v, %v", info, err)
	}
	// Permission bits exist only where the file system has them; on Windows
	// Perm() reports 0666 or 0444 whatever the file's access list says, so the
	// 0600 contract is asserted on the platforms that can express it.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v; want 0600", info.Mode())
	}
}

func TestUseLocalWritesProviderBaseURLAndModel(t *testing.T) {
	h := newUseHarness(t)
	s := h.status(t, "local", "--base-url", "http://127.0.0.1:11434/v1", "--model", "qwen3:8b")
	if s.Mode != "local" || s.Provider != "local" || s.ProviderFrom != "config" || s.Model != "qwen3:8b" || s.ModelFrom != "config" {
		t.Fatalf("status = %+v", s)
	}
	if s.Local.BaseURL != "http://127.0.0.1:11434/v1" || s.Local.Model != "qwen3:8b" {
		t.Fatalf("local = %+v", s.Local)
	}
	// A bare `use local` keeps what an earlier one saved.
	if again := h.status(t, "local"); again.Local.BaseURL != s.Local.BaseURL || again.Local.Model != "qwen3:8b" {
		t.Fatalf("a bare 'use local' changed the saved endpoint: %+v", again.Local)
	}
}

func TestUsePreservesEveryOtherKeyAndComment(t *testing.T) {
	h := newUseHarness(t)
	if err := os.MkdirAll(filepath.Dir(h.config), 0o700); err != nil {
		t.Fatal(err)
	}
	before := "# my settings\nllm.tier.heavy: claude-opus-5-5\nllm.provider.default: anthropic\n\nllm.xai.base_url: https://proxy.example/v1 # via the gateway\n"
	if err := os.WriteFile(h.config, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	h.status(t, "local", "--model", "m1")
	want := "# my settings\nllm.tier.heavy: claude-opus-5-5\nllm.provider.default: local\n\nllm.xai.base_url: https://proxy.example/v1 # via the gateway\nllm.local.model: m1\n"
	if got := h.read(t); got != want {
		t.Fatalf("config =\n%s\nwant\n%s", got, want)
	}
}

func TestUseRefusesToRewriteAKeyWrittenInNestedForm(t *testing.T) {
	h := newUseHarness(t)
	if err := os.MkdirAll(filepath.Dir(h.config), 0o700); err != nil {
		t.Fatal(err)
	}
	before := "llm:\n  provider:\n    default: anthropic\n"
	if err := os.WriteFile(h.config, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.run(t, "local"); err == nil || !strings.Contains(err.Error(), "nested") {
		t.Fatalf("err = %v", err)
	}
	if h.read(t) != before {
		t.Fatal("a refused write changed the file")
	}
}

func TestUseWriteIsAtomicAndAFailureLeavesTheOldFile(t *testing.T) {
	h := newUseHarness(t)
	if err := os.MkdirAll(filepath.Dir(h.config), 0o700); err != nil {
		t.Fatal(err)
	}
	before := "llm.provider.default: anthropic\n"
	if err := os.WriteFile(h.config, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	real := renameFile
	renameFile = func(string, string) error { return errors.New("simulated rename failure") }
	t.Cleanup(func() { renameFile = real })
	if _, err := h.run(t, "local"); err == nil || !strings.Contains(err.Error(), "simulated") {
		t.Fatalf("err = %v", err)
	}
	if h.read(t) != before {
		t.Fatal("a failed write damaged the old file")
	}
	entries, _ := os.ReadDir(filepath.Dir(h.config))
	if len(entries) != 1 {
		t.Fatalf("a temporary file was left behind: %v", entries)
	}
}

func TestUseRefusesAUserConfigInsideTheRepository(t *testing.T) {
	h := newUseHarness(t)
	inside := filepath.Join(h.root, "user.yaml")
	t.Setenv(envUserConfig, inside)
	reg := mustRegistryWithoutUserConfig(t)
	var out bytes.Buffer
	err := runUse(&out, reg, []string{"local"}, func() (*flags.Registry, error) { return newRegistry("") })
	if err == nil || !strings.Contains(err.Error(), "inside the repository") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(inside); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("a file was written inside the repository")
	}
}

// mustRegistryWithoutUserConfig builds a registry as main does when the user
// config is absent, so the command itself meets the path.
func mustRegistryWithoutUserConfig(t *testing.T) *flags.Registry {
	t.Helper()
	reg, err := flags.NewHarnessRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Define(tierFlagDefs()...); err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestUseReportsAnEnvironmentVariableThatShadowsTheSavedChoice(t *testing.T) {
	h := newUseHarness(t)
	t.Setenv(flags.EnvKey(flags.LLMDefaultProvider), "local")
	t.Setenv("MANVI_MODEL", "from-env")
	s := h.status(t, "cloud")
	if !strings.Contains(h.read(t), "llm.provider.default: anthropic") {
		t.Fatalf("the choice was not saved: %q", h.read(t))
	}
	if s.Mode != "local" || s.ProviderFrom != "env" || s.Model != "from-env" || s.ModelFrom != "env" {
		t.Fatalf("status = %+v; the environment wins for this run", s)
	}
	for _, want := range []string{flags.EnvKey(flags.LLMDefaultProvider), "MANVI_MODEL"} {
		found := false
		for _, name := range s.ShadowedBy {
			found = found || name == want
		}
		if !found {
			t.Errorf("shadowed_by = %v; want %s", s.ShadowedBy, want)
		}
	}
	out, err := h.run(t)
	if err != nil || !strings.Contains(out, flags.EnvKey(flags.LLMDefaultProvider)) {
		t.Fatalf("the human status does not say what shadows the choice: %q, %v", out, err)
	}
}

func TestUseShadowsFollowTheMode(t *testing.T) {
	h := newUseHarness(t)
	t.Setenv(flags.EnvKey(tierModelKey(agents.TierMain)), "claude-haiku-5-5")
	t.Setenv(flags.EnvKey(flags.LLMLocalModel), "x")
	cloud := h.status(t, "cloud")
	if len(cloud.ShadowedBy) != 1 || cloud.ShadowedBy[0] != flags.EnvKey(tierModelKey(agents.TierMain)) {
		t.Fatalf("cloud shadowed_by = %v", cloud.ShadowedBy)
	}
	local := h.status(t, "local")
	if len(local.ShadowedBy) != 1 || local.ShadowedBy[0] != flags.EnvKey(flags.LLMLocalModel) || local.Model != "x" {
		t.Fatalf("local shadowed_by = %v, model %q", local.ShadowedBy, local.Model)
	}
}

func TestUseJSONShapeIsTheHostContract(t *testing.T) {
	h := newUseHarness(t)
	out, err := h.run(t, "cloud", "--model", "claude-opus-5-5", "--json")
	if err != nil {
		t.Fatal(err)
	}
	// The path appears in the JSON as a string literal, where a Windows path's
	// backslashes are doubled.
	quoted, err := json.Marshal(h.config)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.ReplaceAll(out, strings.Trim(string(quoted), `"`), "<CONFIG>")
	want := `{"mode":"cloud","provider":"anthropic","provider_from":"config","user_config":"<CONFIG>","shadowed_by":[],` +
		`"model":"claude-opus-5-5","model_from":"config",` +
		`"local":{"base_url":"http://127.0.0.1:8000/v1","base_url_from":"default","model":"","model_from":"default"},` +
		`"cloud":{"model":"claude-opus-5-5","model_from":"config"},"warnings":[]}` + "\n"
	if got != want {
		t.Fatalf("--json =\n%s\nwant\n%s", got, want)
	}
	// With nothing saved the default is cloud, from the catalogue, and the file is not created.
	fresh := newUseHarness(t)
	empty, err := fresh.run(t, "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(empty, `"mode":"cloud"`) || !strings.Contains(empty, `"provider_from":"default"`) || fresh.read(t) != "" {
		t.Fatalf("bare status = %s (config %q)", empty, fresh.read(t))
	}
}

func TestUseRefusesBadInputWithoutWriting(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown mode":           {"hybrid"},
		"extra args":             {"cloud", "local"},
		"extra positional":       {"cloud", "now"},
		"model on no mode":       {"--model", "claude-opus-5-5"},
		"uncatalogued cloud":     {"cloud", "--model", "gpt-4"},
		"empty cloud model":      {"cloud", "--model", ""},
		"base url on cloud":      {"cloud", "--base-url", "http://127.0.0.1:1/v1"},
		"base url scheme":        {"local", "--base-url", "ftp://127.0.0.1/v1"},
		"base url credentials":   {"local", "--base-url", "http://user:pw@127.0.0.1:1/v1"},
		"base url not a url":     {"local", "--base-url", "not a url"},
		"base url no host":       {"local", "--base-url", "http:///v1"},
		"base url query":         {"local", "--base-url", "http://127.0.0.1:1/v1?key=x"},
		"local model whitespace": {"local", "--model", "two words"},
		"local model empty":      {"local", "--model", ""},
		"unknown flag":           {"local", "--credential", "x"},
	} {
		h := newUseHarness(t)
		if _, err := h.run(t, args...); err == nil {
			t.Errorf("%s: accepted %v", name, args)
		}
		if _, err := os.Stat(h.config); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: a file was written", name)
		}
	}
}

func TestUseNeverWritesWhenOnlyReporting(t *testing.T) {
	h := newUseHarness(t)
	out, err := h.run(t)
	if err != nil || !strings.Contains(out, "cloud") {
		t.Fatalf("%q, %v", out, err)
	}
	if h.read(t) != "" {
		t.Fatal("a status request created the user config")
	}
}

func TestTheUserConfigAcceptsTheLocalModelAndEverythingElseStillRefused(t *testing.T) {
	h := newUseHarness(t)
	if err := os.MkdirAll(filepath.Dir(h.config), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.config, []byte("llm.local.model: qwen3:8b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := newRegistry("")
	if err != nil {
		t.Fatalf("llm.local.model refused in the user config: %v", err)
	}
	if got, origin, _ := reg.String(flags.LLMLocalModel); got != "qwen3:8b" || origin != flags.OriginConfig {
		t.Fatalf("llm.local.model = %q from %s", got, origin)
	}
	// It chooses no credential and no endpoint; a key that does stays where it was.
	if err := os.WriteFile(h.config, []byte("llm.local.model: m\nmax_steps: 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newRegistry(""); err == nil || !strings.Contains(err.Error(), "max_steps") {
		t.Fatalf("err = %v", err)
	}
}

// llm.local.model was never operator scope, so the repository file may still
// set it; where both do, the user config is read last and wins, and
// MANVI_LLM_LOCAL_MODEL and then MANVI_MODEL win over both.
func TestLocalModelPrecedenceAcrossRepositoryUserEnvironmentAndMANVIMODEL(t *testing.T) {
	h := newUseHarness(t)
	repoConfig := filepath.Join(h.root, "repo-config.yaml")
	if err := os.WriteFile(repoConfig, []byte("llm.local.model: from-repo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := newRegistry(repoConfig)
	if err != nil {
		t.Fatalf("the repository file may set llm.local.model: %v", err)
	}
	if got, _, _ := reg.String(flags.LLMLocalModel); got != "from-repo" {
		t.Fatalf("repository only = %q", got)
	}
	if err := os.MkdirAll(filepath.Dir(h.config), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.config, []byte("llm.local.model: from-user\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err = newRegistry(repoConfig)
	if err != nil {
		t.Fatal(err)
	}
	if got, _, _ := reg.String(flags.LLMLocalModel); got != "from-user" {
		t.Fatalf("user over repository = %q", got)
	}
	t.Setenv(flags.EnvKey(flags.LLMLocalModel), "from-env")
	if reg, err = newRegistry(repoConfig); err != nil {
		t.Fatal(err)
	}
	if got, origin, _ := reg.String(flags.LLMLocalModel); got != "from-env" || origin != flags.OriginEnv {
		t.Fatalf("environment = %q from %s", got, origin)
	}
}
