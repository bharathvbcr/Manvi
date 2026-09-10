package workflow

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"image/png"
	"math"
)

type PixelRect struct {
	X      uint32 `json:"x"`
	Y      uint32 `json:"y"`
	Width  uint32 `json:"width"`
	Height uint32 `json:"height"`
}
type PixelPoint struct {
	X uint32 `json:"x"`
	Y uint32 `json:"y"`
}
type VisualAnchor struct {
	PNGBase64    string     `json:"png_base64"`
	SHA256       string     `json:"sha256"`
	Width        uint32     `json:"width"`
	Height       uint32     `json:"height"`
	FrameWidth   uint32     `json:"frame_width"`
	FrameHeight  uint32     `json:"frame_height"`
	WindowWidth  float64    `json:"window_width"`
	WindowHeight float64    `json:"window_height"`
	Scale        float64    `json:"scale"`
	Search       PixelRect  `json:"search"`
	Click        PixelPoint `json:"click"`
}
type VisualBounds struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}
type VisualMatch struct {
	TargetID     string       `json:"target_id"`
	AnchorSHA256 string       `json:"anchor_sha256"`
	FrameSHA256  string       `json:"frame_sha256"`
	Matched      PixelRect    `json:"matched"`
	ScreenBounds VisualBounds `json:"screen_bounds"`
}

func (r PixelRect) Inside(w, h uint32) bool {
	return r.Width > 0 && r.Height > 0 && uint64(r.X)+uint64(r.Width) <= uint64(w) && uint64(r.Y)+uint64(r.Height) <= uint64(h)
}
func (a VisualAnchor) Validate() error {
	positive := func(v float64) bool { return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }
	if a.Width < 8 || a.Width > 128 || a.Height < 8 || a.Height > 128 || a.FrameWidth == 0 || a.FrameHeight == 0 || uint64(a.FrameWidth)*uint64(a.FrameHeight) > 8_000_000 || !positive(a.WindowWidth) || !positive(a.WindowHeight) || !positive(a.Scale) || a.Scale > 8 || !a.Search.Inside(a.FrameWidth, a.FrameHeight) || a.Search.Width < a.Width || a.Search.Height < a.Height || a.Click.X >= a.Width || a.Click.Y >= a.Height {
		return errors.New("visual anchor geometry is invalid or exceeds limits")
	}
	if uint64(a.Search.Width-a.Width+1)*uint64(a.Search.Height-a.Height+1) > 1_000_000 {
		return errors.New("visual search exceeds one million placements")
	}
	if len(a.PNGBase64) == 0 || len(a.PNGBase64) > 128<<10 {
		return errors.New("visual PNG exceeds 96 KiB")
	}
	raw, err := base64.StdEncoding.DecodeString(a.PNGBase64)
	if err != nil || len(raw) > 96<<10 {
		return errors.New("invalid visual PNG base64 or size")
	}
	h := sha256.Sum256(raw)
	if a.SHA256 != hex.EncodeToString(h[:]) {
		return errors.New("visual PNG digest differs")
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width != int(a.Width) || cfg.Height != int(a.Height) {
		return errors.New("visual PNG dimensions differ")
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return errors.New("visual PNG is invalid")
	}
	fr, fg, fb, fa := img.At(0, 0).RGBA()
	different := false
	for y := 0; y < cfg.Height; y++ {
		for x := 0; x < cfg.Width; x++ {
			r, g, b, alpha := img.At(x, y).RGBA()
			if alpha != 65535 {
				return errors.New("visual PNG must be opaque")
			}
			if r != fr || g != fg || b != fb || alpha != fa {
				different = true
			}
		}
	}
	if !different {
		return errors.New("visual PNG must be nonuniform")
	}
	return nil
}

func (s Selector) Clone() Selector {
	if s.Visual != nil {
		v := *s.Visual
		s.Visual = &v
	}
	if len(s.Strategies) > 0 {
		strategies := make([]Selector, len(s.Strategies))
		for i, rung := range s.Strategies {
			strategies[i] = rung.Clone()
		}
		s.Strategies = strategies
	}
	return s
}
