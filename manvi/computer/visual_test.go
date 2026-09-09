package computer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

func paintedObservation(t *testing.T, o Observation) Observation {
	t.Helper()
	o.Window.Scale = 1
	pixels, err := base64.StdEncoding.DecodeString(o.Screenshot.Base64)
	if err != nil {
		t.Fatal(err)
	}
	source, err := png.Decode(bytes.NewReader(pixels))
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(source.Bounds())
	draw.Draw(img, img.Bounds(), source, source.Bounds().Min, draw.Src)
	for y := 40; y < 48; y++ {
		for x := 60; x < 68; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8((x - 60) * 30), uint8((y - 40) * 30), 20, 255})
		}
	}
	var b bytes.Buffer
	if err = png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	o.Screenshot.Base64 = base64.StdEncoding.EncodeToString(b.Bytes())
	return o
}
func TestVisualAnchorCreationRejectsProtectedPixelsAndInvalidBounds(t *testing.T) {
	o := paintedObservation(t, observationFixture(t))
	p := PrivacyPolicy{PID: 1, WindowID: 2, SensitiveTargets: []Selector{{Name: "Member ID"}}}
	crop := PixelRect{X: 60, Y: 40, Width: 8, Height: 8}
	a, err := CreateVisualAnchor(o, p, crop, crop, PixelPoint{X: 4, Y: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Validate(); err != nil {
		t.Fatal(err)
	}
	if a.FrameWidth != 100 || a.FrameHeight != 80 || a.Scale != 1 || a.Search != crop {
		t.Fatal("anchor lost frame or geometry binding")
	}
	for _, r := range []PixelRect{{X: 50, Y: 20, Width: 8, Height: 8}, {X: 10, Y: 10, Width: 8, Height: 8}, {X: ^uint32(0), Y: 1, Width: 8, Height: 8}} {
		if _, err := CreateVisualAnchor(o, p, r, r, PixelPoint{}); err == nil {
			t.Fatal("protected padding or invalid crop accepted")
		}
	}
	if _, err := CreateVisualAnchor(o, p, crop, PixelRect{Width: 100, Height: 80}, PixelPoint{}); err == nil {
		t.Fatal("search region includes protected pixels")
	}
	safe, err := Sanitize(o, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreateVisualAnchor(safe, PrivacyPolicy{PID: 1, WindowID: 2}, PixelRect{X: 10, Y: 10, Width: 8, Height: 8}, PixelRect{X: 10, Y: 10, Width: 8, Height: 8}, PixelPoint{}); err == nil {
		t.Fatal("existing redaction lost its protection provenance")
	}
	o.Nodes[0].Bounds = nil
	if _, err := CreateVisualAnchor(o, p, crop, crop, PixelPoint{}); err == nil {
		t.Fatal("unknown sensitive geometry allowed anchor")
	}
}
func TestVisualFingerprintIncludesPixelsOutsideTheAnchor(t *testing.T) {
	o := paintedObservation(t, observationFixture(t))
	a, err := CreateVisualAnchor(o, PrivacyPolicy{PID: 1, WindowID: 2}, PixelRect{X: 60, Y: 40, Width: 8, Height: 8}, PixelRect{X: 60, Y: 40, Width: 8, Height: 8}, PixelPoint{})
	if err != nil {
		t.Fatal(err)
	}
	s := workflow.Selector{Visual: &a}
	before, err := approvalFingerprint(o, s)
	if err != nil {
		t.Fatal(err)
	}
	changed := changePixel(t, o)
	after, err := approvalFingerprint(changed, s)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("non-AX pixels outside crop did not invalidate approval")
	}
	if semanticFingerprint(o) != semanticFingerprint(changed) {
		t.Fatal("fixture changed semantic state")
	}
}
func changePixel(t *testing.T, o Observation) Observation {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(o.Screenshot.Base64)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out := image.NewRGBA(img.Bounds())
	draw.Draw(out, out.Bounds(), img, img.Bounds().Min, draw.Src)
	out.SetRGBA(1, 1, color.RGBA{100, 20, 20, 255})
	var encoded bytes.Buffer
	if err = png.Encode(&encoded, out); err != nil {
		t.Fatal(err)
	}
	o.Screenshot.Base64 = base64.StdEncoding.EncodeToString(encoded.Bytes())
	return o
}

type visualDesktop struct {
	*testDesktop
	mutate      *Observation
	grants      int
	resolutions int
}

func (d *visualDesktop) ResolveTarget(_ context.Context, _ Session, observation string, s Selector) (ResolvedTarget, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.resolutions++
	return ResolvedTarget{Kind: "visual", Visual: &VisualMatch{TargetID: "visual-" + observation, AnchorSHA256: s.Visual.SHA256, FrameSHA256: strings.Repeat("a", 64), Matched: PixelRect{X: 60, Y: 40, Width: 8, Height: 8}, ScreenBounds: workflow.VisualBounds{X: 60, Y: 40, Width: 8, Height: 8}}}, nil
}
func (d *visualDesktop) Focus(_ context.Context, s Session) (Session, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.mutate != nil {
		d.observation = *d.mutate
	}
	return s, nil
}
func (d *visualDesktop) Approve(_ context.Context, _ Session, _ Action) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.grants++
	return "grant", nil
}
func TestVisualRunPublishesMatchAndReapprovesChangedPixels(t *testing.T) {
	opts, base := runFixture(t, true)
	base.observation = paintedObservation(t, base.observation)
	opts.Session.Window = base.observation.Window
	a, err := CreateVisualAnchor(base.observation, PrivacyPolicy{PID: 1, WindowID: 2}, PixelRect{X: 60, Y: 40, Width: 8, Height: 8}, PixelRect{X: 60, Y: 40, Width: 8, Height: 8}, PixelPoint{X: 4, Y: 4})
	if err != nil {
		t.Fatal(err)
	}
	spec := opts.Program.Capability()
	spec.Targets["search"] = workflow.Selector{Visual: &a}
	spec.Steps[0].Kind = "click"
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	opts.Program, err = workflow.Compile(raw)
	if err != nil {
		t.Fatal(err)
	}
	mutation := changePixel(t, base.observation)
	d := &visualDesktop{testDesktop: base, mutate: &mutation}
	opts.Desktop = d
	changed := make(chan struct{}, 1)
	opts.OnRecord = func(r Record) error {
		if r.Event != nil && r.Event.Kind == "approval_changed" {
			changed <- struct{}{}
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	s := waitPhase(t, ctx, run, workflow.AwaitingApproval)
	if s.Observation.Visual == nil {
		t.Fatal("visual match absent from review")
	}
	s.Observation.Visual.TargetID = "mutated"
	if run.Snapshot().Observation.Visual.TargetID == "mutated" {
		t.Fatal("review snapshot aliases actor")
	}
	if err = run.Send(ctx, Control{Kind: "approve", Epoch: s.Epoch, ActionID: s.ActionID(opts.Program), ObservationID: s.Observation.ID}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-ctx.Done():
		t.Fatal("pixel change did not revoke approval")
	}
	d.mu.Lock()
	grants, acts := d.grants, d.acts
	d.mu.Unlock()
	if grants != 0 || acts != 0 {
		t.Fatal("changed visual frame was granted or dispatched")
	}
	s = waitPhase(t, ctx, run, workflow.AwaitingApproval)
	if err = run.Send(ctx, Control{Kind: "cancel", Epoch: s.Epoch}); err != nil {
		t.Fatal(err)
	}
	if _, err = run.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestVisualResponseRejectsSemanticMasqueradeAndInvalidFrameBinding(t *testing.T) {
	a := VisualAnchor{SHA256: strings.Repeat("a", 64), FrameWidth: 100, FrameHeight: 100, Width: 8, Height: 8}
	selector := Selector{Visual: &a}
	for _, r := range []ResolvedTarget{{Kind: "semantic", Node: &Node{ID: "fake"}}, {Kind: "visual", Visual: &VisualMatch{TargetID: "v", AnchorSHA256: a.SHA256, FrameSHA256: "bad", Matched: PixelRect{Width: 8, Height: 8}, ScreenBounds: workflow.VisualBounds{Width: 8, Height: 8}}}} {
		if validateResolved(r, selector) == nil {
			t.Fatal("unbound target accepted")
		}
	}
}
func TestImportedVisualAnchorCannotProbeProtectedPixels(t *testing.T) {
	opts, base := runFixture(t, true)
	base.observation = paintedObservation(t, base.observation)
	opts.Session.Window = base.observation.Window
	a, err := CreateVisualAnchor(base.observation, PrivacyPolicy{PID: 1, WindowID: 2}, PixelRect{X: 60, Y: 40, Width: 8, Height: 8}, PixelRect{X: 60, Y: 40, Width: 8, Height: 8}, PixelPoint{})
	if err != nil {
		t.Fatal(err)
	}
	// This hand-authored search is structurally valid, but overlaps a trusted
	// protected semantic field outside the template crop.
	a.Search = PixelRect{Width: 100, Height: 80}
	secret := "private"
	bounds := Bounds{10, 10, 20, 10}
	base.observation.Nodes = append(base.observation.Nodes, Node{ID: "private", Role: "text_field", Name: "Member ID", Value: &secret, Bounds: &bounds})
	opts.Privacy = PrivacyPolicy{PID: 1, WindowID: 2, SensitiveTargets: []Selector{{Name: "Member ID"}}}
	spec := opts.Program.Capability()
	spec.Targets["search"] = workflow.Selector{Visual: &a}
	spec.Steps[0].Kind = "click"
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	opts.Program, err = workflow.Compile(raw)
	if err != nil {
		t.Fatal(err)
	}
	d := &visualDesktop{testDesktop: base}
	opts.Desktop = d
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(ctx)
	if err != nil || result.State.Phase != workflow.Failed || !strings.Contains(result.State.Reason, "protected pixels") {
		t.Fatalf("privacy rejection absent: %+v %v", result.State, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.resolutions != 0 || d.acts != 0 || d.grants != 0 {
		t.Fatal("private template was sent to the native matcher")
	}
}
func TestPublicVisualMatchBindsDisplayedSanitizedPixels(t *testing.T) {
	raw := paintedObservation(t, observationFixture(t))
	safe, err := Sanitize(raw, PrivacyPolicy{PID: 1, WindowID: 2, SensitiveTargets: []Selector{{Name: "Member ID"}}})
	if err != nil {
		t.Fatal(err)
	}
	bytesRaw, err := frameRGBA(raw)
	if err != nil {
		t.Fatal(err)
	}
	private := sha256.Sum256(bytesRaw)
	v, err := publicVisualMatch(safe, VisualMatch{TargetID: "opaque", FrameSHA256: hex.EncodeToString(private[:])})
	if err != nil {
		t.Fatal(err)
	}
	bytesSafe, err := frameRGBA(safe)
	if err != nil {
		t.Fatal(err)
	}
	displayed := sha256.Sum256(bytesSafe)
	if v.FrameSHA256 != hex.EncodeToString(displayed[:]) || v.FrameSHA256 == hex.EncodeToString(private[:]) {
		t.Fatal("public match leaks private hash or does not bind displayed pixels")
	}
}
