package main

import (
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/harness"
	"github.com/bharathvbcr/Manvi/manvi/prompt"
)

// The assembly-time check names a section the spec shares with a source this
// turn actually registers, so a spec is never logged as applied for a prompt the
// assembler would have cut it out of.
func TestCollidingSectionNamesASectionThePromptAlreadyHas(t *testing.T) {
	reg := specRegistry(t, nil)
	opts := PromptOptions{Provider: "anthropic", TaskToolsOffered: true,
		Skills: prompt.Static(skillsSourceName, skillsSectionOrder, false, "listing")}
	clash := harness.Spec{Name: "x", Sections: []harness.Section{{Name: "ok", Order: 60, Text: "a"}, {Name: skillsSourceName, Order: 61, Text: "b"}}}
	if got := collidingSection(reg, opts, &clash); got != skillsSourceName {
		t.Fatalf("collidingSection = %q", got)
	}
	clean := harness.Spec{Name: "x", Sections: []harness.Section{{Name: "ok", Order: 60, Text: "a"}}}
	if got := collidingSection(reg, opts, &clean); got != "" {
		t.Fatalf("collidingSection = %q on a clean spec", got)
	}
}
