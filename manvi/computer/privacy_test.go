package computer

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func observationFixture(t *testing.T) Observation {
	t.Helper()
	m := image.NewRGBA(image.Rect(0, 0, 100, 80))
	for y := 0; y < 80; y++ {
		for x := 0; x < 100; x++ {
			m.Set(x, y, color.White)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	value := "synthetic-member-42"
	bounds := Bounds{10, 10, 40, 20}
	return Observation{ID: "o", Epoch: 1, Complete: true, Window: Window{PID: 1, ID: 2, Bounds: Bounds{0, 0, 100, 80}}, Nodes: []Node{{ID: "n", Role: "text_field", Name: "Member ID", Value: &value, Bounds: &bounds}}, Screenshot: Screenshot{MIMEType: "image/png", Base64: base64.StdEncoding.EncodeToString(b.Bytes()), Width: 100, Height: 80}}
}
func TestPrivacyMasksBeforeSerializationAndOwnsData(t *testing.T) {
	raw := observationFixture(t)
	p := PrivacyPolicy{PID: 1, WindowID: 2, Watched: []string{"synthetic-member-42"}, SensitiveTargets: []Selector{{Name: "Member ID"}}}
	safe, err := Sanitize(raw, p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(safe)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "synthetic-member-42") {
		t.Fatal("sensitive value exported")
	}
	decoded, err := base64.StdEncoding.DecodeString(safe.Screenshot.Base64)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(decoded))
	if err != nil {
		t.Fatal(err)
	}
	r, _, _, _ := img.At(20, 20).RGBA()
	if r > 10000 {
		t.Fatal("pixels not masked")
	}
	*safe.Nodes[0].Value = "modified"
	safe.Nodes[0].Bounds.X = 90
	if *raw.Nodes[0].Value != "synthetic-member-42" || raw.Nodes[0].Bounds.X != 10 {
		t.Fatal("raw observation aliases sanitized result")
	}
}
func TestPrivacyWithholdsUnknownSensitiveGeometry(t *testing.T) {
	raw := observationFixture(t)
	raw.Nodes[0].Bounds = nil
	if safe, err := Sanitize(raw, PrivacyPolicy{PID: 1, WindowID: 2, SensitiveTargets: []Selector{{Name: "Member ID"}}}); err == nil || safe.ID != "" {
		t.Fatal("unsafe fallback screenshot returned")
	}
}
func TestPrivacyRejectsForeignAndIncompleteCapture(t *testing.T) {
	raw := observationFixture(t)
	if _, err := Sanitize(raw, PrivacyPolicy{PID: 4, WindowID: 2}); err == nil {
		t.Fatal("foreign window accepted")
	}
	raw.Complete = false
	if _, err := Sanitize(raw, PrivacyPolicy{PID: 1, WindowID: 2}); err == nil {
		t.Fatal("incomplete tree establishes privacy")
	}
}
