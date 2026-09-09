package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestWorkbenchAutomationSharesStorageAndSurvivesHostRestart(t *testing.T) {
	c := client(t)
	t.Cleanup(c.Close)
	call := func(method, input string) json.RawMessage {
		t.Helper()
		result, err := c.Workbench(t.Context(), method, json.RawMessage(input))
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		return result
	}
	call("repositories.put", `{"id":"r","request_id":"r","expected_revision":0,"name":"Manvi","identity_key":"r"}`)
	call("items.put", `{"id":"t","request_id":"t","expected_revision":0,"title":"Investigate E42","repository_ids":["r"],"primary_repository_id":"r"}`)
	settings := call("automation.get", `{"id":"profile"}`)
	queued := call("automation.list", `{}`)
	var page struct {
		Total int `json:"total"`
		Items []struct {
			Due int64 `json:"not_before_ms"`
		} `json:"items"`
	}
	if err := json.Unmarshal(queued, &page); err != nil || page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("saved text was not queued: %s (%v)", queued, err)
	}
	c.Close()
	c = New(c.Binary, c.DB)
	t.Cleanup(c.Close)
	if !bytes.Equal(settings, call("automation.get", `{"id":"profile"}`)) || !bytes.Equal(queued, call("automation.list", `{}`)) {
		t.Fatal("host restart lost settings or queued work")
	}
	first := idleChild(t, c)
	prepare := `{"id":"e","request_id":"prepare","expected_revision":0,"task_id":"t","expected_task_revision":1,"expected_settings_revision":1,"provider":"local","model":"fixture-model"}`
	if remaining := time.Until(time.UnixMilli(page.Items[0].Due)); remaining > 0 {
		if remaining > 2*time.Second {
			t.Fatal("debounce exceeds one second")
		}
		select {
		case <-time.After(remaining):
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
	}
	prepared := call("automation.prepare", prepare)
	if !bytes.Equal(prepared, call("automation.prepare", prepare)) {
		t.Fatal("uncertain preparation did not reconcile the original receipt")
	}
	_, err := c.Workbench(t.Context(), "automation.put", json.RawMessage(`{"id":"profile","request_id":"stale","expected_revision":0,"enabled":false}`))
	var refusal *WorkbenchError
	if !errors.As(err, &refusal) || refusal.Code != "revision_conflict" {
		t.Fatalf("settings conflict lost its type: %v", err)
	}
	call("automation.put", `{"id":"profile","request_id":"disable","expected_revision":1,"enabled":false}`)
	if !bytes.Contains(call("enhancements.get", `{"id":"e"}`), []byte(`"state":"dismissed"`)) {
		t.Fatal("disabling automation did not dismiss the pending proposal")
	}
	if idleChild(t, c) != first {
		t.Fatal("automation replaced the persistent store child")
	}
}
