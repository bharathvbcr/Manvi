package serve

import (
	"os"
	"path/filepath"
	"testing"
)

// The checkout hold keys a checkout by directory, so a run in a subdirectory
// reached through a case-variant spelling of a held worktree must be refused
// like one reached through the held spelling. On a case-insensitive volume the
// two spellings are one directory; a lexical containment test calls the
// subdirectory a sibling and admits it.
func TestCheckoutsOverlapRefusesACaseVariantSpellingOfAHeldCheckout(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	held := filepath.Join(base, "repo")
	if err := os.MkdirAll(filepath.Join(held, "pkg", "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "REPO")); err != nil {
		t.Skip("volume is case-sensitive; a case-variant spelling names another directory")
	}

	heldPath, heldInfo, ok := resolvedCheckout(held)
	if !ok {
		t.Fatal("held checkout does not resolve")
	}
	variant := filepath.Join(base, "REPO", "pkg", "sub")
	variantPath, variantInfo, ok := resolvedCheckout(variant)
	if !ok {
		t.Fatal("case-variant subdirectory does not resolve")
	}
	if !checkoutsOverlap(heldInfo, heldPath, variantInfo, variantPath) {
		t.Errorf("a subdirectory of the held checkout, spelled %s, was not held with it", variantPath)
	}
	if !checkoutsOverlap(variantInfo, variantPath, heldInfo, heldPath) {
		t.Errorf("the overlap is not symmetric for %s", variantPath)
	}
}
