package devcouncil

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"manvi/internal/proc"
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
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Planned paths are newline-separated so one containing a comma or a space
	// survives the boundary intact.
	args := []string{"check", "--planned", strings.Join(planned, "\n")}
	if coverage != "" {
		args = append(args, "--coverage", coverage)
	}
	if c.Root != "" {
		args = append(args, "--root", c.Root)
	}
	cmd := exec.CommandContext(ctx, c.Binary, args...)
	cmd.Stdin = strings.NewReader(diff)
	// Capped during the copy, not checked after it: a verifier gone rogue on
	// a large diff used to be able to allocate the whole result before the
	// bound was ever consulted. Same shape as the store boundary's
	// cappedBuffer.
	stdout := &cappedRigorBuffer{limit: maxRigorOutput}
	var stderr bytes.Buffer
	cmd.Stdout = stdout
	cmd.Stderr = &stderr
	// The same bound as the store boundary: killing the process does not
	// unblock Wait while a child holds the stdout pipe.
	cmd.WaitDelay = 2 * time.Second

	runErr, timedOut := proc.RunBounded(ctx, cmd.Run)
	if timedOut {
		return nil, fmt.Errorf("verifier timed out after %s", timeout)
	}
	if stdout.overflow {
		return nil, fmt.Errorf("verifier produced more than %d bytes", maxRigorOutput)
	}

	var out rigorResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.buf.Bytes()), &out); err != nil {
		if runErr != nil {
			return nil, fmt.Errorf("verifier failed: %w (stderr: %s)", runErr, bytes.TrimSpace(stderr.Bytes()))
		}
		return nil, fmt.Errorf("verifier returned unparseable output: %w (%q)", err, stdout.buf.String())
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
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Binary, "health")
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	if err != nil {
		return err
	}
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
// enforceCoverage is verify.diff_coverage.enforce, and it governs exactly one
// thing: whether a coverage finding stops the task. What is *reported* does not
// change with it, because what was measured is a fact about the run and not a
// policy choice — only the consequence is the operator's to set.
func gapsFrom(taskID string, result *rigorResult, enforceCoverage bool) ([]Gap, []NextAction) {
	var gaps []Gap
	var actions []NextAction

	// Severity travels with Blocking so a gap cannot say "high" while claiming
	// not to block, or the reverse. An agent reads one field and a human reads
	// the other, and the two disagreeing is a report that tells them different
	// things about whether the work is finished.
	coverageSeverity := "medium"
	if enforceCoverage {
		coverageSeverity = "high"
	}

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
			// Not blocking by default. Unmeasured is a statement about the
			// harness's inputs — no coverage data was supplied — rather than
			// about the change, and blocking on it would stop every task until
			// coverage collection is wired up. It is reported, always, so the
			// distinction between "covered" and "never measured" survives.
			//
			// Under verify.diff_coverage.enforce it blocks along with the
			// measured gaps below, and it has to. "Nobody measured this" is not
			// evidence the added lines ran; if it stayed advisory while real
			// gaps blocked, the way to satisfy an enforced coverage gate would
			// be to stop supplying coverage — a gate satisfied by removing its
			// own input, which is the same failure as a gate that never ran
			// reporting a pass.
			Blocking: enforceCoverage,
			File:     path,
			Detail: fmt.Sprintf("%s changed but no coverage data was supplied; "+
				"this is not evidence the added lines ran", path),
		}
		gaps = append(gaps, gap)
		actions = append(actions, NextAction{
			GapID: gap.ID, Category: "diff_coverage", File: path, Blocking: enforceCoverage,
			Action: fmt.Sprintf("Characterize baseline behavior: Run the tests with coverage and confirm the added lines in %s execute across edge conditions.", path),
		})
	}

	for _, g := range result.CoverageGaps {
		gap := Gap{
			ID:       fmt.Sprintf("GAP-%s-COVERAGE-%s", taskID, strings.ReplaceAll(g.Path, "/", "-")),
			Type:     "diff_coverage",
			Severity: coverageSeverity,
			Blocking: enforceCoverage,
			File:     g.Path,
			Detail: fmt.Sprintf("%d of %d added lines in %s were not executed by any test (lines %v)",
				len(g.UncoveredLines), g.AddedLines, g.Path, g.UncoveredLines),
		}
		gaps = append(gaps, gap)
		actions = append(actions, NextAction{
			GapID: gap.ID, Category: "diff_coverage", File: g.Path, Blocking: enforceCoverage,
			Action: fmt.Sprintf("Stress-test and harden: Add adversarial tests exercising uncovered lines %v of %s.", g.UncoveredLines, g.Path),
		})
	}

	return gaps, actions
}

// cappedRigorBuffer collects up to a limit and then records that it stopped,
// so a runaway verifier is a reported condition rather than an allocation the
// harness pays for before it ever looks at the result. Deliberately its own
// type rather than an import of the store client's: one boundary's transport
// detail is not another's API.
type cappedRigorBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (b *cappedRigorBuffer) Write(p []byte) (int, error) {
	if remaining := b.limit - b.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			b.buf.Write(p[:remaining])
			b.overflow = true
		} else {
			b.buf.Write(p)
		}
	} else if len(p) > 0 {
		b.overflow = true
	}
	// Always report a full write: returning short would make the copier treat
	// this as an I/O failure and mask the real diagnostic.
	return len(p), nil
}
