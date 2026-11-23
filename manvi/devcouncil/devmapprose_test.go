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
			"3 call site(s) could not be bound; " + WithheldCallSiteSample + "; only 5 of 600 reached key(s) were checked against the unresolved ledger",
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
			"1 call site(s) could not be bound; " + WithheldCallSiteSample + "; 2 call site(s) could not be bound; " + WithheldCallSiteSample + "; only 1 of 9 reached key(s) were checked against the unresolved ledger",
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
