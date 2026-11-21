package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/credentials"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
)

func searchAdapterFor(t *testing.T, cfg map[string]string) (*anthropic.Adapter, error) {
	t.Helper()
	reg := flags.New()
	if err := reg.Define(harnessFlagDefs()...); err != nil {
		t.Fatal(err)
	}
	if err := reg.LoadConfig(cfg); err != nil {
		return nil, err
	}
	adapter := anthropic.New("http://127.0.0.1:1", func() (credentials.Secret, error) { return credentials.Secret{}, nil })
	return adapter, applyWebSearch(adapter, reg)
}

func searchProbeRequest() llm.Request {
	return llm.Request{Model: "claude-haiku-5-5", Tools: []llm.ToolSchema{{Name: "t"}}}
}

func TestWebSearchSettingsDefaultOffAndTurnOnWithABound(t *testing.T) {
	adapter, err := searchAdapterFor(t, nil)
	if err != nil || adapter.MaxSearches(searchProbeRequest()) != 0 {
		t.Fatalf("default: searches=%d err=%v; web search must ship off", adapter.MaxSearches(searchProbeRequest()), err)
	}
	adapter, err = searchAdapterFor(t, map[string]string{webSearchKey: "on"})
	if err != nil || adapter.MaxSearches(searchProbeRequest()) != defaultWebSearchMaxUses {
		t.Fatalf("on: searches=%d err=%v, want the default bound %d", adapter.MaxSearches(searchProbeRequest()), err, defaultWebSearchMaxUses)
	}
	adapter, err = searchAdapterFor(t, map[string]string{webSearchKey: "on", webSearchMaxUsesKey: "7"})
	if err != nil || adapter.MaxSearches(searchProbeRequest()) != 7 {
		t.Fatalf("on with 7: searches=%d err=%v", adapter.MaxSearches(searchProbeRequest()), err)
	}
	for _, bad := range []string{"0", "-1", "21", "x"} {
		if _, err := searchAdapterFor(t, map[string]string{webSearchKey: "on", webSearchMaxUsesKey: bad}); err == nil {
			t.Errorf("a bound of %q was accepted", bad)
		}
	}
	if _, err := searchAdapterFor(t, map[string]string{webSearchKey: "maybe"}); err == nil {
		t.Error("an unknown mode was accepted")
	}
}

// Both settings send model-written text to a third party, and the repository
// config is a file an agent can write.
func TestWebSearchSettingsAreRefusedFromTheRepository(t *testing.T) {
	for _, key := range []string{webSearchKey, webSearchMaxUsesKey} {
		isolatedUserConfig(t)
		dir := t.TempDir()
		value := "on"
		if key == webSearchMaxUsesKey {
			value = "5"
		}
		writeFile(t, dir, "config.yaml", key+": "+value+"\n")
		if _, err := newRegistry(filepath.Join(dir, "config.yaml")); err == nil ||
			!strings.Contains(err.Error(), "operator scope") || !strings.Contains(err.Error(), key) {
			t.Fatalf("a repository config set %s: %v", key, err)
		}
	}
}
