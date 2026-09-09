package computer

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strings"
)

// PrivacyPolicy is supplied by the host, never by model tool arguments.
type PrivacyPolicy struct {
	PID              uint32
	WindowID         uint64
	SensitiveTargets []Selector
	Watched          []string
}

func (p PrivacyPolicy) Scrub(s string) string {
	for _, secret := range p.Watched {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	return s
}
func (p PrivacyPolicy) sensitive(n Node) bool {
	for _, target := range p.SensitiveTargets {
		if (target.Role == "" || target.Role == n.Role) && (target.Name == "" || target.Name == n.Name) && (target.Identifier == "" || (n.Identifier != nil && target.Identifier == *n.Identifier)) {
			return true
		}
	}
	for _, secret := range p.Watched {
		if secret != "" && (strings.Contains(n.Name, secret) || (n.Value != nil && strings.Contains(*n.Value, secret))) {
			return true
		}
	}
	return false
}

// Sanitize creates an owned observation and masks pixels before callers may
// persist them, forward them to a model, or notify journal observers. Failure
// returns no observation: raw image bytes must not become a fallback artifact.
func Sanitize(raw Observation, policy PrivacyPolicy) (Observation, error) {
	if policy.PID == 0 || policy.WindowID == 0 || raw.Window.PID != policy.PID || raw.Window.ID != policy.WindowID {
		return Observation{}, errors.New("capture is outside approved window")
	}
	if !raw.Complete || len(raw.Nodes) > 2000 {
		return Observation{}, errors.New("incomplete accessibility evidence cannot establish capture privacy")
	}
	if raw.Screenshot.MIMEType != "image/png" || len(raw.Screenshot.Base64) > 16<<20 {
		return Observation{}, errors.New("unsupported or oversized screenshot")
	}
	b, err := base64.StdEncoding.DecodeString(raw.Screenshot.Base64)
	if err != nil {
		return Observation{}, errors.New("invalid screenshot encoding")
	}
	config, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return Observation{}, errors.New("invalid PNG screenshot")
	}
	if config.Width < 1 || config.Height < 1 || int64(config.Width)*int64(config.Height) > 8_000_000 || uint32(config.Width) != raw.Screenshot.Width || uint32(config.Height) != raw.Screenshot.Height {
		return Observation{}, errors.New("screenshot dimensions are invalid")
	}
	w := raw.Window.Bounds
	if !finite(w.X) || !finite(w.Y) || !finite(w.Width) || !finite(w.Height) || w.Width <= 0 || w.Height <= 0 {
		return Observation{}, errors.New("capture geometry is unavailable")
	}
	decoded, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return Observation{}, errors.New("screenshot decode failed")
	}
	masked := image.NewRGBA(decoded.Bounds())
	draw.Draw(masked, masked.Bounds(), decoded, decoded.Bounds().Min, draw.Src)
	out := raw
	out.Nodes = make([]Node, len(raw.Nodes))
	out.Window.Title = policy.Scrub(out.Window.Title)
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
		if policy.sensitive(node) {
			rect, err := sensitivePixelRect(node, w, config.Width, config.Height)
			if err != nil {
				return Observation{}, err
			}
			draw.Draw(masked, rect, image.NewUniform(color.RGBA{20, 20, 24, 255}), image.Point{}, draw.Src)
			v := "[redacted]"
			n.Value = &v
		}
		out.Nodes[i] = n
	}
	var result bytes.Buffer
	if err := png.Encode(&result, masked); err != nil {
		return Observation{}, err
	}
	out.Screenshot.Base64 = base64.StdEncoding.EncodeToString(result.Bytes())
	return out, nil
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
