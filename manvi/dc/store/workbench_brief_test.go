package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestWorkbenchBriefSurvivesRestartAndRefusesStaleSource(t *testing.T) {
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
	call("repositories.put", `{"id":"r","request_id":"r","expected_revision":0,"name":"Manvi","identity_key":"clone:r","remote_url":"https://credential@example.test"}`)
	call("items.put", `{"id":"t","request_id":"t","expected_revision":0,"title":"Keep E42","description":"Exact text: $(touch NEVER_EXECUTE) 🧪","repository_ids":["r"],"primary_repository_id":"r"}`)
	first := call("items.brief.get", `{"id":"t","expected_revision":1}`)
	var brief struct {
		Item struct {
			Markdown string `json:"markdown"`
		} `json:"item"`
	}
	if err := json.Unmarshal(first, &brief); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(brief.Item.Markdown, "Exact text: $(touch NEVER_EXECUTE) 🧪") || bytes.Contains(first, []byte("credential@")) {
		t.Fatalf("Incorrect brief: %s", first)
	}
	c.Close()
	c = New(c.Binary, c.DB)
	t.Cleanup(c.Close)
	if result := call("items.brief.get", `{"id":"t","expected_revision":1}`); !bytes.Equal(first, result) {
		t.Fatal("Restart changed the canonical snapshot")
	}
	call("items.put", `{"id":"t","request_id":"edit","expected_revision":1,"title":"Edited task","repository_ids":["r"],"primary_repository_id":"r"}`)
	_, err := c.Workbench(t.Context(), "items.brief.get", json.RawMessage(`{"id":"t","expected_revision":1}`))
	var refusal *WorkbenchError
	if !errors.As(err, &refusal) || refusal.Code != "revision_conflict" {
		t.Fatalf("Stale brief: %v", err)
	}
	if result := call("items.brief.get", `{"id":"t","expected_revision":2}`); !bytes.Contains(result, []byte("Edited task")) {
		t.Fatal("Fresh brief is missing saved changes")
	}
}
