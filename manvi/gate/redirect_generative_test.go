package gate

import (
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"manvi/dc"
	"manvi/flags"
)

// A generated differential. The hand-written corpus in redirect_test.go covers
// the shapes someone thought of; this covers the ones nobody did.
//
// Every command is assembled from a closed vocabulary — a handful of harmless
// builtins, a fixed set of relative target paths, and the shell's own operators
// — so what varies is the *syntax* the gate has to read, never what the command
// is capable of. That is the whole point: the defects this found were all
// parser-shaped, not capability-shaped. `echo hi > .env` was refused and
// `echo $(echo hi > .env)` was not, and the difference was six characters of
// syntax.
//
// The invariant is the same one the corpus asserts, and it is checked against
// the filesystem rather than against an expectation: if the gate allowed the
// command, every file the command actually wrote must be a file the gate would
// also allow a direct write to.

// safeCommands are the only things a generated line may run. None of them can
// destroy anything, and none of them reads stdin.
var safeCommands = []string{`echo hi`, `printf x`, `true`, `false`, `:`}

// safeTargets are the only paths a generated redirection may name.
//
// The mix is weighted, and the weighting is the difference between a check that
// runs and one that does not. The invariant is conditional — *if* the gate
// allowed the line, its writes must also be allowed — so a corpus made mostly
// of protected paths refuses nearly every line and proves almost nothing. The
// benign entries are here to get lines *through* the gate so the comparison
// against the filesystem actually happens; the protected ones keep the hard
// rules in the traffic. Both counts are reported at the end of the run.
var safeTargets = []string{
	// Benign: the task plans one of these and the rest are ordinary files.
	"src/calc.go", "src/calc.go", "src/other.go", "src/other.go",
	"out.txt", "out.txt", "notes.md", "sub/log.txt", "src/helper.go",
	// Protected, planned-against, or escaping.
	".env", ".env.local", ".git/config", "secrets/k.txt", "sub/.env",
	"id_rsa", "a.pem", "../escaped.txt", "./.env", "src/../.env", ".ENV",
}

var redirectOps = []string{">", ">>", ">|", "2>", "&>", "1>", ">  "}

var chainOps = []string{" && ", " || ", "; ", " | ", "\n", " & "}

// wrappers hide a command inside a construct the shell still executes.
var wrappers = []func(string) string{
	func(s string) string { return s },
	func(s string) string { return "$(" + s + ")" },
	func(s string) string { return "`" + s + "`" },
	func(s string) string { return "( " + s + " )" },
	func(s string) string { return "{ " + s + "; }" },
	func(s string) string { return `"$(` + s + `)"` },
	func(s string) string { return "${x:-$(" + s + ")}" },
	func(s string) string { return "echo $(" + s + ")" },
}

// prefixes are the words sh accepts before a command word.
var prefixes = []string{"", "FOO=1 ", `FOO="a b" `, "2>/dev/null ", "command ", "env "}

// generativeTask allows every command word, on purpose.
//
// The rung under test is the one that judges where a command *writes*, and an
// allowlist that refuses the line first means that rung is never reached — at
// a narrower allowlist only 12 of 250 generated lines got far enough to be
// checked, so the run passed while examining almost nothing. Opening the
// allowlist removes the rung that is not being tested and leaves the hard rules
// and the redirect verdict as the only things that can refuse, which is exactly
// the isolation this check wants.
func generativeTask() *dc.Task {
	return &dc.Task{
		ID:              "TASK-GEN",
		PlannedFiles:    []dc.PlannedFile{{Path: "src/calc.go", AllowedChange: dc.ChangeModify}},
		AllowedCommands: []string{"*"},
	}
}

func generateCommandLine(rng *rand.Rand) string {
	clause := func() string {
		s := safeCommands[rng.Intn(len(safeCommands))]
		if rng.Intn(100) < 75 {
			op := redirectOps[rng.Intn(len(redirectOps))]
			target := safeTargets[rng.Intn(len(safeTargets))]
			switch rng.Intn(4) {
			case 0:
				target = `"` + target + `"`
			case 1:
				target = `'` + target + `'`
			}
			s = s + " " + op + " " + target
		}
		return prefixes[rng.Intn(len(prefixes))] + s
	}

	line := wrappers[rng.Intn(len(wrappers))](clause())
	for n := rng.Intn(3); n > 0; n-- {
		line += chainOps[rng.Intn(len(chainOps))] + wrappers[rng.Intn(len(wrappers))](clause())
	}
	return line
}

// TestGeneratedCommandsNeverOutrunTheirOwnWriteVerdict is the soak.
//
// MANVI_GATE_SOAK sets the case count; the default keeps the ordinary suite
// fast while still sampling the space every run. Each run reseeds from a fixed
// base plus the case index, so a failure names an input that can be replayed.
func TestGeneratedCommandsNeverOutrunTheirOwnWriteVerdict(t *testing.T) {
	cases := 250
	if raw := os.Getenv("MANVI_GATE_SOAK"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			t.Fatalf("MANVI_GATE_SOAK=%q is not a positive count", raw)
		}
		cases = n
	}
	seed := int64(20260824)
	if raw := os.Getenv("MANVI_GATE_SEED"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			t.Fatalf("MANVI_GATE_SEED=%q is not a number", raw)
		}
		seed = n
	}

	postures := []string{flags.PostureStrict, flags.PostureDev}
	allowed, refused, wroteSomething := 0, 0, 0

	for i := 0; i < cases; i++ {
		rng := rand.New(rand.NewSource(seed + int64(i)))
		command := generateCommandLine(rng)
		posture := postures[i%len(postures)]

		g := newGate(t, map[string]string{flags.HarnessPosture: posture})
		decision, err := g.EvaluateCommand(command, generativeTask())
		if err != nil {
			// An error is a refusal at every call site; it is not an allow.
			refused++
			continue
		}
		if decision.Blocked() {
			refused++
			continue
		}
		allowed++

		written := runGenerated(t, command)
		if len(written) > 0 {
			wroteSomething++
		}
		for _, path := range written {
			wg := newGate(t, map[string]string{flags.HarnessPosture: posture})
			w, wErr := wg.EvaluateWrite(path, generativeTask(), dc.OpWrite)
			if wErr != nil {
				t.Fatalf("seed %d posture %s: EvaluateWrite(%q): %v", seed+int64(i), posture, path, wErr)
			}
			if w.Blocked() {
				t.Fatalf("seed %d posture %s\n  command: %q\n  was ALLOWED, but wrote %q,\n  which the write gate refuses (%s: %s)",
					seed+int64(i), posture, command, path, w.Rule, w.Reason)
			}
		}
	}

	// A run in which nothing was ever allowed, or nothing ever wrote a file, is
	// a run that proved nothing. Reported rather than passed over: this check
	// is only as good as the traffic reaching it.
	t.Logf("%d generated command lines: %d allowed, %d refused, %d of the allowed wrote at least one file",
		cases, allowed, refused, wroteSomething)
	if allowed == 0 {
		t.Fatal("no generated command was allowed; the invariant was never exercised")
	}
	if wroteSomething == 0 {
		t.Fatal("no allowed command wrote a file; the filesystem side of the comparison was never exercised")
	}
}

// runGenerated executes one generated line in a throwaway tree and returns the
// paths that appeared or changed, relative to it.
//
// The tree is nested one level below the temp root so a `../` target lands
// somewhere still inside the directory the test owns and still gets observed.
func runGenerated(t *testing.T, command string) []string {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	for _, dir := range []string{"src", "sub", ".git", "secrets"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	before := generatedSnapshot(base, root)

	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = root
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %q: %v", command, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		// A generated line should never block; if one does, killing it and
		// carrying on is right, but it is reported rather than hidden.
		_ = cmd.Process.Kill()
		<-done
		t.Errorf("generated command %q did not finish in 10s", command)
	}

	after := generatedSnapshot(base, root)
	var changed []string
	for path, content := range after {
		if prev, existed := before[path]; !existed || prev != content {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	return changed
}

// generatedSnapshot walks the whole owned tree, not just the repo, so a write
// that escaped the root is seen — and returns it spelled relative to the root,
// which is the spelling the gate is asked about.
func generatedSnapshot(base, root string) map[string]string {
	out := map[string]string{}
	_ = filepath.Walk(base, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		body, readErr := os.ReadFile(p)
		if readErr != nil {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			rel = p
		}
		out[filepath.ToSlash(rel)] = string(body)
		return nil
	})
	return out
}
