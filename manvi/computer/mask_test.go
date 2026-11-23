package computer

import (
	"context"
	"errors"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

func TestFoldedMatcherFloorPinsTheShortSecretBehaviour(t *testing.T) {
	short := PrivacyPolicy{Watched: []string{"12345"}} // 5 folded runes: below the floor
	if got := short.Scrub("1 2 3 4 5"); got != "1 2 3 4 5" {
		t.Fatalf("a secret shorter than %d folded runes was matched after folding: %q", minFoldedSecretLen, got)
	}
	if got := short.Scrub("pin 12345 ok"); got != "pin [redacted] ok" {
		t.Fatalf("the literal match of a short secret was lost: %q", got)
	}
	edge := PrivacyPolicy{Watched: []string{"123456"}} // exactly the floor
	if got := edge.Scrub("1 2 3 4 5 6"); got != redactedText {
		t.Fatalf("a secret at the floor was not matched after folding: %q", got)
	}
	if short.sensitive(Node{Name: "1-2-3-4-5"}) || !edge.sensitive(Node{Name: "1-2-3-4-5-6"}) {
		t.Fatal("sensitive() does not apply the same floor as Scrub")
	}
}

func TestFoldSensitiveDropsSeparatorsAndFoldsWidth(t *testing.T) {
	for in, want := range map[string]string{
		"4111 1111":                 "41111111",
		"4111\u00a01111":            "41111111",
		"\uff14\uff11-\uff11\uff11": "4111",
		"4\u200d1\u20111.1,1":       "41111",
		"ABC def":                   "abcdef",
	} {
		if got := foldSensitive(in); got != want {
			t.Fatalf("foldSensitive(%q) = %q, want %q", in, got, want)
		}
	}
}

func splitNodes(parents []string, texts ...string) []Node {
	nodes := make([]Node, len(texts))
	for i, text := range texts {
		v := text
		nodes[i] = Node{ID: string(rune('a' + i)), Role: "text", Value: &v, Bounds: &Bounds{float64(i * 10), 0, 8, 8}}
		if parents != nil {
			p := parents[i]
			nodes[i].Parent = &p
		}
	}
	return nodes
}

func TestSplitSecretIsBoundedPerParentAndWindow(t *testing.T) {
	p := PrivacyPolicy{Watched: []string{"12345678"}}
	flagged := func(plan maskPlan) (n int) {
		for _, s := range plan.sensitive {
			if s {
				n++
			}
		}
		return n
	}
	// Eight single-digit siblings is the widest window searched.
	if n := flagged(p.plan(splitNodes(nil, "1", "2", "3", "4", "5", "6", "7", "8"))); n != 8 {
		t.Fatalf("a secret over %d siblings flagged %d nodes", maxSplitSiblings, n)
	}
	wide := PrivacyPolicy{Watched: []string{"123456789"}}
	if n := flagged(wide.plan(splitNodes(nil, "1", "2", "3", "4", "5", "6", "7", "8", "9"))); n != 0 {
		t.Fatalf("a secret over %d siblings is beyond the bound but flagged %d", maxSplitSiblings+1, n)
	}
	// Siblings of different parents are not adjacent.
	if n := flagged(p.plan(splitNodes([]string{"x", "x", "x", "x", "y", "y", "y", "y"}, "1", "2", "3", "4", "5", "6", "7", "8"))); n != 0 {
		t.Fatalf("nodes of two parents were joined: %d flagged", n)
	}
	// Only the nodes the match overlaps are hidden.
	plan := p.plan(splitNodes(nil, "zz", "1234", "5678", "yy"))
	if plan.sensitive[0] || !plan.sensitive[1] || !plan.sensitive[2] || plan.sensitive[3] {
		t.Fatalf("overlap not exact: %v", plan.sensitive)
	}
	if !plan.hide[1].value || plan.hide[1].name {
		t.Fatalf("hide fields: %+v", plan.hide[1])
	}
}

func TestMaskMemoryMasksARecreatedControlByWhereItLay(t *testing.T) {
	policy := EffectivePolicy(nil, PrivacyPolicy{SensitiveTargets: []Selector{{Role: "text_field", Name: "Card"}}}, nil)
	first := []Node{{ID: "1", Role: "text_field", Name: "Card", Bounds: &Bounds{10, 10, 40, 20}}}
	if plan := policy.plan(first); !plan.sensitive[0] {
		t.Fatal("the named control was not sensitive")
	}
	later := []Node{
		{ID: "2", Role: "text_field", Name: "Card 4242", Bounds: &Bounds{12, 10, 40, 20}}, // re-created, shifted
		{ID: "3", Role: "text_field", Name: "Other", Bounds: &Bounds{200, 200, 40, 20}},   // elsewhere
		{ID: "4", Role: "group", Name: "Window", Bounds: &Bounds{0, 0, 400, 400}},         // contains it, mostly elsewhere
		{ID: "5", Role: "text_field", Name: "Nobounds"},
	}
	plan := policy.plan(later)
	if !plan.sensitive[0] || plan.sensitive[1] || plan.sensitive[2] || plan.sensitive[3] {
		t.Fatalf("remembered masks applied wrongly: %v", plan.sensitive)
	}
	// Without the run's memory the same frame is not masked.
	bare := PrivacyPolicy{SensitiveTargets: policy.SensitiveTargets}
	if plan := bare.plan(later); plan.sensitive[0] {
		t.Fatal("a policy without memory masked by remembered bounds")
	}
}

func TestEffectivePolicyCarriesIdentityTargetsAndWatchedValuesWithoutAliasing(t *testing.T) {
	prog := compileSensitiveEntry(t)
	host := PrivacyPolicy{Watched: []string{"host"}, SensitiveTargets: []Selector{{Name: "Host"}}}
	got := EffectivePolicy(prog, host, map[string]workflow.Value{"card": {Type: "string", Text: "4111111111111111"}})
	if len(got.SensitiveTargets) != 3 || len(got.Watched) != 2 || got.memory == nil {
		t.Fatalf("%+v", got)
	}
	got.Watched[0] = "changed"
	if host.Watched[0] != "host" {
		t.Fatal("the effective policy aliases the host's Watched")
	}
	if again := EffectivePolicy(prog, host, nil); again.memory == got.memory {
		t.Fatal("two effective policies share one memory")
	}
}

// A recovery capture succeeded, then the approval capture failed: the result
// holds a frame but no digest, and must not reset what the run last reviewed.
func TestFailedApprovalCaptureKeepsTheLastReviewedDigest(t *testing.T) {
	opts, base := runFixture(t, true)
	d := &advDesktop{testDesktop: base, observeHook: func(_ int, focused bool, _ *Observation) error {
		if focused {
			return errors.New("capture failed")
		}
		return nil
	}}
	opts.Desktop = d
	opts.Privacy = PrivacyPolicy{PID: 1, WindowID: 2}
	opts.OnRecord = func(Record) error { return nil }
	command := workflow.Command{Kind: "act", ActionID: "run:search", RecoveryID: "rec-1", Step: workflow.Step{ID: "search", Kind: "press", Target: "search", Effect: "change"}, Selector: workflow.Selector{Role: "button", Name: "Search"}}
	last := [32]byte{1, 2, 3}
	result := perform(context.Background(), opts, opts.Session, nil, command, last, nil)
	if result.err == nil || result.observation == nil {
		t.Fatalf("want a recovery frame and a failed approval capture, got err=%v observation=%v", result.err, result.observation != nil)
	}
	if result.fingerprinted {
		t.Fatal("a result whose approval capture failed claims a digest")
	}
	if got := result.nextReviewed(last); got != last {
		t.Fatalf("reviewed was overwritten by a failed capture: %x", got)
	}
	var digested workResult
	digested.fingerprint, digested.fingerprinted = [32]byte{9}, true
	if got := digested.nextReviewed(last); got != ([32]byte{9}) {
		t.Fatalf("a digested result did not become the reviewed one: %x", got)
	}
}
