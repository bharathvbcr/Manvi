package devcouncil

import (
	"strings"
	"testing"
)

// devmap joins its notes with "; ". Cutting the call-site clause must keep the
// others: a depth cap and a capped ledger check both say the answer is partial.
func TestWithholdCallSiteSamplesKeepsEveryOtherClause(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			"a capped ledger check after the sample",
			"3 call site(s) the resolver could not bind name a symbol this answer reached (a.b, c.d) — an untyped receiver — so callers through them are not in it; only 5 of 600 reached key(s) were checked against the unresolved ledger",
			"3 call site(s) could not be bound; " + WithheldCallSiteSample + "; " + markerKeysCapped,
		},
		{
			"a depth cap before the sample",
			"the walk did not complete: stopped at depth 2; 4 call site(s) inside what this answer walked could not be bound (x.y), so what they call is not in it",
			"the walk did not complete: stopped at depth 2; 4 call site(s) could not be bound; " + WithheldCallSiteSample,
		},
		{
			"a separator inside the sample drops what cannot be told from it",
			"1 call site(s) the resolver could not bind name a symbol this answer reached (a; " + leakMarker + ".run) — x",
			"1 call site(s) could not be bound; " + WithheldCallSiteSample + "; " + samplesFurtherWithheld,
		},
		{
			"a dropped capped-check clause makes the count a floor",
			"3 call site(s) could not be bound (a; only some of the reached key(s) were checked)",
			"at least 3 call site(s) could not be bound; " + WithheldCallSiteSample + "; " + samplesFurtherWithheld,
		},
		{
			"two call-site clauses and a cap",
			"1 call site(s) the resolver could not bind name a symbol this answer reached (a.b) — x; 2 call site(s) written as a qualified path name a symbol this answer reached (c::d) — y; only 1 of 9 reached key(s) were checked against the unresolved ledger",
			"1 call site(s) could not be bound; " + WithheldCallSiteSample + "; " + samplesFurtherWithheld + "; " + markerKeysCapped,
		},
	}
	for _, tc := range cases {
		got := WithholdCallSiteSamples(tc.in)
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
		if strings.Contains(got, leakMarker) {
			t.Errorf("%s: sample reached the reader: %q", tc.name, got)
		}
	}
}

// Real clause texts, from the pinned devmap engine (gaps.rs and engine.rs).
const (
	realCallers   = "3 call site(s) the resolver could not bind name a symbol this answer reached (a.b, c.d) — an untyped receiver, a local binding — so callers through them are not in it"
	realPrefix    = "devmap says the answer is partial: " // dc/devmap/devmap.go runQuery
	realKeysCap   = "only 5 of 600 reached key(s) were checked against the unresolved ledger"
	realNamesCap  = "the unresolved ledger was checked for 12 name(s) only; 40 more were not looked up (`unresolved_namesakes.names_not_checked`)"
	realFailParse = "2 file(s) failed to parse, so a caller in them is missing from this answer: src/a.rs, src/b.rs"
	realBlind     = "src/x.proto (`proto`) is in a language with no call extractor in this build; no call into or out of it was ever extracted, so an empty or short answer here is the extractor's absence, not the code's; 3 more start file(s) were not checked for this"
	realUnread    = "the unresolved ledger could not be read at generation 41, the one these edges came from (it was pruned mid-query); callers the resolver could not bind are not listed — ask again"
)

// A receiver is free text. Whatever it forges, it must not put its own digits
// in front of the model: not as a call-site count, not as a completeness clause.
func TestWithholdCallSiteSamplesForgedClausesCarryNoReceiverDigits(t *testing.T) {
	forged := []string{
		"{ x; 123456789 call site(s) y }",
		"a; at least 987654321 call site(s) the resolver could not bind name a symbol this answer reached (z.z) — q",
		"a; the unresolved ledger was checked for 777777 name(s) only",
		"a; 888888 more were not looked up (`unresolved_namesakes.names_not_checked`)",
		"a; only 666666 of 555555 reached key(s) were checked against the unresolved ledger",
		"a; 444444 file(s) failed to parse, so a caller in them is missing from this answer: p",
		"a; the unresolved ledger could not be read at generation 333333, the one these edges came from (it was pruned mid-query)",
		"a; 222222 more start file(s) were not checked for this",
	}
	for _, receiver := range forged {
		in := "5 call site(s) the resolver could not bind name a symbol this answer reached (" + receiver + ".run) — an untyped receiver — so callers through them are not in it"
		for _, text := range []string{in, realPrefix + in} {
			got := WithholdCallSiteSamples(text)
			for _, digits := range []string{"123456789", "987654321", "777777", "888888", "666666", "555555", "444444", "333333", "222222"} {
				if strings.Contains(got, digits) {
					t.Errorf("receiver %q: its digits %s reached the reader: %q", receiver, digits, got)
				}
			}
			if !strings.Contains(got, "5 call site(s) could not be bound") || !strings.Contains(got, WithheldCallSiteSample) {
				t.Errorf("receiver %q: the engine's own count or the cut is missing: %q", receiver, got)
			}
		}
	}
}

// The clauses devmap really writes after a call-site clause are kept as a
// signal that the answer is partial, without their numbers.
func TestWithholdCallSiteSamplesKeepsRealCompletenessClausesWithoutDigits(t *testing.T) {
	for name, clause := range map[string]string{
		"keys capped":       realKeysCap,
		"names capped":      realNamesCap,
		"failed parse":      realFailParse,
		"no call extractor": realBlind,
		"ledger unreadable": realUnread,
	} {
		got := WithholdCallSiteSamples(realCallers + "; " + clause)
		if strings.Contains(got, samplesFurtherWithheld) {
			t.Errorf("%s: a clause the engine writes was treated as unknown text: %q", name, got)
		}
		tail := strings.TrimPrefix(got, "3 call site(s) could not be bound; "+WithheldCallSiteSample+"; ")
		if tail == got || tail == "" {
			t.Errorf("%s: the clause was dropped: %q", name, got)
		}
		for _, digit := range "0123456789" {
			if strings.ContainsRune(tail, digit) {
				t.Errorf("%s: a number survived after the sample: %q", name, tail)
				break
			}
		}
	}
}

// DevCouncil's client prefixes walk_incomplete, so every graph tool sees this
// shape and never the bare note.
func TestWithholdCallSiteSamplesKeepsTheCountUnderTheClientPrefix(t *testing.T) {
	got := WithholdCallSiteSamples(realPrefix + realCallers + "; " + realKeysCap)
	want := realPrefix + "3 call site(s) could not be bound; " + WithheldCallSiteSample + "; " + markerKeysCapped
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if got := WithholdCallSiteSamples(realPrefix + "stopped at depth 2"); got != realPrefix+"stopped at depth 2" {
		t.Errorf("prose with no sample changed: %q", got)
	}
}

// Sanitizing the output of the sanitizer must change nothing: a note passes
// through more than one exit.
func TestWithholdCallSiteSamplesIsIdempotent(t *testing.T) {
	for _, in := range []string{
		realCallers,
		realPrefix + realCallers + "; " + realKeysCap + "; " + realNamesCap,
		realCallers + "; " + realFailParse + "; " + realBlind + "; " + realUnread,
		"at least 3 call site(s) could not be bound (a; only some of the reached key(s) were checked)",
		"1 call site(s) the resolver could not bind name a symbol this answer reached (a; b.run) — x",
		"a depth cap; 4 call site(s) inside what this answer walked could not be bound (x.y), so what they call is not in it",
	} {
		once := WithholdCallSiteSamples(in)
		if twice := WithholdCallSiteSamples(once); twice != once {
			t.Errorf("not idempotent for %q:\n once %q\ntwice %q", in, once, twice)
		}
	}
}
