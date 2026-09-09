package workflow

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func visualDocument(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(x * 30), uint8(y * 30), 20, 255})
		}
	}
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b.Bytes())
	c := fixture(t, true).Capability()
	c.Targets["search"] = Selector{Visual: &VisualAnchor{PNGBase64: base64.StdEncoding.EncodeToString(b.Bytes()), SHA256: hex.EncodeToString(h[:]), Width: 8, Height: 8, FrameWidth: 100, FrameHeight: 100, WindowWidth: 100, WindowHeight: 100, Scale: 1, Search: PixelRect{Width: 50, Height: 50}, Click: PixelPoint{4, 4}}}
	c.Steps[0].Kind = "click"
	c.Steps[0].Effect = "change"
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestVisualClickCapabilityCompiles(t *testing.T) {
	if _, err := Compile(visualDocument(t)); err != nil {
		t.Fatalf("approved visual click with observed terminal checkpoint rejected: %v", err)
	}
}
func TestVisualCompilerRejectsMixedSelectorsAndUnapprovedOrObservedPixels(t *testing.T) {
	for _, mutation := range []func(*Capability){
		func(c *Capability) { s := c.Targets["search"]; s.Name = "Search"; c.Targets["search"] = s },
		func(c *Capability) { c.Steps[0].Effect = "read" },
		func(c *Capability) { c.Steps[0].Kind = "press" },
		func(c *Capability) {
			c.Steps[0].Kind = "extract"
			c.Steps[0].Output = "pixels"
			c.Steps[0].OutputType = "string"
			c.Steps[0].Effect = "read"
		},
		func(c *Capability) { c.Targets["search"].Visual.SHA256 = strings.Repeat("0", 64) },
		func(c *Capability) { c.Targets["search"].Visual.Click.X = 8 },
		func(c *Capability) { c.Targets["search"].Visual.Search.X = ^uint32(0) },
		func(c *Capability) { c.Targets["search"].Visual.WindowWidth = 0 },
	} {
		var c Capability
		if err := json.Unmarshal(visualDocument(t), &c); err != nil {
			t.Fatal(err)
		}
		mutation(&c)
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = Compile(raw); err == nil {
			t.Fatal("unsafe visual artifact compiled")
		}
	}
}
func TestVisualProgramAndEventsOwnTheirAnchorsAndMatches(t *testing.T) {
	p, err := Compile(visualDocument(t))
	if err != nil {
		t.Fatal(err)
	}
	c := p.Capability()
	c.Targets["search"].Visual.SHA256 = "mutated"
	if p.Capability().Targets["search"].Visual.SHA256 == "mutated" {
		t.Fatal("program exposes anchor storage")
	}
	s := start(t, p)
	a := p.Capability().Targets["search"].Visual
	e := inputEvent(s, "observed")
	e.Observation = Observation{ID: "frame", TargetID: "visual-1", Matches: 1, Complete: true, Actionable: true, Visual: &VisualMatch{TargetID: "visual-1", AnchorSHA256: a.SHA256, FrameSHA256: strings.Repeat("a", 64), Matched: PixelRect{Width: 8, Height: 8}}}
	next, _, err := Reduce(p, s, e, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.Observation.Visual.TargetID = "mutated"
	if next.Observation.Visual.TargetID != "visual-1" || next.Phase != AwaitingApproval {
		t.Fatal("visual event aliases state or bypasses approval")
	}
}
