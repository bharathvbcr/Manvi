package computer

import (
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

// Stuck reason codes emitted with InterventionRequest.
const (
	ReasonLadderExhausted     = "ladder_exhausted"
	ReasonNotActionable       = "not_actionable"
	ReasonRecoveriesExhausted = "recoveries_exhausted"
	ReasonApprovalPending     = "approval_pending"
	ReasonDeadlineNear        = "deadline_near"
	ReasonObservationBudget   = "observation_budget"
)

const (
	InterventionRequested = "requested"
	InterventionReturned  = "returned"
	InterventionAbandoned = "abandoned"
)

const (
	ControllerAutomation = "automation"
	ControllerHuman      = "human"
)

// InterventionRequest is the typed handoff record when automation is stuck.
type InterventionRequest struct {
	ID            string `json:"id"`
	Run           string `json:"run"`
	Capability    string `json:"capability"`
	Step          string `json:"step"`
	ReasonCode    string `json:"reason_code"`
	ObservationID string `json:"observation_id,omitempty"`
	FrameID       string `json:"frame_id,omitempty"`
	Controller    string `json:"controller"`
	Status        string `json:"status"`
	Reason        string `json:"reason,omitempty"`
	Sequence      uint64 `json:"sequence,omitempty"`
}

// StuckReasonCode maps reducer pause/approval reasons onto stable codes.
func StuckReasonCode(phase workflow.Phase, reason string) string {
	if phase == workflow.AwaitingApproval {
		return ReasonApprovalPending
	}
	lower := strings.ToLower(reason)
	switch {
	case strings.Contains(lower, "intervention deadline") || strings.Contains(lower, "deadline"):
		return ReasonDeadlineNear
	case strings.Contains(lower, "not actionable"):
		return ReasonNotActionable
	case strings.Contains(lower, "recovery"):
		return ReasonRecoveriesExhausted
	case strings.Contains(lower, "target resolves to 0") || strings.Contains(lower, "ladder"):
		return ReasonLadderExhausted
	default:
		return ReasonObservationBudget
	}
}
