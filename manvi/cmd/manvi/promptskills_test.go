package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/harness"
	"github.com/bharathvbcr/Manvi/manvi/llm/local"
	"github.com/bharathvbcr/Manvi/manvi/prompt"
)

type fakeSkills struct {
	set harness.SkillSet
	err error
}

func (f fakeSkills) LoadSkills(context.Context) (harness.SkillSet, error) { return f.set, f.err }

func someSkills(n int) []harness.Skill {
	out := make([]harness.Skill, n)
	for i := range out {
		out[i] = harness.Skill{
			Name:        fmt.Sprintf("skill-%02d", i),
			Description: "does a thing that is described at some length so the listing has weight",
		}
	}
	return out
}

func assembleWithSkills(t *testing.T, reg *flags.Registry, provider string, loader skillLoader) (string, prompt.Report, []string) {
	t.Helper()
	opts := PromptOptions{Provider: provider, TaskToolsOffered: true}
	opts.Skills, opts.SkillNotices = skillsForPrompt(context.Background(), loader, promptTokenBudget(reg, provider))
	return assemblePromptReported(reg, opts)
}

func TestSkillsSectionPresentIffSkillsExist(t *testing.T) {
	reg := routerRegistry(t, nil)
	with, _, faults := assembleWithSkills(t, reg, "anthropic", fakeSkills{set: harness.SkillSet{Skills: someSkills(3)}})
	if len(faults) != 0 {
		t.Fatalf("faults: %v", faults)
	}
	if !strings.Contains(with, "devcouncil_load_skill") || !strings.Contains(with, "- skill-01 — ") {
		t.Fatalf("the prompt does not list the skills:\n%s", with)
	}
	without, _, _ := assembleWithSkills(t, reg, "anthropic", fakeSkills{})
	if strings.Contains(without, "devcouncil_load_skill") {
		t.Fatal("a repository without skills still got a skills section")
	}
}

func TestSkillsSectionIsByteIdenticalAcrossAssemblies(t *testing.T) {
	reg := routerRegistry(t, nil)
	loader := fakeSkills{set: harness.SkillSet{Skills: someSkills(12)}}
	for _, provider := range []string{"anthropic", local.Name} {
		a, repA, _ := assembleWithSkills(t, reg, provider, loader)
		b, repB, _ := assembleWithSkills(t, reg, provider, loader)
		if a != b {
			t.Fatalf("%s: two assemblies of the same skills differ", provider)
		}
		if fmt.Sprint(repA.Included) != fmt.Sprint(repB.Included) {
			t.Fatalf("%s: the included sections differ", provider)
		}
	}
}

func TestSkillsSectionIsDroppedBeforeEssentialsAndNamed(t *testing.T) {
	reg := routerRegistry(t, nil)
	// Enough weight that no share of the 4,096-token local budget holds it
	// whole, and a budget small enough that the assembler must choose.
	opts := PromptOptions{Provider: local.Name, TaskToolsOffered: true}
	opts.Skills = prompt.Static("skills", skillsSectionOrder, false, strings.Repeat("a skill listing line\n", 4000))
	text, report, faults := assemblePromptReported(reg, opts)
	if strings.Contains(text, "a skill listing line") {
		t.Fatal("an oversized skills section was kept over the budget")
	}
	named := false
	for _, o := range report.Omitted {
		if o.Name == "skills" {
			named = true
		}
	}
	if !named {
		t.Fatalf("the skills section is not named among the omitted sections: %+v", report.Omitted)
	}
	if !strings.Contains(strings.Join(faults, "\n"), `"skills" is missing from the prompt`) {
		t.Fatalf("no fault names the missing skills section: %v", faults)
	}
	if !strings.Contains(text, "You are") && len(report.Included) == 0 {
		t.Fatal("the essential sections did not survive")
	}
}

func TestSkillsListingFitsItsShareOfALocalBudget(t *testing.T) {
	reg := routerRegistry(t, nil)
	budget := promptTokenBudget(reg, local.Name)
	// More skills than any share can list: the listing is cut, says so, and the
	// whole prompt still fits.
	text, _, faults := assembleWithSkills(t, reg, local.Name, fakeSkills{set: harness.SkillSet{Skills: someSkills(150)}})
	if got := prompt.EstimateTokens(text); got > budget {
		t.Fatalf("the prompt is %d tokens against a %d budget", got, budget)
	}
	if !strings.Contains(text, "the skills list was cut here") {
		t.Fatal("a cut listing does not say it was cut")
	}
	if len(faults) != 0 {
		t.Fatalf("faults: %v", faults)
	}
}

func TestRejectedSkillsSurfaceAsNotices(t *testing.T) {
	loader := fakeSkills{set: harness.SkillSet{
		Skills:          someSkills(1),
		Rejected:        []harness.Rejection{{Name: "bad", Reason: "description is required"}},
		RejectedOmitted: 3,
		NotExamined:     7,
	}}
	src, notices := skillsForPrompt(context.Background(), loader, 0)
	if src == nil {
		t.Fatal("a rejected sibling removed the valid skills")
	}
	joined := strings.Join(notices, "\n")
	for _, want := range []string{`"bad"`, "description is required", "3 more skill directories", "7 skill directories"} {
		if !strings.Contains(joined, want) {
			t.Errorf("notices lack %q:\n%s", want, joined)
		}
	}
	reg := routerRegistry(t, nil)
	_, _, faults := assembleWithSkills(t, reg, "anthropic", loader)
	if !strings.Contains(strings.Join(faults, "\n"), "description is required") {
		t.Fatalf("assemblePromptReported did not carry the notices: %v", faults)
	}
}

func TestSkillLoadErrorIsAFaultNotAnEmptySection(t *testing.T) {
	reg := routerRegistry(t, nil)
	text, _, faults := assembleWithSkills(t, reg, "anthropic", fakeSkills{err: errors.New("disk on fire")})
	if strings.Contains(text, "devcouncil_load_skill") {
		t.Fatal("a failed load produced a section")
	}
	if !strings.Contains(strings.Join(faults, "\n"), "disk on fire") {
		t.Fatalf("a failed load was not reported: %v", faults)
	}
	if text == "" {
		t.Fatal("the turn has no prompt")
	}
}
