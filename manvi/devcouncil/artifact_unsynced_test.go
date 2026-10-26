package devcouncil

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
	"github.com/bharathvbcr/Manvi/manvi/artifacts"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// An artifact written with its directory sync unconfirmed is saved, not failed:
// the tool result is not an error (a model that sees an error retries, and the
// retry only learns the artifact exists), it says the artifact was saved, and
// it names the durability risk instead of reporting a clean success.
func TestArtifactSavedReportsUnconfirmedDurabilityWithoutAnError(t *testing.T) {
	art := &artifacts.Artifact{Name: "plan.md", Path: "/s/plan.md", Revision: 2}
	cause := fmt.Errorf("artifacts: plan.md: %w", artifacts.ErrPublishedUnsynced)

	res := artifactSaved("updated", art, cause, policy.Decision{})
	if res.IsError {
		t.Fatalf("a published artifact was reported as an error: %s", res.Text)
	}
	if len(res.Wrote) != 1 || res.Wrote[0] != "/s/plan.md" {
		t.Fatalf("the write is not recorded: %v", res.Wrote)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(res.Text), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["status"] != "updated" || payload["revision"] != float64(2) {
		t.Errorf("payload lost the outcome: %v", payload)
	}
	if payload["durability"] != "unconfirmed" || payload["warning"] == nil {
		t.Errorf("payload does not name the durability risk: %v", payload)
	}

	clean := artifactSaved("created", art, nil, policy.Decision{})
	var cleanPayload map[string]any
	if err := json.Unmarshal([]byte(clean.Text), &cleanPayload); err != nil {
		t.Fatal(err)
	}
	if _, has := cleanPayload["durability"]; has || clean.IsError {
		t.Errorf("a confirmed write was reported as unconfirmed: %v", cleanPayload)
	}
}

// The real create and update handlers, run through the pipeline over a store
// whose directory sync fails: the artifact is published, so neither call is an
// error result, both record the write, both name the durability risk and say not
// to retry, and the artifact reads back at the new revision.
func TestArtifactToolsOverAFailingDirectorySyncReportSavedNotFailed(t *testing.T) {
	f := newFixture(t)
	if out := f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"}); out["task_id"] != "TASK-001" {
		t.Fatalf("checkout = %v", out)
	}
	failing := false
	store, err := artifacts.NewStoreWithDirSync(filepath.Join(t.TempDir(), "artifacts"), func(string) error {
		if failing {
			return errors.New("injected directory sync failure")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f.reg.deps.Artifacts = store

	check := func(step string, res tools.Result, status string, revision float64, content string) {
		t.Helper()
		if res.IsError {
			t.Fatalf("%s: a published artifact was reported as an error: %s", step, res.Text)
		}
		if len(res.Wrote) != 1 {
			t.Errorf("%s: the write is not recorded: %v", step, res.Wrote)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(res.Text), &payload); err != nil {
			t.Fatalf("%s: unparseable payload: %v (%q)", step, err, res.Text)
		}
		if payload["status"] != status || payload["revision"] != revision {
			t.Errorf("%s: payload lost the outcome: %v", step, payload)
		}
		if payload["durability"] != "unconfirmed" {
			t.Errorf("%s: payload does not mark durability unconfirmed: %v", step, payload)
		}
		if w, _ := payload["warning"].(string); !strings.Contains(w, "do not retry") {
			t.Errorf("%s: no do-not-retry warning: %v", step, payload)
		}
		got, err := store.Get("spec.md")
		if err != nil || got.Content != content || float64(got.Revision) != revision {
			t.Fatalf("%s: the artifact is not readable at the new revision: %+v, %v", step, got, err)
		}
	}

	failing = true
	check("create", f.call("devcouncil_create_artifact", map[string]any{
		"name": "spec.md", "content": "v1", "metadata": map[string]any{"summary": "s"},
	}), "created", 1, "v1")
	check("update", f.call("devcouncil_update_artifact", map[string]any{
		"name": "spec.md", "content": "v2",
	}), "updated", 2, "v2")
}
