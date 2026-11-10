package computer

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"reflect"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

// The functions below are the screenshot path exactly as it stood before each
// frame was decoded once: Sanitize decoding and re-encoding, then
// frameFingerprint decoding the raw PNG a second time. They are the oracle the
// production path is held to, byte for byte, and the baseline the benchmark
// measures against. Do not "improve" them.

func oracleSanitize(raw Observation, policy PrivacyPolicy) (Observation, error) {
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
	if config.Width < 1 || config.Height < 1 || int64(config.Width)*int64(config.Height) > 8_000_000 || int64(config.Width) != int64(raw.Screenshot.Width) || int64(config.Height) != int64(raw.Screenshot.Height) {
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

func oracleFrameFingerprint(o Observation) ([32]byte, error) {
	semantic := semanticFingerprint(o)
	if o.Screenshot.MIMEType != "image/png" || len(o.Screenshot.Base64) > 16<<20 {
		return [32]byte{}, errors.New("unsupported or oversized frame")
	}
	raw, err := base64.StdEncoding.DecodeString(o.Screenshot.Base64)
	if err != nil {
		return [32]byte{}, err
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || uint64(cfg.Width)*uint64(cfg.Height) > 8_000_000 || int64(cfg.Width) != int64(o.Screenshot.Width) || int64(cfg.Height) != int64(o.Screenshot.Height) {
		return [32]byte{}, errors.New("invalid frame dimensions")
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return [32]byte{}, err
	}
	rgba := image.NewNRGBA(img.Bounds())
	draw.Draw(rgba, rgba.Bounds(), img, img.Bounds().Min, draw.Src)
	h := sha256.New()
	h.Write(semantic[:])
	h.Write(rgba.Pix)
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

func oracleCapture(raw Observation, policy PrivacyPolicy) (Observation, [32]byte, error) {
	safe, err := oracleSanitize(raw, policy)
	if err != nil {
		return Observation{}, [32]byte{}, err
	}
	fp, err := oracleFrameFingerprint(raw)
	return safe, fp, err
}

func encodePNG(t testing.TB, img image.Image) string {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b.Bytes())
}

// byteOf narrows a test-chosen pixel value that is already within 0..255.
func byteOf(n int) uint8 {
	if n < 0 || n > 255 {
		panic("pixel value out of byte range")
	}
	return uint8(n)
}

// dim32 narrows a test-chosen image dimension.
func dim32(n int) uint32 {
	if n < 0 || int64(n) > math.MaxUint32 {
		panic("image dimension out of range")
	}
	return uint32(n)
}

// patterned returns a frame with structure at every scale, so it compresses like
// a real window rather than like noise or like a flat colour.
func patterned(w, h int, kind string) image.Image {
	set := func(x, y int) color.NRGBA {
		v := byteOf((x*7 + y*3) % 251)
		if (x/32+y/32)%2 == 0 {
			v = 255 - v
		}
		a := uint8(255)
		if kind == "alpha" && (x+y)%5 == 0 {
			a = 128
		}
		return color.NRGBA{v, byteOf(x % 256), byteOf(y % 256), a}
	}
	switch kind {
	case "rgba":
		m := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				m.Set(x, y, set(x, y))
			}
		}
		return m
	case "gray":
		m := image.NewGray(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				m.Set(x, y, color.Gray{set(x, y).R})
			}
		}
		return m
	case "paletted":
		pal := color.Palette{color.White, color.Black, color.RGBA{200, 30, 30, 255}, color.RGBA{30, 30, 200, 255}}
		m := image.NewPaletted(image.Rect(0, 0, w, h), pal)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				m.SetColorIndex(x, y, byteOf((x/8+y/8)%4))
			}
		}
		return m
	default: // nrgba, alpha
		m := image.NewNRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				m.SetNRGBA(x, y, set(x, y))
			}
		}
		return m
	}
}

func parityObservation(t testing.TB, w, h int, kind string, nodes []Node) Observation {
	t.Helper()
	return Observation{ID: "o", Epoch: 1, Complete: true,
		Window:     Window{PID: 1, ID: 2, Title: "Bank - synthetic-secret", Bounds: Bounds{0, 0, float64(w), float64(h)}, Scale: 1},
		Nodes:      nodes,
		Screenshot: Screenshot{MIMEType: "image/png", Base64: encodePNG(t, patterned(w, h, kind)), Width: dim32(w), Height: dim32(h)}}
}

func sensitiveNode(id, name string, x, y, w, h float64) Node {
	v := "synthetic-secret"
	return Node{ID: id, Role: "text_field", Name: name, Value: &v, Enabled: true, Bounds: &Bounds{x, y, w, h}}
}

func TestCaptureMatchesTheDecodeTwiceOracleByteForByte(t *testing.T) {
	const W, H = 120, 90
	policy := PrivacyPolicy{PID: 1, WindowID: 2, Watched: []string{"synthetic-secret"}, SensitiveTargets: []Selector{{Name: "Member ID"}}}
	plain := Node{ID: "p", Role: "button", Name: "Search", Enabled: true, Bounds: &Bounds{50, 40, 20, 10}, Actions: []string{"press"}}
	regions := map[string][]Node{
		"none":            {plain},
		"left edge":       {sensitiveNode("a", "Member ID", 0, 30, 15, 20)},
		"top edge":        {sensitiveNode("a", "Member ID", 30, 0, 20, 12)},
		"right edge":      {sensitiveNode("a", "Member ID", W-15, 30, 15, 20)},
		"bottom edge":     {sensitiveNode("a", "Member ID", 30, H-12, 20, 12)},
		"corner":          {sensitiveNode("a", "Member ID", 0, 0, 10, 10), sensitiveNode("b", "Member ID", W-10, H-10, 10, 10)},
		"overhangs frame": {sensitiveNode("a", "Member ID", W-5, H-5, 40, 40)},
		"whole frame":     {sensitiveNode("a", "Member ID", 0, 0, W, H)},
		"overlapping":     {sensitiveNode("a", "Member ID", 20, 20, 40, 30), sensitiveNode("b", "Member ID", 40, 35, 40, 30), plain},
		"watched value":   {{ID: "w", Role: "text_field", Name: "Notes", Value: func() *string { s := "see synthetic-secret"; return &s }(), Enabled: true, Bounds: &Bounds{60, 60, 30, 10}}},
	}
	for _, kind := range []string{"rgba", "nrgba", "alpha", "gray", "paletted"} {
		for name, nodes := range regions {
			t.Run(kind+"/"+name, func(t *testing.T) {
				raw := parityObservation(t, W, H, kind, nodes)
				wantSafe, wantFP, wantErr := oracleCapture(raw, policy)
				var r workResult
				gotErr := r.capture(raw, policy)
				if (wantErr == nil) != (gotErr == nil) {
					t.Fatalf("error disagreement: oracle %v, capture %v", wantErr, gotErr)
				}
				if wantErr != nil {
					return
				}
				if r.observation.Screenshot.Base64 != wantSafe.Screenshot.Base64 {
					t.Fatal("masked screenshot bytes differ from the oracle")
				}
				if !reflect.DeepEqual(*r.observation, wantSafe) {
					t.Fatal("sanitized observation differs from the oracle")
				}
				if r.takeoverFingerprint != wantFP {
					t.Fatal("frame fingerprint differs from the oracle")
				}
			})
		}
	}
}

func TestCaptureRefusesWhatTheOracleRefuses(t *testing.T) {
	policy := PrivacyPolicy{PID: 1, WindowID: 2}
	good := parityObservation(t, 40, 30, "rgba", nil)
	cases := map[string]func(*Observation){
		"wrong pid":        func(o *Observation) { o.Window.PID = 9 },
		"incomplete":       func(o *Observation) { o.Complete = false },
		"wrong mime":       func(o *Observation) { o.Screenshot.MIMEType = "image/jpeg" },
		"bad base64":       func(o *Observation) { o.Screenshot.Base64 = "!!!" },
		"not a png":        func(o *Observation) { o.Screenshot.Base64 = base64.StdEncoding.EncodeToString([]byte("nope")) },
		"width lies":       func(o *Observation) { o.Screenshot.Width++ },
		"no geometry":      func(o *Observation) { o.Window.Bounds = Bounds{} },
		"truncated pixels": func(o *Observation) { o.Screenshot.Base64 = o.Screenshot.Base64[:len(o.Screenshot.Base64)/2] },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			raw := good
			mutate(&raw)
			_, _, wantErr := oracleCapture(raw, policy)
			var r workResult
			gotErr := r.capture(raw, policy)
			if wantErr == nil || gotErr == nil || r.observation != nil {
				t.Fatalf("oracle %v, capture %v, observation kept %v", wantErr, gotErr, r.observation != nil)
			}
		})
	}
}

func TestApprovalReusesTheCaptureDigestWithoutChangingIt(t *testing.T) {
	policy := PrivacyPolicy{PID: 1, WindowID: 2}
	raw := parityObservation(t, 64, 48, "alpha", []Node{{ID: "p", Role: "button", Name: "Go", Enabled: true}})
	var r workResult
	if err := r.capture(raw, policy); err != nil {
		t.Fatal(err)
	}
	for name, sel := range map[string]workflow.Selector{"semantic": {Role: "button", Name: "Go"}, "visual": {Visual: &workflow.VisualAnchor{}}} {
		want, err := approvalFingerprint(raw, sel)
		if err != nil {
			t.Fatal(err)
		}
		if got := r.approval(raw, sel); got != want {
			t.Fatalf("%s: approval digest differs from approvalFingerprint", name)
		}
	}
}

func BenchmarkSanitizeAndFingerprint(b *testing.B) {
	policy := PrivacyPolicy{PID: 1, WindowID: 2, Watched: []string{"synthetic-secret"}, SensitiveTargets: []Selector{{Name: "Member ID"}}}
	for _, size := range []struct {
		name string
		w, h int
	}{{"small", 300, 200}, {"2MP", 1600, 1250}, {"8MP", 3200, 2500}} {
		raw := parityObservation(b, size.w, size.h, "nrgba", []Node{sensitiveNode("a", "Member ID", 10, 10, 200, 30)})
		b.Run(size.name+"/old", func(b *testing.B) {
			b.SetBytes(int64(len(raw.Screenshot.Base64)))
			for i := 0; i < b.N; i++ {
				if _, _, err := oracleCapture(raw, policy); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(size.name+"/new", func(b *testing.B) {
			b.SetBytes(int64(len(raw.Screenshot.Base64)))
			for i := 0; i < b.N; i++ {
				var r workResult
				if err := r.capture(raw, policy); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
