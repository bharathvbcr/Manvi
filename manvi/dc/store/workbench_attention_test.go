package store

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestWorkbenchAttentionSurvivesClientRestartAndCannotAcceptWork(t *testing.T) {
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
	call("repositories.put", `{"id":"r","request_id":"r","expected_revision":0,"name":"Repo","identity_key":"local:r"}`)
	call("items.put", `{"id":"t","request_id":"t","expected_revision":0,"title":"Private task","repository_ids":["r"],"primary_repository_id":"r"}`)
	call("enhancements.create", `{"id":"p","request_id":"p","expected_revision":0,"task_id":"t","source_revision":1,"fields":["title"],"provider":"local","model":"configured"}`)
	call("enhancements.complete", `{"id":"p","request_id":"ready","expected_revision":1,"title":"Precise title"}`)
	c.Close()
	c = New(c.Binary, c.DB)
	t.Cleanup(c.Close)
	var page struct {
		Items []struct {
			ID     string `json:"id"`
			Kind   string `json:"kind"`
			Target string `json:"target_status"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(call("attention.list", `{}`), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Kind != "enhancement_ready" || page.Items[0].Target != "current" {
		t.Fatalf("bad inbox: %+v", page)
	}
	call("attention.update", fmt.Sprintf(`{"id":%q,"request_id":"read","expected_revision":1,"action":"read"}`, page.Items[0].ID))
	var result struct {
		Item struct {
			State string `json:"state"`
		} `json:"item"`
	}
	if err := json.Unmarshal(call("enhancements.get", `{"id":"p"}`), &result); err != nil {
		t.Fatal(err)
	}
	if result.Item.State != "ready" {
		t.Fatalf("read accepted work: %+v", result)
	}
}
