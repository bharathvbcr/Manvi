//go:build linux || darwin

package serve

import (
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

// Adversarial tests for criterion 9 of lane w24-verdicts-b: who may settle a
// running verification record, and on what evidence.

func zadvPutRow(t *testing.T, h *verifyHarness, record VerifyRecord, owner any) {
	t.Helper()
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if owner != nil {
		ownerRaw, err := json.Marshal(owner)
		if err != nil {
			t.Fatal(err)
		}
		fields["owner"] = ownerRaw
	}
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.EvidenceAppend(context.Background(), verifyEvidenceType, record.ItemID, "", "", data); err != nil {
		t.Fatal(err)
	}
}

func zadvRunning(h *verifyHarness, request string, age time.Duration) VerifyRecord {
	return VerifyRecord{
		RequestID: request, ItemID: "t", RunID: "verify-t-" + request, State: verifyRunning,
		Worktree: h.repo, Base: h.base, Head: "HEAD",
		StartedAt: time.Now().Add(-age).UTC().Format(time.RFC3339),
		Changes:   VerifyChanges{Paths: []string{}}, Degraded: []string{}, Criteria: []VerifyCriterion{},
		CriticReadOnly: true,
	}
}

func zadvGet(t *testing.T, h *verifyHarness, request string) VerifyRecord {
	t.Helper()
	got, err := h.runner.Get(context.Background(), json.RawMessage(`{"item_id":"t","request_id":"`+request+`"}`))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return got.Record
}

// A process that has exited and not been reaped is still listed by the kernel
// (its pid and start time read back), so a record whose owner is a zombie is not
// "gone" by pid and start time alone. A zombie runs nothing: its record must be
// settled.
func TestZadv_w24vb_AZombieOwnersRunningRecordIsSettled(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })
	pid := cmd.Process.Pid
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, err := exec.CommandContext(t.Context(), "ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the child never became a zombie (last ps: %q, %v)", out, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	start, err := codingagent.ProcessStart(pid)
	if err != nil {
		t.Skipf("the platform does not list a zombie (%v): the case cannot arise here", err)
	}
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	h.change()
	zadvPutRow(t, h, zadvRunning(h, "zombie", time.Minute), verifyOwner{PID: pid, Start: start})
	if got := zadvGet(t, h, "zombie"); got.State != verifySettled {
		t.Fatalf("a record whose owner is an unreaped zombie (pid %d) is %q: no process is running it and nothing will settle it", pid, got.State)
	}
}

// The owner says "alive" and the claim is far older than any job can run: a
// wedged owner, a forged owner pid, an owner recorded with no start time whose
// pid has been handed to another process. Each leaves the record running with no
// end on the tip, because the age rule is applied only when identity is unknown.
func TestZadv_w24vb_ALiveLookingOwnerIsNotTrustedPastTheJobDeadline(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-3 * time.Hour).Format(time.RFC3339)
	self := verifyOwner{PID: 100, Start: 7}
	cases := map[string]*verifyOwner{
		"owner alive with its start time":     {PID: 200, Start: 9},
		"owner recorded without a start time": {PID: 200},
	}
	for name, owner := range cases {
		t.Run(name, func(t *testing.T) {
			r := ownerRunner(t, self, map[int]processStartResult{200: {start: 9}}, now)
			if basis, ended := r.ownerEnded(owner, stale); !ended {
				t.Fatalf("a claim three hours old (deadline 15m) with a live-looking owner is still running: %s", basis)
			}
		})
	}
}

// The store is local, but a row's owner is still input. A row whose owner does
// not parse must not make the record unreadable: the base decoded it (an unknown
// key is ignored) and a verdict row is not allowed to vanish because of a
// sidecar field.
func TestZadv_w24vb_AnUnreadableOwnerDoesNotHideTheRecord(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	h.change()
	settled := zadvRunning(h, "garbled", time.Hour)
	settled.State, settled.Outcome = verifySettled, VerifyPassed
	settled.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	zadvPutRow(t, h, settled, "not an object")
	got, err := h.runner.Get(context.Background(), json.RawMessage(`{"item_id":"t","request_id":"garbled"}`))
	if err != nil {
		t.Fatalf("a settled passed row whose owner field is a string cannot be read: %v", err)
	}
	if got.Record.State != verifySettled || got.Record.Outcome != VerifyPassed {
		t.Fatalf("the record read back = %+v, want the settled passed row", got.Record)
	}
}

// A forged owner (pid 1, always alive, with its real start time) pins a running
// record forever: nothing in the owner field is checked against the record. The
// store is local, so this is the cost of trusting the field and the age ceiling
// is what bounds it.
func TestZadv_w24vb_AForgedLiveOwnerCannotPinARunningRecordForever(t *testing.T) {
	start, err := codingagent.ProcessStart(1)
	if err != nil {
		t.Skipf("pid 1 start time unreadable (%v)", err)
	}
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	h.change()
	zadvPutRow(t, h, zadvRunning(h, "forged", 3*time.Hour), verifyOwner{PID: 1, Start: start})
	if got := zadvGet(t, h, "forged"); got.State != verifySettled {
		t.Fatalf("a 3-hour-old running record naming pid 1 as its owner is %q", got.State)
	}
}

// The wire shape: a running or settled record read back through Get carries no
// owner, however the row was stored.
func TestZadv_w24vb_TheOwnerNeverReachesTheWireShape(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	h.change()
	zadvPutRow(t, h, zadvRunning(h, "wire", time.Minute), verifyOwner{PID: 1, Start: 5})
	got, err := h.runner.Get(context.Background(), json.RawMessage(`{"item_id":"t","request_id":"wire"}`))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(wire, &generic); err != nil {
		t.Fatal(err)
	}
	record, _ := generic["record"].(map[string]any)
	if _, leaked := record["owner"]; leaked || strings.Contains(string(wire), "\"pid\"") {
		t.Fatalf("the owner reached the wire: %s", wire)
	}
}

// An older binary wrote no owner. Its running record is left alone inside the
// job deadline plus the grace, and settled after it, naming the basis.
func TestZadv_w24vb_ARecordWithNoOwnerIsSettledAfterDeadlinePlusGraceAndNamesWhy(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	h.change()
	zadvPutRow(t, h, zadvRunning(h, "inside", DefaultVerifyTimeout+verifyOrphanGrace-time.Minute), nil)
	if got := zadvGet(t, h, "inside"); got.State != verifyRunning {
		t.Fatalf("an ownerless claim inside deadline+grace was settled: %+v", got)
	}
	zadvPutRow(t, h, zadvRunning(h, "outside", DefaultVerifyTimeout+verifyOrphanGrace+time.Minute), nil)
	got := zadvGet(t, h, "outside")
	if got.State != verifySettled || got.Outcome != VerifyDegraded {
		t.Fatalf("an ownerless claim past deadline+grace = %+v, want settled degraded", got)
	}
	requireDegraded(t, got, "older than the job deadline")
}

// A pid that cannot be a process is never read as a start time.
func TestZadv_w24vb_ImpossiblePidsAreNeverAStartTime(t *testing.T) {
	for _, pid := range []int{0, -1, -1 << 31, 1 << 31, 1 << 32, 1 << 40, 4194305} {
		start, err := codingagent.ProcessStart(pid)
		if err == nil {
			t.Errorf("ProcessStart(%d) = %d, nil: a pid no kernel hands out answered with a start time", pid, start)
		}
	}
}
