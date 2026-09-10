package devcouncil

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bharathvbcr/Manvi/manvi/internal/proc"
	"io"
	"os/exec"
	"strconv"
	"time"
)

// Evidence wire types project DevCouncil protocols/evidence/v1.md. Decisions
// remain in dc-evidence; this client only invokes and validates its response.
type EvidencePredicate struct {
	Op    string          `json:"op"`
	Value json.RawMessage `json:"value,omitempty"`
}
type EvidenceCriterion struct {
	ID        string            `json:"id"`
	Fact      string            `json:"fact"`
	Required  bool              `json:"required"`
	Predicate EvidencePredicate `json:"predicate"`
}
type EvidenceContract struct {
	SchemaVersion int                 `json:"schema_version"`
	ID            string              `json:"id"`
	Criteria      []EvidenceCriterion `json:"criteria"`
}
type EvidenceAction struct {
	ID          string `json:"id"`
	Sequence    uint64 `json:"sequence"`
	Disposition string `json:"disposition"`
}
type EvidenceObservation struct {
	Sequence      uint64                     `json:"sequence"`
	RunID         string                     `json:"run_id"`
	SessionID     string                     `json:"session_id"`
	Epoch         uint64                     `json:"epoch"`
	AfterActionID string                     `json:"after_action_id"`
	Facts         map[string]json.RawMessage `json:"facts"`
	ArtifactIDs   []string                   `json:"artifact_ids"`
}
type EvidenceArtifact struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes uint64 `json:"size_bytes"`
}
type EvidenceEpochTransition struct {
	Sequence  uint64 `json:"sequence"`
	FromEpoch uint64 `json:"from_epoch"`
	ToEpoch   uint64 `json:"to_epoch"`
	Reason    string `json:"reason"`
}
type EvidenceOutcome struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}
type EvidenceIntervention struct {
	ID         string `json:"id"`
	Sequence   uint64 `json:"sequence"`
	ReasonCode string `json:"reason_code"`
	Status     string `json:"status"`
}
type EvidenceHumanAction struct {
	ID             string `json:"id"`
	Sequence       uint64 `json:"sequence"`
	Kind           string `json:"kind"`
	InterventionID string `json:"intervention_id"`
}
type EvidenceRecoveryApplied struct {
	ID       string `json:"id"`
	Sequence uint64 `json:"sequence"`
	StepID   string `json:"step_id"`
}
type EvidenceLocatorHit struct {
	Target        string `json:"target"`
	StrategyIndex uint64 `json:"strategy_index"`
	Sequence      uint64 `json:"sequence"`
}
type EvidenceBundle struct {
	SchemaVersion    int                         `json:"schema_version"`
	RunID            string                      `json:"run_id"`
	SessionID        string                      `json:"session_id"`
	Epoch            uint64                      `json:"epoch"`
	ContractSHA256   string                      `json:"contract_sha256"`
	CapabilitySHA256 string                      `json:"capability_sha256"`
	PolicySHA256     string                      `json:"policy_sha256,omitempty"`
	Outcome          *EvidenceOutcome            `json:"outcome,omitempty"`
	JournalComplete  bool                        `json:"journal_complete"`
	Degraded         []string                    `json:"degraded"`
	Actions          []EvidenceAction            `json:"actions"`
	Observations     []EvidenceObservation       `json:"observations"`
	Artifacts        []EvidenceArtifact          `json:"artifacts"`
	EpochTransitions []EvidenceEpochTransition   `json:"epoch_transitions"`
	Interventions    []EvidenceIntervention      `json:"interventions"`
	HumanActions     []EvidenceHumanAction       `json:"human_actions"`
	Recoveries       []EvidenceRecoveryApplied   `json:"recoveries"`
	LocatorHits      []EvidenceLocatorHit        `json:"locator_hits"`
}
type EvidenceIssue struct {
	Code    string `json:"code"`
	Verdict string `json:"verdict"`
}
type EvidenceCriterionResult struct {
	ID       string `json:"id"`
	Required bool   `json:"required"`
	Verdict  string `json:"verdict"`
	Reason   string `json:"reason"`
}
type EvidenceReport struct {
	OK             bool                      `json:"ok"`
	SchemaVersion  int                       `json:"evidence_schema_version"`
	Verdict        string                    `json:"verdict"`
	ContractSHA256 string                    `json:"contract_sha256"`
	BundleSHA256   string                    `json:"bundle_sha256"`
	RunID          string                    `json:"run_id"`
	Criteria       []EvidenceCriterionResult `json:"criteria"`
	Issues         []EvidenceIssue           `json:"issues"`
}
type EvidenceRequest struct {
	Binary, ContractPath, BundlePath, ArtifactsRoot, ContractSHA256, CapabilitySHA256, RunID, SessionID string
	Epoch                                                                                               uint64
}

type evidenceOutput struct {
	bytes.Buffer
	limit int
}

func (b *evidenceOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("verifier output exceeds limit")
	}
	return b.Buffer.Write(p)
}
func VerifyEvidence(ctx context.Context, r EvidenceRequest) (EvidenceReport, error) {
	var report EvidenceReport
	if r.Binary == "" || r.ContractPath == "" || r.BundlePath == "" || r.ArtifactsRoot == "" || r.RunID == "" || r.SessionID == "" || r.Epoch == 0 || len(r.ContractSHA256) != 64 || len(r.CapabilitySHA256) != 64 {
		return report, errors.New("independent evidence expectations are incomplete")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.Binary, "evidence-check", "--contract", r.ContractPath, "--bundle", r.BundlePath, "--artifacts-root", r.ArtifactsRoot, "--expected-contract-sha256", r.ContractSHA256, "--expected-capability-sha256", r.CapabilitySHA256, "--expected-run-id", r.RunID, "--expected-session-id", r.SessionID, "--expected-epoch", strconv.FormatUint(r.Epoch, 10))
	proc.ConfigureGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	out := evidenceOutput{limit: 1 << 20}
	stderr := evidenceOutput{limit: 4096}
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err, timedOut := proc.RunBounded(ctx, cmd.Run)
	if timedOut || ctx.Err() != nil {
		return report, ctx.Err()
	}
	if len(bytes.TrimSpace(out.Bytes())) == 0 {
		if err == nil {
			return report, errors.New("evidence verifier produced no report")
		}
		return report, fmt.Errorf("evidence verifier unavailable or produced no report: %w", err)
	}
	d := json.NewDecoder(bytes.NewReader(out.Bytes()))
	d.DisallowUnknownFields()
	if parseErr := d.Decode(&report); parseErr != nil {
		return report, fmt.Errorf("evidence response: %w", parseErr)
	}
	var trailing json.RawMessage
	if d.Decode(&trailing) != io.EOF {
		return report, errors.New("trailing evidence response data")
	}
	if !report.OK || report.SchemaVersion != 1 || report.RunID != r.RunID || report.ContractSHA256 != r.ContractSHA256 {
		return report, errors.New("evidence report identity or schema mismatch")
	}
	if report.Verdict != "passed" && report.Verdict != "failed" && report.Verdict != "incomplete" {
		return report, errors.New("invalid evidence verdict")
	}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() > 2 {
			return report, fmt.Errorf("evidence verifier process: %w", err)
		}
	}
	if report.Verdict == "passed" && err != nil {
		return report, errors.New("verifier process failed despite passed report")
	}
	return report, nil
}
