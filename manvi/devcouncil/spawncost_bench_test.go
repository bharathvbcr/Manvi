package devcouncil

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/dcgrep"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/fnmatch"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/testsupport"
)

// These benchmarks measure the subprocess cost of the tool surface. Run them
// with
//
//	go test ./devcouncil -run '^$' -bench 'Baseline|TurnPhases|GitDiffUntrackedScaling|ReadClassify|SecretPatternMatch|GrepSearch|FindFiles$' -benchtime=20x -count=5
//
// and take the minimum of the count, not the mean: a loaded machine only ever
// adds time. Each benchmark that compares an old path with a new one runs them
// interleaved inside one loop, so drift in machine load lands on both.

// benchRepo builds a repository with tracked committed files, then untracked
// and gitignored content on top, because a live worktree has all three and the
// cost of the baseline and the diff depends on every one of them.
func benchRepo(tb testing.TB, tracked, untracked, ignored int) string {
	tb.Helper()
	root := tb.TempDir()
	git := func(args ...string) {
		tb.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			testsupport.Unavailable(tb, "git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "b@test")
	git("config", "user.name", "b")
	git("config", "gc.auto", "0")
	git("config", "maintenance.auto", "false")
	put := func(rel, body string) {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			tb.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			tb.Fatal(err)
		}
	}
	put(".gitignore", "ignored/\n")
	for i := 0; i < tracked; i++ {
		put(fmt.Sprintf("pkg%d/file%d.go", i%40, i), fmt.Sprintf("package p\n\nfunc F%d() int { return %d }\n", i, i))
	}
	git("add", "-A")
	git("commit", "-qm", "seed")
	for i := 0; i < untracked; i++ {
		put(fmt.Sprintf("new/u%d.go", i), fmt.Sprintf("package n\n\nfunc U%d() {}\n", i))
	}
	for i := 0; i < ignored; i++ {
		put(fmt.Sprintf("ignored/i%d.bin", i), strings.Repeat("x", 512))
	}
	return root
}

func benchRegistry(root string) *Registry {
	return &Registry{deps: Deps{Root: root}, session: &Session{}}
}

// BenchmarkCaptureBaseline compares the shipped baseline against the cold
// read-tree variant it replaced, on a 1,200-file tree with untracked and
// ignored content. The two sub-results are taken in one interleaved loop.
func BenchmarkCaptureBaseline(b *testing.B) {
	root := benchRepo(b, 1200, 40, 200)
	r := benchRegistry(root)
	ctx := context.Background()
	var cold, seeded int64
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		t0 := nowNano()
		if _, err := r.worktreeTree(ctx, false); err != nil {
			b.Fatal(err)
		}
		t1 := nowNano()
		if _, err := r.writeWorktreeTree(ctx); err != nil {
			b.Fatal(err)
		}
		t2 := nowNano()
		if cold == 0 || t1-t0 < cold {
			cold = t1 - t0
		}
		if seeded == 0 || t2-t1 < seeded {
			seeded = t2 - t1
		}
	}
	b.ReportMetric(float64(cold)/1e6, "cold-min-ms")
	b.ReportMetric(float64(seeded)/1e6, "shipped-min-ms")
}

// turnEdits applies what one agent turn typically does to the tree after the
// baseline was taken — three edits, two new files, one deletion — and returns
// the paths a check would examine.
func turnEdits(tb testing.TB, root string) []string {
	tb.Helper()
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o600); err != nil {
			tb.Fatal(err)
		}
	}
	write("pkg1/file1.go", "package p\n\nfunc F1() int { return 100 }\n")
	write("pkg2/file2.go", "package p\n\nfunc F2() int { return 200 }\n")
	write("pkg3/file3.go", "package p\n\nfunc F3() int { return 300 }\n")
	write("new/turn_a.go", "package n\n\nfunc TurnA() {}\n")
	write("new/turn_b.go", "package n\n\nfunc TurnB() {}\n")
	if err := os.Remove(filepath.Join(root, "pkg4/file4.go")); err != nil {
		tb.Fatal(err)
	}
	return []string{
		"pkg1/file1.go", "pkg2/file2.go", "pkg3/file3.go",
		"new/turn_a.go", "new/turn_b.go", "pkg4/file4.go",
	}
}

// BenchmarkTurnPhases measures the two phases of a turn that run in the
// harness rather than in the model: the pre-turn baseline capture and the
// end-of-turn scoped diff taken against it. The fixture is a 1,200-file tree
// with untracked and ignored content, and the diff runs after a turn's worth of
// edits, so both phases see the working set a real turn leaves.
//
// The two phases alternate inside one loop and the minimum of each is
// reported, so drift in machine load lands on both. Spawn counts are the
// number of git children each phase starts.
func BenchmarkTurnPhases(b *testing.B) {
	root := benchRepo(b, 1200, 40, 200)
	r := benchRegistry(root)
	ctx := context.Background()
	before := r.CaptureBaseline(ctx)
	if !before.Captured() {
		b.Fatalf("no baseline: %s", before.Note)
	}
	paths := turnEdits(b, root)

	var capture, scoped, captureSpawns, scopedSpawns int64
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s0, t0 := procSpawns.Load(), nowNano()
		if got := r.CaptureBaseline(ctx); !got.Captured() {
			b.Fatalf("no baseline: %s", got.Note)
		}
		s1, t1 := procSpawns.Load(), nowNano()
		diff, notes, err := r.scopedDiff(ctx, paths, before)
		if err != nil || len(notes) != 0 || !strings.Contains(diff, "turn_a.go") || !strings.Contains(diff, "file4.go") {
			b.Fatalf("scoped diff: err=%v notes=%v\n%s", err, notes, diff)
		}
		s2, t2 := procSpawns.Load(), nowNano()
		if capture == 0 || t1-t0 < capture {
			capture = t1 - t0
		}
		if scoped == 0 || t2-t1 < scoped {
			scoped = t2 - t1
		}
		captureSpawns, scopedSpawns = s1-s0, s2-s1
	}
	b.ReportMetric(float64(capture)/1e6, "baseline-min-ms")
	b.ReportMetric(float64(scoped)/1e6, "scoped-diff-min-ms")
	b.ReportMetric(float64(captureSpawns), "baseline-spawns")
	b.ReportMetric(float64(scopedSpawns), "scoped-diff-spawns")
}

// BenchmarkGitDiffUntrackedScaling renders N untracked files by the batch and
// by the per-file route it replaced (eight git processes at a time), in one
// interleaved loop, and reports the minimum of each with its spawn count.
func BenchmarkGitDiffUntrackedScaling(b *testing.B) {
	for _, n := range []int{1, 8, 16, 24, 64, 256} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			root := benchRepo(b, 50, n, 0)
			paths := untrackedPaths2(b, root)
			ctx := context.Background()
			var perFile, batch int64
			var perFileSpawns, batchSpawns int64
			for i := 0; i < b.N; i++ {
				s0 := procSpawns.Load()
				t0 := nowNano()
				renderUntrackedPerFile(ctx, root, paths, maxGitCaptureBytes)
				t1 := nowNano()
				s1 := procSpawns.Load()
				renderUntracked(ctx, root, paths, maxGitCaptureBytes)
				t2 := nowNano()
				s2 := procSpawns.Load()
				if perFile == 0 || t1-t0 < perFile {
					perFile = t1 - t0
				}
				if batch == 0 || t2-t1 < batch {
					batch = t2 - t1
				}
				perFileSpawns, batchSpawns = s1-s0, s2-s1
			}
			b.ReportMetric(float64(perFile)/1e6, "per-file-min-ms")
			b.ReportMetric(float64(batch)/1e6, "batch-min-ms")
			b.ReportMetric(float64(perFileSpawns), "per-file-spawns")
			b.ReportMetric(float64(batchSpawns), "batch-spawns")
		})
	}
}

func untrackedPaths2(b *testing.B, root string) []string {
	b.Helper()
	out, err := runGit(context.Background(), root, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		b.Fatal(err)
	}
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

// BenchmarkReadClassify is the secret-path verdict over 10,000 paths, the work
// grep does once per matching file.
func BenchmarkReadClassify(b *testing.B) {
	root := b.TempDir()
	paths := make([]string, 10000)
	for i := range paths {
		paths[i] = fmt.Sprintf("pkg%d/sub%d/file%d.go", i%97, i%13, i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, p := range paths {
			if policy.ClassifyRead(root, p) != policy.ReadAllowed {
				b.Fatalf("%s refused", p)
			}
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(len(paths)), "ns/path")
}

// BenchmarkSecretPatternMatch isolates the pattern fold from the filesystem
// work in BenchmarkReadClassify: the same 10,000 paths through
// fnmatch.MatchAnyFold alone. Its ns/path against ClassifyRead's is the share
// of a verdict that case-folding the patterns costs.
func BenchmarkSecretPatternMatch(b *testing.B) {
	paths := make([]string, 10000)
	for i := range paths {
		paths[i] = fmt.Sprintf("pkg%d/sub%d/file%d.go", i%97, i%13, i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, p := range paths {
			if fnmatch.MatchAnyFold(policy.SecretPathPatterns, p) {
				b.Fatalf("%s matched", p)
			}
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(len(paths)), "ns/path")
}

func benchGrep(b *testing.B) (*dcgrep.Client, string) {
	root := benchRepo(b, 3000, 0, 0)
	return dcgrep.New(testsupport.DCGrep(b), root), root
}

// BenchmarkGrepSearch is one dcgrep Search over a 3,000-file tree, one child
// process per call, with and without the candidate index built.
func BenchmarkGrepSearch(b *testing.B) {
	for _, indexed := range []bool{false, true} {
		name := "live-walk"
		if indexed {
			name = "indexed"
		}
		b.Run(name, func(b *testing.B) {
			c, _ := benchGrep(b)
			ctx := context.Background()
			if indexed {
				if res, err := c.Index(ctx, dcgrep.IndexRequest{}); err != nil || !res.OK {
					b.Fatalf("index: %v %+v", err, res)
				}
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				res, err := c.Search(ctx, dcgrep.Request{Pattern: "func F2999"})
				if err != nil || res.Count != 1 {
					b.Fatalf("search: %v %+v", err, res)
				}
			}
		})
	}
}

// BenchmarkFindFiles is one dcgrep List over the same tree.
func BenchmarkFindFiles(b *testing.B) {
	c, _ := benchGrep(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.List(ctx, dcgrep.ListRequest{}); err != nil {
			b.Fatal(err)
		}
	}
}

func nowNano() int64 { return time.Now().UnixNano() }

// BenchmarkAuthorisingTask is one gated operation's lease check: Store.Valid
// and then Store.Task, two round trips through the pooled dcstore child. It
// is the cost an in-process cache of the task record would save, and the
// reason there is none: the record carries the scope the gate enforces, and a
// copy served after another process changed that scope would enforce the old
// one.
func BenchmarkAuthorisingTask(b *testing.B) {
	f := newFixture(b)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, res := f.reg.authorisingTask(ctx, "writing"); res != nil {
			b.Fatal(res.Text)
		}
	}
}
