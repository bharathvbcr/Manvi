package computer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/draw"
	"image/png"
	"math"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

type PixelRect = workflow.PixelRect
type PixelPoint = workflow.PixelPoint
type VisualAnchor = workflow.VisualAnchor
type VisualMatch = workflow.VisualMatch
type ResolvedTarget struct {
	Kind   string       `json:"kind"`
	Node   *Node        `json:"node,omitempty"`
	Visual *VisualMatch `json:"visual,omitempty"`
}
type TargetResolver interface {
	ResolveTarget(context.Context, Session, string, Selector) (ResolvedTarget, error)
}

func (c *Client) ResolveTarget(ctx context.Context, s Session, observationID string, selector Selector) (ResolvedTarget, error) {
	p, err := json.Marshal(struct {
		ObservationID string   `json:"observation_id"`
		Selector      Selector `json:"selector"`
	}{observationID, selector})
	if err != nil {
		return ResolvedTarget{}, err
	}
	raw, err := c.Call(ctx, "resolve_target", &s, p)
	if err != nil {
		return ResolvedTarget{}, err
	}
	var reply ResolvedTarget
	if err = workflow.DecodeStrict(raw, &reply); err != nil {
		return ResolvedTarget{}, err
	}
	if err = validateResolved(reply, selector); err != nil {
		return ResolvedTarget{}, err
	}
	return reply, nil
}
func validateResolved(r ResolvedTarget, s Selector) error {
	if s.Visual == nil {
		if r.Kind != "semantic" || r.Visual != nil || r.Node == nil || r.Node.ID == "" {
			return errors.New("invalid semantic target response")
		}
		return nil
	}
	v := r.Visual
	if r.Kind != "visual" || r.Node != nil || v == nil || v.TargetID == "" || v.AnchorSHA256 != s.Visual.SHA256 || !v.Matched.Inside(s.Visual.FrameWidth, s.Visual.FrameHeight) || v.Matched.Width != s.Visual.Width || v.Matched.Height != s.Visual.Height {
		return errors.New("invalid visual target response")
	}
	if v.FrameSHA256 != "" {
		if _, err := hex.DecodeString(v.FrameSHA256); err != nil || len(v.FrameSHA256) != 64 {
			return errors.New("invalid visual frame digest")
		}
	}
	b := v.ScreenBounds
	if !finite(b.X) || !finite(b.Y) || !finite(b.Width) || !finite(b.Height) || b.Width <= 0 || b.Height <= 0 {
		return errors.New("invalid visual screen bounds")
	}
	return nil
}
func resolveTarget(ctx context.Context, d Desktop, s Session, observationID string, selector Selector) (ResolvedTarget, error) {
	if selector.Visual != nil {
		resolver, ok := d.(TargetResolver)
		if !ok {
			return ResolvedTarget{}, errors.New("desktop does not support native visual resolution")
		}
		r, err := resolver.ResolveTarget(ctx, s, observationID, selector)
		if err != nil {
			return ResolvedTarget{}, err
		}
		return r, validateResolved(r, selector)
	}
	n, err := d.Resolve(ctx, s, observationID, selector)
	if err != nil {
		return ResolvedTarget{}, err
	}
	return ResolvedTarget{Kind: "semantic", Node: &n}, nil
}
func selectorFrom(s workflow.Selector) Selector {
	s = s.Clone()
	return Selector{Role: s.Role, Name: s.Name, Identifier: s.Identifier, AncestorName: s.Ancestor, Visual: s.Visual}
}

// Check the trusted current frame before resolving any imported anchor. The
// native matcher uses private pixels; a protected search would otherwise become
// a template oracle even when creation and clicks require human approval.
func validateVisualPrivacy(raw Observation, policy PrivacyPolicy, a *VisualAnchor) error {
	if a == nil {
		return nil
	}
	if err := a.Validate(); err != nil {
		return err
	}
	if a.FrameWidth != raw.Screenshot.Width || a.FrameHeight != raw.Screenshot.Height || a.WindowWidth != raw.Window.Bounds.Width || a.WindowHeight != raw.Window.Bounds.Height || a.Scale != raw.Window.Scale {
		return errors.New("visual geometry differs from current scoped observation")
	}
	return checkProtectedSearch(raw, policy, a.Search)
}
func checkProtectedSearch(raw Observation, policy PrivacyPolicy, search PixelRect) error {
	w, h := raw.Screenshot.Width, raw.Screenshot.Height
	if !search.Inside(w, h) {
		return errors.New("visual search is outside scoped frame")
	}
	region := image.Rect(int(search.X), int(search.Y), int(search.X+search.Width), int(search.Y+search.Height))
	for _, n := range raw.Nodes {
		if policy.sensitive(n) || (n.Value != nil && *n.Value == "[redacted]") {
			r, err := sensitivePixelRect(n, raw.Window.Bounds, int(w), int(h))
			if err != nil {
				return err
			}
			if r.Overlaps(region) {
				return errors.New("visual search overlaps protected pixels")
			}
		}
	}
	return nil
}

// CreateVisualAnchor accepts a host-owned observation and privacy policy. The
// caller must bind this operation to the current live run/epoch/observation ID.
// Pixel coordinates only select a bounded crop/search; callers cannot supply
// image bytes or promote a match into an accessibility node.
func CreateVisualAnchor(raw Observation, policy PrivacyPolicy, crop, search PixelRect, click PixelPoint) (VisualAnchor, error) {
	safe, err := Sanitize(raw, policy)
	if err != nil {
		return VisualAnchor{}, err
	}
	w, h := safe.Screenshot.Width, safe.Screenshot.Height
	if !crop.Inside(w, h) || !search.Inside(w, h) || crop.X < search.X || crop.Y < search.Y || uint64(crop.X)+uint64(crop.Width) > uint64(search.X)+uint64(search.Width) || uint64(crop.Y)+uint64(crop.Height) > uint64(search.Y)+uint64(search.Height) {
		return VisualAnchor{}, errors.New("crop must be within the scoped frame and search region")
	}
	cropRect := image.Rect(int(crop.X), int(crop.Y), int(crop.X+crop.Width), int(crop.Y+crop.Height))
	if err := checkProtectedSearch(raw, policy, search); err != nil {
		return VisualAnchor{}, err
	}
	encoded, err := base64.StdEncoding.DecodeString(safe.Screenshot.Base64)
	if err != nil {
		return VisualAnchor{}, err
	}
	frame, err := png.Decode(bytes.NewReader(encoded))
	if err != nil {
		return VisualAnchor{}, err
	}
	img := image.NewNRGBA(image.Rect(0, 0, int(crop.Width), int(crop.Height)))
	draw.Draw(img, img.Bounds(), frame, cropRect.Min, draw.Src)
	var out bytes.Buffer
	if err = png.Encode(&out, img); err != nil {
		return VisualAnchor{}, err
	}
	digest := sha256.Sum256(out.Bytes())
	a := VisualAnchor{PNGBase64: base64.StdEncoding.EncodeToString(out.Bytes()), SHA256: hex.EncodeToString(digest[:]), Width: crop.Width, Height: crop.Height, FrameWidth: w, FrameHeight: h, WindowWidth: raw.Window.Bounds.Width, WindowHeight: raw.Window.Bounds.Height, Scale: raw.Window.Scale, Search: search, Click: click}
	if err = a.Validate(); err != nil {
		return VisualAnchor{}, err
	}
	return a, nil
}
func sensitivePixelRect(n Node, w Bounds, width, height int) (image.Rectangle, error) {
	if n.Bounds == nil || !finite(n.Bounds.X) || !finite(n.Bounds.Y) || !finite(n.Bounds.Width) || !finite(n.Bounds.Height) || n.Bounds.Width <= 0 || n.Bounds.Height <= 0 {
		return image.Rectangle{}, errors.New("sensitive region has no reliable bounds")
	}
	sx, sy := float64(width)/w.Width, float64(height)/w.Height
	r := image.Rect(int(math.Floor((n.Bounds.X-w.X)*sx))-2, int(math.Floor((n.Bounds.Y-w.Y)*sy))-2, int(math.Ceil((n.Bounds.X+n.Bounds.Width-w.X)*sx))+2, int(math.Ceil((n.Bounds.Y+n.Bounds.Height-w.Y)*sy))+2).Intersect(image.Rect(0, 0, width, height))
	if r.Empty() {
		return image.Rectangle{}, errors.New("sensitive bounds do not intersect captured surface")
	}
	return r, nil
}
func approvalFingerprint(o Observation, s workflow.Selector) ([32]byte, error) {
	semantic := semanticFingerprint(o)
	if s.Visual == nil {
		return semantic, nil
	}
	return frameFingerprint(o)
}

// frameFingerprint remains executor-private: even masked regions participate in
// review revalidation, without exporting a digest of potentially guessable data.
func frameFingerprint(o Observation) ([32]byte, error) {
	semantic := semanticFingerprint(o)
	rgba, err := frameRGBA(o)
	if err != nil {
		return [32]byte{}, err
	}
	h := sha256.New()
	h.Write(semantic[:])
	h.Write(rgba)
	geometry, err := json.Marshal(struct {
		Bounds Bounds
		Scale  float64
	}{o.Window.Bounds, o.Window.Scale})
	if err != nil {
		return [32]byte{}, err
	}
	h.Write(geometry)
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}
func frameRGBA(o Observation) ([]byte, error) {
	if o.Screenshot.MIMEType != "image/png" || len(o.Screenshot.Base64) > 16<<20 {
		return nil, errors.New("unsupported or oversized frame")
	}
	raw, err := base64.StdEncoding.DecodeString(o.Screenshot.Base64)
	if err != nil {
		return nil, err
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || uint64(cfg.Width)*uint64(cfg.Height) > 8_000_000 || uint32(cfg.Width) != o.Screenshot.Width || uint32(cfg.Height) != o.Screenshot.Height {
		return nil, errors.New("invalid frame dimensions")
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	rgba := image.NewNRGBA(img.Bounds())
	draw.Draw(rgba, rgba.Bounds(), img, img.Bounds().Min, draw.Src)
	return rgba.Pix, nil
}

// Public matches bind the pixels a human actually sees. Native raw frame hashes
// remain private to the broker and the transient resolution response.
func publicVisualMatch(safe Observation, v VisualMatch) (VisualMatch, error) {
	rgba, err := frameRGBA(safe)
	if err != nil {
		return VisualMatch{}, err
	}
	sum := sha256.Sum256(rgba)
	v.FrameSHA256 = hex.EncodeToString(sum[:])
	return v, nil
}
