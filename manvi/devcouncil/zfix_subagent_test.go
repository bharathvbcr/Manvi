package devcouncil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"manvi/agents"
	"manvi/tools"
)

// The sub-agent control plane must never report an outcome it did not achieve.
// Every test here fails against the code as it stood.

// zfixWritingRunner is a child that keeps mutating the working tree until it is
// cancelled, so a kill that cancels nothing is visible as files on disk rather
// than only as a state string.
//
// Its write count is bounded so the test still terminates against the unfixed
// code, where the cancellation never arrives.
type zfixWritingRunner struct {
	dir       string
	maxWrites int
	started   chan struct{}

	mu        sync.Mutex
	writes    int
	cancelled bool
}

func (r *zfixWritingRunner) RunSubAgent(ctx context.Context, req SubAgentRequest) (SubAgentResult, error) {
	select {
	case r.started <- struct{}{}:
	default:
	}
	for i := 0; i < r.maxWrites; i++ {
		select {
		case <-ctx.Done():
			r.mu.Lock()
			r.cancelled = true
			r.mu.Unlock()
			return SubAgentResult{}, ctx.Err()
		default:
		}
		name := filepath.Join(r.dir, fmt.Sprintf("written-after-kill-%03d.txt", i))
		if err := os.WriteFile(name, []byte("the child was still running\n"), 0o600); err != nil {
			return SubAgentResult{}, err
		}
		r.mu.Lock()
		r.writes++
		r.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	return SubAgentResult{Summary: "ran to completion despite the kill", Steps: r.maxWrites}, nil
}

func (r *zfixWritingRunner) state() (writes int, cancelled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writes, r.cancelled
}

// zfixSlowRunner keeps a child in flight long enough for the control plane to
// be asked about it while it runs.
type zfixSlowRunner struct{ delay time.Duration }

func (r *zfixSlowRunner) RunSubAgent(ctx context.Context, req SubAgentRequest) (SubAgentResult, error) {
	select {
	case <-time.After(r.delay):
		return SubAgentResult{Summary: "findings for " + req.Label, Steps: 1}, nil
	case <-ctx.Done():
		return SubAgentResult{}, ctx.Err()
	}
}

// zfixOneSubagentID waits for exactly one instance to be visible and returns its
// conversation ID.
func zfixOneSubagentID(t *testing.T, f *fixture) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		payload := f.payload("devcouncil_manage_subagents", map[string]any{"action": "list"})
		listed, _ := payload["subagents"].([]any)
		if len(listed) == 1 {
			entry, _ := listed[0].(map[string]any)
			if id, _ := entry["conversationId"].(string); id != "" {
				return id
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("no sub-agent became visible to the control plane")
	return ""
}

// Defect 1, and the half of defect 8 that makes it reportable.
//
// Instance.cancel was never assigned outside a test, so kill flipped the state
// to canceling, cancelled nothing, and answered {"killed":[id]} — after which
// the child ran to completion, wrote to the working tree, and the fan-out
// reported it completed.
func TestZfixKillTerminatesTheChildAndTheFanOutDoesNotReportCompletion(t *testing.T) {
	dir := t.TempDir()
	runner := &zfixWritingRunner{dir: dir, maxWrites: 400, started: make(chan struct{}, 1)}
	f, _ := fixtureWithRoles(t, runner)

	dispatched := make(chan tools.Result, 1)
	go func() {
		dispatched <- f.call("devcouncil_invoke_subagent", map[string]any{
			"subagents": []map[string]any{
				{"type_name": "self", "role": "long-runner", "prompt": "keep working"},
			},
		})
	}()

	select {
	case <-runner.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the child never started")
	}
	id := zfixOneSubagentID(t, f)

	killRes := f.call("devcouncil_manage_subagents", map[string]any{
		"action": "kill", "conversation_ids": []string{id},
	})
	if killRes.IsError {
		t.Fatalf("killing a live child failed: %s", killRes.Text)
	}
	if !strings.Contains(killRes.Text, id) {
		t.Fatalf("the kill did not name the child it terminated: %s", killRes.Text)
	}
	writesAtKill, _ := runner.state()

	var res tools.Result
	select {
	case res = <-dispatched:
	case <-time.After(20 * time.Second):
		t.Fatal("the fan-out never returned after the kill")
	}

	writes, cancelled := runner.state()
	if !cancelled {
		t.Fatalf("the kill reported success and the child was never cancelled (%d writes)", writes)
	}
	if writes >= runner.maxWrites {
		t.Fatalf("the child ran to completion after being killed: %d writes", writes)
	}
	if writes-writesAtKill > 5 {
		t.Errorf("the child kept writing after the kill: %d more files", writes-writesAtKill)
	}

	// And the working tree agrees with the counter.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the child's output directory: %v", err)
	}
	if len(entries) != writes {
		t.Errorf("%d files on disk against %d recorded writes", len(entries), writes)
	}

	if strings.Contains(res.Text, `"status":"completed"`) {
		t.Errorf("a terminated child was reported as completed work: %s", res.Text)
	}
	if !res.IsError {
		t.Errorf("a fan-out whose only child was terminated is not a success: %s", res.Text)
	}
}

// Defect 3. The error from Kill was dropped, so an ID nobody had registered
// simply vanished from the answer.
func TestZfixKillingAnUnregisteredIDIsAnError(t *testing.T) {
	f, _ := fixtureWithRoles(t, &recordingRunner{})

	res := f.call("devcouncil_manage_subagents", map[string]any{
		"action": "kill", "conversation_ids": []string{"subagent-does-not-exist"},
	})
	if !res.IsError {
		t.Fatalf("killing an unregistered ID reported success: %s", res.Text)
	}
	if !strings.Contains(res.Text, "subagent-does-not-exist") {
		t.Errorf("the refusal does not name the ID that was not terminated: %s", res.Text)
	}
	if strings.Contains(res.Text, `"killed":["subagent-does-not-exist"]`) {
		t.Errorf("an ID that was never registered was reported as killed: %s", res.Text)
	}
}

// Defect 3. kill_all answered "all subagents terminating" unconditionally, over
// a manager holding nothing.
func TestZfixKillAllWithNothingRegisteredIsAnError(t *testing.T) {
	f, _ := fixtureWithRoles(t, &recordingRunner{})

	res := f.call("devcouncil_manage_subagents", map[string]any{"action": "kill_all"})
	if !res.IsError {
		t.Fatalf("kill_all over an empty control plane reported success: %s", res.Text)
	}
	if strings.Contains(res.Text, "all subagents terminating") {
		t.Errorf("kill_all claimed to be terminating children that do not exist: %s", res.Text)
	}
}

// Defect 8. A finished child could be moved back out of a terminal state by a
// second kill, which then reported success.
func TestZfixKillingAFinishedChildIsAnError(t *testing.T) {
	f, _ := fixtureWithRoles(t, &recordingRunner{})

	if res := f.call("devcouncil_invoke_subagent", map[string]any{
		"subagents": []map[string]any{
			{"type_name": "self", "role": "worker", "prompt": "do the work"},
		},
	}); res.IsError {
		t.Fatalf("dispatch failed: %s", res.Text)
	}
	id := zfixOneSubagentID(t, f)

	res := f.call("devcouncil_manage_subagents", map[string]any{
		"action": "kill", "conversation_ids": []string{id},
	})
	if !res.IsError {
		t.Fatalf("killing a child that had already finished reported success: %s", res.Text)
	}

	// And the finished child is still finished.
	payload := f.payload("devcouncil_manage_subagents", map[string]any{"action": "list"})
	listed, _ := payload["subagents"].([]any)
	if len(listed) != 1 {
		t.Fatalf("expected one instance, got %v", payload["subagents"])
	}
	entry, _ := listed[0].(map[string]any)
	if state, _ := entry["state"].(string); state != agents.StateCompleted {
		t.Fatalf("the completed child was moved out of its terminal state, to %q", state)
	}
}

// Defect 2. The tool answered {"delivered": true} into a channel nothing in
// this harness reads.
func TestZfixSendMessageDoesNotClaimDelivery(t *testing.T) {
	f, _ := fixtureWithRoles(t, &recordingRunner{})

	if res := f.call("devcouncil_invoke_subagent", map[string]any{
		"subagents": []map[string]any{
			{"type_name": "self", "role": "worker", "prompt": "do the work"},
		},
	}); res.IsError {
		t.Fatalf("dispatch failed: %s", res.Text)
	}
	id := zfixOneSubagentID(t, f)

	for _, recipient := range []string{id, "subagent-does-not-exist"} {
		res := f.call("devcouncil_send_message", map[string]any{
			"recipient": recipient, "message": "focus on the parser",
		})
		if !res.IsError {
			t.Errorf("%s: a message nothing delivers was reported as sent: %s", recipient, res.Text)
		}
		if strings.Contains(res.Text, `"delivered":true`) || strings.Contains(res.Text, `"delivered": true`) {
			t.Errorf("%s: claimed delivery: %s", recipient, res.Text)
		}
	}
}

// Defect 4. The guard its twin carries — "a child that ran and produced nothing
// is a failure, not a quiet success" — was missing from this dispatcher.
func TestZfixInvokeReportsAChildThatProducedNothingAsFailed(t *testing.T) {
	f, _ := fixtureWithRoles(t, &recordingRunner{emptyOut: true})

	payload := f.payload("devcouncil_invoke_subagent", map[string]any{
		"subagents": []map[string]any{
			{"type_name": "self", "role": "worker", "prompt": "do the work"},
		},
	})
	report, _ := payload["report"].(map[string]any)
	if failed, _ := report["failed"].(float64); failed != 1 {
		t.Fatalf("a child that produced no work must be reported failed: %v", payload)
	}
	if clean, _ := payload["clean"].(bool); clean {
		t.Fatalf("a fan-out with a failed child is not clean: %v", payload)
	}
	results, _ := payload["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("expected one outcome, got %v", payload["results"])
	}
	outcome, _ := results[0].(map[string]any)
	if status, _ := outcome["status"].(string); status == "completed" {
		t.Fatalf("a child that returned nothing was reported completed: %v", outcome)
	}
}

// Defect 5. A single typo in type_name produced a child with the parent's whole
// tool surface and write access, with nothing anywhere saying the named role
// did not exist.
func TestZfixAnUnknownRoleDoesNotProduceAWritingChild(t *testing.T) {
	runner := &recordingRunner{}
	f, _ := fixtureWithRoles(t, runner)

	payload := f.payload("devcouncil_invoke_subagent", map[string]any{
		"subagents": []map[string]any{
			// "critic" with one letter missing.
			{"type_name": "critc", "role": "reviewer", "prompt": "review it"},
		},
	})

	runner.mu.Lock()
	seen := append([]SubAgentRequest(nil), runner.seen...)
	runner.mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("the runner was asked for %d children, want 1", len(seen))
	}
	if !seen[0].ReadOnly {
		t.Fatal("a misspelled role produced a child that can mutate the working tree")
	}

	unknown, _ := payload["unknown_types"].([]any)
	if len(unknown) != 1 {
		t.Fatalf("the dispatch did not report the unregistered role: %v", payload)
	}
	if name, _ := unknown[0].(string); name != "critc" {
		t.Errorf("unknown_types = %v, want the name that was not registered", unknown)
	}
	results, _ := payload["results"].([]any)
	outcome, _ := results[0].(map[string]any)
	if flagged, _ := outcome["unknown_type"].(bool); !flagged {
		t.Errorf("the child's own outcome does not say its role was unregistered: %v", outcome)
	}
}

// Defect 6. Register overwrites by name, so a model could rewrite the shipped
// read-only, MCP-denied critic inside its own turn — under a name every later
// dispatch still reads as the reviewed one.
func TestZfixAShippedRoleCannotBeRedefined(t *testing.T) {
	f, roles := fixtureWithRoles(t, &recordingRunner{})

	before, ok := roles.Get("critic")
	if !ok {
		t.Fatal("the shipped critic role is missing; this test proves nothing")
	}

	res := f.call("devcouncil_define_subagent", map[string]any{
		"name":               "critic",
		"description":        "replacement",
		"system_prompt":      "you may write",
		"enable_write_tools": true,
		"enable_mcp_tools":   true,
	})
	if !res.IsError {
		t.Fatalf("a shipped role was redefined at runtime: %s", res.Text)
	}

	after, _ := roles.Get("critic")
	if after.SystemPrompt != before.SystemPrompt {
		t.Errorf("the shipped critic's prompt was replaced: %q", after.SystemPrompt)
	}
	if after.EnableWriteTools || after.EnableMCPTools {
		t.Errorf("the shipped critic's permissions were widened: write=%v mcp=%v",
			after.EnableWriteTools, after.EnableMCPTools)
	}

	// The control: a name the harness does not ship is still definable.
	if res := f.call("devcouncil_define_subagent", map[string]any{
		"name": "auditor", "description": "invented mid-turn", "system_prompt": "you audit",
	}); res.IsError {
		t.Fatalf("defining a role under an unused name was refused: %s", res.Text)
	}
}

// Defect 7. Listing marshalled the live *agents.Instance while pool goroutines
// wrote State and StateDetail, with no lock on the reading side. Run under
// -race: this is the path a model reaches directly.
func TestZfixListingDuringAnInFlightFanOutIsRaceFree(t *testing.T) {
	f, _ := fixtureWithRoles(t, &zfixSlowRunner{delay: 60 * time.Millisecond})

	dispatched := make(chan tools.Result, 1)
	go func() {
		dispatched <- f.call("devcouncil_invoke_subagent", map[string]any{
			"subagents": []map[string]any{
				{"type_name": "self", "role": "w1", "prompt": "work"},
				{"type_name": "self", "role": "w2", "prompt": "work"},
				{"type_name": "self", "role": "w3", "prompt": "work"},
			},
		})
	}()

	deadline := time.Now().Add(30 * time.Second)
	listings := 0
	for {
		select {
		case res := <-dispatched:
			if res.IsError {
				t.Fatalf("the fan-out failed: %s", res.Text)
			}
			if listings == 0 {
				t.Fatal("no listing overlapped the fan-out; this test proves nothing")
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the fan-out never finished")
		}
		if res := f.call("devcouncil_manage_subagents", map[string]any{"action": "list"}); res.IsError {
			t.Fatalf("listing during a fan-out failed: %s", res.Text)
		}
		listings++
	}
}

// The control plane has to be one plane. Both getters answered with a fresh
// throwaway on every call, so a role defined in one call was looked up in a
// different registry on the next — and every instance a dispatch registered
// went into a manager the management tool would never see.
func TestZfixTheControlPlaneOutlivesTheCallThatWroteToIt(t *testing.T) {
	runner := &recordingRunner{}
	// Deliberately not fixtureWithRoles: this is the wiring where Deps supplies
	// neither a role catalogue nor an instance manager.
	f := newFixtureRunner(t, runner)

	if res := f.call("devcouncil_define_subagent", map[string]any{
		"name":          "surveyor",
		"description":   "surveys the package layout",
		"system_prompt": "You survey packages.",
	}); res.IsError {
		t.Fatalf("defining a role failed: %s", res.Text)
	}

	payload := f.payload("devcouncil_invoke_subagent", map[string]any{
		"subagents": []map[string]any{
			{"type_name": "surveyor", "role": "s-1", "prompt": "map it"},
		},
	})
	if _, unknown := payload["unknown_types"]; unknown {
		t.Fatalf("a role defined moments earlier was not found: %v", payload)
	}

	runner.mu.Lock()
	seen := append([]SubAgentRequest(nil), runner.seen...)
	runner.mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("the runner was asked for %d children, want 1", len(seen))
	}
	if seen[0].SystemPrompt != "You survey packages." {
		t.Errorf("the defined role's prompt did not reach the child: %q", seen[0].SystemPrompt)
	}

	// And the dispatch is visible to the management tool.
	listing := f.payload("devcouncil_manage_subagents", map[string]any{"action": "list"})
	if count, _ := listing["count"].(float64); count != 1 {
		t.Fatalf("the control plane listed %v children after dispatching one: %v", count, listing)
	}
}
