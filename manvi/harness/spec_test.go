package harness

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/agents"
)

// goodSpec is a spec that validates against an unbounded cloud prompt.
func goodSpec() Spec {
	return Spec{
		Name:     "house",
		Sections: []Section{{Name: "house-style", Order: 60, Text: "Prefer small functions."}},
		Roles:    []RoleSpec{{Name: "doc-writer", Description: "Writes documentation.", SystemPrompt: "Write docs."}},
		Skills:   []string{"release"},
		Lessons:  "house-lessons",
	}
}

func cloud() Env { return Env{Provider: "anthropic", Reserved: []string{"identity", "policy"}} }

func problemFields(p Problems) string {
	var out []string
	for _, x := range p {
		out = append(out, x.Field+": "+x.Message)
	}
	return strings.Join(out, "\n")
}

func TestAGoodSpecValidates(t *testing.T) {
	if p := goodSpec().Validate(cloud()); len(p) != 0 {
		t.Fatalf("a good spec has problems:\n%s", problemFields(p))
	}
}

func TestDecodeRefusesUnknownFields(t *testing.T) {
	_, err := DecodeSpec([]byte(`{"name":"x","bogus":1}`))
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("an unknown field must be refused by name, got %v", err)
	}
	_, err = DecodeSpec([]byte(`{"name":"x","roles":[{"name":"r","description":"d","extra":true}]}`))
	if err == nil || !strings.Contains(err.Error(), "extra") {
		t.Fatalf("an unknown role field must be refused by name, got %v", err)
	}
	for _, bad := range []string{``, `{"name":"x"} {}`, `{"name":"x","name":"y"}`, `[]`} {
		if _, err := DecodeSpec([]byte(bad)); err == nil {
			t.Errorf("DecodeSpec(%q) accepted a malformed document", bad)
		}
	}
	if _, err := DecodeSpec([]byte(`{"name":"x","sections":[{"name":"a","order":60,"text":"` + strings.Repeat("a", MaxSpecBytes) + `"}]}`)); err == nil {
		t.Error("an oversized document was accepted")
	}
}

// The keys a spec must never carry, each refused with its own name and a
// reason, at the top level and inside a role.
func TestEveryForbiddenKeyIsRefusedByName(t *testing.T) {
	keys := []string{
		"write_exceptions", "tier", "tiers", "model", "posture", "mcp", "enable_mcp_tools",
		"enable_write_tools", "flags", "verify", "verify_command", "fetch", "fetch_hosts",
		"hooks", "scripts", "plugins",
	}
	for _, key := range keys {
		for _, doc := range []string{
			fmt.Sprintf(`{"name":"x","%s":"v"}`, key),
			fmt.Sprintf(`{"name":"x","roles":[{"name":"r","description":"d","%s":"v"}]}`, key),
			fmt.Sprintf(`{"name":"x","%s":["v"]}`, strings.ToUpper(key)),
		} {
			_, err := DecodeSpec([]byte(doc))
			if err == nil {
				t.Errorf("%s: accepted", doc)
				continue
			}
			if !strings.Contains(strings.ToLower(err.Error()), key) || !strings.Contains(err.Error(), "not allowed") {
				t.Errorf("%s: the refusal does not name %q and say why: %v", doc, key, err)
			}
		}
	}
}

func TestWriteExceptionsAreRefusedOutright(t *testing.T) {
	_, err := DecodeSpec([]byte(`{"name":"x","roles":[{"name":"r","description":"d","write_exceptions":["write_file"]}]}`))
	if err == nil || !strings.Contains(err.Error(), "write_exceptions") {
		t.Fatalf("write_exceptions must be refused, got %v", err)
	}
}

func TestValidateListsEveryProblem(t *testing.T) {
	s := Spec{
		Name: "Bad Name",
		Sections: []Section{
			{Name: "policy", Order: 60, Text: "collides with an essential section"},
			{Name: "early", Order: 10, Text: "orders before the fixed sections"},
			{Name: "braces", Order: 61, Text: "template {{ .X }}"},
		},
		Roles:   []RoleSpec{{Name: "nodesc"}, {Name: "research", Description: "shadows a built-in"}},
		Lessons: "Not A Library",
	}
	p := s.Validate(cloud())
	want := []string{"name", "sections[0]", "sections[1]", "sections[2]", "roles[0]", "roles[1]", "lessons"}
	for _, w := range want {
		found := false
		for _, x := range p {
			if strings.HasPrefix(x.Field, w) {
				found = true
			}
		}
		if !found {
			t.Errorf("no problem reported for %s; got:\n%s", w, problemFields(p))
		}
	}
	if len(p) < 7 {
		t.Fatalf("every problem must be listed, got %d:\n%s", len(p), problemFields(p))
	}
	if p.Err() == nil || !strings.Contains(p.Err().Error(), "Bad Name") && !strings.Contains(p.Err().Error(), "name") {
		t.Fatalf("Err must carry the problems: %v", p.Err())
	}
}

func TestEssentialNameCollisionIsRefused(t *testing.T) {
	s := goodSpec()
	s.Sections[0].Name = "policy"
	p := s.Validate(cloud())
	if len(p) != 1 || !strings.Contains(p[0].Message, "policy") {
		t.Fatalf("want exactly the collision, got:\n%s", problemFields(p))
	}
	s.Sections[0].Name = "Policy"
	if p := s.Validate(cloud()); len(p) == 0 {
		t.Fatal("a name differing only in case must still collide")
	}
}

func TestSectionsMustSitAfterTheFixedOnes(t *testing.T) {
	s := goodSpec()
	s.Sections[0].Order = MinSectionOrder - 1
	if p := s.Validate(cloud()); len(p) != 1 {
		t.Fatalf("order below %d must be refused once, got:\n%s", MinSectionOrder, problemFields(p))
	}
}

func TestPerProviderAlwaysOnCap(t *testing.T) {
	s := goodSpec()
	s.Sections[0].Text = strings.Repeat("word ", 1200) // about 1200 tokens
	if p := s.Validate(cloud()); len(p) != 0 {
		t.Fatalf("an unbounded cloud prompt must take it:\n%s", problemFields(p))
	}
	local := Env{Provider: "local", Reserved: []string{"identity"}, BudgetTokens: 4096}
	p := s.Validate(local)
	if len(p) != 1 || !strings.Contains(p[0].Message, "tokens") {
		t.Fatalf("a local 4096-token budget must refuse it, got:\n%s", problemFields(p))
	}
	// An unbounded budget still has a hard absolute cap.
	s.Sections[0].Text = strings.Repeat("word ", MaxAlwaysOnTokens*2)
	if p := s.Validate(cloud()); len(p) == 0 {
		t.Fatal("the absolute cap did not hold on an unbounded budget")
	}
}

func TestTemplateBracesAndUnrenderableRunesAreRefused(t *testing.T) {
	for _, text := range []string{"a {{ b", "a }} b", "a ${HOME} b", "bell\x07", "zero\u200bwidth", "private\ue000use"} {
		s := goodSpec()
		s.Sections[0].Text = text
		if p := s.Validate(cloud()); len(p) == 0 {
			t.Errorf("section text %q was accepted", text)
		}
		s = goodSpec()
		s.Roles[0].SystemPrompt = text
		if p := s.Validate(cloud()); len(p) == 0 {
			t.Errorf("role prompt %q was accepted", text)
		}
	}
	s := goodSpec()
	s.Sections[0].Text = "line one\n\tindented line two"
	if p := s.Validate(cloud()); len(p) != 0 {
		t.Fatalf("newline and tab are text, not control noise:\n%s", problemFields(p))
	}
}

func TestDigestChangesOnAnyByteAndIgnoresOrder(t *testing.T) {
	base := goodSpec()
	d := base.Digest()
	if d != goodSpec().Digest() || len(d) != 64 {
		t.Fatalf("the digest is not stable: %q", d)
	}
	mutations := map[string]func(*Spec){
		"name":      func(s *Spec) { s.Name = "other" },
		"section":   func(s *Spec) { s.Sections[0].Text += "." },
		"order":     func(s *Spec) { s.Sections[0].Order++ },
		"role":      func(s *Spec) { s.Roles[0].Description += "." },
		"readonly":  func(s *Spec) { s.Roles[0].ReadOnly = true },
		"tools":     func(s *Spec) { s.Roles[0].AllowedTools = []string{"read_file"} },
		"skill":     func(s *Spec) { s.Skills = []string{"releas"} },
		"lessons":   func(s *Spec) { s.Lessons = "house-lessonz" },
		"newsect":   func(s *Spec) { s.Sections = append(s.Sections, Section{Name: "z", Order: 70, Text: "z"}) },
		"dropskill": func(s *Spec) { s.Skills = nil },
	}
	for name, mutate := range mutations {
		s := goodSpec()
		mutate(&s)
		if s.Digest() == d {
			t.Errorf("a change to %s did not change the digest", name)
		}
	}
	a := Spec{Name: "x", Sections: []Section{{Name: "a", Order: 60, Text: "1"}, {Name: "b", Order: 61, Text: "2"}}}
	b := Spec{Name: "x", Sections: []Section{{Name: "b", Order: 61, Text: "2"}, {Name: "a", Order: 60, Text: "1"}}}
	if a.Digest() != b.Digest() {
		t.Error("the canonical form must sort, so listing order is not content")
	}
}

func TestDecodeRoundTripsToTheSameDigest(t *testing.T) {
	s := goodSpec()
	got, err := DecodeSpec(s.Canonical())
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest() != s.Digest() {
		t.Fatal("a canonical encoding must decode to the same digest")
	}
}

func TestSkillsAndLessonsAreValidated(t *testing.T) {
	s := goodSpec()
	s.Skills = []string{"ok", "Bad Skill", "ok"}
	s.Lessons = strings.Repeat("a", 65)
	if p := s.Validate(cloud()); len(p) != 3 {
		t.Fatalf("want a bad name, a duplicate and a long library name, got:\n%s", problemFields(p))
	}
}

func surface(writes bool, tools ...string) Surface { return Surface{Writes: writes, Tools: tools} }

func TestARoleCannotWidenTheSession(t *testing.T) {
	role := RoleSpec{Name: "helper", Description: "Helps.", ReadOnly: false}

	def, _, err := role.Definition(surface(false, "read_file", "write_file"))
	if err != nil {
		t.Fatal(err)
	}
	if def.EnableWriteTools {
		t.Fatal("a role on a read-only session got write tools")
	}
	def, _, _ = role.Definition(surface(true, "read_file"))
	if !def.EnableWriteTools {
		t.Fatal("a role that did not opt down lost the session's write tools")
	}
	role.ReadOnly = true
	def, _, _ = role.Definition(surface(true, "read_file"))
	if def.EnableWriteTools {
		t.Fatal("a role that opted down kept write tools")
	}
	if def.EnableMCPTools || len(def.WriteExceptions) != 0 || def.Tier != "" || def.Model != "" {
		t.Fatalf("a spec role carries a field a spec may not set: %+v", def)
	}
}

func TestAllowedToolsOnlyRemove(t *testing.T) {
	role := RoleSpec{Name: "helper", Description: "Helps.", AllowedTools: []string{"read_file", "not_a_tool"}}
	def, dropped, err := role.Definition(surface(true, "read_file", "grep"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(def.AllowedTools, ",") != "read_file" || strings.Join(dropped, ",") != "not_a_tool" {
		t.Fatalf("want [read_file] with not_a_tool reported, got %v / %v", def.AllowedTools, dropped)
	}
	// Every name unknown to the session: an empty allowlist would mean no
	// narrowing at all, which is a widening, so the role is refused.
	role.AllowedTools = []string{"not_a_tool"}
	if _, _, err := role.Definition(surface(true, "read_file")); err == nil {
		t.Fatal("a role whose allowlist matches no session tool was accepted, and would have been unrestricted")
	}
}

func TestASpecRoleCannotShadowABuiltInOrAnExistingRole(t *testing.T) {
	s := goodSpec()
	s.Roles[0].Name = "critic"
	if p := s.Validate(cloud()); len(p) != 1 || !strings.Contains(p[0].Message, "built-in") {
		t.Fatalf("shadowing a built-in must be refused:\n%s", problemFields(p))
	}

	reg := agents.NewRegistry()
	if err := reg.Register(agents.Definition{Name: "doc-writer", Description: "authored by a model"}); err != nil {
		t.Fatal(err)
	}
	var roles Roles
	notices := roles.Apply(reg, goodSpec().Roles, surface(true))
	if len(notices) != 1 || !strings.Contains(notices[0], "doc-writer") {
		t.Fatalf("overwriting an existing role must be refused loudly: %v", notices)
	}
	if d, _ := reg.Get("doc-writer"); d.Description != "authored by a model" {
		t.Fatal("the existing role was overwritten")
	}
}

func TestRolesApplyAndAreRemovedWhenTheSpecChanges(t *testing.T) {
	reg := agents.NewRegistry()
	var roles Roles
	if n := roles.Apply(reg, goodSpec().Roles, surface(true)); len(n) != 0 {
		t.Fatalf("notices: %v", n)
	}
	d, ok := reg.Get("doc-writer")
	if !ok || d.EnableMCPTools || !d.EnableWriteTools {
		t.Fatalf("role not registered as expected: %+v %v", d, ok)
	}
	if reg.IsBuiltIn("doc-writer") {
		t.Fatal("a spec role is not built in")
	}
	// Applying again is idempotent.
	roles.Apply(reg, goodSpec().Roles, surface(true))
	// Moving to a spec without it removes it, and never touches a built-in.
	roles.Apply(reg, nil, surface(true))
	if _, ok := reg.Get("doc-writer"); ok {
		t.Fatal("a role of the previous spec outlived it")
	}
	if _, ok := reg.Get("critic"); !ok {
		t.Fatal("a built-in role was removed")
	}
	if err := reg.Unregister("critic"); err == nil {
		t.Fatal("Unregister removed a built-in role")
	}
}

func TestCatalogueAndRepoResolveThroughOneResolver(t *testing.T) {
	cat := &Catalogue{}
	if err := cat.Register(goodSpec()); err != nil {
		t.Fatal(err)
	}
	if err := cat.Register(goodSpec()); err == nil {
		t.Fatal("a duplicate catalogue name was accepted")
	}
	repoSpec := Spec{Name: "repo-only", Sections: []Section{{Name: "x", Order: 60, Text: "y"}}}
	read := func(rel string, limit int64) ([]byte, error) {
		switch rel {
		case ".devcouncil/harness/repo-only.json":
			return repoSpec.Canonical(), nil
		case ".devcouncil/harness/liar.json":
			return goodSpec().Canonical(), nil // file name and spec name disagree
		}
		return nil, fmt.Errorf("reading %s: %w", rel, fs.ErrNotExist)
	}
	repo := RepoSpecs{Read: read}

	got, origin, err := Resolve("house", cat, repo)
	if err != nil || got.Name != "house" || origin != "catalogue" {
		t.Fatalf("catalogue: %v %q %v", got.Name, origin, err)
	}
	got, origin, err = Resolve("repo-only", cat, repo)
	if err != nil || got.Name != "repo-only" || !strings.HasPrefix(origin, ".devcouncil/harness/") {
		t.Fatalf("repo: %v %q %v", got.Name, origin, err)
	}
	if _, _, err := Resolve("liar", cat, repo); err == nil {
		t.Fatal("a file whose spec name differs from its file name was accepted")
	}
	if _, _, err := Resolve("missing", cat, repo); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("an unknown name must be an error naming it: %v", err)
	}
	for _, bad := range []string{"../etc/passwd", "a/b", "", "A"} {
		if _, _, err := Resolve(bad, cat, repo); err == nil {
			t.Errorf("Resolve(%q) was accepted", bad)
		}
	}
	// A read that fails for another reason is not "no such spec".
	boom := RepoSpecs{Read: func(string, int64) ([]byte, error) { return nil, errors.New("withheld") }}
	if _, _, err := Resolve("x", boom); err == nil || !strings.Contains(err.Error(), "withheld") {
		t.Fatalf("a failed read must surface its cause: %v", err)
	}
}

func TestSpecSectionsBecomeNonEssentialSourcesAtTheirOrder(t *testing.T) {
	srcs := goodSpec().Sources()
	if len(srcs) != 1 {
		t.Fatalf("want one source, got %d", len(srcs))
	}
	secs, err := srcs[0].Sections()
	if err != nil || len(secs) != 1 || secs[0].Essential || secs[0].Order != 60 || secs[0].Name != "house-style" {
		t.Fatalf("%+v %v", secs, err)
	}
}

func TestFilterSkillsSelectsAndNamesWhatIsMissing(t *testing.T) {
	set := SkillSet{Skills: []Skill{{Name: "a"}, {Name: "b"}, {Name: "c"}}}
	got := FilterSkills(set, []string{"c", "a", "zzz"})
	if len(got.Skills) != 2 || got.Skills[0].Name != "a" || got.Skills[1].Name != "c" {
		t.Fatalf("%+v", got.Skills)
	}
	if len(got.Rejected) != 1 || got.Rejected[0].Name != "zzz" {
		t.Fatalf("a named skill that does not exist must be reported: %+v", got.Rejected)
	}
	if all := FilterSkills(set, nil); len(all.Skills) != 3 {
		t.Fatal("an empty selection means every skill")
	}
}

func FuzzDecodeSpec(f *testing.F) {
	f.Add(goodSpec().Canonical())
	f.Add([]byte(`{"name":"x","roles":[{"name":"r","description":"d","write_exceptions":["a"]}]}`))
	f.Add([]byte(`{"name":"x","sections":[{"name":"a","order":60,"text":"\u0000"}]}`))
	f.Add([]byte(`[[[[`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		s, err := DecodeSpec(raw)
		if err != nil {
			return
		}
		again, err := DecodeSpec(s.Canonical())
		if err != nil {
			t.Fatalf("an accepted spec's canonical form does not decode: %v", err)
		}
		if again.Digest() != s.Digest() {
			t.Fatalf("canonical re-encoding changed the digest")
		}
		_ = s.Validate(Env{Provider: "local", BudgetTokens: 4096})
	})
}
