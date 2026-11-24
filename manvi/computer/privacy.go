package computer

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

// PrivacyPolicy is supplied by the host, never by model tool arguments.
type PrivacyPolicy struct {
	PID              uint32
	WindowID         uint64
	SensitiveTargets []Selector
	Watched          []string
	// memory is where masked controls lay earlier in the run. It is shared by
	// every copy of the policy and set by EffectivePolicy; without it a frame is
	// masked on its own evidence only.
	memory *maskMemory
}

// sensitiveInputTargets names, by node identity and role, the controls a
// program writes a sensitive parameter into. Those nodes are masked whatever
// the application echoes back into them, because an application may reformat a
// secret ("4111 1111 1111 1111") so that the literal-string Scrub no longer
// recognises it. Every ladder rung of the step's target is included, so
// whichever rung resolves is covered. A rung with no role, name or identifier
// (a purely visual anchor) is skipped: an empty selector matches every node,
// and a visual rung has no semantic node to name. Such a target stays covered
// by the string match only.
func sensitiveInputTargets(c workflow.Capability) []Selector {
	sensitive := make(map[string]bool, len(c.Parameters))
	for _, p := range c.Parameters {
		if p.Sensitive {
			sensitive[p.Name] = true
		}
	}
	var out []Selector
	for _, step := range c.Steps {
		if step.Input.Source != "input" || !sensitive[step.Input.Key] || (step.Kind != "set_value" && step.Kind != "type_text") {
			continue
		}
		for _, rung := range c.Targets[step.Target].Ladder() {
			if s := selectorFrom(rung); s.Role != "" || s.Name != "" || s.Identifier != "" {
				s.Visual = nil
				out = append(out, s)
			}
		}
	}
	return out
}

// Scrub redacts every watched secret from s. A literal occurrence is replaced in
// place; a reformatted one (see foldSensitive) cannot be located in the original
// text, so the whole string is redacted.
func (p PrivacyPolicy) Scrub(s string) string {
	for _, secret := range p.Watched {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, redactedText)
		}
	}
	if p.matcher().foldedCarries(s) {
		return redactedText
	}
	return s
}

// EffectivePolicy is the policy a run applies: the host's policy plus the
// controls the program writes its sensitive parameters into (masked by identity,
// whatever the application echoes back) and the sensitive parameter values
// themselves. StartRun uses it; a caller that sanitizes outside a run (an anchor
// snapshot, a capture after a fence) should too, or those controls are masked
// only by the literal string. The result owns its slices and carries its own
// run-scoped memory of where masked controls lay, so keep one per run.
func EffectivePolicy(program *workflow.Program, policy PrivacyPolicy, inputs map[string]workflow.Value) PrivacyPolicy {
	policy.Watched = append([]string(nil), policy.Watched...)
	policy.SensitiveTargets = ownedSelectors(policy.SensitiveTargets)
	if program != nil {
		capability := program.Capability()
		policy.SensitiveTargets = append(policy.SensitiveTargets, sensitiveInputTargets(capability)...)
		for _, parameter := range capability.Parameters {
			if v, ok := inputs[parameter.Name]; parameter.Sensitive && ok {
				policy.Watched = append(policy.Watched, v.Display())
			}
		}
	}
	policy.memory = &maskMemory{}
	return policy
}

// Sanitize creates an owned observation and masks pixels before callers may
// persist them, forward them to a model, or notify journal observers. Failure
// returns no observation: raw image bytes must not become a fallback artifact.
func Sanitize(raw Observation, policy PrivacyPolicy) (Observation, error) {
	safe, _, err := sanitizeFrame(raw, policy, false)
	return safe, err
}

// frameDigests are the executor-private digests of one capture, each bound to
// where its masks landed. frame is only computed when asked for.
type frameDigests struct {
	semantic [32]byte
	frame    [32]byte
}

// sanitizeFrame is Sanitize with the raw frame decoded once. With digest set it
// also returns the executor-private digests of the capture: the frame digest of
// the unmasked pixels, computed from the same decoded image rather than from a
// second decode of the same bytes, and the semantic one. Both are bound to the
// mask rectangles that were computed, so what the person reviewed includes where
// the masks landed. A sensitive control lying fully outside the captured surface
// has no pixels in the frame: its text is scrubbed and no mask is drawn. One with
// missing or invalid bounds cannot be placed at all and fails the capture.
func sanitizeFrame(raw Observation, policy PrivacyPolicy, digest bool) (Observation, frameDigests, error) {
	if policy.PID == 0 || policy.WindowID == 0 || raw.Window.PID != policy.PID || raw.Window.ID != policy.WindowID {
		return Observation{}, frameDigests{}, errors.New("capture is outside approved window")
	}
	if !raw.Complete || len(raw.Nodes) > 2000 {
		return Observation{}, frameDigests{}, errors.New("incomplete accessibility evidence cannot establish capture privacy")
	}
	if raw.Screenshot.MIMEType != "image/png" || len(raw.Screenshot.Base64) > 16<<20 {
		return Observation{}, frameDigests{}, errors.New("unsupported or oversized screenshot")
	}
	b, err := base64.StdEncoding.DecodeString(raw.Screenshot.Base64)
	if err != nil {
		return Observation{}, frameDigests{}, errors.New("invalid screenshot encoding")
	}
	config, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return Observation{}, frameDigests{}, errors.New("invalid PNG screenshot")
	}
	if config.Width < 1 || config.Height < 1 || int64(config.Width)*int64(config.Height) > 8_000_000 || uint32(config.Width) != raw.Screenshot.Width || uint32(config.Height) != raw.Screenshot.Height {
		return Observation{}, frameDigests{}, errors.New("screenshot dimensions are invalid")
	}
	w := raw.Window.Bounds
	if !finite(w.X) || !finite(w.Y) || !finite(w.Width) || !finite(w.Height) || w.Width <= 0 || w.Height <= 0 {
		return Observation{}, frameDigests{}, errors.New("capture geometry is unavailable")
	}
	decoded, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return Observation{}, frameDigests{}, errors.New("screenshot decode failed")
	}
	masked := image.NewRGBA(decoded.Bounds())
	draw.Draw(masked, masked.Bounds(), decoded, decoded.Bounds().Min, draw.Src)
	out := raw
	out.Nodes = make([]Node, len(raw.Nodes))
	out.Window.Title = policy.Scrub(out.Window.Title)
	plan := policy.plan(raw.Nodes)
	var masks []image.Rectangle
	for i, node := range raw.Nodes {
		n := node
		n.Actions = append([]string(nil), node.Actions...)
		n.NativePath = append([]uint32(nil), node.NativePath...)
		if node.Identifier != nil {
			s := policy.Scrub(*node.Identifier)
			n.Identifier = &s
		}
		if node.Parent != nil {
			s := *node.Parent
			n.Parent = &s
		}
		if node.Bounds != nil {
			bounds := *node.Bounds
			n.Bounds = &bounds
		}
		n.Name = policy.Scrub(n.Name)
		if node.Value != nil {
			v := policy.Scrub(*node.Value)
			n.Value = &v
		}
		if hide := plan.hide[i]; hide.name || hide.value || hide.identifier {
			if hide.name {
				n.Name = redactedText
			}
			if hide.value && n.Value != nil {
				v := redactedText
				n.Value = &v
			}
			if hide.identifier && n.Identifier != nil {
				s := redactedText
				n.Identifier = &s
			}
		}
		if plan.sensitive[i] {
			rect, err := sensitivePixelRect(node, w, config.Width, config.Height)
			if err != nil {
				return Observation{}, frameDigests{}, fmt.Errorf("sensitive control %s: %w", policy.describeNode(node), err)
			}
			if !rect.Empty() {
				draw.Draw(masked, rect, image.NewUniform(color.RGBA{20, 20, 24, 255}), image.Point{}, draw.Src)
				masks = append(masks, rect)
			}
			v := redactedText
			n.Value = &v
		}
		out.Nodes[i] = n
	}
	var result bytes.Buffer
	if err := png.Encode(&result, masked); err != nil {
		return Observation{}, frameDigests{}, err
	}
	out.Screenshot.Base64 = base64.StdEncoding.EncodeToString(result.Bytes())
	digests := frameDigests{semantic: bindMasks(semanticFingerprint(raw), masks)}
	if digest {
		// Taken from the unmasked decode: masked is a separate image, so decoded
		// still holds every original pixel.
		frame, err := frameDigest(raw, nrgbaPix(decoded))
		if err != nil {
			return Observation{}, frameDigests{}, err
		}
		digests.frame = bindMasks(frame, masks)
	}
	return out, digests, nil
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
