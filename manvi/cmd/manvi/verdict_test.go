package main

import (
	"strings"
	"testing"

	"manvi/devcouncil"
)

// The verdict reader is an advance rule, so every test here is an attempt to
// make it certify something it should not. The rule it must never break: no
// judgement is not a pass.

func TestVerdictReadsTheContract(t *testing.T) {
	got := parseVerdict(verdictMarker, "I reviewed the change.\n"+verdictMarker+" PASS\n")
	if !got.Judged || !got.Passed {
		t.Fatalf("verdict = %+v, want a judged pass", got)
	}
	if len(got.Findings) != 0 {
		t.Fatalf("findings = %v on a pass, want none", got.Findings)
	}
}

func TestVerdictCarriesFindingsOnFailure(t *testing.T) {
	got := parseVerdict(verdictMarker, strings.Join([]string{
		"Three problems.",
		verdictMarker + " FAIL",
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
		"marker without a word": verdictMarker,
		"unreadable word":       verdictMarker + " probably fine",
		"prefix of the word":    verdictMarker + " PASSES ONLY IF the caller checks the error",
		"marker inside a quote": `the test is named "` + verdictMarker + ` PASS" in the fixture`,
		"empty answer":          "",
	} {
		t.Run(name, func(t *testing.T) {
			got := parseVerdict(verdictMarker, summary)
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
	got := parseVerdict(verdictMarker, strings.Join([]string{
		verdictMarker + " PASS",
		"on reflection, no:",
		verdictMarker + " FAIL",
	}, "\n"))
	if got.Judged {
		t.Fatalf("verdict = %+v, want no judgement from two disagreeing markers", got)
	}
}

// A repeated agreeing marker is not a contradiction — a model restating its own
// conclusion is ordinary — so it must still be readable.
func TestVerdictAcceptsARepeatedAgreeingMarker(t *testing.T) {
	got := parseVerdict(verdictMarker, verdictMarker+" FAIL\n- one thing\n"+verdictMarker+" FAIL")
	if !got.Judged || got.Passed {
		t.Fatalf("verdict = %+v, want a judged failure", got)
	}
}

// Reconcile is the last line of defence, and this is the state it exists for:
// a child that claims a pass while listing what is wrong with it.
func TestVerdictPassWithFindingsIsNotAPass(t *testing.T) {
	got := parseVerdict(verdictMarker, strings.Join([]string{
		verdictMarker + " PASS",
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
	cases := []devcouncil.SubAgentVerdict{
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
	got := parseVerdict("", verdictMarker+" PASS")
	if got.Judged {
		t.Fatalf("verdict = %+v, want none: nobody asked for a judgement", got)
	}
}

// Bounds. A judge does not get to decide how much of the parent's context its
// objections consume, and a truncated finding must say it was truncated.
func TestVerdictFindingsAreBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString(verdictMarker + " FAIL\n")
	for i := range maxVerdictFindings * 3 {
		b.WriteString("- objection ")
		b.WriteString(strings.Repeat("x", i%3))
		b.WriteString("\n")
	}
	b.WriteString("- " + strings.Repeat("y", maxVerdictFindingRunes*2) + "\n")

	got := parseVerdict(verdictMarker, b.String())
	if len(got.Findings) > maxVerdictFindings {
		t.Fatalf("findings = %d, want at most %d", len(got.Findings), maxVerdictFindings)
	}
	for _, f := range got.Findings {
		if len([]rune(f)) > maxVerdictFindingRunes+len("… (truncated)") {
			t.Fatalf("finding of %d runes exceeded the cap", len([]rune(f)))
		}
	}
}

// Multi-byte input must not be cut mid-rune: a truncation that produces
// invalid UTF-8 corrupts the session event it is written into.
func TestVerdictTruncationIsRuneSafe(t *testing.T) {
	long := strings.Repeat("日本語", maxVerdictFindingRunes)
	got := parseVerdict(verdictMarker, verdictMarker+" FAIL\n- "+long)
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
	got := parseVerdict(verdictMarker, "  \t"+verdictMarker+"  fail  ")
	if !got.Judged || got.Passed {
		t.Fatalf("verdict = %+v, want a judged failure", got)
	}
}
