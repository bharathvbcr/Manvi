package devcouncil

import (
	"context"
	"strings"
	"testing"
)

// graph_context answers from devmap's edges and notes for each file asked
// about, which quote text from that file. A path the read gate refuses is not
// asked about: no devmap process runs for it and nothing devmap knows of it is
// shown.
func TestGraphContextAppliesTheReadGateToItsInputs(t *testing.T) {
	reg, calls := stubDevmap(t, stubHealthy, 0)
	res := reg.graphContext(context.Background(), callOf(t, map[string][]string{"files": {".env", "pkg/ok.go"}}))
	if res.IsError {
		t.Fatalf("graph_context failed: %s", res.Text)
	}
	if strings.Contains(res.Text, "dep-of-.env") {
		t.Errorf("devmap's answer for a refused file reached the model: %s", res.Text)
	}
	if !strings.Contains(res.Text, "withheld") {
		t.Errorf("the refusal is not marked: %s", res.Text)
	}
	if !strings.Contains(res.Text, "dep-of-pkg/ok.go") {
		t.Errorf("a readable file lost its answer: %s", res.Text)
	}
	if got := spawns(calls(), "deps"); got != 1 {
		t.Errorf("devmap deps ran %d times, want 1 (the readable file only)", got)
	}
}

// Every navigation answer states its qualifications through payload, and a
// qualification may be devmap's walk_incomplete, which quotes receiver text.
func TestPayloadWithholdsCallSiteSamplesInDegraded(t *testing.T) {
	note := "1 call site(s) the resolver could not bind name a symbol this answer reached (" +
		leakMarker + ".run) — an untyped receiver — so callers through them are not in it; " +
		"only 5 of 600 reached key(s) were checked against the unresolved ledger"
	res := payload(map[string]any{}, []string{note, "an unrelated qualification"}, false)
	for _, shown := range append([]string{res.Text}, res.Degraded...) {
		if strings.Contains(shown, leakMarker) {
			t.Fatalf("a call-site sample reached the model through degraded: %s", shown)
		}
	}
	if !strings.Contains(res.Text, "only 5 of 600 reached key(s)") || !strings.Contains(res.Text, "an unrelated qualification") {
		t.Errorf("a completeness clause or another qualification was lost: %s", res.Text)
	}
}
