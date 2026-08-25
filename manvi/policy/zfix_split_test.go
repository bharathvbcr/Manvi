package policy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"manvi/dc"
)

// These tests close the splitter bypass and give the suite the external oracle
// that found it.
//
// The hole: SplitCommandChain had no case for a backslash in the unquoted
// state, so `echo \'` flipped it INTO a quoted span and swallowed the rest of
// the line. sh reads \' as a literal quote character and stays unquoted, so it
// went on to run whatever followed the next operator. The gate saw one
// allowlisted command; sh ran two.

// TestEscapedQuoteDoesNotSwallowTheRestOfTheLine pins the class, not the one
// reported spelling. Every escape sh honours in the unquoted state — before a
// single quote, a double quote, another backslash, or opening a $'…' span —
// must leave the operator after it visible as a command boundary.
func TestEscapedQuoteDoesNotSwallowTheRestOfTheLine(t *testing.T) {
	gate := CommandGate{GlobalAllowedCommands: []string{"echo *", "echo"}, HardRules: true}
	task := &dc.Task{ID: "TASK-001", AllowedCommands: []string{"echo *"}}

	// Each prefix is a complete, allowlisted `echo` whose text ends in an
	// escape or quote construct. The `; mkdir OWNED` after it is a second
	// command in sh, so the gate must see two parts and refuse.
	prefixes := []string{
		`echo \'`,
		`echo \"`,
		`echo \\\'`,
		`echo \\\"`,
		`echo \\`,
		`echo $'\''`,
		`echo $'a\'b'`,
		`echo $'\\'`,
		`echo "a\""`,
		`echo 'a'\''b'`,
		`echo \'\'`,
	}
	for _, sep := range []string{";", " ; ", "&&", " && ", "||", "|", " & ", "\n"} {
		for _, prefix := range prefixes {
			cmd := prefix + sep + "mkdir OWNED"
			parts := SplitCommandChain(cmd)
			if len(parts) < 2 {
				t.Errorf("SplitCommandChain(%q) = %q; the escape hid a command boundary", cmd, parts)
			}
			if d := gate.EvaluateCommand(cmd, task); d.Action != Deny {
				t.Errorf("EvaluateCommand(%q) = %v (%s); a second command was never judged",
					cmd, d.Action, d.Reason)
			}
		}
	}
}

// TestEscapedOperatorIsNotABoundary is the other half of the same rule, and it
// is what stops the fix from degenerating into "split on everything". A
// backslash before an operator makes that operator a literal character, so the
// line really is one command and must stay allowed.
func TestEscapedOperatorIsNotABoundary(t *testing.T) {
	gate := CommandGate{GlobalAllowedCommands: []string{"echo *"}, HardRules: true}
	task := &dc.Task{ID: "TASK-001", AllowedCommands: []string{"echo *"}}
	for _, cmd := range []string{
		`echo a\;b`,
		`echo a\&b`,
		`echo a\|b`,
		`echo 'a;b'`,
		`echo "a;b"`,
		`echo $'a;b'`,
	} {
		if got := SplitCommandChain(cmd); len(got) != 1 {
			t.Errorf("SplitCommandChain(%q) = %q, want one part; the operator is quoted", cmd, got)
		}
		if d := gate.EvaluateCommand(cmd, task); d.Action == Deny {
			t.Errorf("EvaluateCommand(%q) = deny (%s); a quoted operator is not a second command",
				cmd, d.Reason)
		}
	}
}

// --- differential oracle -----------------------------------------------------
//
// The two fuzz targets already in commandfuzz_test.go pass vacuously on exactly
// this bypass, and both for the same reason: they take the splitter's own
// output as ground truth. FuzzSplitCommandChainLosesNothing asserts a part does
// not split further, which holds when the same misreading is applied twice; and
// FuzzChainingNeverLaundersADenial returns early on len(parts) < 2, which is
// the shape every one of these bypasses produces. Neither can fail on
// `echo \' ; mkdir OWNED`.
//
// The oracle below replaces that self-reference with an external one: a real
// /bin/sh execution. If the gate says a line may run, then running it must not
// have started a second command, and any file it redirected into must be one
// RedirectTargets named. Both markers are inert — a directory and a one-byte
// file inside t.TempDir().

// The candidate alphabet. Every piece is inert on its own, and candidates are
// only ever assembled from these pieces, so no mutation of the fuzz inputs
// (which are indices, not strings) can build a destructive command line.
var (
	// diffPrefixes are complete commands the allowlist accepts, each ending in
	// a quoting construct that has to be read exactly the way sh reads it.
	diffPrefixes = []string{
		`echo ok`,
		`echo \'`,
		`echo \"`,
		`echo \\\'`,
		`echo \\`,
		`echo \`,
		`echo $'\''`,
		`echo $'a\'b'`,
		`echo "a\""`,
		`echo 'a'`,
		`echo "a"`,
		`echo $'x'`,
		`echo a\;b`,
		`echo ''`,
		`echo ""`,
		``,
	}
	// diffSeparators span every metacharacter sh can read as a boundary, plus
	// the empty string so single-token candidates are covered too.
	diffSeparators = []string{
		";", " ; ", "&&", " && ", "||", " || ", "|", " | ",
		"&", " & ", "\n", " \n ", "\r\n", ";;", "",
	}
	// diffPayloads are the inert markers. Anything that reaches the filesystem
	// is a directory named OWNED or a one-byte file named OWNED_FILE, both
	// inside the case's own temp directory.
	diffPayloads = []string{
		`mkdir OWNED`,
		`echo x > OWNED_FILE`,
		`echo $(mkdir OWNED)`,
		`echo $(echo x > OWNED_FILE)`,
		"echo `echo x > OWNED_FILE`",
		`echo "$(echo x > OWNED_FILE)"`,
		`cat <(echo x > OWNED_FILE)`,
	}
)

const (
	chainMarker    = "OWNED"      // created by a second command running
	redirectMarker = "OWNED_FILE" // created by an output redirection
)

func diffCandidate(prefixIdx, sepIdx, payloadIdx int) string {
	idx := func(v, n int) int {
		v %= n
		if v < 0 {
			v += n
		}
		return v
	}
	return diffPrefixes[idx(prefixIdx, len(diffPrefixes))] +
		diffSeparators[idx(sepIdx, len(diffSeparators))] +
		diffPayloads[idx(payloadIdx, len(diffPayloads))]
}

func differentialGate() (CommandGate, *dc.Task) {
	return CommandGate{
			GlobalAllowedCommands: []string{"echo", "echo *", "cat *", "true", ":"},
			HardRules:             true,
		}, &dc.Task{
			ID:              "TASK-001",
			AllowedCommands: []string{"echo *", "cat *"},
		}
}

// runDifferentialCase executes one candidate under a real sh in its own
// directory and checks the two oracles. It reports whether the candidate was
// executed at all (a case whose gate verdict is Deny is still executed, because
// the interesting direction is allow-and-it-ran).
func runDifferentialCase(t *testing.T, root, command string) {
	t.Helper()

	gate, task := differentialGate()
	decision := gate.EvaluateCommand(command, task)
	targets, opaque, targetErr := RedirectTargets(command)

	dir, err := os.MkdirTemp(root, "case")
	if err != nil {
		// Errorf, not Fatalf: this runs on worker goroutines.
		t.Errorf("temp dir: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// The command runs with HOME pointed at the case directory so no candidate
	// can reach the real one, and with stdin closed so nothing can block.
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+dir)
	cmd.Stdin = strings.NewReader("")
	_ = cmd.Run() // a syntax error or a missing binary is a fine outcome

	ranSecondCommand := exists(filepath.Join(dir, chainMarker))
	wroteRedirect := exists(filepath.Join(dir, redirectMarker))

	// Oracle 1 — the chain oracle. `mkdir OWNED` is never in either allowlist,
	// so if it ran, the gate had to have refused the line.
	if ranSecondCommand && decision.Action != Deny {
		t.Errorf("BYPASS: gate returned %v for %q but sh ran a second command (%s was created)",
			decision.Action, command, chainMarker)
	}

	// Oracle 2 — the redirect oracle. A write the gate never named is a write
	// nothing above it can judge. Refusing the line, failing to analyse it, or
	// reporting the target as opaque all count as naming it.
	if wroteRedirect && decision.Action != Deny && targetErr == nil && !opaque {
		named := false
		for _, target := range targets {
			if filepath.Base(target) == redirectMarker {
				named = true
			}
		}
		if !named {
			t.Errorf("BYPASS: %q wrote %s but RedirectTargets reported %v; "+
				"the redirect rung above this package never saw the write",
				command, redirectMarker, targets)
		}
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// TestShellDifferentialOracle enumerates the whole candidate cross product and
// checks both oracles against a real sh. This is the check that fails on the
// pre-fix splitter, on every backslash spelling at once, rather than on the one
// input someone happened to write down.
func TestShellDifferentialOracle(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("no /bin/sh to differentiate against: %v", err)
	}
	root := t.TempDir()

	var commands []string
	singlePart := 0
	for p := range diffPrefixes {
		for s := range diffSeparators {
			for l := range diffPayloads {
				command := diffCandidate(p, s, l)
				if strings.TrimSpace(command) == "" {
					continue
				}
				commands = append(commands, command)
				if len(SplitCommandChain(command)) < 2 {
					singlePart++
				}
			}
		}
	}
	candidates := len(commands)

	// Each case is a process spawn in its own directory, so they are
	// independent and run on a bounded pool rather than serially.
	sem := make(chan struct{}, 12)
	var wg sync.WaitGroup
	for _, command := range commands {
		wg.Add(1)
		go func(command string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			runDifferentialCase(t, root, command)
		}(command)
	}
	wg.Wait()

	t.Logf("differential oracle: %d candidates executed, %d of them single-part",
		candidates, singlePart)

	// FuzzChainingNeverLaundersADenial skips len(parts) < 2 outright, and every
	// closed bypass in this file has that shape. If the corpus ever stopped
	// containing single-part candidates the oracle would go quiet without
	// failing, so the count is asserted rather than merely logged.
	if singlePart == 0 {
		t.Fatal("no single-part candidates were checked; the shape every bypass takes went untested")
	}
	if candidates < 300 {
		t.Fatalf("only %d candidates; the alphabet shrank and the oracle lost coverage", candidates)
	}
}

// FuzzShellDifferentialOracle is the same oracle under the fuzzer. Its inputs
// are indices into the fixed candidate tables rather than free-form strings, so
// the mutator explores the metacharacter grammar — backslash included — while
// remaining structurally unable to assemble a command that is not built from
// the inert pieces above.
func FuzzShellDifferentialOracle(f *testing.F) {
	// Seeds: the reported bypass, its control, and one of each variant family.
	for _, seed := range [][3]int{
		{1, 0, 0}, // echo \' ; mkdir OWNED   — the reported bypass
		{0, 0, 0}, // echo ok ; mkdir OWNED   — the control
		{2, 2, 0}, // echo \" && mkdir OWNED
		{3, 4, 0}, // echo \\\' || mkdir OWNED
		{6, 0, 0}, // echo $'\'' ; mkdir OWNED
		{0, 0, 3}, // echo ok ; echo $(echo x > OWNED_FILE)
		{0, 0, 4}, // backtick redirect
		{0, 0, 6}, // process-substitution redirect
		{0, 14, 1},
	} {
		f.Add(seed[0], seed[1], seed[2])
	}

	if _, err := os.Stat("/bin/sh"); err != nil {
		f.Skipf("no /bin/sh to differentiate against: %v", err)
	}
	root := f.TempDir()

	f.Fuzz(func(t *testing.T, prefixIdx, sepIdx, payloadIdx int) {
		command := diffCandidate(prefixIdx, sepIdx, payloadIdx)
		if strings.TrimSpace(command) == "" {
			return
		}
		runDifferentialCase(t, root, command)
	})
}

// TestDifferentialOracleWouldHaveCaughtTheReportedInput states the reported
// bypass and its control side by side, so a regression names itself instead of
// arriving as one failure among hundreds.
func TestDifferentialOracleWouldHaveCaughtTheReportedInput(t *testing.T) {
	gate, task := differentialGate()

	bypass := `echo \' ; mkdir OWNED`
	control := `echo ok ; mkdir OWNED`

	if got := len(SplitCommandChain(bypass)); got != 2 {
		t.Fatalf("SplitCommandChain(%q) produced %d parts, want 2 (control %q produces %d)",
			bypass, got, control, len(SplitCommandChain(control)))
	}
	for _, cmd := range []string{bypass, control} {
		if d := gate.EvaluateCommand(cmd, task); d.Action != Deny {
			t.Fatalf("EvaluateCommand(%q) = %v (%s), want deny", cmd, d.Action, d.Reason)
		}
	}
}

// TestSplitterAndScannersShareOneQuoteReading is the structural assertion. The
// bypass existed because four scanners each carried their own copy of sh's
// quoting rules and one copy was wrong. They now share shellQuoteStep, and this
// pins that they agree: for every candidate, a line the splitter reads as a
// single command must not hide a substitution the substitution scanner then
// disagrees about the position of.
func TestSplitterAndScannersShareOneQuoteReading(t *testing.T) {
	for _, cmd := range []string{
		`echo \' $(mkdir OWNED)`,
		`echo $'\'' $(mkdir OWNED)`,
		`echo \" $(mkdir OWNED)`,
		`echo '$(mkdir OWNED)'`,
		`echo "$(mkdir OWNED)"`,
	} {
		spans, err := liveSubstitutions(cmd)
		if err != nil {
			t.Fatalf("liveSubstitutions(%q): %v", cmd, err)
		}
		quoted := strings.Contains(cmd, `'$(`)
		if quoted && len(spans) != 0 {
			t.Errorf("liveSubstitutions(%q) = %v; a single-quoted substitution is data", cmd, spans)
		}
		if !quoted && len(spans) == 0 {
			t.Errorf("liveSubstitutions(%q) found nothing; the escape moved the quote state", cmd)
		}
	}

	// And the same input read by every scanner in the file lands on the same
	// quote state, which is what makes "fixed in one place" mean anything.
	for _, cmd := range []string{
		`echo \' ; cat << EOF`,
		`echo $'\'' ; cat << EOF`,
	} {
		if !hasHeredoc(cmd) {
			t.Errorf("hasHeredoc(%q) = false; the escape hid a heredoc introducer", cmd)
		}
	}
}

func init() {
	// Guards the payload table against a well-meaning edit that swaps in
	// something with side effects outside the case directory.
	for _, p := range diffPayloads {
		if !strings.Contains(p, chainMarker) && !strings.Contains(p, redirectMarker) {
			panic(fmt.Sprintf("differential payload %q touches something other than the inert markers", p))
		}
	}
}
