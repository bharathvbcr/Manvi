package devcouncil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// hostPut rewrites an item the way a person's host would: the item as it is,
// with the given fields overridden.
func (e *workEnv) hostPut(id string, override map[string]any) json.RawMessage {
	e.t.Helper()
	it := e.item(id)
	body := it.resend("host-"+id+"-"+strconv.FormatInt(it.Revision, 10), it.Status)
	for key, value := range override {
		body[key] = value
	}
	raw, err := json.Marshal(body)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.workbench("items.put", string(raw))
}

// queuedIDs is the set of tasks waiting in the automatic-enhancement queue.
func (e *workEnv) queuedIDs() map[string]bool {
	e.t.Helper()
	var out struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(e.workbench("automation.list", `{}`), &out); err != nil {
		e.t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, item := range out.Items {
		ids[item.ID] = true
	}
	return ids
}

func (e *workEnv) agentProposed() []boardItem {
	e.t.Helper()
	var out struct {
		Items []boardItem `json:"items"`
	}
	if err := json.Unmarshal(e.workbench("items.list", `{"repository_id":"this-repo","deleted":false,"limit":200}`), &out); err != nil {
		e.t.Fatal(err)
	}
	var proposed []boardItem
	for _, it := range out.Items {
		for _, label := range it.Labels {
			if label == agentProposedLabel {
				proposed = append(proposed, it)
			}
		}
	}
	return proposed
}

// Criterion: Create allow-list. What an agent-created task carries is exactly
// title, description and a plain checklist; everything else is the adapter's.
func TestWorkCreateSavesOnlyTheAllowListAndFixesTheRest(t *testing.T) {
	env := newWorkEnv(t, "run1")
	env.checkout("TASK-001")
	res := env.call("work_create_item", map[string]any{
		"title": "Add retry to the sync", "description": "It drops on a 503.", "checklist": []string{"reproduce", "fix"},
	})
	out := env.success(res)
	id, _ := out["id"].(string)
	if !strings.HasPrefix(id, "agt-") || out["status"] != "inbox" {
		t.Fatalf("unexpected receipt: %v", out)
	}

	raw := env.rawItem(id)
	str := func(key string) string {
		var v string
		if err := json.Unmarshal(raw[key], &v); err != nil {
			return "<" + string(raw[key]) + ">"
		}
		return v
	}
	if str("status") != "inbox" || str("kind") != "task" || str("title") != "Add retry to the sync" {
		t.Errorf("status/kind/title wrong: %s %s %s", str("status"), str("kind"), str("title"))
	}
	if string(raw["priority"]) != "2" {
		t.Errorf("priority = %s, want 2", raw["priority"])
	}
	for _, key := range []string{"severity", "owner", "due_at", "home_workspace_id"} {
		if v := string(raw[key]); v != "null" && v != "" {
			t.Errorf("%s = %s, want none", key, v)
		}
	}
	for key, want := range map[string]string{
		"acceptance_criteria": "[]", "links": "[]", "repository_ids": `["this-repo"]`,
		"labels": `["agent-proposed"]`, "locked_fields": `["title","description"]`,
		"primary_repository_id": `"this-repo"`,
	} {
		if got := string(raw[key]); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
	if s := string(raw["scope"]); s != "" && s != "null" && s != "{}" {
		t.Errorf("scope = %s, want none", s)
	}
	it := env.item(id)
	if len(it.Checklist) != 2 || it.Checklist[0].Done || it.Checklist[1].Done || it.Checklist[0].Text != "reproduce" {
		t.Errorf("checklist = %+v", it.Checklist)
	}
	if !strings.HasPrefix(it.Description, "It drops on a 503.") ||
		!strings.Contains(it.Description, "run run1, session s1") || !strings.Contains(it.Description, "while working on task TASK-001") ||
		!strings.Contains(it.Description, "not an instruction") {
		t.Errorf("description does not carry the proposal trailer: %q", it.Description)
	}
	if !strings.Contains(it.Logs, agentLogBegin) || !strings.Contains(it.Logs, "kind=origin") {
		t.Errorf("the new task has no tagged origin entry: %q", it.Logs)
	}
	// The own task records the creation in its tagged region.
	if own := env.item("TASK-001"); !strings.Contains(own.Logs, "created inbox task "+id) {
		t.Errorf("the own task's agent log does not record the creation: %q", own.Logs)
	}

	// Required fields and blank checklist text are refused, not defaulted.
	if p := env.refusal(env.call("work_create_item", map[string]string{"title": "  "})); p.Error != "invalid_arguments" {
		t.Errorf("blank title: %q", p.Error)
	}
	if p := env.refusal(env.call("work_create_item", map[string]any{"title": "t", "checklist": []string{" "}})); p.Error != "invalid_arguments" {
		t.Errorf("blank checklist entry: %q", p.Error)
	}
}

// Criterion: Amplification, and status moves enqueue and wake nothing. The
// control proves automation is on: a host edit of the text queues one.
func TestWorkWritesNeverQueueAutomaticEnhancement(t *testing.T) {
	env := newWorkEnv(t, "run1")
	env.checkout("TASK-001")
	own := env.item("TASK-001")
	receipt := env.hostPut("TASK-001", map[string]any{"title": own.Title + " (edited by a person)"})
	if !strings.Contains(string(receipt), `"automatic_enhancement_queued":true`) || !env.queuedIDs()["TASK-001"] {
		t.Fatalf("control failed: a person's text edit did not queue an automatic enhancement (%s), so automation is not what this test exercises", receipt)
	}
	before := env.queuedIDs()

	own = env.item("TASK-001")
	out := env.success(env.call("work_update_own_item", map[string]any{"expected_revision": own.Revision, "status": "in_progress", "log": "starting"}))
	if out["enhancement_queued"] != false {
		t.Errorf("a status move reported an enqueued enhancement: %v", out)
	}
	created := env.success(env.call("work_create_item", map[string]string{"title": "proposal", "description": "d"}))
	if created["enhancement_queued"] != false {
		t.Errorf("a created task reported an enqueued enhancement: %v", created)
	}
	id := fmt.Sprint(created["id"])
	after := env.queuedIDs()
	if after[id] || len(after) != len(before) {
		t.Errorf("the agent's writes changed the automatic queue: before %v, after %v", before, after)
	}
	// Both locks are what keep the created task out of the queue.
	if locked := env.rawItem(id)["locked_fields"]; string(locked) != `["title","description"]` {
		t.Errorf("created task locked_fields = %s", locked)
	}
}

// Criterion: Authority. The agent edits only its own task, within the
// allow-list, and every other field, task and verb is refused with a typed error
// while the task stays as it was.
func TestWorkUpdateAuthorityIsOwnTaskAndAllowListOnly(t *testing.T) {
	env := newWorkEnv(t, "run1")
	env.seed("other-task", "")
	env.seed("p", `,"owner":"alice","severity":"high","priority":1,"acceptance_criteria":["must pass"],"locked_fields":["description"],"scope":{"planned_files":[{"path":"a.go","allowed_change":"modify"}]},"checklist":[{"text":"one","done":false},{"text":"two","done":false}]`)
	env.checkout("p")
	before := env.item("p")
	env.board.reset()

	for _, field := range []string{"scope", "locked_fields", "acceptance_criteria", "owner", "priority", "severity", "due_at", "kind", "archived", "links", "labels", "repository_ids", "primary_repository_id", "title", "description", "done", "delete", "merge"} {
		args := map[string]any{"expected_revision": before.Revision, field: "x"}
		p := env.refusal(env.call("work_update_own_item", args))
		if p.Error != "forbidden_field" || p.Field != field {
			t.Errorf("update with %s: %+v, want forbidden_field naming it", field, p)
		}
	}
	// Another task, by id: refused, and the other task is untouched.
	otherBefore := env.item("other-task")
	p := env.refusal(env.call("work_update_own_item", map[string]any{"id": "other-task", "expected_revision": otherBefore.Revision, "status": "review"}))
	if p.Error != "not_own_task" {
		t.Errorf("a write to another task answered %q, want not_own_task", p.Error)
	}
	if after := env.item("other-task"); after.Revision != otherBefore.Revision || after.Status != otherBefore.Status {
		t.Error("another task was written")
	}
	// The move stops at review.
	for _, status := range []string{"done", "ready", "backlog", "inbox", "bogus"} {
		if p := env.refusal(env.call("work_update_own_item", map[string]any{"expected_revision": before.Revision, "status": status})); p.Error != "status_refused" {
			t.Errorf("status %q answered %q, want status_refused", status, p.Error)
		}
	}
	if n := env.putCalls(); n != 0 {
		t.Fatalf("%d items.put call(s) reached the store for refused writes", n)
	}
	if after := env.item("p"); after.Revision != before.Revision {
		t.Fatalf("a refused write changed the task: revision %d -> %d", before.Revision, after.Revision)
	}

	// The allowed moves.
	env.success(env.call("work_update_own_item", map[string]any{"expected_revision": before.Revision, "status": "in_progress", "tick": []int{1}, "log": "working on it"}))
	mid := env.item("p")
	if mid.Status != "in_progress" || mid.Checklist[0].Done || !mid.Checklist[1].Done || mid.Checklist[1].Text != "two" {
		t.Fatalf("allowed update wrong: %+v", mid)
	}
	env.success(env.call("work_update_own_item", map[string]any{"expected_revision": mid.Revision, "status": "review"}))
	if env.item("p").Status != "review" {
		t.Error("the move to review did not land")
	}
	// Out-of-range ticks are refused rather than clamped.
	cur := env.item("p")
	if p := env.refusal(env.call("work_update_own_item", map[string]any{"expected_revision": cur.Revision, "tick": []int{2}})); p.Error != "invalid_arguments" {
		t.Errorf("tick out of range answered %q", p.Error)
	}
	// A task a person accepted is not reopened by an agent.
	env.hostPut("p", map[string]any{"status": "done"})
	cur = env.item("p")
	if p := env.refusal(env.call("work_update_own_item", map[string]any{"expected_revision": cur.Revision, "status": "in_progress"})); p.Error != "status_refused" {
		t.Errorf("reopening a done task answered %q", p.Error)
	}
}

// Criterion: Preserved-field set. The store keeps exactly locked_fields, logs,
// archived, checklist, links and scope when they are omitted and drops every
// other field to a default; the adapter resends the rest, so a status move leaves
// each of them unchanged.
func TestWorkItemsPutPreservesExactlyTheDocumentedFieldsAndTheAdapterResendsTheRest(t *testing.T) {
	env := newWorkEnv(t, "run1")
	env.addRepository("other")
	env.seed("anchor", "")
	full := `,"description":"d","kind":"bug","priority":3,"severity":"high","owner":"alice","due_at":1900000000,` +
		`"labels":["x","y"],"acceptance_criteria":["ac one"],"locked_fields":["description"],"logs":"human log",` +
		`"checklist":[{"text":"c","done":false}],"links":[{"kind":"related","item_id":"anchor"}],` +
		`"scope":{"planned_files":[{"path":"a.go","allowed_change":"modify"}]},"position":7`
	env.seed("pin", full)
	env.seed("probe", full)

	// Part one pins the store: a put that omits everything optional.
	probe := env.item("probe")
	env.workbench("items.put", `{"request_id":"bare","id":"probe","expected_revision":`+strconv.FormatInt(probe.Revision, 10)+
		`,"title":"probe","repository_ids":["this-repo"],"primary_repository_id":"this-repo"}`)
	bare := env.rawItem("probe")
	for _, key := range []string{"locked_fields", "logs", "archived", "checklist", "links", "scope"} {
		if want := env.rawItem("pin")[key]; string(bare[key]) != string(want) {
			t.Errorf("omitting %s did not preserve it: %s, want %s", key, bare[key], want)
		}
	}
	dropped := map[string]string{"labels": "[]", "acceptance_criteria": "[]", "owner": "null", "severity": "null", "due_at": "null", "kind": `"feature"`, "priority": "1", "position": "0"}
	for key, want := range dropped {
		got := string(bare[key])
		if key == "owner" || key == "severity" || key == "due_at" {
			if got != "null" && got != "" {
				t.Errorf("omitting %s kept %s", key, got)
			}
			continue
		}
		if got != want {
			t.Errorf("omitting %s left %s, want the default %s", key, got, want)
		}
	}

	// Part two: the adapter's status move on the full task changes nothing but
	// status, revision, the update time and the appended log.
	env.checkout("pin")
	pinBefore := env.rawItem("pin")
	pin := env.item("pin")
	env.success(env.call("work_update_own_item", map[string]any{"expected_revision": pin.Revision, "status": "in_progress"}))
	pinAfter := env.rawItem("pin")
	for key, was := range pinBefore {
		switch key {
		case "revision", "status", "updated_at", "logs":
			continue
		}
		if string(pinAfter[key]) != string(was) {
			t.Errorf("a status move changed %s: %s -> %s", key, was, pinAfter[key])
		}
	}
	if string(pinAfter["status"]) != `"in_progress"` {
		t.Errorf("status = %s", pinAfter["status"])
	}
	if pin := env.item("pin"); !strings.HasPrefix(pin.Logs, "human log") {
		t.Errorf("the human's log prefix was rewritten: %q", pin.Logs)
	}
}

// Criterion: Concurrency. A stale expected_revision is refused before any write,
// and a race the store catches is reported as stale, not as a failure to retry
// blindly.
func TestWorkStaleExpectedRevisionIsRefused(t *testing.T) {
	env := newWorkEnv(t, "run1")
	env.checkout("TASK-001")
	own := env.item("TASK-001")
	env.success(env.call("work_update_own_item", map[string]any{"expected_revision": own.Revision, "log": "first"}))
	env.board.reset()
	p := env.refusal(env.call("work_update_own_item", map[string]any{"expected_revision": own.Revision, "log": "second, from a stale read"}))
	if p.Error != "stale_revision" {
		t.Errorf("a stale revision answered %q, want stale_revision", p.Error)
	}
	if n := env.putCalls(); n != 0 {
		t.Errorf("a stale write reached the store %d time(s)", n)
	}

	// The store's own refusal, on a write that passed the adapter's read.
	cur := env.item("TASK-001")
	env.board.failMethod = func(method string, _ int) error {
		if method == "items.put" {
			return &store.WorkbenchError{Code: "revision_conflict", Message: "stale"}
		}
		return nil
	}
	p = env.refusal(env.call("work_update_own_item", map[string]any{"expected_revision": cur.Revision, "log": "raced"}))
	if p.Error != "stale_revision" {
		t.Errorf("the store's revision_conflict answered %q, want stale_revision", p.Error)
	}
	if env.item("TASK-001").Revision != cur.Revision {
		t.Error("a conflicting write changed the task")
	}
}

// Criterion: Liveness, in-process. A write needs a lease the store still calls
// valid and that the session owns.
func TestWorkWritesNeedALiveOwnedLease(t *testing.T) {
	env := newWorkEnv(t, "run1")
	own := env.item("TASK-001")
	env.board.reset()

	if p := env.refusal(env.call("work_update_own_item", map[string]any{"expected_revision": own.Revision, "log": "x"})); p.Error != "no_lease" {
		t.Errorf("update without a lease: %q, want no_lease", p.Error)
	}
	if p := env.refusal(env.call("work_create_item", map[string]string{"title": "x"})); p.Error != "no_lease" {
		t.Errorf("create without a lease: %q, want no_lease", p.Error)
	}

	env.checkout("TASK-001")
	// An inherited child session reads but does not write.
	child := env.reg.RootSession().NewChildSession(nil)
	ctx := WithSession(context.Background(), child)
	if p := env.refusal(env.callCtx(ctx, "work_update_own_item", map[string]any{"expected_revision": own.Revision, "log": "x"})); p.Error != "inherited_session" {
		t.Errorf("inherited update: %q, want inherited_session", p.Error)
	}
	if p := env.refusal(env.callCtx(ctx, "work_create_item", map[string]string{"title": "x"})); p.Error != "inherited_session" {
		t.Errorf("inherited create: %q, want inherited_session", p.Error)
	}
	if res := env.callCtx(ctx, "work_get_item", map[string]string{"id": "TASK-001"}); res.IsError {
		t.Errorf("an inherited child cannot read the board: %s", res.Text)
	}

	// The lease expires mid-session (here: the store no longer holds it).
	state := env.reg.Session()
	if released, err := env.store.Release(context.Background(), state.TaskID, state.Token); err != nil || !released {
		t.Fatalf("release: %v %v", released, err)
	}
	if p := env.refusal(env.call("work_update_own_item", map[string]any{"expected_revision": own.Revision, "log": "x"})); p.Error != "lease_invalid" {
		t.Errorf("update after the lease ended: %q, want lease_invalid", p.Error)
	}
	if p := env.refusal(env.call("work_create_item", map[string]string{"title": "x"})); p.Error != "lease_invalid" {
		t.Errorf("create after the lease ended: %q, want lease_invalid", p.Error)
	}
	if n := env.putCalls(); n != 0 {
		t.Errorf("%d write(s) reached the store without a live lease", n)
	}
}

// Criterion: Liveness recheck. The lease is read again immediately before the
// put. The run ends between the check and the put, a store error on the second
// read, and a cancelled call each refuse and write nothing.
func TestWorkLivenessIsRecheckedImmediatelyBeforeThePut(t *testing.T) {
	t.Run("lease ends between check and put", func(t *testing.T) {
		env := newWorkEnv(t, "run1")
		env.checkout("TASK-001")
		own := env.item("TASK-001")
		env.api.beforePut = func(ctx context.Context) {
			state := env.reg.Session()
			if _, err := env.store.Release(ctx, state.TaskID, state.Token); err != nil {
				t.Errorf("release in the hook: %v", err)
			}
		}
		env.board.reset()
		p := env.refusal(env.call("work_update_own_item", map[string]any{"expected_revision": own.Revision, "log": "x"}))
		if p.Error != "lease_invalid" {
			t.Errorf("answered %q, want lease_invalid", p.Error)
		}
		if n := env.putCalls(); n != 0 {
			t.Errorf("the write reached the store %d time(s) after the lease ended", n)
		}
		if env.item("TASK-001").Revision != own.Revision {
			t.Error("the task changed")
		}
	})
	t.Run("store error on the recheck", func(t *testing.T) {
		env := newWorkEnv(t, "run1")
		env.checkout("TASK-001")
		own := env.item("TASK-001")
		env.board.validFail = func(n int) error {
			if n == 2 {
				return errors.New("store unreachable")
			}
			return nil
		}
		env.board.reset()
		p := env.refusal(env.call("work_update_own_item", map[string]any{"expected_revision": own.Revision, "log": "x"}))
		if p.Error != "liveness_unknown" || env.putCalls() != 0 {
			t.Errorf("a failed recheck answered %q with %d put(s), want liveness_unknown and none", p.Error, env.putCalls())
		}
	})
	t.Run("store error on the first check", func(t *testing.T) {
		env := newWorkEnv(t, "run1")
		env.checkout("TASK-001")
		own := env.item("TASK-001")
		env.board.validFail = func(int) error { return errors.New("store unreachable") }
		p := env.refusal(env.call("work_update_own_item", map[string]any{"expected_revision": own.Revision, "log": "x"}))
		if p.Error != "liveness_unknown" {
			t.Errorf("answered %q, want liveness_unknown", p.Error)
		}
	})
	t.Run("call cancelled before the put", func(t *testing.T) {
		env := newWorkEnv(t, "run1")
		env.checkout("TASK-001")
		own := env.item("TASK-001")
		ctx, cancel := context.WithCancel(context.Background())
		env.api.beforePut = func(context.Context) { cancel() }
		env.board.reset()
		p := env.refusal(env.callCtx(ctx, "work_update_own_item", map[string]any{"expected_revision": own.Revision, "log": "x"}))
		if p.Error != "cancelled" || env.putCalls() != 0 {
			t.Errorf("answered %q with %d put(s), want cancelled and none", p.Error, env.putCalls())
		}
	})
}

var requestIDShape = regexp.MustCompile(`^agent_run1_s1_[0-9a-f]{16}$`)

// Criterion: request_id durability. The id is host-minted, in the identifier
// alphabet within 128 characters, the same for the same write after a restart,
// and a replayed receipt or an idempotency conflict fails closed.
func TestWorkRequestIDIsHostMintedDurableAndFailsClosedOnReplay(t *testing.T) {
	env := newWorkEnv(t, "run1")
	// An owner string the agent chose must not reach the identity.
	if res := env.call("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001", "owner": "evil_owner"}); res.IsError {
		t.Fatal(res.Text)
	}
	own := env.item("TASK-001")
	env.board.reset()
	out := env.success(env.call("work_update_own_item", map[string]any{"expected_revision": own.Revision, "log": "one"}))
	req, _ := out["request_id"].(string)
	if !requestIDShape.MatchString(req) || len(req) > 128 {
		t.Fatalf("request id %q is not agent_{run}_{session}_{n} within the rules", req)
	}
	puts := env.board.called("items.put")
	if len(puts) != 1 || !strings.Contains(string(puts[0].input), `"request_id":"`+req+`"`) {
		t.Fatalf("the put did not carry the minted id: %v", puts)
	}

	// Durable: a restarted host mints the same id for the same write against the
	// same revision, and a different one for a different write or revision.
	content := map[string]any{"status": "", "tick": []int(nil), "log": "one", "related": ""}
	sessA := env.reg.RootSession()
	a1, refused := env.api.requestID(sessA, "update", "TASK-001", own.Revision, content)
	if refused != nil {
		t.Fatal(refused.Text)
	}
	if a1 != req {
		t.Fatalf("re-deriving the id gave %q, want the minted %q", a1, req)
	}
	restarted := env.restart("run1")
	a2, refused := restarted.api.requestID(restarted.reg.RootSession(), "update", "TASK-001", own.Revision, content)
	if refused != nil {
		t.Fatal(refused.Text)
	}
	if a2 != a1 {
		t.Errorf("a restarted host minted %q, want %q: the id is not durable", a2, a1)
	}
	if other, _ := restarted.api.requestID(restarted.reg.RootSession(), "update", "TASK-001", own.Revision+1, content); other == a1 {
		t.Error("a different revision minted the same id")
	}
	if other, _ := restarted.api.requestID(restarted.reg.RootSession(), "update", "TASK-001", own.Revision, map[string]any{"log": "two"}); other == a1 {
		t.Error("different content minted the same id")
	}

	// Write again after the restart, from the new process.
	env.success(env.call("devcouncil_release_task", map[string]any{}))
	restarted.checkout("TASK-001")
	cur := restarted.item("TASK-001")
	again := restarted.success(restarted.call("work_update_own_item", map[string]any{"expected_revision": cur.Revision, "log": "after the restart"}))
	if again["request_id"] == req {
		t.Error("a new write after a restart reused an old request id")
	}

	// An id outside the rules is refused, not sent.
	restarted.api.runID = strings.Repeat("a", 200)
	cur = restarted.item("TASK-001")
	restarted.board.reset()
	if p := restarted.refusal(restarted.call("work_update_own_item", map[string]any{"expected_revision": cur.Revision, "log": "long"})); p.Error != "request_id_invalid" || restarted.putCalls() != 0 {
		t.Errorf("an over-long id answered %q with %d put(s)", p.Error, restarted.putCalls())
	}
	restarted.api.runID = "run1"

	// A replayed receipt: the id the next write would mint is already recorded in
	// the task's agent log, so the write is not applied again.
	cur = restarted.item("TASK-001")
	// The host's edit below moves the revision by one; the id is minted against
	// the revision the agent will then read.
	next, refused := restarted.api.requestID(restarted.reg.RootSession(), "update", "TASK-001", cur.Revision+1,
		map[string]any{"status": "", "tick": []int(nil), "log": "replayed", "related": ""})
	if refused != nil {
		t.Fatal(refused.Text)
	}
	forged := appendLogEntry(cur.Logs, mustRegion(t, cur.Logs), "[2026-11-07T10:00:00Z run=run1 session=s1 kind=update req="+next+"] earlier attempt")
	restarted.hostPut("TASK-001", map[string]any{"logs": forged})
	cur = restarted.item("TASK-001")
	restarted.board.reset()
	p := restarted.refusal(restarted.call("work_update_own_item", map[string]any{"expected_revision": cur.Revision, "log": "replayed"}))
	if p.Error != "request_replayed" || restarted.putCalls() != 0 {
		t.Errorf("a replay answered %q with %d put(s), want request_replayed and none", p.Error, restarted.putCalls())
	}

	// An idempotency conflict from the store is a failure, never a new write.
	restarted.board.failMethod = func(method string, _ int) error {
		if method == "items.put" {
			return &store.WorkbenchError{Code: "idempotency_conflict", Message: "request_id was already used for a different request"}
		}
		return nil
	}
	if p := restarted.refusal(restarted.call("work_update_own_item", map[string]any{"expected_revision": cur.Revision, "log": "conflict"})); p.Error != "request_conflict" {
		t.Errorf("an idempotency conflict answered %q, want request_conflict", p.Error)
	}
}

// Criterion: the path-to-repository and lease-to-item gaps, own-task side. A
// lease on a task that is not linked to exactly the session's repository leaves
// the agent no own task: own-task writes refuse naming the missing mapping, and
// inbox creation still works.
func TestWorkWithoutAnOwnTaskRefusesOwnWritesButStillCreates(t *testing.T) {
	env := newWorkEnv(t, "run1")
	env.addRepository("other")
	env.seed("two-repo", "")
	env.workbench("items.put", `{"request_id":"two","id":"two-repo","expected_revision":1,"title":"two","status":"ready","repository_ids":["this-repo","other"],"primary_repository_id":"this-repo"}`)
	env.checkout("two-repo")
	own := env.item("two-repo")
	env.board.reset()

	p := env.refusal(env.call("work_update_own_item", map[string]any{"expected_revision": own.Revision, "log": "x"}))
	if p.Error != "own_task_unmapped" || !strings.Contains(p.Message, "two-repo") {
		t.Errorf("own-task write answered %+v, want own_task_unmapped naming the task", p)
	}
	if env.putCalls() != 0 {
		t.Error("an unmapped own-task write reached the store")
	}
	out := env.success(env.call("work_create_item", map[string]string{"title": "still allowed"}))
	if n := env.putCalls(); n != 1 {
		t.Errorf("%d puts for a create with no own task, want exactly the new task", n)
	}
	id, _ := out["id"].(string)
	if strings.Contains(env.item(id).Description, "while working on task") {
		t.Error("the trailer names an own task that does not exist")
	}
	if env.item("two-repo").Revision != own.Revision {
		t.Error("creating a task changed a task that is not the agent's own")
	}
}

// Criterion: Audit. Every agent write carries the host-minted request id, a run
// and session prefix in the tagged log region, and the agent-proposed label.
func TestWorkEveryWriteIsAttributedInsideTheTaggedRegion(t *testing.T) {
	env := newWorkEnv(t, "run1")
	env.checkout("TASK-001")
	own := env.item("TASK-001")
	upd := env.success(env.call("work_update_own_item", map[string]any{"expected_revision": own.Revision, "status": "in_progress", "log": "line one\nline two"}))
	logs := env.item("TASK-001").Logs
	prefix := regexp.MustCompile(`\[\d{4}-\d{2}-\d{2}T[0-9:]+Z run=run1 session=s1 kind=update req=` + regexp.QuoteMeta(fmt.Sprint(upd["request_id"])) + `\] status ready -> in_progress\n  line one\n  line two\n`)
	if !prefix.MatchString(logs) {
		t.Errorf("the update's audit entry is missing or malformed in %q", logs)
	}
	if !strings.Contains(logs, agentLogBegin) || !strings.Contains(logs, agentLogEnd) {
		t.Errorf("the entry is not inside the tagged block: %q", logs)
	}
	created := env.success(env.call("work_create_item", map[string]string{"title": "p"}))
	id, _ := created["id"].(string)
	item := env.item(id)
	if len(item.Labels) != 1 || item.Labels[0] != agentProposedLabel {
		t.Errorf("labels = %v", item.Labels)
	}
	if !strings.Contains(item.Logs, "run=run1 session=s1 kind=origin req="+fmt.Sprint(created["request_id"])) {
		t.Errorf("the new task's log does not attribute its origin: %q", item.Logs)
	}
}

// Criterion: Log space. Appends go inside the tagged region, the human's text on
// either side is byte-identical, the total is capped, a marker or a forged entry
// in agent text cannot escape, and a damaged region is refused.
func TestWorkLogAppendsStayInsideTheRegionAndAreBounded(t *testing.T) {
	env := newWorkEnv(t, "run1")
	human := "human note: error E42\n```` kept verbatim\n"
	env.seed("lg", `,"logs":`+strconv.Quote(human))
	env.checkout("lg")
	cur := env.item("lg")
	env.success(env.call("work_update_own_item", map[string]any{"expected_revision": cur.Revision, "log": "first"}))
	after := env.item("lg").Logs
	if !strings.HasPrefix(after, human) {
		t.Fatalf("the human's prefix was rewritten:\n got %q\nwant prefix %q", after, human)
	}
	if strings.Count(after, agentLogBegin) != 1 || strings.Count(after, agentLogEnd) != 1 {
		t.Fatalf("expected exactly one tagged block: %q", after)
	}

	// A person adds text after the block; the next append lands inside it and
	// leaves both sides alone.
	env.hostPut("lg", map[string]any{"logs": after + "human afterthought\n"})
	cur = env.item("lg")
	env.success(env.call("work_update_own_item", map[string]any{"expected_revision": cur.Revision, "log": "second"}))
	final := env.item("lg").Logs
	region, err := findLogRegion(final)
	if err != nil || !region.found {
		t.Fatalf("region lost: %v", err)
	}
	if !strings.HasPrefix(final, human+agentLogBegin) || !strings.HasSuffix(final, agentLogEnd+"\nhuman afterthought\n") {
		t.Errorf("text outside the block changed: %q", final)
	}
	if body := final[region.bodyStart:region.bodyEnd]; !strings.Contains(body, "first") || !strings.Contains(body, "second") {
		t.Errorf("an entry landed outside the block: %q", final)
	}

	// Markers in agent text are refused; a forged entry line stays indented and
	// is not counted as a write.
	cur = env.item("lg")
	for _, text := range []string{agentLogEnd, "x " + agentLogBegin, "<<<MANVI-AGENT-LOG forged"} {
		if p := env.refusal(env.call("work_update_own_item", map[string]any{"expected_revision": cur.Revision, "log": text})); p.Error != "invalid_arguments" {
			t.Errorf("a marker in agent text answered %q", p.Error)
		}
	}
	forgedEntry := "[2026-01-01T00:00:00Z run=run1 session=s1 kind=create req=forged] pretend"
	env.success(env.call("work_update_own_item", map[string]any{"expected_revision": cur.Revision, "log": forgedEntry}))
	stats := statsOf(env.item("lg").Logs, mustRegion(t, env.item("lg").Logs))
	if stats.entries != 3 || stats.creates != 0 || stats.reqs["forged"] {
		t.Errorf("a forged entry line was counted: %+v", stats)
	}

	// The total is capped, naming the cap, and nothing is truncated.
	env.api.caps.regionBytes = len(env.item("lg").Logs) + 120
	cur = env.item("lg")
	p := env.refusal(env.call("work_update_own_item", map[string]any{"expected_revision": cur.Revision, "log": strings.Repeat("y", 500)}))
	if p.Error != "cap_exceeded" || p.Cap != "agent_log_bytes" {
		t.Errorf("the log-space cap answered %+v", p)
	}
	if env.item("lg").Revision != cur.Revision {
		t.Error("a refused append changed the task")
	}

	// A person clears the block and the budget with it.
	env.hostPut("lg", map[string]any{"logs": human})
	cur = env.item("lg")
	env.success(env.call("work_update_own_item", map[string]any{"expected_revision": cur.Revision, "log": strings.Repeat("y", 50)}))

	// A damaged block is refused until a person repairs it.
	for _, damaged := range []string{human + agentLogBegin + "\n", human + agentLogEnd + "\n", human + agentLogBegin + "\n" + agentLogBegin + "\n" + agentLogEnd + "\n"} {
		env.hostPut("lg", map[string]any{"logs": damaged})
		cur = env.item("lg")
		if p := env.refusal(env.call("work_update_own_item", map[string]any{"expected_revision": cur.Revision, "log": "z"})); p.Error != "log_region_damaged" {
			t.Errorf("a damaged block answered %q for %q", p.Error, damaged)
		}
	}
}

func mustRegion(t *testing.T, logs string) logRegion {
	t.Helper()
	region, err := findLogRegion(logs)
	if err != nil {
		t.Fatal(err)
	}
	return region
}

// Criterion: Caps. The numbers are the documented ones; each cap names itself,
// nothing is truncated, and the per-(repository, task) budget is shared by a
// continuation.
func TestWorkCapsAreTheDocumentedNumbersAndNeverTruncate(t *testing.T) {
	c := defaultWorkCaps
	if c.createsPerTask != 5 || c.writesPerTask != 30 || c.descriptionBytes != 16<<10 || c.titleChars != 200 ||
		c.logAppendBytes != 16<<10 || c.regionBytes != 64<<10 || c.createsPerHour != 10 || c.openProposed != 50 {
		t.Fatalf("the default caps drifted from the documented numbers: %+v", c)
	}
	env := newWorkEnv(t, "run1")
	env.checkout("TASK-001")
	own := env.item("TASK-001")
	env.board.reset()

	for name, tc := range map[string]struct {
		tool    string
		args    map[string]any
		cap     string
		limit   int
		allowed map[string]any
	}{
		"title":       {"work_create_item", map[string]any{"title": strings.Repeat("t", 201)}, "title_chars", 200, map[string]any{"title": strings.Repeat("t", 200)}},
		"description": {"work_create_item", map[string]any{"title": "t", "description": strings.Repeat("d", 16<<10+1)}, "description_bytes", 16 << 10, nil},
		"log":         {"work_update_own_item", map[string]any{"expected_revision": own.Revision, "log": strings.Repeat("l", 16<<10+1)}, "log_append_bytes", 16 << 10, nil},
		"checklist":   {"work_create_item", map[string]any{"title": "t", "checklist": make([]string, 33)}, "checklist_entries", 32, nil},
	} {
		p := env.refusal(env.call(tc.tool, tc.args))
		if p.Error != "cap_exceeded" || p.Cap != tc.cap || p.Limit != tc.limit {
			t.Errorf("%s: %+v, want cap_exceeded naming %s with limit %d", name, p, tc.cap, tc.limit)
		}
	}
	if n := env.putCalls(); n != 0 {
		t.Errorf("%d write(s) were attempted past a cap (a cap must not truncate and write)", n)
	}
	// Exactly at the limit is allowed, whole.
	okTitle := strings.Repeat("t", 200)
	out := env.success(env.call("work_create_item", map[string]any{"title": okTitle, "description": strings.Repeat("d", 16<<10)}))
	id, _ := out["id"].(string)
	if got := env.item(id); got.Title != okTitle || !strings.HasPrefix(got.Description, strings.Repeat("d", 16<<10)) {
		t.Error("a title and description at the cap were not stored whole")
	}
}

// Criterion: Caps keyed by (repository, task), shared by continuations. A second
// run on the same task sees the first run's writes, because the count is read
// from the store and not held in a run.
func TestWorkTaskBudgetIsSharedByAContinuation(t *testing.T) {
	env := newWorkEnv(t, "run1")
	env.api.caps.writesPerTask = 3
	env.checkout("TASK-001")
	for i := 0; i < 2; i++ {
		cur := env.item("TASK-001")
		env.success(env.call("work_update_own_item", map[string]any{"expected_revision": cur.Revision, "log": "run one " + strconv.Itoa(i)}))
	}
	env.success(env.call("devcouncil_release_task", map[string]any{}))

	second := env.restart("run2")
	second.api.caps.writesPerTask = 3
	second.checkout("TASK-001")
	cur := second.item("TASK-001")
	second.success(second.call("work_update_own_item", map[string]any{"expected_revision": cur.Revision, "log": "run two"}))
	cur = second.item("TASK-001")
	p := second.refusal(second.call("work_update_own_item", map[string]any{"expected_revision": cur.Revision, "log": "one too many"}))
	if p.Error != "cap_exceeded" || p.Cap != "writes_per_task" || p.Limit != 3 {
		t.Errorf("a continuation got a fresh budget: %+v", p)
	}
	if second.item("TASK-001").Revision != cur.Revision {
		t.Error("a write past the budget landed")
	}
}

// Criterion: 5 created tasks per task, at the documented number.
func TestWorkCreatedTasksPerTaskCapIsFive(t *testing.T) {
	env := newWorkEnv(t, "run1")
	env.checkout("TASK-001")
	for i := 0; i < 5; i++ {
		env.success(env.call("work_create_item", map[string]string{"title": "p" + strconv.Itoa(i)}))
	}
	p := env.refusal(env.call("work_create_item", map[string]string{"title": "p6"}))
	if p.Error != "cap_exceeded" || p.Cap != "created_tasks_per_task" || p.Limit != 5 {
		t.Errorf("the sixth create answered %+v", p)
	}
	if n := len(env.agentProposed()); n != 5 {
		t.Errorf("%d agent-proposed tasks exist, want 5", n)
	}
}

// Criterion: Repository-wide caps, counted from the store, so a host restart does
// not reset them, serialised so concurrent creates cannot both pass.
func TestWorkRepositoryCapsHoldAcrossRestartAndConcurrentCreates(t *testing.T) {
	t.Run("open proposals", func(t *testing.T) {
		env := newWorkEnv(t, "run1")
		env.api.caps.openProposed, env.api.caps.createsPerTask, env.api.caps.writesPerTask = 3, 100, 100
		env.checkout("TASK-001")
		for i := 0; i < 3; i++ {
			env.success(env.call("work_create_item", map[string]string{"title": "p" + strconv.Itoa(i)}))
		}
		p := env.refusal(env.call("work_create_item", map[string]string{"title": "p3"}))
		if p.Error != "cap_exceeded" || p.Cap != "open_proposed_per_repository" || p.Limit != 3 || p.Got != 3 {
			t.Errorf("the open-proposal cap answered %+v, want both numbers", p)
		}
		// A restarted host has no memory of the creates and still refuses.
		restarted := env.restart("run1b")
		restarted.api.caps.openProposed, restarted.api.caps.createsPerTask, restarted.api.caps.writesPerTask = 3, 100, 100
		env.success(env.call("devcouncil_release_task", map[string]any{}))
		restarted.checkout("TASK-001")
		if p := restarted.refusal(restarted.call("work_create_item", map[string]string{"title": "p4"})); p.Cap != "open_proposed_per_repository" {
			t.Errorf("after a restart the cap answered %+v", p)
		}
		// A person moving a proposal out of the inbox frees a slot.
		first := restarted.agentProposed()[0]
		restarted.hostPut(first.ID, map[string]any{"status": "backlog"})
		restarted.success(restarted.call("work_create_item", map[string]string{"title": "p5"}))
	})
	t.Run("per hour", func(t *testing.T) {
		env := newWorkEnv(t, "run1")
		env.api.caps.createsPerHour, env.api.caps.createsPerTask, env.api.caps.writesPerTask = 3, 100, 100
		env.checkout("TASK-001")
		for i := 0; i < 3; i++ {
			env.success(env.call("work_create_item", map[string]string{"title": "p" + strconv.Itoa(i)}))
		}
		// Moving them out of the inbox does not hide them from the hourly count.
		for _, it := range env.agentProposed() {
			env.hostPut(it.ID, map[string]any{"status": "backlog"})
		}
		p := env.refusal(env.call("work_create_item", map[string]string{"title": "p3"}))
		if p.Error != "cap_exceeded" || p.Cap != "created_per_repository_per_hour" || p.Limit != 3 || p.Got != 3 {
			t.Errorf("the hourly cap answered %+v, want both numbers", p)
		}
	})
	t.Run("concurrent creates at the cap", func(t *testing.T) {
		env := newWorkEnv(t, "run1")
		env.api.caps.createsPerHour, env.api.caps.createsPerTask, env.api.caps.writesPerTask = 3, 100, 100
		env.checkout("TASK-001")
		var wg sync.WaitGroup
		results := make([]tools.Result, 8)
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i] = env.call("work_create_item", map[string]string{"title": "c" + strconv.Itoa(i)})
			}()
		}
		wg.Wait()
		created, refused := 0, 0
		for _, res := range results {
			if !res.IsError {
				created++
				continue
			}
			var p refusalPayload
			if err := json.Unmarshal([]byte(res.Text), &p); err != nil {
				t.Fatalf("not a typed refusal: %q", res.Text)
			}
			if p.Cap != "created_per_repository_per_hour" {
				t.Errorf("unexpected refusal %+v", p)
			}
			refused++
		}
		if created != 3 || refused != 5 {
			t.Errorf("%d created and %d refused at a cap of 3 across 8 concurrent creates", created, refused)
		}
		if n := len(env.agentProposed()); n != 3 {
			t.Errorf("%d agent-proposed tasks exist, want 3", n)
		}
	})
}
