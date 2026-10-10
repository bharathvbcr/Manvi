package computer

import (
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
	ReasonExternalInteraction = "external_interaction"
	ReasonHumanTakeover       = "human_takeover"
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

// StuckReasonCode maps a stuck run onto a stable reason code. The code comes
// from the cause the reducer recorded on the state, never from the prose of
// Reason: native text such as an "incomplete observation: deadline banner
// visible" says nothing about why the run stopped.
func StuckReasonCode(s workflow.State) string {
	if s.Phase == workflow.AwaitingApproval {
		return ReasonApprovalPending
	}
	switch s.Cause {
	case workflow.FailureDeadline:
		return ReasonDeadlineNear
	case workflow.FailureNotActionable:
		return ReasonNotActionable
	case workflow.FailureAmbiguousTarget:
		return ReasonLadderExhausted
	case workflow.FailureExternalInteraction:
		return ReasonExternalInteraction
	case workflow.FailureHumanTakeover:
		return ReasonHumanTakeover
	default:
		return ReasonObservationBudget
	}
}
