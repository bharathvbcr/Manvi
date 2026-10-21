package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/harness"
)

// Attack 7: the operator-scope claim has to hold for every way a repository
// config can spell the key, not only the one flat lowercase line.
func TestARepositoryConfigCannotSelectASpecWhateverTheSpelling(t *testing.T) {
	spellings := map[string]string{
		"flat":          "harness.spec: sneaky\n",
		"nested":        "harness:\n  spec: sneaky\n",
		"quoted":        "\"harness.spec\": sneaky\n",
		"padded":        "harness.spec : sneaky\n",
		"tab nested":    "harness:\n\tspec: sneaky\n",
		"bom":           "\ufeffharness.spec: sneaky\n",
		"crlf":          "harness.spec: sneaky\r\n",
		"after comment": "# x\nharness.spec: sneaky # why\n",
	}
	for name, body := range spellings {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(envUserConfig, filepath.Join(dir, "none.yaml"))
			reg, err := newRegistry(cfg)
			if err == nil {
				got, src, _ := reg.String(harnessSpecKey)
				t.Fatalf("a repository config selected a spec: value %q from %v", got, src)
			}
		})
	}
}

// Attack 5/2: a spec role that a model later redefines under the same name
// (devcouncil_define_subagent overwrites any non-shipped name) is the model's
// definition from then on. The next turn's Apply must not silently put the
// spec's definition back over it, and removing the spec must not delete it.
func TestASpecDoesNotRemoveARoleAModelRedefinedUnderItsName(t *testing.T) {
	reg := agents.NewRegistry()
	var roles harness.Roles
	rs := []harness.RoleSpec{{Name: "scout", Description: "Looks around.", ReadOnly: true}}
	roles.Apply(reg, rs, harness.Surface{Writes: true})

	modelDef := agents.Definition{Name: "scout", Role: "scout", Description: "model's", SystemPrompt: "mine", EnableWriteTools: true}
	if err := reg.Register(modelDef); err != nil {
		t.Fatal(err)
	}
	roles.Apply(reg, nil, harness.Surface{Writes: true})
	got, ok := reg.Get("scout")
	if !ok || got.SystemPrompt != "mine" {
		t.Fatalf("removing the spec deleted a role the model defined: %+v ok=%v", got, ok)
	}
}

func TestASpecDoesNotOverwriteARoleAModelRedefinedUnderItsName(t *testing.T) {
	reg := agents.NewRegistry()
	var roles harness.Roles
	rs := []harness.RoleSpec{{Name: "scout", Description: "Looks around.", ReadOnly: true}}
	roles.Apply(reg, rs, harness.Surface{Writes: true})
	modelDef := agents.Definition{Name: "scout", Role: "scout", Description: "model's", SystemPrompt: "mine", EnableWriteTools: true}
	if err := reg.Register(modelDef); err != nil {
		t.Fatal(err)
	}
	notices := roles.Apply(reg, rs, harness.Surface{Writes: true})
	got, _ := reg.Get("scout")
	if got.SystemPrompt != "mine" && len(notices) == 0 {
		t.Fatalf("the next turn silently replaced the model's role with the spec's: %+v", got)
	}
}

// Attack 4: reservedPromptNames is computed before the skills listing and the
// project instructions are resolved into opts, so it must still name every
// source the assembled prompt owns once they are present.
func TestReservedNamesCoverEverySourceOfAFullyPopulatedPrompt(t *testing.T) {
	reg := specRegistry(t, nil)
	skills, _ := skillsForPrompt(context.Background(), fakeSkills{set: harness.SkillSet{Skills: []harness.Skill{{Name: "a", Description: "d"}}}}, 0)
	full := PromptOptions{Provider: "anthropic", TaskToolsOffered: true, Skills: skills}
	bare := PromptOptions{Provider: "anthropic", TaskToolsOffered: true}
	reservedBare := reservedPromptNames(reg, bare)
	var missing []string
	for _, n := range reservedPromptNames(reg, full) {
		found := false
		for _, r := range reservedBare {
			if r == n {
				found = true
			}
		}
		if !found {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("names the assembled prompt owns but validation was not told about: %s", strings.Join(missing, ", "))
	}
}

// Attack 4/5 end to end: a spec that validated, was digested and logged as
// applied must have its sections in the prompt. A section named like the skills
// listing passes Validate (the listing is resolved after the spec) and is then
// dropped by the assembler's duplicate refusal, so the log says "spec applied,
// digest D" for a prompt that does not carry the spec's text.
func TestAValidatedSpecSectionIsNeverSilentlyAbsentFromThePromptItIsLoggedOn(t *testing.T) {
	skills := fakeSkills{set: harness.SkillSet{Skills: []harness.Skill{{Name: "alpha", Description: "first"}}}}
	spec := harness.Spec{Name: "house", Sections: []harness.Section{{Name: "skills", Order: 61, Text: "SPEC-SKILLS-TEXT"}}}
	files := repoFiles{harness.SpecsDir + "/house.json": string(spec.Canonical())}
	reg := specRegistry(t, map[string]string{harnessSpecKey: "house"})
	text, _, faults, id := assembleTurn(t, reg, "anthropic", inputsFor(files, agents.NewRegistry(), skills), &turnState{})
	if id.Digest == "" {
		return // refused outright: acceptable, nothing was logged as applied
	}
	if !strings.Contains(text, "SPEC-SKILLS-TEXT") {
		t.Fatalf("spec %s was recorded as applied but its section is not in the prompt (faults: %v)", id.Digest[:8], faults)
	}
}
