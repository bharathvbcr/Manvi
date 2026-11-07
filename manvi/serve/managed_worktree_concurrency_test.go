package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mapclient "github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/devmap"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/testsupport"
	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

func TestManagedRunnerParallelDistinctWorktreesAndSameCheckoutBusy(t *testing.T) {
	wtA := scanRepo(t, map[string]string{"CLAUDE.md": "rules\n"})
	wtBRaw := filepath.Join(t.TempDir(), "lane-b")
	addCmd := exec.CommandContext(t.Context(), "git", "worktree", "add", "--detach", wtBRaw, "HEAD") //nolint:gosec // G204: literal git; wtBRaw is a path under t.TempDir()
	addCmd.Dir = wtA
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	wtB, err := filepath.EvalSymlinks(wtBRaw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		rm := exec.CommandContext(context.WithoutCancel(t.Context()), "git", "worktree", "remove", "--force", wtB) //nolint:gosec // G204: literal git; wtB is the worktree this test added
		rm.Dir = wtA
		_, _ = rm.CombinedOutput()
	})

	gitDirOut := strings.TrimSpace(gitIn(t, wtB, "rev-parse", "--git-dir"))
	if !filepath.IsAbs(gitDirOut) {
		gitDirOut = filepath.Join(wtB, gitDirOut)
	}
	wtBGitDir, err := filepath.EvalSymlinks(gitDirOut)
	if err != nil {
		t.Fatal(err)
	}
	commonDir := filepath.Join(wtA, ".git")

	client := store.New(testsupport.DCStore(t), filepath.Join(t.TempDir(), "profile.sqlite"))
	t.Cleanup(func() { client.Close() })

	enhancementCall(t, client, "repositories.put", fmt.Sprintf(
		`{"id":"r","request_id":"r","expected_revision":0,"name":"Repo","identity_key":%q}`, "local:"+commonDir))
	enhancementCall(t, client, "items.put",
		`{"id":"t-a1","request_id":"t-a1","expected_revision":0,"title":"Lane A1","repository_ids":["r"],"primary_repository_id":"r"}`)
	enhancementCall(t, client, "items.put",
		`{"id":"t-b1","request_id":"t-b1","expected_revision":0,"title":"Lane B1","repository_ids":["r"],"primary_repository_id":"r"}`)
	enhancementCall(t, client, "items.put",
		`{"id":"t-a2","request_id":"t-a2","expected_revision":0,"title":"Lane A2","repository_ids":["r"],"primary_repository_id":"r"}`)

	enhancementCall(t, client, "runs.prepare", fmt.Sprintf(
		`{"id":"run-a1","request_id":"prep-a1","expected_revision":0,"kind":"managed","task_id":"t-a1","source_revision":1,"repository_id":"r","repository_revision":1,"provider":"claude","permission_mode":"ask","cwd":%q,"git_dir":%q,"git_common_dir":%q,"head_oid":null,"max_active_runs":4}`,
		wtA, commonDir, commonDir))
	enhancementCall(t, client, "runs.prepare", fmt.Sprintf(
		`{"id":"run-b1","request_id":"prep-b1","expected_revision":0,"kind":"managed","task_id":"t-b1","source_revision":1,"repository_id":"r","repository_revision":1,"provider":"claude","permission_mode":"ask","cwd":%q,"git_dir":%q,"git_common_dir":%q,"head_oid":null,"max_active_runs":4}`,
		wtB, wtBGitDir, commonDir))

	// Layer 1 (dc-store): preparing a second run on wtA while run-a1 holds wtA is
	// refused with checkout_busy, whereas wtB was admitted above on the same repo.
	_, storeBusyErr := client.Workbench(t.Context(), "runs.prepare", json.RawMessage(fmt.Sprintf(
		`{"id":"run-a2","request_id":"prep-a2","expected_revision":0,"kind":"managed","task_id":"t-a2","source_revision":1,"repository_id":"r","repository_revision":1,"provider":"claude","permission_mode":"ask","cwd":%q,"git_dir":%q,"git_common_dir":%q,"head_oid":null,"max_active_runs":4}`,
		wtA, commonDir, commonDir)))
	if storeBusyErr == nil || !strings.Contains(storeBusyErr.Error(), "checkout_busy") {
		t.Fatalf("dc-store runs.prepare on busy wtA = %v, want checkout_busy", storeBusyErr)
	}

	var mu sync.Mutex
	sessions := map[string]*managedTestSession{}
	runner, err := NewManagedRunner(client, func(ctx context.Context, options codingagent.Options) (ManagedSession, error) {
		cfg := codingagent.Effective{
			Cwd: options.Cwd, ApprovalPolicy: "on-request", ApprovalsReviewer: "user",
			Sandbox: codingagent.Sandbox{Type: "readOnly"}, Model: "fixture", ModelProvider: "fixture",
		}
		cfg.Thread.ID = "thread-" + filepath.Base(options.Cwd)
		cfg.Thread.Cwd = options.Cwd
		cfg.Thread.Ephemeral = true
		s := &managedTestSession{ctx: ctx, config: cfg, events: make(chan codingagent.Event, 16), withRequest: true}
		mu.Lock()
		sessions[options.Cwd] = s
		mu.Unlock()
		return s, nil
	}, func(err error) string { return err.Error() })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Close() })
	if err := runner.SetHarness(newTestResolver(t)); err != nil {
		t.Fatal(err)
	}

	// Prepare and activate run-a1 (wtA) and run-b1 (wtB) concurrently!
	var prepA, prepB ManagedPrepared
	var errA, errB error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		prepA, errA = runner.Prepare(t.Context(), json.RawMessage(
			`{"id":"run-a1","request_id":"launch-a1","expected_revision":1,"harness":{"kind":"named","name":"registered"}}`))
		if errA == nil {
			_, errA = runner.Activate(t.Context(), json.RawMessage(fmt.Sprintf(
				`{"id":"run-a1","owner_id":%q,"session_id":%q,"process_id":%d,"process_start":"fixture-native-observation"}`,
				prepA.Owner, prepA.Session, prepA.PID)))
		}
	}()
	go func() {
		defer wg.Done()
		prepB, errB = runner.Prepare(t.Context(), json.RawMessage(
			`{"id":"run-b1","request_id":"launch-b1","expected_revision":1,"harness":{"kind":"named","name":"registered"}}`))
		if errB == nil {
			_, errB = runner.Activate(t.Context(), json.RawMessage(fmt.Sprintf(
				`{"id":"run-b1","owner_id":%q,"session_id":%q,"process_id":%d,"process_start":"fixture-native-observation"}`,
				prepB.Owner, prepB.Session, prepB.PID)))
		}
	}()
	wg.Wait()

	if errA != nil || errB != nil {
		t.Fatalf("parallel distinct-worktree harnessed runs failed: wtA=%v wtB=%v", errA, errB)
	}

	// Both checkouts must hold live harness blocks simultaneously.
	for name, dir := range map[string]string{"wtA": wtA, "wtB": wtB} {
		if busy := runner.HarnessBusy(dir); busy == nil || busy.Code != ErrHarnessBusy {
			t.Fatalf("%s is not marked busy while its harness run is live: %v", name, busy)
		}
		if body := readFile(t, dir, "CLAUDE.md"); !strings.Contains(body, harnessBeginMarker) {
			t.Fatalf("%s CLAUDE.md missing live harness block: %q", name, body)
		}
	}

	// Layer 2 (ManagedRunner.enterCheckout): even through a symlink alias to wtA,
	// same-checkout admission fails closed with ErrHarnessBusy while wtB is live.
	wtAAlias := filepath.Join(t.TempDir(), "wtA-symlink")
	if err := os.Symlink(wtA, wtAAlias); err != nil {
		t.Fatal(err)
	}
	probeJob := &managedJob{done: make(chan struct{}), harnessRef: ManagedHarness{Kind: "named", Name: "registered"}}
	sameCheckoutErr := runner.enterCheckout(probeJob, managedRun{Cwd: wtAAlias})
	var typed *Error
	if !errors.As(sameCheckoutErr, &typed) || typed.Code != ErrHarnessBusy {
		t.Fatalf("concurrent enterCheckout on busy wtA symlink returned %v, want %s", sameCheckoutErr, ErrHarnessBusy)
	}

	// Stop both live runs cleanly and verify both checkouts restore CLAUDE.md byte-for-byte.
	for _, runID := range []string{"run-a1", "run-b1"} {
		if err := runner.Stop(json.RawMessage(fmt.Sprintf(`{"id":%q}`, runID))); err != nil {
			t.Fatalf("Stop %s: %v", runID, err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runner.HarnessBusy(wtA) == nil && runner.HarnessBusy(wtB) == nil {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	for name, dir := range map[string]string{"wtA": wtA, "wtB": wtB} {
		if busy := runner.HarnessBusy(dir); busy != nil {
			t.Fatalf("%s remained busy after run stopped: %v", name, busy)
		}
		if got := readFile(t, dir, "CLAUDE.md"); got != "rules\n" {
			t.Fatalf("%s CLAUDE.md was not restored cleanly: %q", name, got)
		}
	}
}

func TestWarmDevmapPoolReusesUpToBoundAndClosesCleanly(t *testing.T) {
	pool := NewWarmDevmapPool("devmap-nonexistent-binary")
	t.Cleanup(func() { _ = pool.Close() })

	realRoot := resolvedDir(t)
	linkParent := t.TempDir()
	symlinkRoot := filepath.Join(linkParent, "alias")
	if err := os.Symlink(realRoot, symlinkRoot); err != nil {
		t.Fatal(err)
	}

	c1 := pool.ForRoot(realRoot)
	c2 := pool.ForRoot(symlinkRoot)
	if c1 != c2 {
		t.Fatal("WarmDevmapPool created distinct clients for canonical root and its symlink")
	}

	pooled := []*mapclient.Client{c1}
	for i := 1; i < maxWarmDevmapPoolEntries; i++ {
		r := resolvedDir(t)
		c := pool.ForRoot(r)
		if c != pool.ForRoot(r) {
			t.Fatalf("slot %d was not reused on repeat lookup", i)
		}
		pooled = append(pooled, c)
	}

	// Overflow root (#17) gets a fresh unpooled client without evicting the existing 16.
	overflowRoot := resolvedDir(t)
	over1 := pool.ForRoot(overflowRoot)
	over2 := pool.ForRoot(overflowRoot)
	if over1 == over2 {
		t.Fatal("overflow root past maxWarmDevmapPoolEntries was pooled beyond the bound")
	}
	if pool.ForRoot(realRoot) != pooled[0] {
		t.Fatal("overflow lookup evicted an existing warm session")
	}

	if err := pool.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := pool.Close(); err != nil {
		t.Fatalf("second idempotent Close: %v", err)
	}
	afterClose := pool.ForRoot(realRoot)
	if afterClose == pooled[0] {
		t.Fatal("closed pool returned a closed client")
	}
}
