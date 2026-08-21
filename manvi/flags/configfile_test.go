package flags

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func parse(t *testing.T, doc string) map[string]string {
	t.Helper()
	values, err := ParseConfig(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("ParseConfig(%q): %v", doc, err)
	}
	return values
}

func parseErr(t *testing.T, doc string) string {
	t.Helper()
	if _, err := ParseConfig(strings.NewReader(doc)); err != nil {
		return err.Error()
	}
	t.Fatalf("ParseConfig(%q) was accepted", doc)
	return ""
}

func TestAFlatMappingReads(t *testing.T) {
	got := parse(t, `# the harness settings that belong in a commit
llm.local.base_url: http://127.0.0.1:11434/v1
llm.local.model: qwen3.8:27b-mlx

llm.provider.default: local
`)
	want := map[string]string{
		"llm.local.base_url":   "http://127.0.0.1:11434/v1",
		"llm.local.model":      "qwen3.8:27b-mlx",
		"llm.provider.default": "local",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d keys, want %d: %v", len(got), len(want), got)
	}
}

func TestANestedMappingReads(t *testing.T) {
	got := parse(t, `
llm:
  local:
    base_url: http://127.0.0.1:11434/v1
    model: qwen3.8:27b-mlx
  provider:
    default: local
harness:
  posture: strict
`)
	want := map[string]string{
		"llm.local.base_url":   "http://127.0.0.1:11434/v1",
		"llm.local.model":      "qwen3.8:27b-mlx",
		"llm.provider.default": "local",
		"harness.posture":      "strict",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestDevCouncilConfigFileReads(t *testing.T) {
	doc := `commands:
  lint: []
  test: []
  typecheck: []
execution:
  checkpoint_before_each_task: true
  cursor_resume_mode: 'off'
  max_repair_attempts: 3
gates:
  block_dependency_changes_without_approval: true
  block_failed_commands: true
integrations:
  cli_agents:
    profiles:
      prod:
        prompt_preamble: 'Profile: prod. Keep edits minimal and explicitly within
          task scope.'
        timeout_seconds: 1800
models:
  provider: openrouter
verification:
  rigor:
    enabled: true
llm:
  local:
    model: qwen3.8:27b-mlx
`
	got := parse(t, doc)
	if got["execution.max_repair_attempts"] != "3" {
		t.Errorf("execution.max_repair_attempts = %q, want 3", got["execution.max_repair_attempts"])
	}
	if got["execution.cursor_resume_mode"] != "off" {
		t.Errorf("execution.cursor_resume_mode = %q, want off", got["execution.cursor_resume_mode"])
	}
	if got["gates.block_failed_commands"] != "true" {
		t.Errorf("gates.block_failed_commands = %q, want true", got["gates.block_failed_commands"])
	}
	if got["models.provider"] != "openrouter" {
		t.Errorf("models.provider = %q, want openrouter", got["models.provider"])
	}
	if got["llm.local.model"] != "qwen3.8:27b-mlx" {
		t.Errorf("llm.local.model = %q, want qwen3.8:27b-mlx", got["llm.local.model"])
	}
}

func TestAValueKeepsItsColons(t *testing.T) {
	got := parse(t, "llm.local.model: qwen3.8:27b-mlx\nllm.local.base_url: http://127.0.0.1:8080/v1\n")
	if got["llm.local.model"] != "qwen3.8:27b-mlx" {
		t.Errorf("model = %q; the value was cut at its own colon", got["llm.local.model"])
	}
	if got["llm.local.base_url"] != "http://127.0.0.1:8080/v1" {
		t.Errorf("base_url = %q", got["llm.local.base_url"])
	}
}

func TestCommentsEndAValueOnlyAfterWhitespace(t *testing.T) {
	got := parse(t, strings.Join([]string{
		"llm.local.model: qwen3.8:27b-mlx  # the one this machine serves",
		"llm.local.base_url: http://127.0.0.1:8080/v1#frag",
		"llm.local.stop: '<|im_end|>,#not-a-comment'",
	}, "\n"))
	if got["llm.local.model"] != "qwen3.8:27b-mlx" {
		t.Errorf("model = %q; the trailing comment was not removed", got["llm.local.model"])
	}
	if got["llm.local.base_url"] != "http://127.0.0.1:8080/v1#frag" {
		t.Errorf("base_url = %q; a # with no space before it is part of the value", got["llm.local.base_url"])
	}
	if got["llm.local.stop"] != "<|im_end|>,#not-a-comment" {
		t.Errorf("stop = %q; a quoted value keeps its #", got["llm.local.stop"])
	}
}

func TestQuotedValuesSurviveIntact(t *testing.T) {
	got := parse(t, "a: \"  padded  \"\nb: ''\nc: '#'\n")
	if got["a"] != "  padded  " {
		t.Errorf("a = %q; quoting is what preserves surrounding space", got["a"])
	}
	if v, ok := got["b"]; !ok || v != "" {
		t.Errorf(`b = %q, %v; "" is a real value and must not be a parse failure`, v, ok)
	}
	if got["c"] != "#" {
		t.Errorf("c = %q", got["c"])
	}
}

func TestMalformedSyntaxIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, doc, want string }{
		{"no colon", "a 1\n", "no \":\" found"},
		{"empty key", ": 1\n", "key is empty"},
		{"unclosed quote", `a: "open` + "\n", "never closed"},
		{"junk after quote", `a: "x" y` + "\n", "after the closing quote"},
		{"duplicate key", "a: 1\na: 2\n", "already set on line 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseErr(t, tc.doc)
			if !strings.Contains(got, tc.want) {
				t.Errorf("error = %q, want it to mention %q", got, tc.want)
			}
			if !strings.Contains(got, "line ") {
				t.Errorf("error = %q; a refusal must name the line it is about", got)
			}
		})
	}
}

func TestAMissingFileIsNotAnError(t *testing.T) {
	r := New()
	if err := DefineHarnessFlags(r); err != nil {
		t.Fatal(err)
	}
	found, err := LoadConfigFile(r, filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("a missing config file was an error: %v", err)
	}
	if found {
		t.Error("a file that is not there was reported as found")
	}
}

func TestAnUnknownKeyInHarnessNamespaceIsRejected(t *testing.T) {
	r := New()
	if err := DefineHarnessFlags(r); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("llm.local.modle: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfigFile(r, path)
	if err == nil {
		t.Fatal("a misspelled key was accepted")
	}
	if !strings.Contains(err.Error(), "llm.local.modle") {
		t.Errorf("error = %v, want it to name the key", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %v, want it to name the file", err)
	}
}

func TestSharedDevCouncilConfigLoadsCleanly(t *testing.T) {
	r := New()
	if err := DefineHarnessFlags(r); err != nil {
		t.Fatal(err)
	}
	doc := `commands:
  lint: []
  test: []
execution:
  max_repair_attempts: 3
gates:
  block_failed_commands: true
verification:
  rigor:
    enabled: true
llm:
  local:
    model: qwen3.8:27b-mlx
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	found, err := LoadConfigFile(r, path)
	if err != nil {
		t.Fatalf("LoadConfigFile failed on shared DevCouncil config: %v", err)
	}
	if !found {
		t.Fatal("config file was not found")
	}

	// Verify MANVI setting was loaded
	model, _, err := r.String(LLMLocalModel)
	if err != nil || model != "qwen3.8:27b-mlx" {
		t.Errorf("llm.local.model = %q, want qwen3.8:27b-mlx", model)
	}

	// Verify alias verification.rigor.enabled -> verify.rigor.enabled
	rigor, _, err := r.Bool(VerifyRigorEnabled)
	if err != nil || !rigor {
		t.Errorf("verify.rigor.enabled = %v, want true", rigor)
	}
}
