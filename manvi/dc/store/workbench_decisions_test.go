package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestDecisionBindingHashesExactPayloadAndCannotRedeliverAfterRestart(t *testing.T) {
	c := client(t)
	t.Cleanup(c.Close)
	call := func(method, raw string) json.RawMessage {
		t.Helper()
		out, err := c.Workbench(t.Context(), method, json.RawMessage(raw))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	root := t.TempDir()
	call("repositories.put", fmt.Sprintf(`{"id":"r","request_id":"r","expected_revision":0,"name":"Repo","identity_key":%q}`, "local:"+root))
	call("items.put", `{"id":"t","request_id":"t","expected_revision":0,"title":"Task","repository_ids":["r"],"primary_repository_id":"r"}`)
	call("runs.prepare", fmt.Sprintf(`{"id":"run","request_id":"prep","expected_revision":0,"task_id":"t","source_revision":1,"repository_id":"r","repository_revision":1,"provider":"codex","permission_mode":"ask","cwd":%q,"git_dir":%q,"git_common_dir":%q,"head_oid":null}`, root, root, root))
	call("runs.claim", `{"id":"run","request_id":"claim-run","expected_revision":1,"owner_id":"host","session_id":"session"}`)
	call("runs.started", `{"id":"run","request_id":"start","expected_revision":2,"owner_id":"host","session_id":"session","process_id":123,"process_start":"fixture-process"}`)
	source := DecisionRequest{ID: "decision", RequestID: "create", RunID: "run", OwnerID: "host", SessionID: "session", ProviderThreadID: "thread", ProviderTurnID: "turn", ProtocolRequestID: "n:42", Kind: "permission", Payload: `{"command":"git status"}`, Deadline: time.Now().Unix() + 600}
	created, err := c.CreateDecision(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Item struct {
			PayloadDigest string `json:"payload_digest"`
			State         string `json:"state"`
		} `json:"item"`
	}
	if err = json.Unmarshal(created, &wire); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(source.Payload))
	if wire.Item.PayloadDigest != hex.EncodeToString(digest[:]) {
		t.Fatal("payload digest changed")
	}
	call("decisions.decide", fmt.Sprintf(`{"id":"decision","request_id":"decide","expected_revision":1,"payload_digest":%q,"decision":"allow_once"}`, wire.Item.PayloadDigest))
	changed := source
	changed.Payload = `{"command":"git push"}`
	if _, err = c.ClaimDecision(t.Context(), changed, "deliver", 2); err == nil {
		t.Fatal("changed payload consumed permission")
	}
	if _, err = c.ClaimDecision(t.Context(), source, "deliver", 2); err != nil {
		t.Fatal(err)
	}
	c.Close()
	c = New(c.Binary, c.DB)
	t.Cleanup(c.Close)
	if _, err = c.ClaimDecision(t.Context(), source, "deliver", 2); err == nil {
		t.Fatal("restart reissued a consumed decision")
	}
	if err = json.Unmarshal(call("decisions.get", `{"id":"decision"}`), &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Item.State != "dispatching" {
		t.Fatalf("lost provider receipt became %s", wire.Item.State)
	}
}
