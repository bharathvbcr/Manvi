package computer

import (
	"encoding/json"
	"strings"
	"testing"
)

// Bare pointer motion is tolerated by the macOS admission guard for
// element-addressed accessibility actions, but only because it is recorded. If
// sanitization or serialization dropped the flag, a run would look unattended
// in evidence while a person was actually at the machine.
func TestPointerMotionSurvivesSanitizationIntoEvidence(t *testing.T) {
	raw := observationFixture(t)
	raw.PointerMotion = true
	p := PrivacyPolicy{PID: 1, WindowID: 2, Watched: []string{"synthetic-member-42"}, SensitiveTargets: []Selector{{Name: "Member ID"}}}
	safe, err := Sanitize(raw, p)
	if err != nil {
		t.Fatal(err)
	}
	if !safe.PointerMotion {
		t.Fatal("sanitization dropped the recorded pointer motion")
	}
	b, err := json.Marshal(safe)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"pointer_motion":true`) {
		t.Fatalf("recorded motion missing from exported observation: %s", b)
	}
}

// A run with no interference must serialize exactly as before, so existing
// evidence bytes and their hashes stay comparable.
func TestPointerMotionIsAbsentFromTheWireWhenNothingWasObserved(t *testing.T) {
	raw := observationFixture(t)
	p := PrivacyPolicy{PID: 1, WindowID: 2}
	safe, err := Sanitize(raw, p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(safe)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "pointer_motion") {
		t.Fatalf("quiet run emitted a pointer_motion field: %s", b)
	}
	var back Observation
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.PointerMotion {
		t.Fatal("absent field decoded as observed motion")
	}
}

// The broker reports motion on the receipt for the dispatch itself.
func TestReceiptCarriesPointerMotionFromTheBroker(t *testing.T) {
	wire := []byte(`{"action_id":"a","delivery":"sent","dispatched_at_ms":7,"verified":false,"pointer_motion":true}`)
	var r Receipt
	if err := json.Unmarshal(wire, &r); err != nil {
		t.Fatal(err)
	}
	if !r.PointerMotion || r.Delivery != "sent" || r.ActionID != "a" {
		t.Fatalf("receipt did not carry broker fields: %+v", r)
	}
	quiet := []byte(`{"action_id":"a","delivery":"sent","dispatched_at_ms":7,"verified":false}`)
	r = Receipt{}
	if err := json.Unmarshal(quiet, &r); err != nil {
		t.Fatal(err)
	}
	if r.PointerMotion {
		t.Fatal("absent field decoded as observed motion")
	}
}
