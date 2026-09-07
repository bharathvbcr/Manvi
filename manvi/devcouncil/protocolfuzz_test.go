package devcouncil

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/internal/testsupport"
)

// FuzzVerifierChildAnswersEveryRequestWithExactlyOneObject drives the real
// dcverify across the real process boundary with fuzzed requests.
//
// The verifier is the boundary with the widest request surface of the three:
// unlike the store, whose requests are identifiers, this one takes a unified
// diff on stdin and an LCOV profile on disk, and it parses both. Everything it
// is asked about is attacker-adjacent by construction — the diff is what a
// model just wrote, and the gates reading it are the secret scanner and the
// scope classifier — so a request that makes it print nothing, print twice, or
// exit without a diagnosis is a rigor check that could not run reporting the
// same way as one that ran and passed.
//
// The invariants are the ones rigorClient.run() is written against:
//
//   - It always returns inside the bound. A verifier that does not is a turn
//     that never ends.
//   - Its stdout is exactly one JSON object. run() decodes the whole stream
//     with json.Unmarshal, so a second object makes the answer unparseable.
//   - A non-zero exit always carries a parseable ok:false with a reason.
//   - An unparseable diff is always an error and never an empty clean result.
//     This is the distinction the whole contract rests on: an empty finding
//     list means these gates ran and found nothing.
//   - No finding ever quotes the credential it found. The secret scanner runs
//     over fuzzed text, and a report that copied a key into the evidence trail
//     is the failure the gate exists to prevent.
func FuzzVerifierChildAnswersEveryRequestWithExactlyOneObject(f *testing.F) {
	bin := testsupport.DCVerify(f)
	// One directory for the whole run. The coverage argument has to name a real
	// path — a missing one is deliberately fatal in the verifier — so the fuzzed
	// profile is written here rather than passed as a path the fuzzer invented,
	// which would only ever exercise the not-found branch.
	dir := f.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o750); err != nil {
		f.Fatal(err)
	}

	const realDiff = "diff --git a/src/a.go b/src/a.go\n" +
		"--- a/src/a.go\n+++ b/src/a.go\n@@ -1,1 +1,2 @@\n package a\n+const k = 1\n"

	for _, seed := range []struct {
		diff, planned, coverage string
		cmd                     uint8
	}{
		{realDiff, "src/a.go", "", 0},
		{realDiff, "src/a.go", "mode: set\nmanvi/src/a.go:2.1,2.10 1 1\n", 0},
		{realDiff, "src/a.go", "not a coverage file", 0},
		{realDiff, "", "", 0},
		{realDiff, "\n\n\n", "", 0},
		{realDiff, "a\nb\nc", "", 0},
		{"", "src/a.go", "", 0},
		{"this is not a diff", "src/a.go", "", 0},
		{"diff --git a/x b/x", "x", "", 0},
		{"--- a/x\n+++ b/x\n@@ bad hunk @@\n", "x", "", 0},
		{"diff --git a/a b/a\n--- a/a\n+++ b/a\n@@ -0,0 +1 @@\n+sk-ant-api03-" + strings.Repeat("A", 28) + "\n", "a", "", 0},
		{"diff --git a/a b/a\n--- a/a\n+++ b/a\n@@ -0,0 +1 @@\n+// TODO: implement\n", "a", "", 0},
		{strings.Repeat("+x\n", 5000), "a", "", 0},
		{"\x00\x01\x02", "a", "", 0},
		{"@@ -1,1 +1,1 @@\n", "../../etc/passwd", "", 0},
		{realDiff, "--planned", "", 0},
		{"", "", "", 1},       // health
		{"", "", "", 2},       // an unknown subcommand
		{realDiff, "", "", 3}, // no subcommand at all — check by default
		// The regression this target found on its first run.
		{realDiff, "a\xff.go", "", 0},
		{realDiff, "src/a.go", "\xff\xfe", 0},
		{"diff --git a/\xff b/\xff\n--- a/\xff\n+++ b/\xff\n@@ -0,0 +1 @@\n+x\n", "a", "", 0},
	} {
		f.Add(seed.cmd, seed.diff, seed.planned, seed.coverage)
	}

	f.Fuzz(func(t *testing.T, cmdIdx uint8, diff, planned, coverage string) {
		// A NUL in an argument is refused by the operating system before either
		// plane sees it, so it says nothing about the protocol and is the one
		// value skipped. Invalid UTF-8 is deliberately not skipped: paths reach
		// this binary from a diff and from a working tree, where Unix filenames
		// are bytes, and reading them through std::env::args() used to panic
		// the verifier — exit 101 with nothing on stdout, which the caller can
		// only report as a rigor check that failed for no stated reason. The
		// diff is stdin and unrestricted; arbitrary bytes there are exactly
		// what the parser has to survive.
		if strings.ContainsRune(planned, 0) {
			return
		}

		args := []string{"--root", root}
		switch cmdIdx % 4 {
		case 0:
			args = append(args, "check")
		case 1:
			args = append(args, "health")
		case 2:
			args = append(args, "not-a-subcommand")
		case 3:
			// No positional at all, which the verifier treats as `check`.
		}
		if planned != "" {
			args = append(args, "--planned", planned)
		}
		if coverage != "" {
			profile := filepath.Join(dir, "coverage.info")
			if err := os.WriteFile(profile, []byte(coverage), 0o600); err != nil {
				t.Fatal(err)
			}
			args = append(args, "--coverage", profile)
		}

		stdout, exitCode, err := runVerifierChild(t, bin, args, diff)
		if err != nil {
			t.Fatalf("dcverify %v: %v", args, err)
		}
		assertOneDiagnosedVerifierObject(t, args, stdout, exitCode)

		// The producer's real bytes through the consumer's real decision.
		var out rigorResult
		if json.Unmarshal(stdout, &out) != nil {
			return // already reported by the assertion above if it is a fault
		}
		if !out.OK {
			// A refusal must say why, or the caller turns it into a named
			// degradation with nothing in the name.
			if strings.TrimSpace(out.Error) == "" {
				t.Fatalf("dcverify %v reported ok:false with no reason (%q)", args, stdout)
			}
			return
		}
		// An accepted result must not have been produced by a gate that could
		// not run: ok:true means these gates ran, and every count it reports
		// has to be about files it actually parsed.
		if out.Files < 0 {
			t.Fatalf("dcverify %v reported %d files (%q)", args, out.Files, stdout)
		}
		if out.Files == 0 && len(out.Findings) > 0 {
			t.Fatalf("dcverify %v reported %d finding(s) over 0 parsed files (%q)",
				args, len(out.Findings), stdout)
		}
		for _, finding := range out.Findings {
			if finding.Gate == "" || finding.Severity == "" {
				t.Fatalf("dcverify %v returned an unclassified finding %+v (%q)", args, finding, stdout)
			}
			// The secret scanner's own rule, asserted against every diff the
			// fuzzer builds rather than against the one planted credential in
			// verify.sh: evidence names the shape, never the value.
			if finding.Gate == "secret_scan" && quotesItsOwnInput(finding.Evidence, diff) {
				t.Fatalf("dcverify %v reproduced the credential it found in its evidence %q",
					args, finding.Evidence)
			}
		}
	})
}

// quotesItsOwnInput reports whether a finding's evidence contains a long
// verbatim run from the input it was scanning. Short overlaps are unavoidable —
// the evidence names the shape, and shape names appear in the text — so the run
// has to be long enough that only a copy explains it.
func quotesItsOwnInput(evidence, input string) bool {
	const runLength = 20
	if len(evidence) < runLength {
		return false
	}
	for i := 0; i+runLength <= len(evidence); i++ {
		if strings.Contains(input, evidence[i:i+runLength]) {
			return true
		}
	}
	return false
}

// verifierBound is how long one dcverify invocation may take. Parsing a diff
// already in memory is bounded work, so anything reaching this is a child that
// will not return — the failure being looked for, not a slow machine.
const verifierBound = 60 * time.Second

func runVerifierChild(t *testing.T, binary string, args []string, stdin string) ([]byte, int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), verifierBound)
	defer cancel()

	// #nosec G204 -- the binary is the one testsupport built; driving it
	// across the real process boundary is what this target exists for.
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.WaitDelay = 2 * time.Second

	start := time.Now()
	out, err := cmd.Output()
	elapsed := time.Since(start)

	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return nil, 0, err
	}
	if ctx.Err() != nil {
		t.Fatalf("dcverify %v did not return within %s (took %s); a boundary call that never "+
			"returns is a turn that never ends", args, verifierBound, elapsed)
	}
	return out, cmd.ProcessState.ExitCode(), nil
}

// assertOneDiagnosedVerifierObject is the wire contract dcverify is held to:
// one JSON object on stdout, and never a non-zero exit without a reason in it.
func assertOneDiagnosedVerifierObject(t *testing.T, args []string, stdout []byte, exitCode int) {
	t.Helper()

	dec := json.NewDecoder(strings.NewReader(string(stdout)))
	var first map[string]json.RawMessage
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("dcverify %v (exit %d) did not print one JSON object: %v (output %q)",
			args, exitCode, err, stdout)
	}
	if first == nil {
		t.Fatalf("dcverify %v (exit %d) printed a JSON null, which decodes into an empty result "+
			"indistinguishable from a clean one", args, exitCode)
	}
	if _, err := dec.Token(); err != io.EOF {
		rest, _ := io.ReadAll(dec.Buffered())
		t.Fatalf("dcverify %v (exit %d) printed more than one JSON value; trailing bytes %q (whole output %q)",
			args, exitCode, strings.TrimSpace(string(rest)), stdout)
	}
	if _, present := first["ok"]; !present {
		t.Fatalf("dcverify %v (exit %d) printed an object with no \"ok\" field: %q", args, exitCode, stdout)
	}

	if exitCode == 0 {
		return
	}
	var failure struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(stdout, &failure); err != nil {
		t.Fatalf("dcverify %v exited %d and its output does not decode: %v (%q)", args, exitCode, err, stdout)
	}
	if failure.OK {
		t.Fatalf("dcverify %v exited %d while reporting ok:true (%q)", args, exitCode, stdout)
	}
	if strings.TrimSpace(failure.Error) == "" {
		t.Fatalf("dcverify %v exited %d with no error to report (%q); an exit code with no "+
			"diagnosis leaves the caller with nothing to tell an operator", args, exitCode, stdout)
	}
}
