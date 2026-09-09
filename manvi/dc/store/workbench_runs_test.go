package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func TestWorkbenchLaunchClaimRemainsConsumedAfterHostRestart(t *testing.T) {
	c := client(t)
	t.Cleanup(c.Close)
	call := func(method, input string) json.RawMessage {
		t.Helper()
		result, err := c.Workbench(t.Context(), method, json.RawMessage(input))
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	quote := func(value string) string {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	root := filepath.Join(t.TempDir(), "checkout with spaces")
	common := filepath.Join(root, ".git")
	call("repositories.put", fmt.Sprintf(`{"id":"repo","request_id":"repo","expected_revision":0,"name":"Manvi","identity_key":%s}`, quote("local:"+common)))
	call("items.put", `{"id":"task","request_id":"task","expected_revision":0,"title":"Keep E42","description":"Exact source must survive restart","repository_ids":["repo"],"primary_repository_id":"repo"}`)
	prepare := fmt.Sprintf(`{"id":"run","request_id":"prepare","expected_revision":0,"task_id":"task","source_revision":1,"repository_id":"repo","repository_revision":1,"provider":"claude","permission_mode":"inspect","cwd":%s,"git_dir":%s,"git_common_dir":%s,"head_oid":null}`, quote(root), quote(common), quote(common))
	prepared := call("runs.prepare", prepare)
	if bytes.Contains(prepared, []byte("Exact source")) {
		t.Fatal("Preparation receipt repeated the large source brief")
	}
	claim := `{"id":"run","request_id":"claim","expected_revision":1,"owner_id":"host","session_id":"terminal"}`
	call("runs.claim", claim)
	c.Close()
	c = New(c.Binary, c.DB)
	t.Cleanup(c.Close)
	_, err := c.Workbench(t.Context(), "runs.claim", json.RawMessage(claim))
	var refusal *WorkbenchError
	if !errors.As(err, &refusal) || refusal.Code != "claim_consumed" {
		t.Fatalf("Consumed claim was reissued: %v", err)
	}
	result := call("runs.get", `{"id":"run"}`)
	var saved struct {
		Item struct {
			State   string `json:"state"`
			Session string `json:"session_id"`
			Brief   struct {
				Task struct {
					Description string `json:"description"`
				} `json:"task"`
			} `json:"brief"`
		} `json:"item"`
	}
	if err := json.Unmarshal(result, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Item.State != "starting" || saved.Item.Session != "terminal" || saved.Item.Brief.Task.Description != "Exact source must survive restart" {
		t.Fatalf("Incorrect durable launch: %s", result)
	}
	page := call("runs.list", `{"task_id":"task","limit":1}`)
	if bytes.Contains(page, []byte("Exact source")) {
		t.Fatal("Run list loaded the complete brief")
	}
}
