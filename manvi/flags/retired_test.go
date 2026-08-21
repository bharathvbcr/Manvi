package flags

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The four llm.provider.*.enabled keys were declared, documented as enabling an
// adapter, and read by nothing. This pins their removal: a key that is defined
// is a key `manvi flags` lists and an operator will set.
func TestTheProviderEnableSwitchesAreGone(t *testing.T) {
	reg := New()
	if err := DefineHarnessFlags(reg); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"llm.provider.anthropic.enabled",
		"llm.provider.gemini.enabled",
		"llm.provider.xai.enabled",
		"llm.provider.local.enabled",
	} {
		if _, ok := reg.Def(key); ok {
			t.Errorf("%s is still defined, and nothing in the harness reads it: "+
				"llm.provider.default plus the provider's credential decide which adapter runs", key)
		}
	}
}

// Removing the key is only half of it. LoadEnv iterates the flags that are
// defined, so a variable naming a removed one is not refused — it is not looked
// at, which is the same silence the operator already had and the reason they
// never learned the setting did nothing.
func TestRetiredEnvNamesEverySettingThatIsGone(t *testing.T) {
	got := RetiredEnv([]string{
		"MANVI_LLM_PROVIDER_LOCAL_ENABLED=false",
		"MANVI_LLM_PROVIDER_GEMINI_ENABLED=true",
		"MANVI_LLM_LOCAL_MODEL=qwen3.8:27b-mlx",
		"HOME=/home/x",
	})
	if len(got) != 2 {
		t.Fatalf("reported %d retired variables, want 2: %+v", len(got), got)
	}
	for _, r := range got {
		if r.Why == "" {
			t.Errorf("%s is reported with no explanation; an operator is told to unset it and not why", r.Env)
		}
	}
	if got[0].Env != "MANVI_LLM_PROVIDER_GEMINI_ENABLED" || got[1].Env != "MANVI_LLM_PROVIDER_LOCAL_ENABLED" {
		t.Errorf("unexpected variables reported: %+v", got)
	}
	if len(RetiredEnv([]string{"MANVI_LLM_LOCAL_MODEL=x", "HOME=/home/x"})) != 0 {
		t.Error("a live setting was reported as retired")
	}
}

// A config file is refused either way, but "unknown key" reads as a typo the
// operator should correct rather than as a setting somebody removed.
func TestARetiredKeyInTheConfigFileSaysItWasRemoved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(
		"llm.provider.local.enabled: false\nllm.local.model: qwen3.8:27b-mlx\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := NewHarnessRegistry(path)
	if err == nil {
		t.Fatal("a config file setting a removed key was accepted")
	}
	for _, want := range []string{"llm.provider.local.enabled", "no longer has", "llm.provider.default"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}
