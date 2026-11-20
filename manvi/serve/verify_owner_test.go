package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/testsupport"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/internal/storeguard"
)

// A running verification record belongs to the process that claimed it. These
// tests drive two real processes against one profile store: the test binary
// itself, and a re-exec of it that claims a verification and never settles it.

const verifyOwnerHelperEnv = "MANVI_VERIFY_OWNER_HELPER"

// TestVerifyOwnerHelperProcess is the owning process of the two-process tests.
// Run by an ordinary test pass it returns at once; re-executed with the
// environment its parent sets it claims one verification, tells the parent, and
// blocks in the critic until the parent kills it. It does not close its runner:
// a process that was killed does not settle its record.
func TestVerifyOwnerHelperProcess(t *testing.T) {
	if os.Getenv(verifyOwnerHelperEnv) != "1" {
		return
	}
	client := storeguard.NewClient(t, os.Getenv("MANVI_HELPER_DCSTORE"), os.Getenv("MANVI_HELPER_DB"))
	critic := &scriptedCritic{answer: func(ctx context.Context, _ string, _ int) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}}
	runner, err := NewVerifyRunner(VerifyDeps{
		Client: client, Critic: critic,
		Devmap:         func(string) DevmapClient { return newScriptedDevmap() },
		NewVerifier:    func(string, string, string) (devcouncil.PathChecker, error) { return passingChecker(), nil },
		VerifierBinary: "/bin/sh",
	})
	if err != nil {
		fmt.Println("HELPER-FAILED", err)
		os.Exit(3)
	}
	raw := fmt.Sprintf(`{"request_id":"req-1","item_id":"t","worktree":%q,"base":%q,"head":"HEAD"}`,
		os.Getenv("MANVI_HELPER_REPO"), os.Getenv("MANVI_HELPER_BASE"))
	if _, err := runner.Start(context.Background(), json.RawMessage(raw)); err != nil {
		fmt.Println("HELPER-FAILED", err)
		os.Exit(3)
	}
	fmt.Println("HELPER-CLAIMED")
	select {}
}

// startOwnerProcess runs the helper against h's store and worktree and returns
// once it has claimed the verification.
func startOwnerProcess(t *testing.T, h *verifyHarness) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestVerifyOwnerHelperProcess$", "-test.timeout=10m") //nolint:gosec // G204: re-executes this test binary
	cmd.Env = append(os.Environ(),
		verifyOwnerHelperEnv+"=1",
		"MANVI_HELPER_DCSTORE="+testsupport.DCStore(t),
		"MANVI_HELPER_DB="+h.db,
		"MANVI_HELPER_REPO="+h.repo,
		"MANVI_HELPER_BASE="+h.base,
	)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill() // already gone when the test killed it
		_ = cmd.Wait()
	})
	claimed := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(out)
		for scanner.Scan() {
			if line := scanner.Text(); strings.HasPrefix(line, "HELPER-") {
				claimed <- line
				return
			}
		}
		claimed <- "the helper exited without claiming"
	}()
	select {
	case line := <-claimed:
		if line != "HELPER-CLAIMED" {
			t.Fatalf("the owning process: %s", line)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("the owning process never claimed the verification")
	}
	return cmd
}

func verifyRows(t *testing.T, h *verifyHarness) int {
	t.Helper()
	rows, truncated, err := h.client.EvidenceList(context.Background(), "t")
	if err != nil || truncated {
		t.Fatalf("EvidenceList: %v truncated=%v", err, truncated)
	}
	n := 0
	for _, row := range rows {
		if row.Type == verifyEvidenceType {
			n++
		}
	}
	return n
}

// Criterion 9: the failing-before test. Process B reading process A's running
// record while A is alive does not settle it and appends nothing. This holds on
// every platform: where process start times cannot be read, a fresh claim is not
// abandoned either.
func TestAnotherProcessDoesNotSettleALiveOwnersRunningRecord(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	h.change()
	startOwnerProcess(t, h)

	before := verifyRows(t, h)
	got, err := h.runner.Get(context.Background(), json.RawMessage(`{"item_id":"t","request_id":"req-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Record.State != verifyRunning || len(got.Record.Degraded) != 0 {
		t.Fatalf("a read from another process settled a live owner's record: %+v", got.Record)
	}
	if replay := h.start("t", "req-1"); !replay.Replayed || replay.Record.State != verifyRunning {
		t.Fatalf("a replay from another process = %+v, want the owner's running record", replay)
	}
	if after := verifyRows(t, h); after != before {
		t.Fatalf("another process appended %d row(s) to a live owner's record", after-before)
	}

}
