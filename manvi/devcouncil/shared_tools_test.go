package devcouncil

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// TestEverySharedDevCouncilToolIsRegistered verifies Manvi registers every
// canonical tool named by DevCouncil's dc.SharedToolNames(), while explicitly
// pinning the intentional session-bound parameter differences between Manvi's
// in-process harness Registry (one Registry.session lease) and DevCouncil's
// standalone stateless MCP server (explicit client_id / lease_token arguments).
func TestEverySharedDevCouncilToolIsRegistered(t *testing.T) {
	f := newFixture(t)
	byName := make(map[string]tools.Tool)
	for _, tl := range f.reg.Tools() {
		byName[tl.Schema.Name] = tl
	}

	// Intentional session-bound parameter omissions relative to dc.SharedToolSpecs():
	// Manvi holds the active task and lease token inside Registry.session so an
	// agent never handles raw lease_token credentials or supplies client_id/task_id
	// on lease-bound tools.
	expectedSessionOmissions := map[string][]string{
		"devcouncil_get_diff":           {"task_id", "paths", "staged"},
		"devcouncil_checkout_task":      {"client_id", "agent", "force"},
		"devcouncil_renew_lease":        {"task_id", "lease_token"},
		"devcouncil_release_task":       {"task_id", "lease_token"},
		"devcouncil_next_task":          {"client_id"},
		"devcouncil_verify_task":        {"task_id", "lease_token"},
		"devcouncil_get_gaps":           {"task_id"},
		"devcouncil_policy_check_write": {"task_id"},
	}

	for _, name := range dc.SharedToolNames() {
		if !f.pipe.Has(name) {
			t.Errorf("canonical shared tool %q missing from Manvi tool surface", name)
		}
		tl, ok := byName[name]
		if !ok {
			t.Fatalf("canonical shared tool %q missing from Registry.Tools()", name)
		}
		spec, ok := dc.SharedToolSpecByName(name)
		if !ok {
			t.Fatalf("dc.SharedToolSpecByName(%q) returned false", name)
		}

		manviProps := schemaProps(t, tl.Schema.InputSchema)
		dcProps := schemaProps(t, spec.InputSchema)
		omitted := make(map[string]bool)
		for _, k := range expectedSessionOmissions[name] {
			omitted[k] = true
		}
		for prop := range dcProps {
			if omitted[prop] {
				continue
			}
			if _, present := manviProps[prop]; !present {
				t.Errorf("%s: DevCouncil property %q missing from Manvi schema (and not in expectedSessionOmissions)", name, prop)
			}
		}
	}
}

// TestPolicyCheckWriteSupportsOperation verifies devcouncil_policy_check_write
// routes operation ("create" | "modify" | "delete", defaulting to "modify")
// cleanly into the write gate and rejects unknown operations.
func TestPolicyCheckWriteSupportsOperation(t *testing.T) {
	f := newFixture(t)
	if out := f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"}); out["acquired"] != true {
		t.Fatalf("checkout failed: %v", out)
	}

	// Default (omitted) and "modify" succeed for a modify-planned path.
	for _, op := range []string{"", "modify", "create"} {
		args := map[string]string{"path": "src/calc.go"}
		if op != "" {
			args["operation"] = op
		}
		res := f.call("devcouncil_policy_check_write", args)
		if res.IsError {
			t.Fatalf("operation %q unexpectedly returned error: %s", op, res.Text)
		}
	}

	// TASK-001 forbids deletes on src/calc.go ("no_delete" / AllowedChange);
	// operation="delete" must evaluate against delete semantics rather than OpWrite.
	del := f.payload("devcouncil_policy_check_write", map[string]string{
		"path":      "src/calc.go",
		"operation": "delete",
	})
	if allowed, _ := del["allowed"].(bool); allowed {
		t.Fatalf("expected operation=delete on modify-only planned file to be blocked, got %v", del)
	}

	bad := f.call("devcouncil_policy_check_write", map[string]string{
		"path":      "src/calc.go",
		"operation": "truncate",
	})
	if !bad.IsError || !strings.Contains(bad.Text, "invalid operation") {
		t.Fatalf("expected error on unknown operation, got %+v", bad)
	}
}

func schemaProps(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var obj struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal input schema %s: %v", string(raw), err)
	}
	return obj.Properties
}
