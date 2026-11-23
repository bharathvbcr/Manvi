package devcouncil

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/childenv"
)

// rigorClient runs the Rust verifier over a diff.
//
// The same process boundary as the store, for the same reason: the analysis
// plane is Rust because these gates are CPU-bound, deterministic text work, and
// joining the two languages with cgo would cost the static binary the execution
// plane was chosen for.
type rigorClient struct {
	Binary  string
	Timeout time.Duration
	// Root is the repository root, passed to the verifier so absolute LCOV
	// paths (llvm-cov and grcov emit those) reduce to the repo-relative form
	// the diff uses. Without it every such file reported as unmeasured.
	Root string
}

// Finding is one rigor result.
type Finding struct {
	Gate     string `json:"gate"`
	Severity string `json:"severity"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	// Evidence identifies what triggered the finding. For a secret finding it
	// is the shape and a length, never the value: a report that quoted the key
	// would copy it into the evidence trail and the session log, which is what
	// the gate exists to prevent.
	Evidence string `json:"evidence"`
	Message  string `json:"message"`
}

// CoverageGap is added lines no test executed.
type CoverageGap struct {
	Path           string `json:"path"`
	AddedLines     int    `json:"added_lines"`
	UncoveredLines []int  `json:"uncovered_lines"`
}

// rigorResult is what the verifier returns.
type rigorResult struct {
	OK                 bool          `json:"ok"`
	Error              string        `json:"error"`
	Files              int           `json:"files"`
	InScope            []string      `json:"in_scope"`
	Orphans            []string      `json:"orphans"`
	UntouchedPlanned   []string      `json:"untouched_planned"`
	Findings           []Finding     `json:"findings"`
	CoverageUnmeasured []string      `json:"coverage_unmeasured"`
	CoverageGaps       []CoverageGap `json:"coverage_gaps"`
}

// maxRigorOutput bounds the reply. Findings are one per added line at worst, so
// a large but legitimate result is possible; an unbounded read is not.
const (
	maxRigorOutput = 16 << 20
	// maxRigorHealthOutput bounds the identity reply, which is one small
	// object; anything larger is not an honest answer to "who are you".
	maxRigorHealthOutput = 64 << 10
	// maxRigorStderr bounds diagnostics, which travel on an error string.
	maxRigorStderr = 64 << 10
	// maxPatchReadBytes bounds how large a file patchFile will pull into
	// memory for a read-modify-write.
	maxPatchReadBytes = 64 << 20
)

// Lean kernel deadline model. dc-proc's lean_kernel.rs applies the per-step
// timeout to each child process separately. For one .lean file a check runs, in
// order: a libdir probe (capped at min(step, 10s)), one bounded compile for each
// staged in-tree local import (at most MAX_STAGED_LOCAL_MODULES = 64, the
// "Stage and compile any unbuilt in-tree .lean dependencies" loop), then the
// Lean compile, the `#print axioms` driver and `lean4checker --fresh` (steps
// 1-3 there). dc-verify's rigor.rs repeats that whole sequence once per .lean
// file in the diff. The deadline here must cover that work, not one step.
const (
	defaultRigorTimeout      = 30 * time.Second
	defaultLeanKernelStep    = 180 * time.Second
	minLeanKernelStep        = 50 * time.Millisecond
	maxLeanKernelStep        = 600 * time.Second
	leanKernelSequentialRuns = 3
	leanKernelLibdirProbe    = 10 * time.Second
	leanKernelMaxStaged      = 64
	leanKernelMargin         = 60 * time.Second
	minLeanRigorTimeout      = 240 * time.Second
	// defaultLeanRigorCap bounds the whole Lean deadline whatever the diff
	// and the step say. The worst case grows with files x staged imports x
	// step (one file at the 180s default is already 12,070s), and an
	// unbounded wait on a wedged verifier holds the turn and its lease for
	// as long as the arithmetic says. A check that reaches the cap is
	// reported as one that could not run (leanBoundError), never as a pass.
	defaultLeanRigorCap = 45 * time.Minute
)

// leanRigorCap is defaultLeanRigorCap as a variable so a test can drive the
// cap without waiting 45 minutes. Nothing else assigns it.
var leanRigorCap = defaultLeanRigorCap

// leanKernelStep mirrors dc-proc's configured_lean_kernel_timeout
// (lean_kernel.rs): DC_LEAN_KERNEL_TIMEOUT_MS clamped to 50ms..600s, 180s when
// unset or not an unsigned decimal. dc-proc parses with raw.trim().parse::<u64>(),
// and Rust's integer FromStr accepts exactly one leading '+', which Go's
// ParseUint does not, so that one sign is stripped here ("+300000" is 300s,
// "++300000" and "+-1" are the default).
func leanKernelStep() time.Duration {
	raw := strings.TrimPrefix(strings.TrimSpace(os.Getenv("DC_LEAN_KERNEL_TIMEOUT_MS")), "+")
	if raw == "" {
		return defaultLeanKernelStep
	}
	ms, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return defaultLeanKernelStep
	}
	switch {
	case ms < uint64(minLeanKernelStep/time.Millisecond):
		return minLeanKernelStep
	case ms > uint64(maxLeanKernelStep/time.Millisecond):
		return maxLeanKernelStep
	default:
		return time.Duration(ms) * time.Millisecond
	}
}

// leanBound describes the Lean work a deadline was computed from, so a timeout
// can say what it was a bound on.
type leanBound struct {
	files          int
	importsPerFile int
}

// leanBoundError is a dcverify timeout under a Lean-derived deadline. It is an
// error, so it reaches runRigor's did-not-run path: a check that was cut off is
// not a check that passed, and not an ordinary finding either.
type leanBoundError struct {
	bound   time.Duration
	exceeds leanBound
}

func (e *leanBoundError) Error() string {
	return fmt.Sprintf("could not run: the Lean check exceeded its %s bound (%d files, up to %d local imports each)",
		humanBound(e.bound), e.exceeds.files, e.exceeds.importsPerFile)
}

func humanBound(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%d-minute", d/time.Minute)
	}
	return d.Round(time.Millisecond).String()
}

// leanFilesInDiff counts the distinct .lean paths a unified diff names, from its
// "diff --git" and "+++" headers. Deleted and renamed files are counted too: the
// count is an upper bound on what dc-verify can check, never a lower one.
func leanFilesInDiff(diff string) int {
	seen := map[string]struct{}{}
	add := func(path string) {
		path = strings.Trim(strings.TrimSpace(path), "\"")
		if strings.HasSuffix(path, ".lean") {
			seen[path] = struct{}{}
		}
	}
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			if i := strings.LastIndex(line, " b/"); i >= 0 {
				add(line[i+3:])
			}
		case strings.HasPrefix(line, "+++ b/"):
			add(strings.TrimPrefix(line, "+++ b/"))
		}
	}
	return len(seen)
}

// deadline is the dcverify deadline for one diff. An explicit Timeout wins.
// With DC_VERIFY_LEAN_KERNEL on (auto or require, the only values dcverify
// accepts: dc-verify's rigor.rs fails closed on anything else, so 1 and true do
// not widen the deadline) it is computed from the work the check will run: per
// .lean file in the diff, probe + (3 + staged local imports) x step, summed over
// files, plus a 60s margin, never under 240s and never over leanRigorCap.
// Otherwise it is 30s.
//
// The staged-import count is not read from the tree. Counting them from here
// would be a second copy of dc-proc's staging rule (the header grammar, the
// search bases around the spec path, the libdir skip, the symlink and size
// refusals), and a copy that drifts is a deadline that is silently wrong. The
// bound used is the cap dc-proc itself enforces, 64 per file, which is sound for
// every tree and pessimistic for most. The cap on the total is what keeps that
// from meaning an unbounded wait.
//
// DevCouncil's dcverify.Client.effectiveTimeout is unexported and budgets one
// step plus 60s, so it cannot be called from here; the step clamp is pinned
// against its table in rigor_hijack_test.go instead.
func (c rigorClient) deadline(diff string) (time.Duration, *leanBound) {
	if c.Timeout > 0 {
		return c.Timeout, nil
	}
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("DC_VERIFY_LEAN_KERNEL")))
	if mode != "auto" && mode != "require" {
		return defaultRigorTimeout, nil
	}
	files := leanFilesInDiff(diff)
	step := leanKernelStep()
	perFile := min(step, leanKernelLibdirProbe) + (leanKernelSequentialRuns+leanKernelMaxStaged)*step
	limit := leanRigorCap
	want := limit
	if files <= int(limit/perFile) {
		want = max(time.Duration(files)*perFile+leanKernelMargin, minLeanRigorTimeout)
	}
	return min(want, limit), &leanBound{files: files, importsPerFile: leanKernelMaxStaged}
}

// rigorActionText is DevCouncil's actionText (devcouncil/verify/next_actions.go)
// for the gates dcverify emits, character for character, so one finding reads
// the same whether it was raised through DevCouncil or through Manvi. The gate
// names map to DevCouncil's gap types in devcouncil/verify/rigor.go
// (gapsFromFindings): secret_scan is security_risk, stub_detection is
// stub_detected, stub_allowed is stub_declared. assert_free_test and
// skipped_test carry their text as the gap's recommended fix, which actionText
// returns for a type it has no case for. A finding with no path names "the
// affected file", and the stub texts always carry a line, as there.
func rigorActionText(f Finding) string {
	target := "the affected file"
	if f.Path != "" {
		target = f.Path
	}
	loc := target + ":" + strconv.Itoa(f.Line)
	switch f.Gate {
	case "secret_scan":
		return "Remove the detected secret/finding from the diff and rotate any exposed credential."
	case "stub_detection":
		return "Replace the stub/placeholder at " + loc + " with a real implementation, then re-verify. " +
			"Do not mark work complete while placeholders remain."
	case "stub_allowed":
		return "Review the intentional stub declared at " + loc + "; replace it before marking done."
	case "assert_free_test":
		return "Make the added test assert on the behaviour it is named for, or call a helper that does."
	case "skipped_test":
		return "Remove the unconditional skip, or make the skip conditional on what the test needs."
	case "runner_hijack":
		return "Remove the test-runner hijack in " + target + " so the real test harness executes and reports failures honestly."
	case "lean_tcb":
		return "Replace the Lean 4 TCB soundness hole, metaprogramming escape, or non-standard axiom/extern hook in " + target + " with a kernel- and lean4checker-verified proof, then re-verify."
	default:
		return f.Message
	}
}

// run feeds a diff to the verifier and returns its findings.
//
// An unparseable diff is an error rather than an empty result, and the caller
// turns that into a named degradation. The distinction is the whole contract:
// an empty finding list means these gates ran and found nothing.
func (c rigorClient) run(ctx context.Context, diff string, planned []string, coverage string) (*rigorResult, error) {
	if c.Binary == "" {
		return nil, errors.New("no dcverify binary configured")
	}
	timeout, lean := c.deadline(diff)
	// Planned paths are newline-separated so one containing a comma or a space
	// survives the boundary intact.
	args := []string{"check", "--planned", strings.Join(planned, "\n")}
	if coverage != "" {
		args = append(args, "--coverage", coverage)
	}
	if c.Root != "" {
		args = append(args, "--root", c.Root)
	}
	// Capped during the copy, not checked after it: a verifier gone rogue on
	// a large diff used to be able to allocate the whole result before the
	// bound was ever consulted.
	res := runProc(ctx, procSpec{
		argv: append([]string{c.Binary}, args...), stdin: strings.NewReader(diff), env: childenv.Inherit(),
		timeout: timeout, stdoutLimit: maxRigorOutput, stderrLimit: maxRigorStderr,
	})
	if res.timedOut {
		if lean != nil && lean.files > 0 {
			return nil, &leanBoundError{bound: timeout, exceeds: *lean}
		}
		return nil, fmt.Errorf("verifier timed out after %s", timeout)
	}
	if res.stdoutNote != "" {
		return nil, fmt.Errorf("verifier produced more than %d bytes", maxRigorOutput)
	}
	runErr := res.err

	var out rigorResult
	if err := json.Unmarshal(bytes.TrimSpace(res.stdout), &out); err != nil {
		if runErr != nil {
			return nil, fmt.Errorf("verifier failed: %w (stderr: %s)", runErr, bytes.TrimSpace(res.stderr))
		}
		return nil, fmt.Errorf("verifier returned unparseable output: %w (%q)", err, res.stdout)
	}
	if !out.OK {
		reason := out.Error
		if reason == "" {
			reason = "no reason given"
		}
		return nil, errors.New(reason)
	}
	return &out, nil
}

// available reports whether the verifier can be reached, by asking it to
// identify itself rather than by the absence of an error.
func (c rigorClient) available(ctx context.Context) error {
	if c.Binary == "" {
		return errors.New("no dcverify binary configured")
	}
	res := runProc(ctx, procSpec{
		argv: []string{c.Binary, "health"}, env: childenv.Inherit(), timeout: 10 * time.Second,
		stdoutLimit: maxRigorHealthOutput, stderrLimit: maxRigorStderr,
	})
	switch {
	case res.timedOut:
		return fmt.Errorf("%s health timed out", c.Binary)
	case res.err != nil:
		return res.err
	case res.stdoutNote != "":
		return fmt.Errorf("%s health replied with more than %d bytes", c.Binary, maxRigorHealthOutput)
	}
	out := res.stdout
	var health struct {
		OK            bool   `json:"ok"`
		Verifier      string `json:"verifier"`
		SchemaVersion int    `json:"schema_version"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &health); err != nil {
		return fmt.Errorf("unparseable health reply: %w", err)
	}
	if !health.OK || health.Verifier != "dc-verify" {
		return fmt.Errorf("%s identified itself as %q, not %q", c.Binary, health.Verifier, "dc-verify")
	}
	if health.SchemaVersion != rigorSchemaVersion {
		return fmt.Errorf("%s speaks schema %d, this harness speaks %d",
			c.Binary, health.SchemaVersion, rigorSchemaVersion)
	}
	return nil
}

// VerifierAvailable reports whether the rigor gates can run, by asking the
// binary to identify itself. It is exported so `harness doctor` reports the
// same fact the verifier will act on, rather than a second opinion.
func VerifierAvailable(ctx context.Context, binary string) error {
	return rigorClient{Binary: binary}.available(ctx)
}

// rigorSchemaVersion must match dcverify's. A mismatch is refused rather than
// decoded through the wrong shape.
const rigorSchemaVersion = 1

// gapsFrom turns rigor findings into the typed gaps and next actions the rest
// of the surface already routes on, so an agent does not learn a second
// vocabulary for the same kind of answer.
//
// A coverage finding is reported and never blocks. This took an
// enforceCoverage argument, carrying verify.diff_coverage.enforce, until
// DevCouncil retired that key (f7f427b) as one nothing read: the only caller
// resolved it to its default on every run, so the parameter had exactly one
// reachable value and promising the other was the whole defect. What is
// *reported* never depended on it — what was measured is a fact about the run
// rather than a policy choice — so only the consequence is fixed here.
func gapsFrom(taskID string, result *rigorResult) ([]Gap, []NextAction) {
	var gaps []Gap
	var actions []NextAction

	// Severity travels with Blocking so a gap cannot say "high" while claiming
	// not to block, or the reverse. An agent reads one field and a human reads
	// the other, and the two disagreeing is a report that tells them different
	// things about whether the work is finished.
	const coverageSeverity = "medium"

	for i, f := range result.Findings {
		blocking := f.Severity == "blocking"
		severity := "medium"
		if blocking {
			severity = "high"
		}
		gap := Gap{
			ID:       fmt.Sprintf("GAP-%s-%s-%d", taskID, strings.ToUpper(f.Gate), i+1),
			Type:     f.Gate,
			Severity: severity,
			Blocking: blocking,
			File:     f.Path,
			Detail:   fmt.Sprintf("%s:%d — %s (%s)", f.Path, f.Line, f.Message, f.Evidence),
		}
		gaps = append(gaps, gap)

		action := NextAction{
			GapID: gap.ID, Category: f.Gate, File: f.Path, Blocking: blocking,
			Tool: "devcouncil_read_file", Arguments: map[string]any{"path": f.Path},
		}
		action.Action = rigorActionText(f)
		actions = append(actions, action)
	}

	for _, path := range result.CoverageUnmeasured {
		gap := Gap{
			ID:       fmt.Sprintf("GAP-%s-COVERAGE-UNMEASURED-%s", taskID, strings.ReplaceAll(path, "/", "-")),
			Type:     "diff_coverage",
			Severity: coverageSeverity,
			// Not blocking. Unmeasured is a statement about the harness's
			// inputs — no coverage data was supplied — rather than about the
			// change, and blocking on it would stop every task until coverage
			// collection is wired up. It is reported, always, so the
			// distinction between "covered" and "never measured" survives.
			//
			// If a blocking coverage gate is ever wanted, it belongs in
			// dcverify with the gate itself, reached by finding the binary,
			// and not behind a host-side flag that leaves the gate running
			// exactly as before.
			Blocking: false,
			File:     path,
			Detail: fmt.Sprintf("%s changed but no coverage data was supplied; "+
				"this is not evidence the added lines ran", path),
		}
		gaps = append(gaps, gap)
		actions = append(actions, NextAction{
			GapID: gap.ID, Category: "diff_coverage", File: path, Blocking: false,
			Action: fmt.Sprintf("Characterize baseline behavior: Run the tests with coverage and confirm the added lines in %s execute across edge conditions.", path),
		})
	}

	for _, g := range result.CoverageGaps {
		gap := Gap{
			ID:       fmt.Sprintf("GAP-%s-COVERAGE-%s", taskID, strings.ReplaceAll(g.Path, "/", "-")),
			Type:     "diff_coverage",
			Severity: coverageSeverity,
			Blocking: false,
			File:     g.Path,
			Detail: fmt.Sprintf("%d of %d added lines in %s were not executed by any test (lines %v)",
				len(g.UncoveredLines), g.AddedLines, g.Path, g.UncoveredLines),
		}
		gaps = append(gaps, gap)
		actions = append(actions, NextAction{
			GapID: gap.ID, Category: "diff_coverage", File: g.Path, Blocking: false,
			Action: fmt.Sprintf("Stress-test and harden: Add adversarial tests exercising uncovered lines %v of %s.", g.UncoveredLines, g.Path),
		})
	}

	return gaps, actions
}
