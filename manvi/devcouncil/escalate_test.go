package devcouncil

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"manvi/flags"
	"manvi/policy"
	"manvi/ui"
)

// recordingApprover answers with a fixed decision and records what it was
// asked, so a test can assert on the question as well as the answer.
type recordingApprover struct {
	decision ui.Decision
	err      error
	asked    []ui.Request
}

func (a *recordingApprover) Approve(ctx context.Context, req ui.Request) (ui.Decision, error) {
	a.asked = append(a.asked, req)
	return a.decision, a.err
}

// withApprover attaches an approval seam to a fixture's tools.
func (f *fixture) withApprover(a ui.Approver) *fixture {
	f.reg.deps.Approver = a
	return f
}

func TestNoApproverMeansABlockStaysABlock(t *testing.T) {
	// Every headless run and the whole CLI take this path. Attaching the seam
	// must not have changed what happens when nothing is attached.
	f := newFixture(t)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	blocked := f.call("devcouncil_write_file", map[string]string{
		"path": "internal/helper.go", "content": "package calc\n",
	})
	if !blocked.IsError {
		t.Fatal("an unplanned write was allowed with no approver attached")
	}
	if blocked.GrantID != "" {
		t.Fatalf("a grant was issued with nobody to issue it: %s", blocked.GrantID)
	}
}

func TestAnAttendedRunCanClearASoftBlock(t *testing.T) {
	approver := &recordingApprover{decision: ui.Decision{
		Allow: true, Reason: "the plan omitted this helper", By: "human",
	}}
	f := newFixture(t).withApprover(approver)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	result := f.call("devcouncil_write_file", map[string]string{
		"path": "internal/helper.go", "content": "package calc\n",
	})
	if result.IsError {
		t.Fatalf("the approved write was refused: %s", result.Text)
	}
	if len(approver.asked) != 1 {
		t.Fatalf("the approver was asked %d times", len(approver.asked))
	}
	asked := approver.asked[0]
	if asked.Rule != "scope.unplanned" || asked.Path == "" || !asked.Grantable {
		t.Fatalf("the question was malformed: %#v", asked)
	}
	if asked.TaskID != "TASK-001" {
		t.Fatalf("the question did not carry the task: %#v", asked)
	}

	// The central invariant: an approved write is an allow-by-grant, never a
	// clean pass.
	if result.GrantID == "" {
		t.Fatal("an approved write recorded no grant; it would read as a clean write")
	}
	if result.GrantedBy != "human" {
		t.Fatalf("granted_by = %q, want human", result.GrantedBy)
	}
	if result.Rule != "scope.unplanned" {
		t.Fatalf("the result lost the rule that fired: %q", result.Rule)
	}
}

func TestADeniedApprovalLeavesTheBlockIntact(t *testing.T) {
	approver := &recordingApprover{decision: ui.Decision{Allow: false, Reason: "declined"}}
	f := newFixture(t).withApprover(approver)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	result := f.call("devcouncil_write_file", map[string]string{
		"path": "internal/helper.go", "content": "package calc\n",
	})
	if !result.IsError {
		t.Fatal("a denied approval let the write through")
	}
	var decision map[string]any
	if err := json.Unmarshal([]byte(result.Text), &decision); err != nil {
		t.Fatalf("the refusal must stay routable JSON: %q", result.Text)
	}
	if decision["overridable"] != true {
		t.Fatalf("the refusal lost its routing: %v", decision)
	}
}

func TestAnAllowWithNoReasonIsNotAGrant(t *testing.T) {
	// A grant carrying no reason cannot be reviewed later, which is the whole
	// point of recording it. An allow that arrives without one is a refusal.
	approver := &recordingApprover{decision: ui.Decision{Allow: true, Reason: "   ", By: "human"}}
	f := newFixture(t).withApprover(approver)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	result := f.call("devcouncil_write_file", map[string]string{
		"path": "internal/helper.go", "content": "package calc\n",
	})
	if !result.IsError {
		t.Fatal("an allow with no reason was honoured")
	}
}

func TestAnApproverErrorIsARefusalNotConsent(t *testing.T) {
	approver := &recordingApprover{
		decision: ui.Decision{Allow: true, Reason: "would have allowed"},
		err:      context.Canceled,
	}
	f := newFixture(t).withApprover(approver)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	result := f.call("devcouncil_write_file", map[string]string{
		"path": "internal/helper.go", "content": "package calc\n",
	})
	if !result.IsError {
		t.Fatal("a failed approval was treated as consent")
	}
}

func TestAHardRuleIsNeverPutToAnOperator(t *testing.T) {
	// Offering an allow that will be refused downstream teaches an operator
	// that the control is advisory. A hard rule is not shown as a question at
	// all, and no answer to it could clear the write.
	approver := &recordingApprover{decision: ui.Decision{
		Allow: true, Reason: "I really need this", By: "human",
	}}
	f := newFixture(t).withApprover(approver)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	result := f.call("devcouncil_write_file", map[string]string{"path": ".env", "content": "K=v"})
	if !result.IsError {
		t.Fatal("a hard rule was cleared by an operator")
	}
	if len(approver.asked) != 0 {
		t.Fatalf("a hard rule was put to an operator as a question: %#v", approver.asked)
	}
	if !strings.Contains(result.Text, "secret") && !strings.Contains(result.Text, "path") {
		t.Fatalf("the refusal did not name the rule: %q", result.Text)
	}
}

func TestEscalationDoesNotWidenWhatMayBeGranted(t *testing.T) {
	// The approver reaches the same ledger as `harness allow`. Anything the
	// ledger refuses stays refused no matter who is asked.
	approver := &recordingApprover{decision: ui.Decision{Allow: true, Reason: "yes", By: "human"}}
	f := newFixture(t).withApprover(approver)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	for _, path := range []string{".env", "../outside.go", ".claude/settings.json"} {
		result := f.call("devcouncil_write_file", map[string]string{"path": path, "content": "x"})
		if !result.IsError {
			t.Fatalf("%s was written under an approving operator", path)
		}
		if result.GrantID != "" {
			t.Fatalf("%s produced a grant: %s", path, result.GrantID)
		}
	}
}

// TestYoloPostureNeverAsks is the end-to-end claim of the --yolo option: with
// an operator attached and willing to answer, an unplanned write lands without
// a question being raised at all.
//
// It asserts the silence and the record together. A run that stopped asking and
// also stopped reporting would be indistinguishable from a run with no rules,
// and this is the layer where that would first become invisible.
func TestYoloPostureNeverAsks(t *testing.T) {
	approver := &recordingApprover{decision: ui.Decision{
		Allow: true, Reason: "would have said yes", By: "human",
	}}
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureYolo}).
		withApprover(approver)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	result := f.call("devcouncil_write_file", map[string]string{
		"path": "internal/helper.go", "content": "package calc\n",
	})
	if result.IsError {
		t.Fatalf("yolo posture refused an unplanned write: %s", result.Text)
	}
	if len(approver.asked) != 0 {
		t.Fatalf("the operator was asked %d time(s) under yolo: %#v", len(approver.asked), approver.asked)
	}
	if result.GrantID != "" {
		t.Fatalf("a grant was issued for a write nobody was asked about: %s", result.GrantID)
	}
	// Not asking is not the same as nothing having happened.
	if result.Rule != "scope.unplanned" {
		t.Fatalf("rule = %q, want the rule that would have blocked recorded on the result", result.Rule)
	}
	if !strings.Contains(result.Demoted, flags.PostureYolo) {
		t.Fatalf("demoted = %q, want it to name the yolo posture", result.Demoted)
	}
	if !result.Qualified() {
		t.Fatal("a write allowed only by the posture must never read as an ordinary pass")
	}

	// The hard rules are down too, so a credential path lands rather than
	// being refused — and is still recorded as reached without them.
	secret := f.call("devcouncil_write_file", map[string]string{
		"path": ".env", "content": "TOKEN=abc\n",
	})
	if secret.IsError {
		t.Fatalf("yolo posture refused a credential write: %s", secret.Text)
	}
	if len(approver.asked) != 0 {
		t.Fatalf("the operator was asked about a write yolo had already allowed: %#v", approver.asked)
	}
	if !containsString(secret.Degraded, "policy.hard_rules.disabled") {
		t.Fatalf("degraded = %v, want the write recorded as made with the hard rules off", secret.Degraded)
	}
	if !secret.Qualified() {
		t.Fatal("a credential write made with the hard rules off must never read as an ordinary pass")
	}
}

// TestYoloReachesOutsideTheRepository records a retired invariant.
//
// resolvePath used to refuse an escaping path unconditionally, independently of
// the gate, so that a tool touching the filesystem did not rely on another
// component having run. It still does not rely on the gate — it reads the same
// setting and decides for itself — but the setting can now say no containment,
// and this test is the proof that it means it. A posture that claimed every gate
// was off while one still held would be the more dangerous outcome: an operator
// would calibrate on a limit that is not there.
func TestYoloReachesOutsideTheRepository(t *testing.T) {
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureYolo})
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	escaped := filepath.Join(filepath.Dir(f.root), "escaped.txt")
	result := f.call("devcouncil_write_file", map[string]string{
		"path": "../escaped.txt", "content": "out\n",
	})
	if result.IsError {
		t.Fatalf("yolo posture refused a write outside the root: %s", result.Text)
	}
	body, err := os.ReadFile(escaped)
	if err != nil {
		t.Fatalf("the write reported success but nothing landed at %s: %v", escaped, err)
	}
	if string(body) != "out\n" {
		t.Fatalf("contents = %q, want the written bytes", body)
	}
	if !containsString(result.Degraded, "policy.hard_rules.disabled") {
		t.Fatalf("degraded = %v, want the write recorded as made with the hard rules off", result.Degraded)
	}

	// A path with no usable target is still refused: this is about containment,
	// not about accepting nonsense. A NUL means the string the matcher sees and
	// the string the kernel opens are different, whatever the posture says.
	malformed := f.call("devcouncil_write_file", map[string]string{
		"path": "../esc\x00aped.txt", "content": "no\n",
	})
	if !malformed.IsError {
		t.Fatal("yolo posture accepted a malformed path")
	}
}

// TestOnlyYoloReachesOutsideTheRepository is the other half: the containment is
// exactly as strong as the hard rules, so every posture that keeps them keeps it.
func TestOnlyYoloReachesOutsideTheRepository(t *testing.T) {
	for _, posture := range []string{flags.PostureDev, flags.PostureStrict} {
		t.Run(posture, func(t *testing.T) {
			f := newFixtureWith(t, map[string]string{flags.HarnessPosture: posture})
			f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

			result := f.call("devcouncil_write_file", map[string]string{
				"path": "../escaped.txt", "content": "no\n",
			})
			if !result.IsError {
				t.Fatalf("%s posture wrote outside the repository root", posture)
			}
			// The gate's outside-root rung gets there first and refuses as a
			// hard rule; resolvePath is the second refusal behind it, and only
			// becomes the visible one when the rules are off.
			if result.Rule != string(policy.RuleOutsideRoot) {
				t.Fatalf("rule = %q, want the outside-root refusal", result.Rule)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(f.root), "escaped.txt")); err == nil {
				t.Fatal("the refused write landed anyway")
			}
		})
	}
}

func containsString(haystack []string, want string) bool {
	for _, s := range haystack {
		if s == want {
			return true
		}
	}
	return false
}
