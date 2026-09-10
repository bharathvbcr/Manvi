package policy

import (
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/dc"
)

func TestMatchesAnyIsThePlannedPathLoop(t *testing.T) {
	planned := []dc.PlannedFile{
		{Path: "src/foo.go"},
		{Path: "docs/*.md"},
		{Path: `windows\slash.txt`},
	}
	cases := []struct {
		path string
		want bool
	}{
		{"src/foo.go", true},
		{"docs/readme.md", true},
		// Python fnmatch '*' crosses a separator; a glob that did not would
		// silently refuse a planned subtree this gate is meant to allow.
		{"docs/sub/readme.md", true},
		{"windows/slash.txt", true},
		{"other.go", false},
	}
	for _, tc := range cases {
		got := MatchesPlannedPath(tc.path, planned)
		if got != tc.want {
			t.Errorf("MatchesPlannedPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
		patterns := make([]string, len(planned))
		for i, pf := range planned {
			patterns[i] = pf.Path
		}
		if any := matchesAny(patterns, NormalizePlannedCandidate(tc.path)); got != any {
			t.Errorf("MatchesPlannedPath(%q) = %v but matchesAny = %v", tc.path, got, any)
		}
	}
}

func TestPathMatchesExactAndGlob(t *testing.T) {
	if !pathMatches("src/a.go", "src/a.go") {
		t.Fatal("an exact path must match itself")
	}
	if !pathMatches("src/*.go", "src/a.go") {
		t.Fatal("a glob must match a file in that directory")
	}
	if !pathMatches("src/*.go", "src/sub/a.go") {
		t.Fatal("Python fnmatch '*' crosses a directory; this gate must too")
	}
}
