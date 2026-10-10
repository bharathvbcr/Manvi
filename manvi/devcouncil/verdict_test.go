package devcouncil

import (
	"context"
	"strings"
	"testing"
)

// The verdict reader is an advance rule, so every test here is an attempt to
// make it certify something it should not. The rule it must never break: no
// judgement is not a pass.

func TestVerdictReadsTheContract(t *testing.T) {
	got := ParseVerdict(VerdictMarker, "I reviewed the change.\n"+VerdictMarker+" PASS\n")
	if !got.Judged || !got.Passed {
		t.Fatalf("verdict = %+v, want a judged pass", got)
	}
	if len(got.Findings) != 0 {
		t.Fatalf("findings = %v on a pass, want none", got.Findings)
	}
}

func TestVerdictCarriesFindingsOnFailure(t *testing.T) {
	got := ParseVerdict(VerdictMarker, strings.Join([]string{
		"Three problems.",
		VerdictMarker + " FAIL",
		"- the retry is unbounded",
		"- the error is swallowed",
	}, "\n"))
	if !got.Judged || got.Passed {
		t.Fatalf("verdict = %+v, want a judged failure", got)
	}
	if len(got.Findings) != 2 {
		t.Fatalf("findings = %v, want both objections", got.Findings)
	}
}

// The whole class this reader exists to refuse: prose that reads like a
// verdict, and prose that contains the verdict word while saying the opposite.
func TestVerdictRefusesProse(t *testing.T) {
	for name, summary := range map[string]string{
		"no marker at all":      "This all looks good to me. PASSED.",
		"marker word in prose":  "It has not PASSED review yet.",
		"marker without a word": VerdictMarker,
		"unreadable word":       VerdictMarker + " probably fine",
		"prefix of the word":    VerdictMarker + " PASSES ONLY IF the caller checks the error",
		"marker inside a quote": `the test is named "` + VerdictMarker + ` PASS" in the fixture`,
		"empty answer":          "",
	} {
		t.Run(name, func(t *testing.T) {
			got := ParseVerdict(VerdictMarker, summary)
			if got.Judged {
				t.Fatalf("verdict = %+v, want no judgement — %q must not certify anything", got, summary)
			}
			if got.Passed {
				t.Fatal("an unjudged verdict reported Passed")
			}
		})
	}
}

// Two markers that disagree have no honest tie-break, and picking one would be
// the harness inventing the answer.
func TestVerdictRefusesContradiction(t *testing.T) {
	got := ParseVerdict(VerdictMarker, strings.Join([]string{
		VerdictMarker + " PASS",
		"on reflection, no:",
		VerdictMarker + " FAIL",
	}, "\n"))
	if got.Judged {
		t.Fatalf("verdict = %+v, want no judgement from two disagreeing markers", got)
	}
}

// A repeated agreeing marker is not a contradiction — a model restating its own
// conclusion is ordinary — so it must still be readable.
func TestVerdictAcceptsARepeatedAgreeingMarker(t *testing.T) {
	got := ParseVerdict(VerdictMarker, VerdictMarker+" FAIL\n- one thing\n"+VerdictMarker+" FAIL")
	if !got.Judged || got.Passed {
		t.Fatalf("verdict = %+v, want a judged failure", got)
	}
}

// Reconcile is the last line of defence, and this is the state it exists for:
// a child that claims a pass while listing what is wrong with it.
func TestVerdictPassWithFindingsIsNotAPass(t *testing.T) {
	got := ParseVerdict(VerdictMarker, strings.Join([]string{
		VerdictMarker + " PASS",
		"- but the lock is never released",
	}, "\n"))
	if !got.Judged {
		t.Fatal("a readable marker should still be a judgement")
	}
	if got.Passed {
		t.Fatal("a pass carrying objections was certified; it must fail closed")
	}
}

func TestReconcileNeverUpgrades(t *testing.T) {
	cases := []SubAgentVerdict{
		{Judged: false, Passed: true},
		{Judged: false, Passed: true, Findings: []string{"x"}},
		{Judged: true, Passed: true, Findings: []string{"x"}},
	}
	for _, in := range cases {
		if got := in.Reconcile(); got.Judged && got.Passed {
			t.Fatalf("Reconcile(%+v) = %+v, which certifies work it should not", in, got)
		}
	}
}

// A caller that asked for no judgement must not have one invented from an
// ordinary builder's prose.
func TestVerdictNotParsedWhenNoneWasAskedFor(t *testing.T) {
	got := ParseVerdict("", VerdictMarker+" PASS")
	if got.Judged {
		t.Fatalf("verdict = %+v, want none: nobody asked for a judgement", got)
	}
}

// Bounds. A judge does not get to decide how much of the parent's context its
// objections consume, and a truncated finding must say it was truncated.
func TestVerdictFindingsAreBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString(VerdictMarker + " FAIL\n")
	for i := range MaxVerdictFindings * 3 {
		b.WriteString("- objection ")
		b.WriteString(strings.Repeat("x", i%3))
		b.WriteString("\n")
	}
	b.WriteString("- " + strings.Repeat("y", MaxVerdictFindingRunes*2) + "\n")

	got := ParseVerdict(VerdictMarker, b.String())
	if len(got.Findings) > MaxVerdictFindings {
		t.Fatalf("findings = %d, want at most %d", len(got.Findings), MaxVerdictFindings)
	}
	for _, f := range got.Findings {
		if len([]rune(f)) > MaxVerdictFindingRunes+len("… (truncated)") {
			t.Fatalf("finding of %d runes exceeded the cap", len([]rune(f)))
		}
	}
}

// Multi-byte input must not be cut mid-rune: a truncation that produces
// invalid UTF-8 corrupts the session event it is written into.
func TestVerdictTruncationIsRuneSafe(t *testing.T) {
	long := strings.Repeat("日本語", MaxVerdictFindingRunes)
	got := ParseVerdict(VerdictMarker, VerdictMarker+" FAIL\n- "+long)
	if len(got.Findings) != 1 {
		t.Fatalf("findings = %v", got.Findings)
	}
	if !strings.HasSuffix(got.Findings[0], "… (truncated)") {
		t.Fatal("a shortened finding did not say it was shortened")
	}
	for _, r := range got.Findings[0] {
		if r == '�' {
			t.Fatal("truncation cut a rune in half")
		}
	}
}

// Leading whitespace is ordinary in model output — an indented block, a list
// item — and must not hide the verdict.
func TestVerdictToleratesIndentation(t *testing.T) {
	got := ParseVerdict(VerdictMarker, "  \t"+VerdictMarker+"  fail  ")
	if !got.Judged || got.Passed {
		t.Fatalf("verdict = %+v, want a judged failure", got)
	}
}

// The per-criterion form reuses ParseVerdict under a marker per criterion, so
// every rule above holds for each criterion. What is new, and tested here, is
// the bookkeeping around it: which lines belong to which criterion, and what
// silence about a criterion means.

func TestCriterionVerdictsReadEachCriterionSeparately(t *testing.T) {
	got := ParseCriterionVerdicts(strings.Join([]string{
		"Reviewed both.",
		"MANVI-VERDICT[1]: PASS",
		"MANVI-VERDICT[2]: FAIL",
		"- the error path returns nil",
	}, "\n"), 2)
	if !got.Emitted || len(got.Items) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got.Items[0].Status != CriterionMet {
		t.Errorf("criterion 1 = %+v, want met", got.Items[0])
	}
	if got.Items[1].Status != CriterionUnmet || len(got.Items[1].Findings) != 1 {
		t.Errorf("criterion 2 = %+v, want unmet with its one objection", got.Items[1])
	}
}

func TestCriterionVerdictsTreatSilenceAboutACriterionAsUnaddressed(t *testing.T) {
	got := ParseCriterionVerdicts("MANVI-VERDICT[1]: PASS\n", 3)
	if got.Items[1].Status != CriterionUnaddressed || got.Items[2].Status != CriterionUnaddressed {
		t.Fatalf("got %+v, want criteria 2 and 3 unaddressed, never met", got.Items)
	}
}

func TestCriterionVerdictsWithNoMarkerAtAllEmitNothing(t *testing.T) {
	got := ParseCriterionVerdicts("Everything is fine, ship it.", 2)
	if got.Emitted {
		t.Fatal("prose was read as a verdict")
	}
	for _, item := range got.Items {
		if item.Status != CriterionUnaddressed {
			t.Fatalf("item %+v, want unaddressed", item)
		}
	}
}

func TestCriterionVerdictsVoidAContradictedCriterionWithoutTouchingTheOthers(t *testing.T) {
	got := ParseCriterionVerdicts("MANVI-VERDICT[1]: PASS\nMANVI-VERDICT[1]: FAIL\nMANVI-VERDICT[2]: PASS\n", 2)
	if got.Items[0].Status != CriterionConflict {
		t.Errorf("criterion 1 = %+v, want conflict", got.Items[0])
	}
	if got.Items[1].Status != CriterionMet {
		t.Errorf("criterion 2 = %+v, want met: its own line is unambiguous", got.Items[1])
	}
}

func TestCriterionVerdictsRefuseAnIndexOutsideTheCriteria(t *testing.T) {
	got := ParseCriterionVerdicts("MANVI-VERDICT[9]: PASS\nMANVI-VERDICT[0]: PASS\nMANVI-VERDICT[x]: PASS\n", 2)
	if !got.Emitted {
		t.Fatal("a stray marker is still evidence the judge tried to answer")
	}
	for _, item := range got.Items {
		if item.Status != CriterionUnaddressed {
			t.Fatalf("a line for criterion that does not exist certified %+v", item)
		}
	}
}

func TestCriterionVerdictsDoNotLetOneCriterionsObjectionsLeakIntoTheNext(t *testing.T) {
	got := ParseCriterionVerdicts("MANVI-VERDICT[1]: PASS\nMANVI-VERDICT[2]: FAIL\n- only about two\n", 2)
	if got.Items[0].Status != CriterionMet {
		t.Fatalf("criterion 1 = %+v: criterion 2's objection downgraded it", got.Items[0])
	}
}

func TestCriterionVerdictsDowngradeAPassThatListsObjections(t *testing.T) {
	got := ParseCriterionVerdicts("MANVI-VERDICT[1]: PASS\n- but it never closes the file\n", 1)
	if got.Items[0].Status == CriterionMet {
		t.Fatal("a pass listing an objection was certified")
	}
}

func TestVerifyPresentSeparatesDeletedPathsAndDemotesAPass(t *testing.T) {
	checker := &presenceChecker{present: []string{"a.go"}, missing: []string{"gone.go"}}
	got := VerifyPresent(context.Background(), checker, []string{"a.go", "gone.go"}, "", Baseline{})
	if strings.Join(checker.checked, ",") != "a.go" {
		t.Fatalf("the check was handed %v, want only the path still on disk", checker.checked)
	}
	if got.Verdict != VerdictDegraded || len(got.Omitted) != 1 || !strings.Contains(got.Omitted[0], "gone.go") {
		t.Fatalf("got %+v, want a degraded report naming the deleted path as omitted", got)
	}
}

type presenceChecker struct {
	present, missing []string
	checked          []string
}

func (c *presenceChecker) ExistingPaths([]string) ([]string, []string) { return c.present, c.missing }

func (c *presenceChecker) VerifyPaths(_ context.Context, paths []string, _ string, _ Baseline) PathReport {
	c.checked = paths
	return PathReport{Verdict: VerdictPassed}
}
