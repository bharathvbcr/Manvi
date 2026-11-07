package serve

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// prepared is a chat.prepare answer read as the host reads it: the steps, and
// the two fields the answer gained, by their wire names so the test compiles
// against a build that does not have them yet.
type prepared struct {
	Steps             []PrepareStep `json:"steps"`
	ServerSideEditing bool          `json:"server_side_editing"`
	ServerSideNote    string        `json:"server_side_note"`
}

// prepareRaw sends chat.prepare params as the host's JSON, so a field the type
// does not yet have is still on the wire. The request is a bulky history in a
// small window: the shape that always produced steps.
func prepareRaw(t *testing.T, d *driver, session, provider string) prepared {
	t.Helper()
	params := map[string]any{
		"session_id":     session,
		"system":         "you are a coding agent",
		"messages":       bulkyHistory(20, 120),
		"context_window": 16384,
	}
	if provider != "" {
		params["provider"] = provider
	}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	resp := d.call(Request{ID: fmt.Sprint(d.n), Op: OpChatPrepare, Params: raw})
	if !resp.OK {
		t.Fatalf("chat.prepare failed: %+v", resp.Error)
	}
	var out prepared
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// For a provider that binds thinking to history, rewriting an earlier tool
// result invalidates every thinking block signed after it. This plane cannot
// strip them, because the history it sees carries no reasoning, so it must not
// produce the rewrite: no steps, and a result that tells the host what to do
// instead.
func TestPrepareForAnAnthropicHostPlansNothingAndSaysWhy(t *testing.T) {
	for _, provider := range []string{"anthropic", " Anthropic "} {
		d := newDriver(t)
		got := prepareRaw(t, d, "s-"+strings.TrimSpace(provider), provider)
		if len(got.Steps) != 0 {
			t.Fatalf("provider %q: %d steps were planned; each one edits history behind signed thinking", provider, len(got.Steps))
		}
		if !got.ServerSideEditing {
			t.Errorf("provider %q: the result does not say that no plan was made on purpose", provider)
		}
		for _, want := range []string{"clear_tool_uses_20250919", "context-management-2025-06-27", "invalidate"} {
			if !strings.Contains(got.ServerSideNote, want) {
				t.Errorf("provider %q: the note does not mention %q: %s", provider, want, got.ServerSideNote)
			}
		}
	}
}

// Declining must leave the ledger alone: a later prepare for the same session
// that does not name the provider plans exactly what a first one would, not
// less because something was recorded.
func TestDecliningToPlanLeavesNoLedgerBehind(t *testing.T) {
	d := newDriver(t)
	if got := prepareRaw(t, d, "same", "anthropic"); len(got.Steps) != 0 {
		t.Fatalf("anthropic planned %d steps", len(got.Steps))
	}
	first := prepareRaw(t, d, "same", "")
	if len(first.Steps) == 0 {
		t.Fatal("the control history planned nothing; the fixture no longer exceeds its window")
	}
	fresh := prepareRaw(t, newDriver(t), "other", "")
	if len(first.Steps) != len(fresh.Steps) {
		t.Errorf("after an anthropic prepare the session planned %d steps, a fresh one %d", len(first.Steps), len(fresh.Steps))
	}
}

// Every other provider, and a host that names none, keeps the plane as it was.
func TestPrepareForOtherProvidersStillPlans(t *testing.T) {
	for _, provider := range []string{"", "openai", "gemini", "xai", "local"} {
		d := newDriver(t)
		got := prepareRaw(t, d, "s", provider)
		if len(got.Steps) == 0 || got.ServerSideEditing {
			t.Errorf("provider %q: steps=%d server_side_editing=%v, want a plan", provider, len(got.Steps), got.ServerSideEditing)
		}
	}
}
