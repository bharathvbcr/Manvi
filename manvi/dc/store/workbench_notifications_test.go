package store

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestWorkbenchNativeClaimSurvivesClientRestartWithoutAnotherDelivery(t *testing.T) {
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
	call("notifications.settings.put", `{"id":"profile","request_id":"enable","expected_revision":1,"enabled":true}`)
	call("enhancements.create", `{"id":"p","request_id":"p","expected_revision":0,"task_id":"t","source_revision":1,"fields":["title"],"provider":"local","model":"configured"}`)
	var event struct {
		Sequence int64 `json:"sequence"`
	}
	if err := json.Unmarshal(call("enhancements.complete", `{"id":"p","request_id":"complete","expected_revision":1,"title":"Precise title"}`), &event); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("event-%d", event.Sequence)
	claim := fmt.Sprintf(`{"id":%q,"request_id":"claim","expected_revision":0,"minute_of_day":720}`, id)
	call("notifications.claim", claim)
	c.Close()
	c = New(c.Binary, c.DB)
	t.Cleanup(c.Close)
	var page struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(call("notifications.pending.list", `{"minute_of_day":720}`), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 0 {
		t.Fatalf("restart offered %d duplicate deliveries", page.Total)
	}
	if _, err := c.Workbench(t.Context(), "notifications.claim", json.RawMessage(claim)); err == nil {
		t.Fatal("replayed delivery claim was accepted")
	}
	var result struct {
		Item struct {
			State string `json:"state"`
		} `json:"item"`
	}
	if err := json.Unmarshal(call("notifications.delivery.get", fmt.Sprintf(`{"id":%q}`, id)), &result); err != nil {
		t.Fatal(err)
	}
	if result.Item.State != "uncertain" {
		t.Fatalf("lost outcome became %q", result.Item.State)
	}
}
