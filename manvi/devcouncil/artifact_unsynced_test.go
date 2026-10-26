package devcouncil

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
	"github.com/bharathvbcr/Manvi/manvi/artifacts"
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
