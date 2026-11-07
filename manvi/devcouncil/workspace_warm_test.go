package devcouncil

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/devmap"
	"github.com/bharathvbcr/Manvi/manvi/agents"
)

func TestWorkspaceWarmMCPHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_WORKSPACE_WARM_HELPER") != "1" {
		return
	}
	logFile := os.Getenv("WORKSPACE_HELPER_LOG")
	repoRoot := os.Getenv("WORKSPACE_HELPER_ROOT")

	var args []string
	for i, a := range os.Args {
		if a == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	record := func(entry string) {
		if logFile == "" {
			return
		}
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err == nil {
			_, _ = fmt.Fprintln(f, entry)
			_ = f.Close()
		}
	}

	if len(args) >= 2 && args[len(args)-2] == "manifest" && args[len(args)-1] == "--help" {
		record("probe")
		_, _ = os.Stdout.WriteString("Usage: devmap manifest --output <FILE> --graph-output <FILE>\n")
		os.Exit(0)
	}

	if len(args) >= 1 && args[0] == "seed" {
		hasStacked := false
		for _, a := range args[1:] {
			if a == "--mode" || strings.HasPrefix(a, "--mode=") {
				_, _ = os.Stderr.WriteString("error: unexpected argument '--mode' found\n")
				os.Exit(2)
			}
			if a == "--stacked" {
				hasStacked = true
			}
		}
		if !hasStacked {
			_, _ = os.Stderr.WriteString("error: expected --stacked flag for zero-copy seed\n")
			os.Exit(2)
		}
		wtPath := args[len(args)-1]
		record("seed:" + strings.Join(args, " "))
		ci := filepath.Join(wtPath, ".devcouncil", "codeintel")
		// The seed writes only under the worktree it was handed, so it goes
		// through an os.Root on that worktree.
		wt, err := os.OpenRoot(wtPath)
		if err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}
		if err := wt.MkdirAll(".devcouncil/codeintel", 0o750); err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}
		leaseBody := fmt.Sprintf(`{"version":1,"base_db":%q,"delta_db":%q}`+"\n",
			filepath.Join(repoRoot, ".devcouncil", "codeintel", "devmap.sqlite"),
			filepath.Join(ci, "delta.sqlite"))
		if err := wt.WriteFile(".devcouncil/codeintel/stack_lease.json", []byte(leaseBody), 0o600); err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}
		_, _ = fmt.Fprintf(os.Stdout, `{"mode":"stacked","worktree":%q}`+"\n", wtPath)
		os.Exit(0)
	}

	if len(args) == 0 || args[len(args)-1] != "mcp" {
		record("cli:" + strings.Join(args, "_"))
		os.Exit(2)
	}

	record("mcp")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	var writeMu sync.Mutex
	writeJSON := func(v any) {
		b, _ := json.Marshal(v)
		b = append(b, '\n')
		writeMu.Lock()
		_, _ = os.Stdout.Write(b)
		writeMu.Unlock()
	}

	for scanner.Scan() {
		var req map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		method, _ := req["method"].(string)
		id, hasID := req["id"]
		if !hasID {
			continue
		}
		switch method {
		case "initialize":
			writeJSON(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"protocolVersion": "2025-11-25",
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]any{"name": "devmap", "version": "0.1.0"},
				},
			})
		case "tools/call":
			params, _ := req["params"].(map[string]any)
			toolName, _ := params["name"].(string)
			toolArgs, _ := params["arguments"].(map[string]any)
			callRoot, _ := toolArgs["repo_path"].(string)
			if callRoot == "" {
				callRoot = repoRoot
			}
			repoAttr := map[string]any{
				"root":          callRoot,
				"store":         filepath.Join(callRoot, ".devcouncil", "codeintel", "devmap.sqlite"),
				"resolved_from": "repo_path",
			}
			var structured map[string]any
			switch toolName {
			case "devmap_status":
				structured = map[string]any{
					"generation_id":     7,
					"node_count":        50,
					"edge_count":        90,
					"pending_count":     0,
					"quarantined_count": 0,
					"is_fresh":          true,
					"degraded_reason":   nil,
					"coverage_gaps":     map[string]any{},
					"repository":        repoAttr,
				}
			case "devmap_dependencies":
				target, _ := toolArgs["target"].(string)
				structured = map[string]any{
					"items": []map[string]any{
						{"source_file": target, "target_file": "dep-of-" + target},
					},
					"hidden":     0,
					"repository": repoAttr,
				}
			case "devmap_search":
				structured = map[string]any{
					"items": []map[string]any{
						{"file_path": "a.go", "symbol_name": "A", "kind": "func"},
					},
					"hidden":     0,
					"repository": repoAttr,
				}
			case "devmap_dead_symbols":
				structured = map[string]any{
					"items":      []map[string]any{},
					"hidden":     0,
					"repository": repoAttr,
				}
			default:
				structured = map[string]any{"repository": repoAttr}
			}
			rawText, _ := json.Marshal(structured)
			writeJSON(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"content":           []map[string]any{{"type": "text", "text": string(rawText)}},
					"structuredContent": structured,
				},
			})
		}
	}
	os.Exit(0)
}

func writeWarmHelperScript(t *testing.T, root string) (string, func() []string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "helper.log")
	wrapper := filepath.Join(dir, "devmap-helper")
	script := fmt.Sprintf("#!/bin/sh\nexport GO_WANT_WORKSPACE_WARM_HELPER=1\nexport WORKSPACE_HELPER_LOG=%q\nexport WORKSPACE_HELPER_ROOT=%q\nexec %q -test.run=^TestWorkspaceWarmMCPHelperProcess$ -- \"$@\"\n",
		logPath, root, exe)
	// The wrapper is exec'd by the code under test, so it needs the execute bit
	// and no mode at or below 0600 can carry one.
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil { //nolint:gosec // G306: a script the test then execs must be executable
		t.Fatal(err)
	}
	return wrapper, func() []string {
		raw, err := os.ReadFile(filepath.Clean(logPath))
		if err != nil {
			return nil
		}
		var out []string
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if strings.TrimSpace(line) != "" {
				out = append(out, line)
			}
		}
		return out
	}
}

func TestGraphContext25FilesUsesSingleWarmMCPProcess(t *testing.T) {
	rawDir := t.TempDir()
	root, err := filepath.EvalSymlinks(rawDir)
	if err != nil {
		t.Fatal(err)
	}
	wrapper, readLog := writeWarmHelperScript(t, root)
	mc := devmap.New(wrapper, root)
	mc.EnableWarmMCP()
	t.Cleanup(func() { _ = mc.Close() })

	reg := &Registry{deps: Deps{Root: root, Map: mc}, session: &Session{}}
	in := files(25)
	res := reg.graphContext(context.Background(), callOf(t, map[string][]string{"files": in}))
	if res.IsError {
		t.Fatalf("warm graph_context failed: %s", res.Text)
	}
	var answer contextAnswer
	if err := json.Unmarshal([]byte(res.Text), &answer); err != nil {
		t.Fatal(err)
	}
	if len(answer.Files) != len(in) {
		t.Fatalf("got %d files, want %d", len(answer.Files), len(in))
	}
	for i, f := range answer.Files {
		if f.Path != in[i] {
			t.Fatalf("slot %d = %s, want %s", i, f.Path, in[i])
		}
		if len(f.DependsOn) != 1 || f.DependsOn[0] != "dep-of-"+in[i] {
			t.Fatalf("slot %d deps = %+v", i, f)
		}
	}

	if qRes := reg.graphQuery(context.Background(), callOf(t, map[string]string{"query": "A"})); qRes.IsError {
		t.Fatalf("warm graph_query failed: %s", qRes.Text)
	}
	if dRes := reg.codeDead(context.Background(), callOf(t, map[string]bool{})); dRes.IsError {
		t.Fatalf("warm code_dead failed: %s", dRes.Text)
	}

	entries := readLog()
	mcpSpawns, cliSpawns := 0, 0
	for _, e := range entries {
		if e == "mcp" {
			mcpSpawns++
		}
		if strings.HasPrefix(e, "cli:") {
			cliSpawns++
		}
	}
	if mcpSpawns != 1 || cliSpawns != 0 {
		t.Fatalf("warm client spawned mcp=%d cli=%d (entries=%v); want exactly 1 warm mcp process and 0 cold CLI fallbacks",
			mcpSpawns, cliSpawns, entries)
	}
	if err := mc.Close(); err != nil {
		t.Fatalf("Close warm client: %v", err)
	}
}

func initCommittedGitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	raw := t.TempDir()
	root, err := filepath.EvalSymlinks(raw)
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // G204: literal git; args are this test's own fixture commands
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-b", "main")
	for rel, body := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	git("commit", "-m", "initial")
	return root
}

func TestGitWorktreeCoordinatorParallelIsolationRetentionAndCleanup(t *testing.T) {
	// Intentionally omit .gitignore so `.devcouncil/codeintel/stack_lease.json`
	// created by `devmap seed --mode stacked` shows up as `??` in git status:
	// clean read-only sub-agent worktrees must still be reclaimed, not leaked.
	root := initCommittedGitRepo(t, map[string]string{
		"shared.txt": "parent baseline\n",
		"pkg/a.go":   "package pkg\n",
	})
	parentCI := filepath.Join(root, ".devcouncil", "codeintel")
	if err := os.MkdirAll(parentCI, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parentCI, "devmap.sqlite"), []byte("sqlite-stub"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrapper, readLog := writeWarmHelperScript(t, root)
	baseDir := t.TempDir()
	coord := &GitWorktreeCoordinator{
		Root:         root,
		BaseDir:      baseDir,
		DevmapBinary: wrapper,
	}

	const lanes = 8
	leases := make([]*WorkspaceLease, lanes)
	errs := make([]error, lanes)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range lanes {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			leases[idx], errs[idx] = coord.ProvisionWorktree(context.Background(), fmt.Sprintf("lane-%d", idx))
		}(i)
	}
	close(start)
	wg.Wait()

	for i := range lanes {
		if errs[i] != nil {
			t.Fatalf("lane %d ProvisionWorktree failed: %v", i, errs[i])
		}
		lease := leases[i]
		if lease == nil || lease.Path == "" || !lease.DevmapSeeded {
			t.Fatalf("lane %d lease = %+v, want DevmapSeeded=true", i, lease)
		}
		info, err := os.Stat(lease.ContainerDir)
		if err != nil {
			t.Fatalf("lane %d ContainerDir stat: %v", i, err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Fatalf("lane %d ContainerDir perm = %o, want 0700", i, perm)
		}
		if _, err := os.Stat(filepath.Join(lease.Path, ".devcouncil", "codeintel", "stack_lease.json")); err != nil {
			t.Fatalf("lane %d missing stack_lease.json: %v", i, err)
		}
	}

	seedCalls := 0
	for _, entry := range readLog() {
		if strings.HasPrefix(entry, "seed:seed --stacked --from "+root) {
			seedCalls++
		}
	}
	if seedCalls != lanes {
		t.Fatalf("recorded %d stacked seed calls, want %d (%v)", seedCalls, lanes, readLog())
	}

	// Mutate lanes 0..3 in four distinct ways; leave lanes 4..7 clean.
	// Lane 0: tracked file edit.
	if err := os.WriteFile(filepath.Join(leases[0].Path, "shared.txt"), []byte("lane 0 edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Lane 1: untracked user file outside .devcouncil/.devmap.
	if err := os.WriteFile(filepath.Join(leases[1].Path, ".devcouncil_user_note.txt"), []byte("user note\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Lane 2: committed change on detached HEAD (working tree clean, HEAD moved).
	commitCmd := exec.CommandContext(t.Context(), "git", "-c", "user.name=Test", "-c", "user.email=test@example.com",
		"commit", "--allow-empty", "-m", "lane 2 commit")
	commitCmd.Dir = leases[2].Path
	if out, err := commitCmd.CombinedOutput(); err != nil {
		t.Fatalf("lane 2 git commit: %v\n%s", err, out)
	}
	// Lane 3: corrupt .git file so cleanliness check cannot prove clean -> must fail closed and retain!
	if err := os.WriteFile(filepath.Join(leases[3].Path, ".git"), []byte("gitdir: /nonexistent/corrupt/gitdir\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := range 4 {
			_, _ = coord.ReleaseWorktree(context.Background(), leases[i], false)
		}
	})

	retained := make([]bool, lanes)
	relErrs := make([]error, lanes)
	for i := range lanes {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			retained[idx], relErrs[idx] = coord.ReleaseWorktree(context.Background(), leases[idx], true)
		}(i)
	}
	wg.Wait()

	for i := range 3 {
		if relErrs[i] != nil || !retained[i] {
			t.Fatalf("dirty lane %d: retained=%v err=%v, want retained=true err=nil", i, retained[i], relErrs[i])
		}
		if _, err := os.Stat(leases[i].Path); err != nil {
			t.Fatalf("dirty lane %d worktree was deleted: %v", i, err)
		}
	}
	if !retained[3] || relErrs[3] == nil {
		t.Fatalf("corrupted lane 3 must fail closed (retained=true, non-nil err), got retained=%v err=%v", retained[3], relErrs[3])
	}
	if _, err := os.Stat(leases[3].Path); err != nil {
		t.Fatalf("corrupted lane 3 worktree was deleted despite fail-closed retention: %v", err)
	}
	for i := 4; i < lanes; i++ {
		if relErrs[i] != nil || retained[i] {
			t.Fatalf("clean lane %d: retained=%v err=%v, want retained=false err=nil", i, retained[i], relErrs[i])
		}
		if _, err := os.Stat(leases[i].ContainerDir); !os.IsNotExist(err) {
			t.Fatalf("clean lane %d ContainerDir still exists on disk: %v", i, err)
		}
	}
}

func findStackedDevmapBinary(t *testing.T) string {
	t.Helper()
	manviRoot := repoRoot(t)
	candidates := []string{
		filepath.Join(manviRoot, "..", "DevCouncil", ".gitpulse", "worktrees", "zero-copy-worktrees-fix", "rust", "target", "debug", "devmap"),
		filepath.Join(manviRoot, "..", "DevCouncil", "rust", "target", "debug", "devmap"),
		filepath.Join(manviRoot, "..", "DevCouncil", "rust", "target", "release", "devmap"),
	}
	if p, err := exec.LookPath("devmap"); err == nil {
		candidates = append(candidates, p)
	}
	for _, c := range candidates {
		info, err := os.Stat(c)
		if err != nil || info.IsDir() {
			continue
		}
		out, err := exec.CommandContext(t.Context(), c, "seed", "--help").CombinedOutput() //nolint:gosec // G204: c is a devmap candidate the test locates on this host, probed with a fixed help flag
		if err == nil && strings.Contains(string(out), "--stacked") {
			return c
		}
	}
	t.Skip("no devmap binary with `seed --stacked` support available on host")
	return ""
}

// devmapCommand builds a command for the devmap binary findStackedDevmapBinary
// located; the binary is found on this host and the arguments are the test's own.
func devmapCommand(t *testing.T, bin string, args ...string) *exec.Cmd {
	t.Helper()
	return exec.CommandContext(t.Context(), bin, args...) //nolint:gosec // G204: bin was probed by findStackedDevmapBinary, args are fixed by the caller
}

func TestGitWorktreeCoordinatorRealDevmapBinaryStackedSeed(t *testing.T) {
	devmapBin := findStackedDevmapBinary(t)
	root := initCommittedGitRepo(t, map[string]string{
		"pkg/calc.go": "package pkg\n\nfunc ComputeAnswer() int { return 42 }\n",
	})
	t.Cleanup(func() {
		// `devmap seed --stacked` seals shared base snapshot directories (`0555`)
		// and files (`0444`) under `.git/devmap/bases` so SQLite readers cannot
		// mutate or open WAL sidecars in place. Restore owner write bits before
		// t.TempDir()'s os.RemoveAll runs.
		basesDir := filepath.Join(root, ".git", "devmap", "bases")
		// os.Root keeps every chmod inside basesDir even if a path in the walk
		// is swapped for a symlink while it runs.
		bases, err := os.OpenRoot(basesDir)
		if err != nil {
			return
		}
		defer func() { _ = bases.Close() }()
		_ = filepath.WalkDir(basesDir, func(p string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, relErr := filepath.Rel(basesDir, p)
			if relErr != nil {
				return relErr
			}
			if d.IsDir() {
				_ = bases.Chmod(rel, 0o700)
			} else {
				_ = bases.Chmod(rel, 0o600)
			}
			return nil
		})
	})
	buildCmd := devmapCommand(t, devmapBin, "--root", root, "build", "--manifest", "--json")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("devmap build on fixture root failed: %v\n%s", err, out)
	}

	coord := &GitWorktreeCoordinator{
		Root:         root,
		BaseDir:      t.TempDir(),
		DevmapBinary: devmapBin,
	}
	lease, err := coord.ProvisionWorktree(context.Background(), "real-stacked")
	if err != nil {
		t.Fatalf("ProvisionWorktree failed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = coord.ReleaseWorktree(context.Background(), lease, false)
	})
	if !lease.DevmapSeeded {
		t.Fatalf("real devmap binary stacked seed failed (DevmapSeeded=false): %s", lease.DevmapNote)
	}
	foundLease := false
	for _, stateDir := range []string{".devmap", ".devcouncil"} {
		if _, err := os.Stat(filepath.Join(lease.Path, stateDir, "codeintel", "stack_lease.json")); err == nil {
			foundLease = true
			break
		}
	}
	if !foundLease {
		t.Fatalf("real stacked worktree missing stack_lease.json under .devmap/.devcouncil")
	}
	statusCmd := devmapCommand(t, devmapBin, "--root", lease.Path, "status", "--json")
	statusOut, err := statusCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("devmap status on seeded worktree failed: %v\n%s", err, statusOut)
	}
	if !strings.Contains(string(statusOut), `"is_fresh":true`) {
		t.Fatalf("seeded worktree status expected is_fresh:true, got: %s", statusOut)
	}
}

func TestSubagentWorkspaceSchemaAndValidation(t *testing.T) {
	runner := &recordingRunner{}
	f, _ := fixtureWithRoles(t, runner, agents.Definition{
		Name:             "worker",
		Role:             "Worker",
		EnableWriteTools: true,
	})
	root := initCommittedGitRepo(t, map[string]string{"a.txt": "x\n"})
	f.reg.deps.Workspaces = &GitWorktreeCoordinator{Root: root, BaseDir: t.TempDir()}
	f.pipe = f.pipe.CloneEmpty()
	if err := f.reg.Register(f.pipe); err != nil {
		t.Fatal(err)
	}

	for _, toolName := range []string{"devcouncil_invoke_subagent", "devcouncil_spawn_subagents"} {
		raw := schemaFor(t, f, toolName)
		if !strings.Contains(raw, `"workspace"`) || !strings.Contains(raw, `"worktree"`) {
			t.Fatalf("%s did not advertise workspace enum when coordinator is attached: %s", toolName, raw)
		}
	}

	// Unsupported modes fail closed before any child executes, even in mixed batches.
	for _, badMode := range []string{"scratch", "branch", "container"} {
		res := f.call("devcouncil_invoke_subagent", map[string]any{
			"subagents": []map[string]any{
				{"type_name": "worker", "role": "ok", "prompt": "work", "workspace": "inherit"},
				{"type_name": "worker", "role": "bad", "prompt": "work", "workspace": badMode},
			},
		})
		if !res.IsError || !strings.Contains(res.Text, badMode) {
			t.Fatalf("invoke_subagent accepted unsupported mode %q: %s", badMode, res.Text)
		}
		sRes := f.call("devcouncil_spawn_subagents", map[string]any{
			"tasks": []map[string]any{
				{"label": "ok", "prompt": "work", "workspace": "inherit"},
				{"label": "bad", "prompt": "work", "workspace": badMode},
			},
		})
		if !sRes.IsError || !strings.Contains(sRes.Text, badMode) {
			t.Fatalf("spawn_subagents accepted unsupported mode %q: %s", badMode, sRes.Text)
		}
	}
	if len(runner.seen) != 0 {
		t.Fatalf("refused workspace calls still ran %d child(ren)", len(runner.seen))
	}
}
