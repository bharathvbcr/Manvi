package main

import (
	"encoding/json/v2"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/prompt"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// The operator settings that choose which tool schemas a turn offers. They are
// written as literals on purpose: a key the catalogue does not define fails the
// registry load, which is the failure these tests should show before the keys
// exist.
const (
	coreOnlyKey = "harness.core_tools_only"
	dynamicKey  = "harness.dynamic_tools"
	batchKey    = "harness.batch_tool"
)

func offeredNames(req llm.Request) map[string]bool {
	names := map[string]bool{}
	for _, s := range req.Tools {
		names[s.Name] = true
	}
	return names
}

// firstRequest runs one TUI turn on the replay provider, which is not the
// local provider, and returns what the model was first sent.
func firstRequest(t *testing.T, set map[string]string) llm.Request {
	t.Helper()
	reqs := tuiTurn(t, truncatedThenDone(), set)
	if len(reqs) == 0 {
		t.Fatal("the model was never asked")
	}
	return reqs[0]
}

// Every provider can ask for the core set or for dynamic activation, and the
// defaults stay what they were: a non-local provider is offered everything.
// Observed from the request the model was sent.
func TestOperatorSettingsReachTheOfferedSchemas(t *testing.T) {
	full := firstRequest(t, nil)
	fullNames := offeredNames(full)
	if !fullNames[tools.BatchToolName] || !fullNames["devcouncil_spawn_subagents"] {
		t.Fatalf("the default surface lost a tool it offered before: batch=%v spawn=%v",
			fullNames[tools.BatchToolName], fullNames["devcouncil_spawn_subagents"])
	}

	t.Run("core only", func(t *testing.T) {
		req := firstRequest(t, map[string]string{coreOnlyKey: "true"})
		if len(req.Tools) >= len(full.Tools) {
			t.Fatalf("core_tools_only offered %d tools, the full set is %d", len(req.Tools), len(full.Tools))
		}
		if offeredNames(req)["devcouncil_spawn_subagents"] {
			t.Error("core_tools_only still offered a sub-agent tool")
		}
		if !offeredNames(req)[tools.BatchToolName] {
			t.Error("core_tools_only dropped devcouncil_batch, which is a core tool")
		}
	})

	t.Run("dynamic", func(t *testing.T) {
		req := firstRequest(t, map[string]string{dynamicKey: "true"})
		if len(req.Tools) >= len(full.Tools) {
			t.Fatalf("dynamic_tools offered %d tools, the full set is %d", len(req.Tools), len(full.Tools))
		}
		if !offeredNames(req)[tools.ActivateToolName] {
			t.Error("dynamic_tools did not offer the activation tool, so an omitted tool cannot be fetched")
		}
		if !strings.Contains(req.System, tools.ActivateToolName) {
			t.Error("dynamic_tools offered the tools but the prompt does not describe discovery")
		}
	})

	t.Run("local flags keep working off the local provider only", func(t *testing.T) {
		req := firstRequest(t, map[string]string{"llm.local.core_tools_only": "true"})
		if len(req.Tools) != len(full.Tools) {
			t.Fatalf("a local-only setting changed a non-local offer: %d vs %d", len(req.Tools), len(full.Tools))
		}
	})
}

// harness.batch_tool=off withholds devcouncil_batch and with it the prompt's
// batch line and the serial-lookup rule, which only fires while a batch is
// offered.
func TestBatchToolSwitchWithholdsTheToolTheLineAndTheRule(t *testing.T) {
	on := firstRequest(t, nil)
	if !offeredNames(on)[tools.BatchToolName] || !strings.Contains(on.System, "devcouncil_batch") {
		t.Fatal("the default turn does not offer the batch tool and name it in the prompt")
	}

	set := map[string]string{batchKey: "off"}
	off := firstRequest(t, set)
	if offeredNames(off)[tools.BatchToolName] {
		t.Error("harness.batch_tool=off still offered devcouncil_batch")
	}
	if strings.Contains(off.System, "devcouncil_batch") {
		t.Error("harness.batch_tool=off left devcouncil_batch in the system prompt")
	}
	if len(off.Tools) != len(on.Tools)-1 {
		t.Errorf("off withheld %d tools, want exactly the batch tool", len(on.Tools)-len(off.Tools))
	}

	reqs := tuiTurn(t, lookupsThenDone(), set)
	var sent strings.Builder
	for _, m := range reqs[len(reqs)-1].Messages {
		for _, b := range m.Content {
			if r, ok := b.(llm.ToolResultBlock); ok {
				for _, part := range r.Content {
					if tb, ok := part.(llm.TextBlock); ok {
						sent.WriteString(tb.Text)
					}
				}
			}
		}
	}
	if strings.Contains(sent.String(), "refused, not run") || strings.Contains(sent.String(), "devcouncil_batch") {
		t.Errorf("the serial-lookup rule fired with no batch tool to point at: %q", sent.String())
	}
}

// The three settings change what the agent can reach, so a repository config
// the agent can write may not set them.
func TestToolOfferingSettingsAreRefusedFromTheRepository(t *testing.T) {
	for _, key := range []string{coreOnlyKey, dynamicKey, batchKey} {
		t.Run(key, func(t *testing.T) {
			isolatedUserConfig(t)
			dir := t.TempDir()
			value := "true"
			if key == batchKey {
				value = "off"
			}
			writeFile(t, dir, "config.yaml", key+": "+value+"\n")
			if _, err := newRegistry(filepath.Join(dir, "config.yaml")); err == nil ||
				!strings.Contains(err.Error(), "operator scope") || !strings.Contains(err.Error(), key) {
				t.Fatalf("a repository config set %s without the operator-scope refusal: %v", key, err)
			}
		})
	}
}

// Records what the offered surface costs per request, estimated with the same
// function the prompt budget uses, and holds only what must be true: the core
// set is smaller than the whole. Run with -v to read the numbers.
func TestCoreSurfaceIsSmallerThanTheFullSurface(t *testing.T) {
	t.Chdir(e2eRepo(t))
	t.Setenv("MANVI_HARNESS_INIT_ENABLED", "false")
	_, pipeline, err := nativeToolsWith(registryWith(t, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	estimate := func(schemas []llm.ToolSchema) int {
		raw, err := json.Marshal(schemas)
		if err != nil {
			t.Fatal(err)
		}
		return prompt.EstimateTokens(string(raw))
	}
	full, core := pipeline.Schemas(), pipeline.CoreSchemas()
	fullTokens, coreTokens := estimate(full), estimate(core)
	t.Logf("tool schemas: full %d tools, ~%d tokens; core %d tools, ~%d tokens; difference ~%d tokens per request",
		len(full), fullTokens, len(core), coreTokens, fullTokens-coreTokens)
	if len(core) == 0 || len(core) >= len(full) {
		t.Fatalf("core set has %d tools, full set %d", len(core), len(full))
	}
	if coreTokens >= fullTokens {
		t.Fatalf("core set estimates %d tokens, full set %d", coreTokens, fullTokens)
	}
}
