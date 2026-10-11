package devcouncil

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// run feeds a diff to the verifier and returns its findings.
//
// An unparseable diff is an error rather than an empty result, and the caller
// turns that into a named degradation. The distinction is the whole contract:
// an empty finding list means these gates ran and found nothing.
func (c rigorClient) run(ctx context.Context, diff string, planned []string, coverage string) (*rigorResult, error) {
	if c.Binary == "" {
		return nil, errors.New("no dcverify binary configured")
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
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
		switch f.Gate {
		case "secret_scan":
			action.Action = fmt.Sprintf(
				"Hardening violation: Remove the credential at %s:%d immediately and rotate it. "+
					"Deleting the line does not remove the object from git history.", f.Path, f.Line)
		case "stub_detection":
			action.Action = fmt.Sprintf(
				"Deconstruct and implement placeholder at %s:%d completely; avoid placeholders or fake values in delivered code.", f.Path, f.Line)
		default:
			action.Action = f.Message
		}
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
