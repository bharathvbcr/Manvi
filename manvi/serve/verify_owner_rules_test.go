package serve

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

// ownerRunner is a runner whose process identity and whose view of other
// processes a test sets.
func ownerRunner(t *testing.T, self verifyOwner, starts map[int]processStartResult, now time.Time) *VerifyRunner {
	t.Helper()
	t.Cleanup(func() {
		processStartOf = codingagent.ProcessStart
		processZombieOf = codingagent.ProcessZombie
	})
	processZombieOf = func(pid int) (bool, error) { return zombiePids[pid], nil }
	processStartOf = func(pid int) (int64, error) {
		got, known := starts[pid]
		if !known {
			return 0, errors.New("not scripted")
		}
		return got.start, got.err
	}
	return &VerifyRunner{deps: VerifyDeps{Timeout: 15 * time.Minute, Now: func() time.Time { return now }}, owner: self}
}

// zombiePids are the scripted pids that have exited and not been reaped.
var zombiePids = map[int]bool{}

type processStartResult struct {
	start int64
	err   error
}

func TestOwnerEndedNamesItsBasis(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Minute).Format(time.RFC3339)
	stale := now.Add(-time.Hour).Format(time.RFC3339)
	self := verifyOwner{PID: 100, Start: 7}
	cases := []struct {
		name      string
		owner     *verifyOwner
		startedAt string
		starts    map[int]processStartResult
		ended     bool
		basis     string
	}{
		{"this process owns it", &verifyOwner{PID: 100, Start: 7}, fresh, nil, true, "this process claimed it"},
		{"owner alive, inside the deadline", &verifyOwner{PID: 200, Start: 9}, fresh, map[int]processStartResult{200: {start: 9}}, false, ""},
		{"owner alive, past the deadline", &verifyOwner{PID: 200, Start: 9}, stale, map[int]processStartResult{200: {start: 9}}, true, "although the owner"},
		{"owner is a zombie", &verifyOwner{PID: 200, Start: 9}, fresh, map[int]processStartResult{200: {start: 9}}, true, "zombie"},
		{"owner gone", &verifyOwner{PID: 200, Start: 9}, fresh, map[int]processStartResult{200: {err: codingagent.ErrNoProcess}}, true, "no longer exists"},
		{"pid reused by another process", &verifyOwner{PID: 200, Start: 9}, fresh, map[int]processStartResult{200: {start: 11}}, true, "start time 11"},
		{"our own pid reused after a restart", &verifyOwner{PID: 100, Start: 3}, fresh, map[int]processStartResult{100: {start: 7}}, true, "start time 7"},
		{"liveness unknown, inside the deadline", &verifyOwner{PID: 200, Start: 9}, fresh, map[int]processStartResult{200: {err: codingagent.ErrProcessStartUnsupported}}, false, ""},
		{"liveness unknown, past the deadline", &verifyOwner{PID: 200, Start: 9}, stale, map[int]processStartResult{200: {err: codingagent.ErrProcessStartUnsupported}}, true, "older than the job deadline"},
		{"no owner recorded, inside the deadline", nil, fresh, nil, false, ""},
		{"no owner recorded, past the deadline", nil, stale, nil, true, "older than the job deadline"},
		{"unreadable claim time", nil, "yesterday", nil, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := ownerRunner(t, self, tc.starts, now)
			zombiePids = map[int]bool{200: tc.name == "owner is a zombie"}
			basis, ended := r.ownerEnded(tc.owner, tc.startedAt)
			if ended != tc.ended || (tc.ended && !strings.Contains(basis, tc.basis)) {
				t.Fatalf("ownerEnded = (%q, %v), want ended=%v naming %q", basis, ended, tc.ended, tc.basis)
			}
		})
	}
}

// The owner is stored beside the record, never in it: the wire record a host
// reads is unchanged.
func TestTheOwnerIsStoredBesideTheRecordNotInIt(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	h.change()
	record := h.settled("t", "req-1")
	wire, err := json.Marshal(VerifyResult{Record: record})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "owner") {
		t.Fatalf("the owner reached the wire record: %s", wire)
	}
	rows, _, err := h.client.EvidenceList(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Owner *verifyOwner `json:"owner"`
	}
	for _, row := range rows {
		if row.Type == verifyEvidenceType {
			if err := json.Unmarshal(row.DataJSON, &stored); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if stored.Owner == nil || stored.Owner.PID != os.Getpid() {
		t.Fatalf("stored owner = %+v, want this process (pid %d)", stored.Owner, os.Getpid())
	}
}
