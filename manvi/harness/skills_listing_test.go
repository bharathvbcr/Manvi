package harness

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/prompt"
)

func manySkills(n int) []Skill {
	out := make([]Skill, n)
	for i := range out {
		out[i] = Skill{Name: fmt.Sprintf("skill-%03d", i), Description: "does a thing described at some length so the listing has weight"}
	}
	return out
}

// A cut listing names the skills whose description it dropped, inside the
// budget, and Listed is exactly the set of names the text carries.
func TestCutListingNamesWhatItCut(t *testing.T) {
	skills := manySkills(120)
	full, _ := SkillsSection(skills, 0)
	budget := prompt.EstimateTokens(full) / 3
	l, ok := RenderSkills(skills, budget)
	if !ok {
		t.Fatal("no section")
	}
	if got := prompt.EstimateTokens(l.Text); got > budget {
		t.Fatalf("%d tokens against a %d budget", got, budget)
	}
	if !strings.Contains(l.Text, namesLead) || !strings.Contains(l.Text, "every skill is named below") {
		t.Fatalf("the cut skills are not named:\n%s", l.Text)
	}
	if len(l.Listed) != len(skills) {
		t.Fatalf("Listed has %d of %d names", len(l.Listed), len(skills))
	}
	for _, n := range l.Listed {
		if !strings.Contains(l.Text, n) {
			t.Fatalf("%s is in Listed but not in the text", n)
		}
	}
}

// When even the names do not fit, the notice counts what is named nowhere and
// does not claim every skill is named.
func TestCutNoticeIsHonestWhenNamesDoNotFit(t *testing.T) {
	skills := manySkills(MaxSkills)
	budget := prompt.EstimateTokens(skillsPreamble) + 120
	l, ok := RenderSkills(skills, budget)
	if !ok {
		t.Fatal("no section")
	}
	if strings.Contains(l.Text, "every skill is named below") {
		t.Fatalf("claims every skill is named:\n%s", l.Text)
	}
	unnamed := len(skills) - len(l.Listed)
	if unnamed <= 0 {
		t.Fatal("setup: expected skills named nowhere")
	}
	want := fmt.Sprintf("%d of those are not named below either", unnamed)
	if !strings.Contains(l.Text, want) {
		t.Fatalf("notice lacks %q:\n%s", want, l.Text)
	}
	if got := prompt.EstimateTokens(l.Text); got > budget {
		t.Fatalf("%d tokens against a %d budget", got, budget)
	}
}

// A cancelled read is a failed load, not a rejection of every skill.
func TestLoadSkillsCancellationIsAFailureNotRejections(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a", "b", "c"} {
		writeSkill(t, root, n, "---\nname: "+n+"\ndescription: d\n---\n")
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		read := func(string, int64) ([]byte, error) { return nil, fmt.Errorf("reading: %w", cause) }
		set, err := LoadSkills(context.Background(), root, read)
		if err == nil || len(set.Rejected) != 0 {
			t.Fatalf("%v: err=%v rejected=%v", cause, err, set.Rejected)
		}
	}
}

func TestLookupSkillExcludingOffersOnlyUnlistedNames(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 30; i++ {
		n := fmt.Sprintf("s%02d", i)
		writeSkill(t, root, n, "---\nname: "+n+"\ndescription: d\n---\n")
	}
	listed := map[string]bool{}
	for i := 0; i < 25; i++ {
		listed[fmt.Sprintf("s%02d", i)] = true
	}
	l, err := LookupSkillExcluding(context.Background(), root, stubRead(""), "nope", listed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(l.Available, ",") != "s25,s26,s27,s28,s29" || l.Directories != 30 || l.AvailableTotal != 5 {
		t.Fatalf("available=%v total=%d dirs=%d", l.Available, l.AvailableTotal, l.Directories)
	}
}
