package workflow

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// Overlay is a compile-time tenant patch over a base capability. It may replace
// target ladders and optionally narrow limits — never widen budgets or invent
// new target keys. Digest of CompileWithOverlay is sha256(base ‖ overlay).
type Overlay struct {
	SchemaVersion int                 `json:"schema_version"`
	CapabilityID  string              `json:"capability_id"`
	Revision      string              `json:"revision"`
	Tenant        string              `json:"tenant"`
	Targets       map[string]Selector `json:"targets,omitempty"`
	Limits        *Limits             `json:"limits,omitempty"`
}

// DecodeOverlay strictly decodes and validates overlay identity/shape.
func DecodeOverlay(raw []byte) (Overlay, error) {
	var o Overlay
	if err := DecodeStrict(raw, &o); err != nil {
		return Overlay{}, fmt.Errorf("overlay: %w", err)
	}
	if err := o.validate(); err != nil {
		return Overlay{}, err
	}
	return o, nil
}

func (o Overlay) validate() error {
	if o.SchemaVersion != SchemaVersion {
		return errors.New("unsupported overlay schema")
	}
	if !namePattern.MatchString(o.CapabilityID) || o.Revision == "" || o.Tenant == "" {
		return errors.New("overlay requires capability_id, revision, and tenant")
	}
	if len(o.Targets) == 0 && o.Limits == nil {
		return errors.New("overlay must replace targets or narrow limits")
	}
	if len(o.Targets) > 128 {
		return errors.New("overlay supports at most 128 target replacements")
	}
	for k, s := range o.Targets {
		if !namePattern.MatchString(k) {
			return fmt.Errorf("overlay target %q needs a valid name", k)
		}
		if err := validateTargetSelector(k, s); err != nil {
			return err
		}
	}
	if o.Limits != nil {
		if err := validateLimits(*o.Limits); err != nil {
			return fmt.Errorf("overlay limits: %w", err)
		}
	}
	return nil
}

func validateLimits(l Limits) error {
	if l.MaxActions < 1 || l.MaxActions > 1000 || l.ActiveSeconds < 1 || l.ActiveSeconds > 3600 || l.ObservationAttempts < 1 || l.ObservationAttempts > 5 || l.InterventionSeconds < 1 || l.InterventionSeconds > 3600 {
		return errors.New("limits are missing or outside supported bounds")
	}
	return nil
}

// ApplyOverlay returns a copy of base with overlay target replacements and
// optional narrowed limits applied. The overlay capability_id/revision must
// match base; target keys must already exist on base; limits may only shrink.
func ApplyOverlay(base Capability, o Overlay) (Capability, error) {
	if err := o.validate(); err != nil {
		return Capability{}, err
	}
	if o.CapabilityID != base.ID || o.Revision != base.Revision {
		return Capability{}, errors.New("overlay capability_id/revision does not match base")
	}
	out := base
	out.Parameters = append([]Parameter(nil), base.Parameters...)
	out.Steps = append([]Step(nil), base.Steps...)
	out.Outputs = append([]OutputDecl(nil), base.Outputs...)
	out.Outcomes = make([]OutcomeDef, len(base.Outcomes))
	for i, oc := range base.Outcomes {
		oc.Outputs = append([]string(nil), oc.Outputs...)
		out.Outcomes[i] = oc
	}
	out.Recoveries = append([]Recovery(nil), base.Recoveries...)
	out.Targets = make(map[string]Selector, len(base.Targets))
	for k, v := range base.Targets {
		out.Targets[k] = v.Clone()
	}
	for k, replacement := range o.Targets {
		if _, ok := out.Targets[k]; !ok {
			return Capability{}, fmt.Errorf("overlay references unknown target %q", k)
		}
		out.Targets[k] = replacement.Clone()
	}
	if o.Limits != nil {
		narrowed, err := narrowLimits(base.Limits, *o.Limits)
		if err != nil {
			return Capability{}, err
		}
		out.Limits = narrowed
	}
	return out, nil
}

func narrowLimits(base, over Limits) (Limits, error) {
	if err := validateLimits(over); err != nil {
		return Limits{}, err
	}
	if over.MaxActions > base.MaxActions || over.ActiveSeconds > base.ActiveSeconds || over.ObservationAttempts > base.ObservationAttempts || over.InterventionSeconds > base.InterventionSeconds {
		return Limits{}, errors.New("overlay limits widen base budgets")
	}
	return over, nil
}

// CompileWithOverlay merges overlay onto base, compiles the result, and sets
// Digest to sha256(baseRaw ‖ overlayRaw) so identity covers both documents.
func CompileWithOverlay(baseRaw, overlayRaw []byte) (*Program, error) {
	o, err := DecodeOverlay(overlayRaw)
	if err != nil {
		return nil, err
	}
	var base Capability
	if err = DecodeStrict(baseRaw, &base); err != nil {
		return nil, fmt.Errorf("capability: %w", err)
	}
	merged, err := ApplyOverlay(base, o)
	if err != nil {
		return nil, err
	}
	mergedRaw, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	p, err := Compile(mergedRaw)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	_, _ = h.Write(baseRaw)
	_, _ = h.Write(overlayRaw)
	p.digest = hex.EncodeToString(h.Sum(nil))
	// Preserve merged artifact bytes for callers that persist the effective program.
	p.raw = bytes.Clone(mergedRaw)
	return p, nil
}

// OverlayDigest returns sha256(base ‖ overlay) without compiling.
func OverlayDigest(baseRaw, overlayRaw []byte) string {
	h := sha256.New()
	_, _ = h.Write(baseRaw)
	_, _ = h.Write(overlayRaw)
	return hex.EncodeToString(h.Sum(nil))
}
