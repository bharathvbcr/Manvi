package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/harness"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/local"
	"github.com/bharathvbcr/Manvi/manvi/prompt"
	"github.com/bharathvbcr/Manvi/manvi/session"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// specRegistry is a harness registry with the spec selector defined, as
// newRegistry defines it, and the given settings loaded.
func specRegistry(t *testing.T, settings map[string]string) *flags.Registry {
	t.Helper()
	return registryWith(t, settings)
}

// repoFiles is a fake repository reader over .devcouncil/harness.
type repoFiles map[string]string

func (r repoFiles) read(context.Context) harness.ReadFunc {
	return func(rel string, _ int64) ([]byte, error) {
		if v, ok := r[rel]; ok {
			return []byte(v), nil
		}
		return nil, fmt.Errorf("reading %s: %w", rel, fs.ErrNotExist)
	}
}

func specJSON(name, text string) string {
	b, _ := json.Marshal(harness.Spec{Name: name, Sections: []harness.Section{{Name: "house-style", Order: 60, Text: text}}})
	return string(b)
}

func inputsFor(files repoFiles, roles *agents.Registry, skills skillLoader) turnInputs {
	if skills == nil {
		skills = fakeSkills{}
	}
	return turnInputs{skills: skills, repoRead: files.read, roles: roles, pipeline: nil}
}

func assembleTurn(t *testing.T, reg *flags.Registry, provider string, in turnInputs, st *turnState) (string, prompt.Report, []string, specIdentity) {
	t.Helper()
	opts := PromptOptions{Provider: provider, TaskToolsOffered: true}
	id := resolveTurnInputs(context.Background(), reg, in, st, &opts)
	text, report, faults := assemblePromptReported(reg, opts)
	return text, report, faults, id
}

func TestHarnessSpecIsAnOperatorScopeSetting(t *testing.T) {
	reg := specRegistry(t, nil)
	if err := reg.Set(flags.Agent, harnessSpecKey, "x"); err == nil {
		t.Fatal("an agent set the spec selector")
	}
	if err := reg.Set(flags.Human, harnessSpecKey, "x"); err != nil {
		t.Fatal(err)
	}

	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte("harness.spec: sneaky\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := newRegistry(cfg)
	if err == nil || !strings.Contains(err.Error(), "operator scope") || !strings.Contains(err.Error(), harnessSpecKey) {
		t.Fatalf("a repository config naming a spec must be refused with the rule, got %v", err)
	}
}

func TestTwoAssembliesOfTheSameSpecAreByteIdentical(t *testing.T) {
	reg := specRegistry(t, map[string]string{harnessSpecKey: "house"})
	files := repoFiles{harness.SpecsDir + "/house.json": specJSON("house", "Prefer small functions.")}
	for _, provider := range []string{"anthropic", local.Name} {
		a, _, fa, ida := assembleTurn(t, reg, provider, inputsFor(files, nil, nil), &turnState{})
		b, _, fb, idb := assembleTurn(t, reg, provider, inputsFor(files, nil, nil), &turnState{})
		if len(fa)+len(fb) != 0 {
			t.Fatalf("%s: faults %v %v", provider, fa, fb)
		}
		if a != b || ida != idb {
			t.Fatalf("%s: two assemblies of one spec differ", provider)
		}
		if !strings.Contains(a, "Prefer small functions.") {
			t.Fatalf("%s: the spec's text is not in the prompt", provider)
		}
		if ida.Name != "house" || len(ida.Digest) != 64 {
			t.Fatalf("%s: identity %+v", provider, ida)
		}
	}
}

func TestASpecSectionNeverDisplacesAnEssentialSectionUnderBudgetPressure(t *testing.T) {
	// A 2,048-token window is a 256-token prompt budget, far under what the
	// essential sections cost; the spec section is within its own share (64).
	settings := map[string]string{harnessSpecKey: "house", flags.LLMLocalContextWindow: "2048"}
	reg := specRegistry(t, settings)
	files := repoFiles{harness.SpecsDir + "/house.json": specJSON("house", "Prefer small functions and name the invariant.")}

	_, base, _, _ := assembleTurn(t, specRegistry(t, map[string]string{flags.LLMLocalContextWindow: "2048"}), local.Name, inputsFor(files, nil, nil), &turnState{})
	text, report, faults, id := assembleTurn(t, reg, local.Name, inputsFor(files, nil, nil), &turnState{})

	if strings.Contains(text, "Prefer small functions") {
		t.Fatal("the spec section survived a budget the essentials already exceed")
	}
	included := map[string]bool{}
	for _, in := range report.Included {
		included[in.Name] = true
	}
	for _, in := range base.Included {
		if !included[in.Name] {
			t.Errorf("essential section %q was displaced by the spec", in.Name)
		}
	}
	dropped := false
	for _, o := range report.Omitted {
		if o.Name == "house-style" {
			dropped = true
		}
	}
	if !dropped {
		t.Fatalf("the spec section is not named among the omitted: %+v", report.Omitted)
	}
	if !strings.Contains(strings.Join(faults, "\n"), `"house-style" is missing from the prompt`) {
		t.Fatalf("no fault names the dropped spec section: %v", faults)
	}

	log := session.NewLog()
	if err := recordPromptAssembly(log, report, text, promptTokenBudget(reg, local.Name), id); err != nil {
		t.Fatal(err)
	}
	d := lastPromptAssembled(t, log)
	if !slices.ContainsFunc(d.Dropped, func(s string) bool { return strings.HasPrefix(s, "house-style:") }) {
		t.Fatalf("PromptAssembled.Dropped does not name the spec section: %v", d.Dropped)
	}
}

func lastPromptAssembled(t *testing.T, log *session.Log) session.PromptAssembledData {
	t.Helper()
	var d session.PromptAssembledData
	found := false
	for _, e := range log.Events() {
		if e.Type == session.PromptAssembled {
			found = true
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !found {
		t.Fatal("no prompt/assembled event")
	}
	return d
}

func TestPromptAssembledCarriesTheSpecAndAnEditChangesTheDigest(t *testing.T) {
	reg := specRegistry(t, map[string]string{harnessSpecKey: "house"})
	files := repoFiles{harness.SpecsDir + "/house.json": specJSON("house", "Version one.")}
	st := &turnState{}
	log := session.NewLog()

	record := func() session.PromptAssembledData {
		text, report, _, id := assembleTurn(t, reg, "anthropic", inputsFor(files, nil, nil), st)
		if err := recordPromptAssembly(log, report, text, 0, id); err != nil {
			t.Fatal(err)
		}
		return lastPromptAssembled(t, log)
	}
	one := record()
	if one.SpecName != "house" || len(one.SpecDigest) != 64 {
		t.Fatalf("spec identity not recorded: %+v", one)
	}
	if again := record(); again.SpecDigest != one.SpecDigest || again.Digest != one.Digest {
		t.Fatal("an unchanged spec changed the digest")
	}
	files[harness.SpecsDir+"/house.json"] = specJSON("house", "Version two.")
	two := record()
	if two.SpecDigest == one.SpecDigest || two.Digest == one.Digest {
		t.Fatal("a spec edit between turns did not change the spec digest and the prompt digest")
	}

	raw, _ := json.Marshal(session.PromptAssembledData{Tokens: 1})
	if strings.Contains(string(raw), "spec_") {
		t.Fatalf("the additive fields must be omitted when empty: %s", raw)
	}
}

func TestAnUnknownOrInvalidSpecIsAFaultAndTheTurnRuns(t *testing.T) {
	files := repoFiles{
		harness.SpecsDir + "/clash.json": `{"name":"clash","sections":[{"name":"identity","order":60,"text":"x"}]}`,
		harness.SpecsDir + "/wide.json":  `{"name":"wide","roles":[{"name":"r","description":"d","write_exceptions":["write_file"]}]}`,
	}
	baseline, _, _, _ := assembleTurn(t, specRegistry(t, nil), "anthropic", inputsFor(files, nil, nil), &turnState{})

	for name, want := range map[string]string{
		"nonesuch": "no harness spec named",
		"clash":    "collides",
		"wide":     "write_exceptions",
	} {
		reg := specRegistry(t, map[string]string{harnessSpecKey: name})
		text, _, faults, id := assembleTurn(t, reg, "anthropic", inputsFor(files, nil, nil), &turnState{})
		if !strings.Contains(strings.Join(faults, "\n"), want) {
			t.Errorf("%s: no fault says %q: %v", name, want, faults)
		}
		if text != baseline || id != (specIdentity{}) {
			t.Errorf("%s: the turn did not run exactly as it does without a spec", name)
		}
	}
}

func TestReservedNamesComeFromTheAssemblersOwnSources(t *testing.T) {
	reg := specRegistry(t, nil)
	opts := PromptOptions{Provider: "anthropic", TaskToolsOffered: true}
	reserved := reservedPromptNames(reg, opts)
	for name := range sectionNames(t, promptSources(reg, opts)) {
		if !slices.Contains(reserved, name) {
			t.Errorf("fixed section %q is not reserved against a spec", name)
		}
	}
}

func TestASpecSelectsSkillsAndRegistersRolesNoWiderThanTheSession(t *testing.T) {
	skills := fakeSkills{set: harness.SkillSet{Skills: []harness.Skill{
		{Name: "alpha", Description: "first"}, {Name: "beta", Description: "second"},
	}}}
	spec := harness.Spec{
		Name:   "house",
		Skills: []string{"beta", "gamma"},
		Roles:  []harness.RoleSpec{{Name: "scribe", Description: "Writes things."}},
	}
	files := repoFiles{harness.SpecsDir + "/house.json": string(spec.Canonical())}
	roles := agents.NewRegistry()

	// A session whose only tool is read-only.
	pipe := tools.NewRegistry(bus.New())
	if err := pipe.Register(tools.Tool{Schema: llm.ToolSchema{Name: "read_file"}, ReadOnly: true,
		Handler: func(context.Context, tools.Call) tools.Result { return tools.Result{} }}); err != nil {
		t.Fatal(err)
	}
	in := inputsFor(files, roles, skills)
	in.pipeline = pipe

	reg := specRegistry(t, map[string]string{harnessSpecKey: "house"})
	st := &turnState{}
	text, _, faults, _ := assembleTurn(t, reg, "anthropic", in, st)

	if !strings.Contains(text, "- beta") || strings.Contains(text, "- alpha") {
		t.Fatalf("the spec's skill selection was not applied:\n%s", text)
	}
	if !strings.Contains(strings.Join(faults, "\n"), "gamma") {
		t.Fatalf("a selected skill that does not exist must be a notice: %v", faults)
	}
	def, ok := roles.Get("scribe")
	if !ok {
		t.Fatal("the spec's role was not registered")
	}
	if def.EnableWriteTools || def.EnableMCPTools || len(def.WriteExceptions) != 0 {
		t.Fatalf("a spec role on a read-only session is wider than the session: %+v", def)
	}
	if roles.IsBuiltIn("scribe") {
		t.Fatal("a spec role is not built in")
	}

	// Unselecting the spec removes its role and restores the full listing.
	reg2 := specRegistry(t, nil)
	text, _, _, _ = assembleTurn(t, reg2, "anthropic", in, st)
	if _, ok := roles.Get("scribe"); ok {
		t.Fatal("a role outlived the spec that registered it")
	}
	if !strings.Contains(text, "- alpha") {
		t.Fatal("without a spec every skill is listed")
	}
}
