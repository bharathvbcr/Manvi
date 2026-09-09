package computer

import (
	"context"
	"errors"
	"slices"
	"strings"
)

func (r *workResult) capture(raw Observation, privacy PrivacyPolicy) error {
	safe, err := Sanitize(raw, privacy)
	if err != nil {
		return err
	}
	fingerprint, err := frameFingerprint(raw)
	if err != nil {
		return err
	}
	r.observation = &safe
	r.takeoverFingerprint = fingerprint
	return nil
}

// A review is captured before focus invalidates the native observation and HID
// baseline. It owns the exact action payload and the target the person selected.
type humanActionReview struct {
	action      Action
	epoch       uint64
	target      Node
	fingerprint [32]byte
}

type humanRevalidation struct {
	observation *Observation
	targetID    string
}

func reviewHumanAction(c Control, o *Observation, fingerprint [32]byte) (humanActionReview, error) {
	a := c.Action
	if o == nil || !o.Complete || o.ID == "" || o.Epoch != c.Epoch || a.ObservationID != o.ID || (c.ObservationID != "" && c.ObservationID != o.ID) || (c.ActionID != "" && c.ActionID != a.ActionID) {
		return humanActionReview{}, errors.New("human action requires the current complete paused observation")
	}
	if a.ActionID == "" || len(a.ActionID) > 160 || a.TargetID == "" || len(a.TargetID) > 160 || a.ApprovalID != "" || (a.Effect != "read" && a.Effect != "change") {
		return humanActionReview{}, errors.New("human action identity or approval binding is invalid")
	}
	var target *Node
	for i := range o.Nodes {
		if o.Nodes[i].ID == a.TargetID {
			if target != nil {
				return humanActionReview{}, errors.New("human target identity is ambiguous")
			}
			target = &o.Nodes[i]
		}
	}
	if target == nil || !target.Enabled || !slices.Contains(target.Actions, a.Kind) {
		return humanActionReview{}, errors.New("human target does not advertise the requested action")
	}
	if a.Text != nil && (len(*a.Text) > 8192 || strings.ContainsRune(*a.Text, '\x00')) {
		return humanActionReview{}, errors.New("human input text is invalid or exceeds its limit")
	}
	switch a.Kind {
	case "set_value", "type_text":
		if !target.Editable || a.Text == nil || a.ScrollY != nil {
			return humanActionReview{}, errors.New("human text input requires an editable target and text")
		}
	case "press", "click":
		if a.Text != nil || a.ScrollY != nil {
			return humanActionReview{}, errors.New("human button action contains unrelated input")
		}
	case "scroll":
		if a.Text != nil || a.ScrollY == nil || *a.ScrollY == 0 || *a.ScrollY < -20 || *a.ScrollY > 20 {
			return humanActionReview{}, errors.New("human scroll input is invalid")
		}
	default:
		return humanActionReview{}, errors.New("unsupported human input kind")
	}
	// This takeover surface selects semantic nodes, not free desktop coordinates.
	if a.X != nil || a.Y != nil {
		return humanActionReview{}, errors.New("human takeover requires the selected semantic target")
	}
	return humanActionReview{action: ownedAction(a), epoch: c.Epoch, target: *target, fingerprint: fingerprint}, nil
}

func sameHumanTarget(a, b Node) bool {
	if a.Role != b.Role || a.Name != b.Name || a.Enabled != b.Enabled || a.Editable != b.Editable || !slices.Equal(a.NativePath, b.NativePath) || !slices.Equal(a.Actions, b.Actions) {
		return false
	}
	if (a.Identifier == nil) != (b.Identifier == nil) || (a.Identifier != nil && *a.Identifier != *b.Identifier) {
		return false
	}
	return (a.Bounds == nil && b.Bounds == nil) || (a.Bounds != nil && b.Bounds != nil && *a.Bounds == *b.Bounds)
}

func revalidateHumanAction(ctx context.Context, opts RunOptions, session Session, review humanActionReview) (humanRevalidation, error) {
	var result humanRevalidation
	if err := ctx.Err(); err != nil {
		return result, err
	}
	focuser, ok := opts.Desktop.(Focuser)
	if !ok {
		return result, errors.New("desktop cannot focus for human action revalidation")
	}
	if _, err := focuser.Focus(ctx, session); err != nil {
		return result, err
	}
	raw, err := opts.Desktop.Observe(ctx, session)
	if err != nil {
		return result, err
	}
	if raw.ID == "" || raw.ID == review.action.ObservationID || raw.Epoch != review.epoch || raw.Epoch != session.Epoch {
		return result, errors.New("human action revalidation did not produce a fresh same-epoch observation")
	}
	var captured workResult
	if err := captured.capture(raw, opts.Privacy); err != nil {
		return result, err
	}
	if captured.takeoverFingerprint != review.fingerprint {
		return result, errors.New("application state or frame changed during human review; refresh and select the action again")
	}
	result.observation = captured.observation
	for _, node := range result.observation.Nodes {
		if sameHumanTarget(review.target, node) {
			if result.targetID != "" || node.ID == "" {
				return result, errors.New("human action target became ambiguous")
			}
			result.targetID = node.ID
		}
	}
	if result.targetID == "" {
		return result, errors.New("human action target changed during review")
	}
	matches := 0
	for _, node := range result.observation.Nodes {
		if node.ID == result.targetID {
			matches++
		}
	}
	if matches != 1 {
		return result, errors.New("human action target identity became ambiguous")
	}
	return result, ctx.Err()
}
