package devcouncil

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// Adversarial tests for b28-refguard: newCommits over rev-list --stdin and
// runGitStdin.

// advFastImport feeds a fast-import stream to the repository at root.
func advFastImport(t *testing.T, root, stream string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "-C", root, "fast-import", "--quiet") // #nosec G204 -- the binary is the literal "git"; the arguments are this test's own.
	cmd.Stdin = strings.NewReader(stream)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fast-import: %v\n%s", err, out)
	}
}

type advGraph struct {
	root                                string
	c1, c2, c3, s1, merge               string
	annTag, tree, blob, unknown, chain0 string
}

// advBuildGraph makes c1 <- c2 <- c3, c2 <- s1, merge(c3, s1), an annotated tag
// of c1, and returns the ids the equivalence cases use.
func advBuildGraph(t *testing.T, f *fixture) advGraph {
	t.Helper()
	var in strings.Builder
	commit := func(ref string, mark int, from, merge int) {
		fmt.Fprintf(&in, "commit %s\nmark :%d\ncommitter t <t@example.com> %d +0000\ndata 3\nc%d\n", ref, mark, 1700000000+mark, mark)
		if from > 0 {
			fmt.Fprintf(&in, "from :%d\n", from)
		}
		if merge > 0 {
			fmt.Fprintf(&in, "merge :%d\n", merge)
		}
		fmt.Fprintf(&in, "M 644 inline f%d\ndata 2\n%d\n\n", mark, mark)
	}
	commit("refs/heads/adv/main", 1, 0, 0)
	commit("refs/heads/adv/main", 2, 1, 0)
	commit("refs/heads/adv/main", 3, 2, 0)
	commit("refs/heads/adv/side", 4, 2, 0)
	commit("refs/heads/adv/main", 5, 3, 4)
	in.WriteString("tag advtag\nfrom :1\ntagger t <t@example.com> 1700000100 +0000\ndata 2\nt\n\n")
	advFastImport(t, f.root, in.String())
	g := advGraph{root: f.root}
	g.c1 = gitOut(t, f.root, "rev-parse", "refs/heads/adv/main~3^{commit}")
	g.c2 = gitOut(t, f.root, "rev-parse", "refs/heads/adv/main~2")
	g.c3 = gitOut(t, f.root, "rev-parse", "refs/heads/adv/main^1")
	g.s1 = gitOut(t, f.root, "rev-parse", "refs/heads/adv/side")
	g.merge = gitOut(t, f.root, "rev-parse", "refs/heads/adv/main")
	g.annTag = gitOut(t, f.root, "rev-parse", "refs/tags/advtag")
	g.tree = gitOut(t, f.root, "rev-parse", "refs/heads/adv/main^{tree}")
	g.blob = gitOut(t, f.root, "rev-parse", "refs/heads/adv/main:f5")
	g.unknown = "1234567890123456789012345678901234567890"
	return g
}

// advOldNewCommits is newCommits as it was before the change: the excluded ids
// as arguments.
func advOldNewCommits(ctx context.Context, r *Registry, oid string, recorded []string) ([]string, error) {
	if _, err := runGit(ctx, r.deps.Root, "rev-parse", "--verify", "--quiet", oid+"^{commit}"); err != nil {
		kind, kerr := runGit(ctx, r.deps.Root, "cat-file", "-t", oid)
		if kerr != nil {
			return nil, kerr
		}
		if strings.TrimSpace(kind) == "commit" {
			return nil, err
		}
		if peeled, perr := runGit(ctx, r.deps.Root, "rev-parse", "--verify", "--quiet", oid+"^{}"); perr == nil {
			if t, terr := runGit(ctx, r.deps.Root, "cat-file", "-t", strings.TrimSpace(peeled)); terr == nil && strings.TrimSpace(t) == "commit" {
				return nil, err
			}
		}
		return nil, nil
	}
	args := []string{"rev-list", fmt.Sprintf("--max-count=%d", maxGuardCommits+1), oid + "^{commit}"}
	if len(recorded) > 0 {
		args = append(args, "--not")
		args = append(args, recorded...)
	}
	out, err := runGit(ctx, r.deps.Root, args...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

func TestAdvB28NewCommitsStdinMatchesArgv(t *testing.T) {
	f := shellCommitFixture(t, false)
	g := advBuildGraph(t, f)
	cases := []struct {
		name     string
		oid      string
		recorded []string
	}{
		{"merge excluding side", g.merge, []string{g.s1}},
		{"merge excluding first parent", g.merge, []string{g.c3}},
		{"merge excluding both parents", g.merge, []string{g.c3, g.s1}},
		{"oid recorded twice", g.merge, []string{g.s1, g.s1, g.c3, g.s1}},
		{"empty recorded list", g.merge, nil},
		{"recorded list holds the tip", g.merge, []string{g.merge}},
		{"tip and others", g.merge, []string{g.c1, g.merge, g.s1}},
		{"recorded holds a tree", g.merge, []string{g.tree}},
		{"recorded holds a blob", g.merge, []string{g.blob}},
		{"recorded holds an annotated tag", g.merge, []string{g.annTag}},
		{"recorded holds an unknown oid", g.merge, []string{g.unknown}},
		{"unknown oid among valid ones", g.merge, []string{g.s1, g.unknown, g.c3}},
		{"oid is an annotated tag of a commit", g.annTag, []string{g.s1}},
		{"oid is a tree", g.tree, []string{g.s1}},
		{"oid is a blob", g.blob, nil},
		{"oid is unknown", g.unknown, []string{g.s1}},
		{"oid is the root commit", g.c1, []string{g.s1}},
		{"upper-case recorded oid", g.merge, []string{strings.ToUpper(g.s1)}},
		{"abbreviated recorded oid", g.merge, []string{g.s1[:12]}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantC, wantErr := advOldNewCommits(t.Context(), f.reg, c.oid, c.recorded)
			gotC, gotErr := f.reg.newCommits(t.Context(), c.oid, c.recorded)
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("error mismatch: argv form err=%v, stdin form err=%v (argv=%v stdin=%v)", wantErr, gotErr, wantC, gotC)
			}
			if !slices.Equal(wantC, gotC) {
				t.Fatalf("argv form %v, stdin form %v", wantC, gotC)
			}
		})
	}
}

func TestAdvB28NewCommitsUnknownRecordedOIDRefuses(t *testing.T) {
	f := shellCommitFixture(t, false)
	g := advBuildGraph(t, f)
	got, err := f.reg.newCommits(t.Context(), g.merge, []string{g.s1, g.unknown})
	if err == nil {
		t.Fatalf("an unresolvable excluded oid was ignored: newCommits = %v, want an error", got)
	}
}

// rev-list --stdin ends its input at the first empty line, so one empty
// recorded entry would silently drop every exclusion after it, and the guard
// would over-report new commits. The argv form refused ("ambiguous argument").
func TestAdvB28NewCommitsEmptyRecordedEntryRefusesLikeArgv(t *testing.T) {
	f := shellCommitFixture(t, false)
	g := advBuildGraph(t, f)
	recorded := []string{g.c3, "", g.s1}
	_, wantErr := advOldNewCommits(t.Context(), f.reg, g.merge, recorded)
	got, gotErr := f.reg.newCommits(t.Context(), g.merge, recorded)
	if wantErr == nil {
		t.Fatalf("expected the argv form to refuse an empty oid")
	}
	if gotErr == nil {
		t.Fatalf("newCommits accepted an empty recorded oid and returned %v (argv form refused: %v)", got, wantErr)
	}
}

// The same class: an entry that carries a newline injects a further stdin line.
func TestAdvB28NewCommitsRecordedEntryWithNewlineRefuses(t *testing.T) {
	f := shellCommitFixture(t, false)
	g := advBuildGraph(t, f)
	recorded := []string{g.c3 + "\n\n" + g.s1}
	_, wantErr := advOldNewCommits(t.Context(), f.reg, g.merge, recorded)
	got, gotErr := f.reg.newCommits(t.Context(), g.merge, recorded)
	if wantErr == nil {
		t.Fatalf("expected the argv form to refuse an entry with a newline")
	}
	if gotErr == nil {
		t.Fatalf("newCommits accepted a recorded entry with a newline and returned %v (argv form refused: %v)", got, wantErr)
	}
}

func TestAdvB28NewCommitsMaxCountMatchesArgv(t *testing.T) {
	f := shellCommitFixture(t, false)
	var in strings.Builder
	for i := 1; i <= maxGuardCommits+50; i++ {
		fmt.Fprintf(&in, "commit refs/heads/advchain\nmark :%d\ncommitter t <t@example.com> %d +0000\ndata 1\nx\n", i, 1700000000+i)
		if i > 1 {
			fmt.Fprintf(&in, "from :%d\n", i-1)
		}
		in.WriteString("\n")
	}
	advFastImport(t, f.root, in.String())
	tip := gitOut(t, f.root, "rev-parse", "refs/heads/advchain")
	want, werr := advOldNewCommits(t.Context(), f.reg, tip, nil)
	got, gerr := f.reg.newCommits(t.Context(), tip, nil)
	if werr != nil || gerr != nil {
		t.Fatalf("errors: %v %v", werr, gerr)
	}
	if len(got) != maxGuardCommits+1 || !slices.Equal(want, got) {
		t.Fatalf("argv form %d commits, stdin form %d commits, want both %d", len(want), len(got), maxGuardCommits+1)
	}
}

func TestAdvB28RunGitStdinLargeInputToAChildThatDoesNotReadIt(t *testing.T) {
	f := shellCommitFixture(t, false)
	in := strings.Repeat("0123456789012345678901234567890123456789\n", 200000) // 8.2 MB, far past the pipe buffer
	done := make(chan error, 1)
	go func() {
		_, err := runGitStdin(t.Context(), f.root, in, "rev-parse", "--git-dir")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a child that exits without reading its stdin failed the call: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("runGitStdin deadlocked: 8 MB of input to a child that never reads it")
	}
}

func TestAdvB28RunGitStdinLargeInputIsFullyDelivered(t *testing.T) {
	f := shellCommitFixture(t, false)
	head := headOf(t, f.root)
	var in strings.Builder
	in.WriteString(head + "\n--not\n")
	for i := 0; i < 200000; i++ {
		in.WriteString(head + "\n")
	}
	out, err := runGitStdin(t.Context(), f.root, in.String(), "rev-list", "--stdin")
	if err != nil {
		t.Fatalf("8 MB of exclusions: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("every commit was excluded, got %q", out)
	}
}

func TestAdvB28RunGitStdinCancelledMidWriteReturns(t *testing.T) {
	f := shellCommitFixture(t, false)
	in := strings.Repeat("0123456789012345678901234567890123456789\n", 200000)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	time.AfterFunc(500*time.Millisecond, cancel)
	type res struct {
		err error
		d   time.Duration
	}
	done := make(chan res, 1)
	go func() {
		start := time.Now()
		// The alias runs a shell command that never reads stdin, so the write
		// of the 8 MB blocks on a full pipe until the context ends it.
		_, err := runGitStdin(ctx, f.root, in, "-c", "alias.advslow=!sleep 120", "advslow")
		done <- res{err, time.Since(start)}
	}()
	select {
	case r := <-done:
		if r.err == nil {
			t.Fatalf("cancelled call returned no error")
		}
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("error does not wrap context.Canceled: %v", r.err)
		}
		if r.d > 15*time.Second {
			t.Fatalf("cancellation took %s", r.d)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("runGitStdin ignored cancellation while writing its input")
	}
}

func TestAdvB28RunGitStdinOutputIsCapped(t *testing.T) {
	f := shellCommitFixture(t, false)
	_, err := runGitStdin(t.Context(), f.root, "x\n", "-c", "alias.advbig=!head -c 20000000 /dev/zero", "advbig")
	if err == nil || !strings.Contains(err.Error(), "output truncated") {
		t.Fatalf("20 MB of output was not refused with the capture note: %v", err)
	}
}

func TestAdvB28RunGitStdinIsCountedBySpawnCounter(t *testing.T) {
	f := shellCommitFixture(t, false)
	start := procSpawns.Load()
	if _, err := runGitStdin(t.Context(), f.root, "", "rev-parse", "--git-dir"); err != nil {
		t.Fatal(err)
	}
	if d := procSpawns.Load() - start; d != 1 {
		t.Fatalf("runGitStdin counted %d spawns, want 1", d)
	}
}

func TestAdvB28NewCommitsSpawnCountIndependentOfRecordedSize(t *testing.T) {
	f := shellCommitFixture(t, false)
	g := advBuildGraph(t, f)
	count := func(n int) int64 {
		rec := slices.Repeat([]string{g.s1}, n)
		start := procSpawns.Load()
		if _, err := f.reg.newCommits(t.Context(), g.merge, rec); err != nil {
			t.Fatal(err)
		}
		return procSpawns.Load() - start
	}
	small, large := count(1), count(5000)
	if small != large {
		t.Fatalf("newCommits starts %d git processes with 1 recorded oid and %d with 5000", small, large)
	}
}

// The guard test's refs all sit on one commit, so a process per distinct object
// id (rather than per ref) leaves its two counts equal. This asks the check to
// catch that regression; it does not.
func TestAdvB28SpawnCheckCatchesAPerDistinctOIDProcess(t *testing.T) {
	f := shellCommitFixture(t, false)
	head := headOf(t, f.root)
	perOID := func(snap *refSnapshot) {
		for _, oid := range snap.oids() {
			if _, err := runGit(t.Context(), f.root, "cat-file", "-t", oid); err != nil {
				t.Fatalf("cat-file %s: %v", oid, err)
			}
		}
	}
	addBulkRefs(t, f.root, head, 0, 10)
	small := guardSpawns(t, f, perOID)
	addBulkRefs(t, f.root, head, 10, 1000)
	large := guardSpawns(t, f, perOID)
	if msg := spawnScaling(small, large, 10, 1000); msg == "" {
		t.Errorf("a guard with a process per distinct oid (%d and %d spawns) passed the scaling check: the fixture puts every ref on one commit", small, large)
	}
}

// The production guard, with refs on distinct commits: its spawn count must not
// grow with the number of distinct oids.
func TestAdvB28GuardSpawnsConstantWithDistinctOIDs(t *testing.T) {
	f := shellCommitFixture(t, false)
	mk := func(from, to int) {
		var in strings.Builder
		for i := from; i < to; i++ {
			fmt.Fprintf(&in, "commit refs/heads/advd/b%04d\ncommitter t <t@example.com> %d +0000\ndata 1\nx\n\n", i, 1700000000+i)
		}
		advFastImport(t, f.root, in.String())
	}
	mk(0, 10)
	small := guardSpawns(t, f, nil)
	mk(10, 300)
	large := guardSpawns(t, f, nil)
	if msg := spawnScaling(small, large, 10, 300); msg != "" {
		t.Error(msg)
	}
}

// N refs that move to one new commit are judged once.
func TestAdvB28MovedRefsToOneCommitSpawnConstant(t *testing.T) {
	spawns := func(n int) int64 {
		f := shellCommitFixture(t, false)
		before, err := f.reg.snapshotRefs(t.Context())
		if err != nil || before == nil {
			t.Fatalf("snapshot: %v", err)
		}
		advFastImport(t, f.root, "commit refs/heads/advnew\ncommitter t <t@example.com> 1700009999 +0000\ndata 1\nx\n\n")
		tip := gitOut(t, f.root, "rev-parse", "refs/heads/advnew")
		addBulkRefs(t, f.root, tip, 0, n)
		start := procSpawns.Load()
		if v, _ := f.reg.verifyRefs(t.Context(), before, "true"); v != nil {
			t.Fatalf("clean moves reported: %+v", v)
		}
		return procSpawns.Load() - start
	}
	small, large := spawns(3), spawns(300)
	if small != large {
		t.Fatalf("verifyRefs starts %d git processes for 3 moved refs and %d for 300 moved to the same commit", small, large)
	}
}

// rev-list --stdin applies --not to the lines after it only, and an empty --not
// section excludes nothing. Asserted through runGitStdin, the call newCommits makes.
func TestAdvB28StdinNotAppliesOnlyToLaterLinesAndEmptyExcludesNothing(t *testing.T) {
	f := shellCommitFixture(t, false)
	g := advBuildGraph(t, f)
	list := func(in string) []string {
		out, err := runGitStdin(t.Context(), f.root, in, "rev-list", "--stdin")
		if err != nil {
			t.Fatalf("rev-list --stdin %q: %v", in, err)
		}
		return strings.Fields(out)
	}
	// c3 positive, s1 negative: c3 and its ancestors not reachable from s1.
	if got := list(g.c3 + "\n--not\n" + g.s1 + "\n"); !slices.Equal(got, []string{g.c3}) {
		t.Errorf("c3 --not s1 = %v, want [c3]", got)
	}
	// The negation is after, so s1 is positive here and c3 is the excluded one.
	if got := list(g.s1 + "\n--not\n" + g.c3 + "\n"); !slices.Equal(got, []string{g.s1}) {
		t.Errorf("s1 --not c3 = %v, want [s1]", got)
	}
	// c2 listed before --not stays positive; only the line after is excluded.
	if got := list(g.c2 + "\n--not\n" + g.c1 + "\n"); !slices.Equal(got, []string{g.c2}) {
		t.Errorf("c2 --not c1 = %v, want [c2]", got)
	}
	with, without := list(g.merge+"\n--not\n"), list(g.merge+"\n")
	if !slices.Equal(with, without) || len(with) == 0 {
		t.Errorf("an empty --not section changed the answer: %v vs %v", with, without)
	}
}
