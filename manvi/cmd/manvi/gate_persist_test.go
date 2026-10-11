package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/grants"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/testsupport"
)

// persistedGrants reads the ledger file the way the next process will.
func persistedGrants(t *testing.T) []grants.Grant {
	t.Helper()
	data, err := os.ReadFile(grantLedgerPath())
	if err != nil {
		t.Fatalf("the ledger was never written: %v", err)
	}
	var saved []grants.Grant
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("the ledger on disk does not parse: %v", err)
	}
	return saved
}

// TestARevokedGrantIsRevokedOnDisk. The ledger was written only when a grant
// was issued, so a revoke (and a single-use grant being spent) lived in memory
// and the next process loaded the grant as live again: a human's revoke undone
// by a restart. Every ledger mutation must reach the file, and as a rename, not
// a truncate, so a crash cannot leave half a ledger for loadGrants to refuse.
func TestARevokedGrantIsRevokedOnDisk(t *testing.T) {
	defer setProjectRootForTest(t.TempDir())()
	reg, err := flags.NewHarnessRegistry(configPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Set(flags.Human, flags.HarnessPosture, flags.PostureStrict); err != nil {
		t.Fatal(err)
	}
	g, _, err := buildGate(reg)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := g.EvaluateWrite(filepath.Join(g.Root, "a.go"), demoTask(""), dc.OpWrite)
	if err != nil || !decision.Blocked() || !decision.Overridable() {
		t.Fatalf("want an overridable block to grant against, got %+v, %v", decision, err)
	}
	grant, err := g.RequestOverride(decision, grants.Grantor{Authority: grants.Human, ID: "op"}, "reviewed", "")
	if err != nil {
		t.Fatal(err)
	}
	if saved := persistedGrants(t); len(saved) != 1 || saved[0].Consumed {
		t.Fatalf("after issue the file holds %+v, want one live grant", saved)
	}

	if err := g.Ledger.Revoke(grant.ID); err != nil {
		t.Fatal(err)
	}
	saved := persistedGrants(t)
	if len(saved) != 1 || !saved[0].Consumed {
		t.Fatalf("after revoke the file holds %+v, want the grant recorded as consumed", saved)
	}

	left, _ := filepath.Glob(filepath.Join(filepath.Dir(grantLedgerPath()), ".safefile-*"))
	if len(left) != 0 {
		t.Fatalf("temp files left beside the ledger: %v", left)
	}
}

// TestCheckRefusesAnOperandTheGateCannotVerifyAsAHardBlock. `manvi check --cmd`
// must say what the agent's own command gate will say: a program the gate does
// not model, aimed outside the root or at the tool's own state, is a hard block
// (status 7, nothing to grant), never an overridable one an operator is invited
// to clear with `manvi allow`.
func TestCheckRefusesAnOperandTheGateCannotVerifyAsAHardBlock(t *testing.T) {
	defer setProjectRootForTest(t.TempDir())()
	reg, err := flags.NewHarnessRegistry(configPath())
	if err != nil {
		t.Fatal(err)
	}
	g, _, err := buildGate(reg)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"go build -o /tmp/x ./...", "sqlite3 .devcouncil/store.db .tables"} {
		var out strings.Builder
		err := check(&out, g, []string{"--cmd", command})
		if !errors.Is(err, errCheckHardBlocked) || !strings.Contains(out.String(), "command.operand.unverifiable") {
			t.Errorf("%q: err = %v, want a hard block by command.operand.unverifiable:\n%s", command, err, out.String())
		}
	}
}

// TestWatchNamesALeaseTheStoreCouldNotRead. `manvi watch` counted ready tasks
// and active leases and said nothing of the task that is in neither list
// because its lease has an expiry no one can parse, so the report read as a
// complete account of a store that had withheld a task.
func TestWatchNamesALeaseTheStoreCouldNotRead(t *testing.T) {
	binary := testsupport.DCStore(t)
	db := filepath.Join(t.TempDir(), "state.sqlite")
	t.Setenv("MANVI_STORE_BINARY", binary)
	t.Setenv("MANVI_STORE_DB", db)
	if _, err := storeClient().ActiveLeases(t.Context()); err != nil {
		t.Fatalf("store: %v", err)
	}
	seed := "INSERT INTO tasks (id,title,description,planned_files_json,status) VALUES ('TASK-BAD','t','d','[]','ready');" +
		"INSERT INTO task_leases (id,task_id,owner,lease_token,status,created_at,expires_at) " +
		"VALUES ('L-BAD','TASK-BAD','someone','tok','active','2026-01-01T00:00:00Z','not a timestamp');"
	if out, err := exec.CommandContext(t.Context(), testsupport.Tool(t, "sqlite3"), db, seed).CombinedOutput(); err != nil {
		t.Fatalf("seeding failed: %v %s", err, out)
	}
	reg, err := flags.NewHarnessRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := watch(&out, reg, []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "L-BAD") {
		t.Fatalf("the unreadable lease is not in the report:\n%s", out.String())
	}
}
