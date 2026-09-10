package workflow

import (
	"encoding/json"
	"testing"
)

func TestApplyOverlayReplacesTargetLadder(t *testing.T) {
	base := schemaBase(t)
	base.ID = "bank.balance"
	base.Revision = "1"
	overlay := Overlay{
		SchemaVersion: 1,
		CapabilityID:  "bank.balance",
		Revision:      "1",
		Tenant:        "south",
		Targets: map[string]Selector{
			"search": {
				Strategies: []Selector{{Role: "button", Name: "Find member"}},
				Rationale:  "South labels search as Find member",
				Stability:  "semantic",
			},
		},
	}
	merged, err := ApplyOverlay(base, overlay)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Targets["search"].Primary().Name != "Find member" {
		t.Fatalf("search ladder not replaced: %+v", merged.Targets["search"])
	}
	if merged.Targets["balance"].Primary().Name != "Balance" {
		t.Fatal("untouched target changed")
	}
}

func TestApplyOverlayRejectsUnknownTargetAndWidenLimits(t *testing.T) {
	base := schemaBase(t)
	base.ID = "bank.balance"
	base.Revision = "1"
	base.Limits = DefaultLimits()
	if _, err := ApplyOverlay(base, Overlay{
		SchemaVersion: 1, CapabilityID: "bank.balance", Revision: "1", Tenant: "south",
		Targets: map[string]Selector{"ghost": {Role: "button", Name: "X"}},
	}); err == nil {
		t.Fatal("unknown target accepted")
	}
	wide := DefaultLimits()
	wide.MaxActions = base.Limits.MaxActions + 1
	if _, err := ApplyOverlay(base, Overlay{
		SchemaVersion: 1, CapabilityID: "bank.balance", Revision: "1", Tenant: "south",
		Limits: &wide,
	}); err == nil {
		t.Fatal("widened limits accepted")
	}
}

func TestCompileWithOverlayDigestCoversBaseAndOverlay(t *testing.T) {
	base := schemaBase(t)
	base.ID = "bank.balance"
	base.Revision = "1"
	baseRaw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	overlay := Overlay{
		SchemaVersion: 1,
		CapabilityID:  "bank.balance",
		Revision:      "1",
		Tenant:        "south",
		Targets: map[string]Selector{
			"search": {
				Strategies: []Selector{{Role: "button", Name: "Find member"}, {Identifier: "search.button"}},
				Rationale:  "renamed primary with identifier fallback",
				Stability:  "semantic",
			},
		},
		Limits: &Limits{MaxActions: 20, ActiveSeconds: 300, ObservationAttempts: 3, InterventionSeconds: 600},
	}
	overlayRaw, err := json.Marshal(overlay)
	if err != nil {
		t.Fatal(err)
	}
	p, err := CompileWithOverlay(baseRaw, overlayRaw)
	if err != nil {
		t.Fatal(err)
	}
	want := OverlayDigest(baseRaw, overlayRaw)
	if p.Digest() != want {
		t.Fatalf("digest=%s want=%s", p.Digest(), want)
	}
	baseOnly, err := Compile(baseRaw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Digest() == baseOnly.Digest() {
		t.Fatal("overlay digest collided with base-only digest")
	}
	if p.Capability().Targets["search"].Primary().Name != "Find member" {
		t.Fatal("compiled program missing overlay replacement")
	}
	if p.Capability().Limits.MaxActions != 20 {
		t.Fatalf("limits not narrowed: %+v", p.Capability().Limits)
	}
}

func TestCompileWithOverlayRejectsIdentityMismatch(t *testing.T) {
	base := schemaBase(t)
	base.ID = "bank.balance"
	base.Revision = "1"
	baseRaw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	overlayRaw, err := json.Marshal(Overlay{
		SchemaVersion: 1, CapabilityID: "other", Revision: "1", Tenant: "south",
		Targets: map[string]Selector{"search": {Role: "button", Name: "Search"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = CompileWithOverlay(baseRaw, overlayRaw); err == nil {
		t.Fatal("capability identity mismatch accepted")
	}
}
