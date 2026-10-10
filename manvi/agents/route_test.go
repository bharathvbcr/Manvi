package agents

import (
	"strings"
	"testing"
)

// TestRouteOverEveryShippedRole pins the router for each shipped role across
// readOnly × verdict × the fan-out width boundary.
//
// Each row states the expected tier per case rather than recomputing it, so a
// change to the routing rules or to a role's permissions shows up here as a
// named difference.
func TestRouteOverEveryShippedRole(t *testing.T) {
	type want struct {
		verdict  Tier // asked for a verdict, any readOnly, any width
		writing  Tier // readOnly=false, no verdict, width below BasicFanoutWidth
		wideW    Tier // readOnly=false, no verdict, width at or above it
		narrowRO Tier // readOnly=true, no verdict, width below BasicFanoutWidth
		wideRO   Tier // readOnly=true, no verdict, width at or above it
	}
	expect := map[string]want{
		// Read-only roles: the role's own floor makes readOnly=false irrelevant.
		"research": {verdict: TierLite, writing: TierLite, wideW: TierBasic, narrowRO: TierLite, wideRO: TierBasic},
		"planner":  {verdict: TierLite, writing: TierLite, wideW: TierBasic, narrowRO: TierLite, wideRO: TierBasic},
		// The critic is the one role whose ceiling reaches advisor, and it
		// reaches it only when asked for a verdict.
		"critic": {verdict: TierAdvisor, writing: TierLite, wideW: TierBasic, narrowRO: TierLite, wideRO: TierBasic},
		// Writing roles: heavy while they may write, capped at heavy for a
		// verdict, and routed like any read-only child when the caller took
		// their writes away.
		"builder":       {verdict: TierHeavy, writing: TierHeavy, wideW: TierHeavy, narrowRO: TierLite, wideRO: TierBasic},
		"stress_tester": {verdict: TierHeavy, writing: TierHeavy, wideW: TierHeavy, narrowRO: TierLite, wideRO: TierBasic},
		"self":          {verdict: TierHeavy, writing: TierHeavy, wideW: TierHeavy, narrowRO: TierLite, wideRO: TierBasic},
	}

	shipped := NewRegistry().List()
	if len(shipped) != len(expect) {
		t.Fatalf("%d shipped roles, %d rows here: every shipped role needs a row", len(shipped), len(expect))
	}
	narrow := []int{0, 1, BasicFanoutWidth - 1}
	wide := []int{BasicFanoutWidth, BasicFanoutWidth + 1}

	for _, def := range shipped {
		w, ok := expect[def.Name]
		if !ok {
			t.Errorf("shipped role %q has no row", def.Name)
			continue
		}
		check := func(readOnly, verdict bool, width int, want Tier) {
			t.Helper()
			if got := Route(def, readOnly, verdict, width); got != want {
				t.Errorf("Route(%s, readOnly=%v, verdict=%v, width=%d) = %s, want %s",
					def.Name, readOnly, verdict, width, got, want)
			}
		}
		for _, width := range append(append([]int{}, narrow...), wide...) {
			check(false, true, width, w.verdict)
			check(true, true, width, w.verdict)
		}
		for _, width := range narrow {
			check(false, false, width, w.writing)
			check(true, false, width, w.narrowRO)
		}
		for _, width := range wide {
			check(false, false, width, w.wideW)
			check(true, false, width, w.wideRO)
		}
	}
}

// No child is ever routed to main: that tier is the dispatching agent's own.
func TestRouteNeverPlacesAChildOnMain(t *testing.T) {
	roles := append(NewRegistry().List(),
		Definition{Name: "untyped-writer", EnableWriteTools: true},
		Definition{Name: "untyped-reader"},
		Definition{Name: "stated-advisor", Tier: TierAdvisor, EnableWriteTools: true},
	)
	for _, def := range roles {
		for _, ro := range []bool{false, true} {
			for _, v := range []bool{false, true} {
				for _, width := range []int{1, BasicFanoutWidth} {
					if got := Route(def, ro, v, width); got == TierMain {
						t.Errorf("Route(%s, %v, %v, %d) = main", def.Name, ro, v, width)
					}
				}
			}
		}
	}
}

// A role with no stated tier is capped by its permissions, never by the top.
func TestRouteCapsAnUnstatedRoleByItsPermissions(t *testing.T) {
	reader := Definition{Name: "r"}
	if got := Route(reader, false, true, 1); got != TierLite {
		t.Errorf("an unstated read-only role asked for a verdict routed to %s, want lite", got)
	}
	writer := Definition{Name: "w", EnableWriteTools: true}
	if got := Route(writer, false, true, 1); got != TierHeavy {
		t.Errorf("an unstated writing role asked for a verdict routed to %s, want heavy", got)
	}
	cheap := Definition{Name: "c", Tier: TierBasic, EnableWriteTools: true}
	if got := Route(cheap, false, false, 1); got != TierBasic {
		t.Errorf("a role that states basic routed to %s", got)
	}
}

// Every shipped role states its tier; none leaves it to be derived.
func TestEveryShippedRoleStatesItsTier(t *testing.T) {
	for _, def := range NewRegistry().List() {
		if _, ok := ParseTier(string(def.Tier)); !ok {
			t.Errorf("shipped role %q states no tier (%q)", def.Name, def.Tier)
		}
	}
}

func TestModelPlacementAcceptsOnlyTiersAtOrBelowTheCeiling(t *testing.T) {
	for _, tc := range []struct {
		spec    string
		ceiling Tier
		want    Tier
		errHas  []string
	}{
		{spec: "", ceiling: TierLite, want: ""},
		{spec: "inherit", ceiling: TierLite, want: ""},
		{spec: " Lite ", ceiling: TierLite, want: TierLite},
		{spec: "basic", ceiling: TierHeavy, want: TierBasic},
		{spec: "claude-fable-5-1", ceiling: TierLite, errHas: []string{"not a tier", "basic, lite"}},
		{spec: "anthropic/claude-opus-5-5", ceiling: TierHeavy, errHas: []string{"not a tier", "basic, lite, heavy"}},
		{spec: "local", ceiling: TierHeavy, errHas: []string{"not a tier"}},
		{spec: "advisor", ceiling: TierLite, errHas: []string{"above", "basic, lite"}},
		{spec: "heavy", ceiling: TierLite, errHas: []string{"above"}},
	} {
		got, err := ModelPlacement(tc.spec, tc.ceiling)
		if len(tc.errHas) > 0 {
			if err == nil {
				t.Errorf("ModelPlacement(%q, %s) = %q, want a refusal", tc.spec, tc.ceiling, got)
				continue
			}
			for _, s := range tc.errHas {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("ModelPlacement(%q, %s) error %q does not mention %q", tc.spec, tc.ceiling, err, s)
				}
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ModelPlacement(%q, %s) = %q, %v; want %q", tc.spec, tc.ceiling, got, err, tc.want)
		}
	}
}
