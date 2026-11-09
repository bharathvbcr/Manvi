package main

import (
	"strings"
	"testing"
)

// Adversarial tests for lane w17-agy, ft-47905b10 criterion 4 (toolbinary).
// The install-order test needs a home directory and is in the unix sibling.

// parseSemver is what orders installed Claude versions now. Names that are not
// SemVer 2.0.0 must be skipped, not mis-ordered or panicked on.
func TestAdvW17InstalledVersionNamesParseStrictly(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"2.1.316", true},
		{"2.1.316-rc.1", true},
		{"2.1.316-rc.01", false}, // numeric prerelease identifier with a leading zero
		{"2.01.316", false},      // leading zero in the core
		{"02.1.316", false},
		{"1.0.0-", false},     // empty prerelease
		{"1.0.0-a..b", false}, // empty identifier
		{"1.0.0-a.", false},
		{"1.0.0+", false}, // empty build metadata
		{"1.0.0+a.", false},
		{"1.0.0+build.5", true},
		{"v2.5.0", true}, // a leading v is accepted
		{"1.0", false},
		{"1.0.0.0", false},
		{"1.0.x", false},
		{"-1.0.0", false},
		{"99999999999999999999.0.0", false}, // overflows uint64
		{"1.0.0-é", false},
		{"", false},
		{"claude", false},
		{"2.1.316.tmp", false},
		{"2.1.316-backup", true}, // a prerelease by SemVer; ranks below 2.1.316
		{strings.Repeat("1", 5000) + ".0.0", false},
		{"1.0.0-" + strings.Repeat("a.", 2500) + "a", true},
	} {
		if _, ok := parseSemver(tc.name); ok != tc.ok {
			n := tc.name
			if len(n) > 40 {
				n = n[:40] + "..."
			}
			t.Errorf("parseSemver(%q) ok=%v, want %v", n, ok, tc.ok)
		}
	}
}
