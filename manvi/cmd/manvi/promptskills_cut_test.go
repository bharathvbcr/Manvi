package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/harness"
)

// A skills directory too large to list is reported on the prompt-fault path, so
// an operator whose skill is missing from the listing is told the listing was
// cut and not left to conclude the skill is malformed.
func TestACutSkillsListingSurfacesAsANotice(t *testing.T) {
	loader := fakeSkills{set: harness.SkillSet{Skills: someSkills(2), ListingCut: true}}
	src, notices := skillsForPrompt(context.Background(), loader, 0)
	if src == nil {
		t.Fatal("a cut listing removed the skills that were read")
	}
	want := fmt.Sprintf("more than %d entries", harness.MaxSkillDirEntries)
	if !strings.Contains(strings.Join(notices, "\n"), want) {
		t.Fatalf("notices lack %q: %v", want, notices)
	}
	if _, clean := skillsForPrompt(context.Background(), fakeSkills{set: harness.SkillSet{Skills: someSkills(2)}}, 0); len(clean) != 0 {
		t.Fatalf("an uncut listing produced notices: %v", clean)
	}
}
