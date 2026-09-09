package computer

import "errors"

func validateRecovery(c Control, o *Observation, enabled, used bool) (Selector, error) {
	if !enabled || used {
		return Selector{}, errors.New("assisted recovery is disabled or already consumed")
	}
	a := c.Action
	if o == nil || o.ID == "" || c.ObservationID != o.ID || a.ObservationID != o.ID || c.Epoch != o.Epoch || a.ActionID == "" || c.ActionID != a.ActionID {
		return Selector{}, errors.New("assisted recovery requires the current observation and action identity")
	}
	if a.Effect != "read" || (a.Kind != "press" && a.Kind != "click") || a.Text != nil || a.X != nil || a.Y != nil || a.ScrollY != nil || a.ApprovalID != "" {
		return Selector{}, errors.New("assisted recovery only permits semantic read-only Back/Dismiss button input")
	}
	for _, n := range o.Nodes {
		if n.ID == a.TargetID && n.Role == "button" && n.Enabled && (n.Name == "Back" || n.Name == "Dismiss") {
			return Selector{Role: "button", Name: n.Name}, nil
		}
	}
	return Selector{}, errors.New("assisted recovery target is not an enabled Back/Dismiss button")
}
