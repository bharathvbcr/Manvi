package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/prompt"
)

// rootRead is a stand-in for the repository's contained reader: it reads under
// root, honours the limit, and refuses a path that resolves outside root. The
// production reader is devcouncil.ReadPinned; the escape cases are exercised
// against that one in manvi/devcouncil.
func rootRead(root string) ReadFunc {
	return func(rel string, limit int64) ([]byte, error) {
		r, err := os.OpenRoot(root)
		if err != nil {
			return nil, err
		}
		defer func() { _ = r.Close() }()
		f, err := r.Open(rel)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		st, err := f.Stat()
		if err != nil {
			return nil, err
		}
		if st.Size() > limit {
			return nil, os.ErrInvalid
		}
		b := make([]byte, st.Size())
		_, err = f.Read(b)
		if err != nil && st.Size() > 0 {
			return nil, err
		}
		return b, nil
	}
}

func writeSkill(t *testing.T, root, dir, content string) {
	t.Helper()
	p := filepath.Join(root, SkillsDir, dir)
	if err := os.MkdirAll(p, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, SkillFile), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func load(t *testing.T, root string) SkillSet {
	t.Helper()
	set, err := LoadSkills(root, rootRead(root))
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	return set
}

func reasonFor(t *testing.T, set SkillSet, name string) string {
	t.Helper()
	r, ok := set.Rejection(name)
	if !ok {
		t.Fatalf("%q was not rejected; skills=%v rejected=%v", name, set.Skills, set.Rejected)
	}
	return r
}

func TestValidSkillParses(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "go-review", "---\nname: go-review\ndescription: \"Review Go: carefully\"\nother: x\n---\n\n# Body\nsteps\n")
	set := load(t, root)
	if len(set.Skills) != 1 || len(set.Rejected) != 0 {
		t.Fatalf("got %+v", set)
	}
	s := set.Skills[0]
	if s.Name != "go-review" || s.Description != "Review Go: carefully" || s.Body != "# Body\nsteps\n" {
		t.Errorf("parsed wrong: %+v", s)
	}
	if got, ok := set.Find("go-review"); !ok || got.Name != "go-review" {
		t.Errorf("Find: %v %v", got, ok)
	}
}

func TestNameMustEqualDirectory(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "alpha", "---\nname: beta\ndescription: d\n---\nbody")
	writeSkill(t, root, "noname", "---\ndescription: d\n---\nbody")
	set := load(t, root)
	if len(set.Skills) != 0 {
		t.Fatalf("accepted: %+v", set.Skills)
	}
	if r := reasonFor(t, set, "alpha"); !strings.Contains(r, `"beta"`) || !strings.Contains(r, `"alpha"`) {
		t.Errorf("reason should name both: %q", r)
	}
	if r := reasonFor(t, set, "noname"); !strings.Contains(r, "no name") {
		t.Errorf("reason: %q", r)
	}
}

func TestMissingDescriptionRejected(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "a", "---\nname: a\n---\nbody")
	writeSkill(t, root, "b", "---\nname: b\ndescription:   \n---\nbody")
	writeSkill(t, root, "c", "---\nname: c\ndescription: >\n  folded\n---\nbody")
	set := load(t, root)
	if len(set.Skills) != 0 {
		t.Fatalf("accepted: %+v", set.Skills)
	}
	for _, n := range []string{"a", "b"} {
		if r := reasonFor(t, set, n); !strings.Contains(r, "no description") {
			t.Errorf("%s: %q", n, r)
		}
	}
	if r := reasonFor(t, set, "c"); !strings.Contains(r, "multi-line") {
		t.Errorf("block scalar must be refused, not become the description %q: %q", ">", r)
	}
}

func TestNoFrontmatterRejectedWithReason(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "plain", "# just markdown\n")
	writeSkill(t, root, "open", "---\nname: open\ndescription: d\nnever closed\n")
	set := load(t, root)
	if r := reasonFor(t, set, "plain"); !strings.Contains(r, "no frontmatter") {
		t.Errorf("plain: %q", r)
	}
	if r := reasonFor(t, set, "open"); !strings.Contains(r, "not closed") {
		t.Errorf("open: %q", r)
	}
}

func TestCRLFFrontmatter(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "win", "\ufeff---\r\nname: win\r\ndescription: 'it''s fine'\r\n---\r\nbody\r\n")
	set := load(t, root)
	if len(set.Skills) != 1 || set.Skills[0].Description != "it's fine" {
		t.Fatalf("got %+v", set)
	}
}

func TestBadNamesRefused(t *testing.T) {
	for _, n := range []string{"", "../x", "a/b", ".hidden", "Upper", "аpple" /* Cyrillic a */, "-lead", "a b", "a.b", "a\x00b", "ｆｕｌｌ"} {
		if ValidSkillName(n) {
			t.Errorf("ValidSkillName(%q) = true", n)
		}
	}
	for _, n := range []string{"a", "go-review", "x_1", "0day"} {
		if !ValidSkillName(n) {
			t.Errorf("ValidSkillName(%q) = false", n)
		}
	}

	root := t.TempDir()
	for _, d := range []string{".hidden", "Upper", "аpple"} {
		writeSkill(t, root, d, "---\nname: "+d+"\ndescription: d\n---\nb")
	}
	set := load(t, root)
	if len(set.Skills) != 0 || len(set.Rejected) != 3 {
		t.Fatalf("got %+v", set)
	}
	for _, r := range set.Rejected {
		if !strings.Contains(r.Reason, "must match") {
			t.Errorf("%q: %q", r.Name, r.Reason)
		}
	}
}

func TestSymlinkEscapeRefused(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, SkillFile), []byte("---\nname: evil\ndescription: d\n---\nsecret"), 0o600); err != nil {
		t.Fatal(err)
	}
	skills := filepath.Join(root, SkillsDir)
	if err := os.MkdirAll(skills, 0o750); err != nil {
		t.Fatal(err)
	}
	// A skill directory that is a link out of the repository.
	if err := os.Symlink(outside, filepath.Join(skills, "evil")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	// A SKILL.md that is a link out of the repository.
	writeSkill(t, root, "filelink", "x")
	fl := filepath.Join(skills, "filelink", SkillFile)
	if err := os.Remove(fl); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, SkillFile), fl); err != nil {
		t.Fatal(err)
	}
	set := load(t, root)
	if len(set.Skills) != 0 {
		t.Fatalf("a skill outside the repository was loaded: %+v", set.Skills)
	}
	reasonFor(t, set, "evil")
	reasonFor(t, set, "filelink")
}

func TestSymlinkedSkillsDirOutsideRootIsAnError(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".agents"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, SkillsDir)); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if _, err := LoadSkills(root, rootRead(root)); err == nil {
		t.Fatal("a skills directory that escapes the repository must be an error, not 'no skills'")
	}
}

func TestSizeCapEnforced(t *testing.T) {
	root := t.TempDir()
	head := "---\nname: big\ndescription: d\n---\n"
	writeSkill(t, root, "big", head+strings.Repeat("x", MaxSkillBytes))
	writeSkill(t, root, "fits", "---\nname: fits\ndescription: d\n---\n"+strings.Repeat("x", MaxSkillBytes-len("---\nname: fits\ndescription: d\n---\n")))
	set := load(t, root)
	if _, ok := set.Find("big"); ok {
		t.Fatal("oversized skill accepted")
	}
	if r := reasonFor(t, set, "big"); !strings.Contains(r, "cannot read") {
		t.Errorf("reason: %q", r)
	}
	if _, ok := set.Find("fits"); !ok {
		t.Errorf("a skill exactly at the cap must load: %+v", set.Rejected)
	}
}

func TestOverlongDescriptionRejected(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "long", "---\nname: long\ndescription: "+strings.Repeat("é", MaxDescriptionRunes+1)+"\n---\nb")
	if r := reasonFor(t, load(t, root), "long"); !strings.Contains(r, "over the") {
		t.Errorf("reason: %q", r)
	}
}

func TestMissingSkillsDirIsNoSkills(t *testing.T) {
	set, err := LoadSkills(t.TempDir(), rootRead(t.TempDir()))
	if err != nil || len(set.Skills) != 0 || len(set.Rejected) != 0 {
		t.Fatalf("got %+v, %v", set, err)
	}
}

func TestSkillsDirThatIsAFileIsAnError(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".agents"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, SkillsDir), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSkills(root, rootRead(root)); err == nil {
		t.Fatal("an unlistable skills directory must be an error")
	}
}

func TestDeterministicSortedOrder(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"zeta", "alpha", "mid", "beta2", "beta"} {
		writeSkill(t, root, n, "---\nname: "+n+"\ndescription: d "+n+"\n---\nb")
	}
	writeSkill(t, root, "Bad", "---\nname: Bad\ndescription: d\n---\nb")
	writeSkill(t, root, "aaa-bad", "no frontmatter")
	a, b := load(t, root), load(t, root)
	var names []string
	for _, s := range a.Skills {
		names = append(names, s.Name)
	}
	if got := strings.Join(names, ","); got != "alpha,beta,beta2,mid,zeta" {
		t.Errorf("order: %s", got)
	}
	if len(a.Rejected) != 2 || a.Rejected[0].Name != `"Bad"` || a.Rejected[1].Name != "aaa-bad" {
		t.Errorf("rejected order: %+v", a.Rejected)
	}
	if !equalSets(a, b) {
		t.Error("two loads of one tree differ")
	}
}

func equalSets(a, b SkillSet) bool {
	if len(a.Skills) != len(b.Skills) || len(a.Rejected) != len(b.Rejected) {
		return false
	}
	for i := range a.Skills {
		if a.Skills[i] != b.Skills[i] {
			return false
		}
	}
	for i := range a.Rejected {
		if a.Rejected[i] != b.Rejected[i] {
			return false
		}
	}
	return true
}

func TestMaxSkillsBound(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < MaxSkills+3; i++ {
		n := "s" + string(rune('a'+i/26/26%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i%26))
		writeSkill(t, root, n, "---\nname: "+n+"\ndescription: d\n---\nb")
	}
	set := load(t, root)
	if len(set.Skills) != MaxSkills || len(set.Rejected) != 0 || set.NotExamined != 3 {
		t.Fatalf("skills=%d rejected=%d notExamined=%d", len(set.Skills), len(set.Rejected), set.NotExamined)
	}
}

func TestSkillsSection(t *testing.T) {
	if text, ok := SkillsSection(nil, 0); ok || text != "" {
		t.Fatalf("no skills must mean no section, got %q %v", text, ok)
	}
	in := []Skill{{Name: "zeta", Description: "last"}, {Name: "alpha", Description: "first"}}
	a, ok := SkillsSection(in, 0)
	if !ok {
		t.Fatal("no section")
	}
	b, _ := SkillsSection(in, 0)
	if a != b {
		t.Error("two calls differ")
	}
	if in[0].Name != "zeta" {
		t.Error("the caller's slice was reordered")
	}
	if !strings.Contains(a, "devcouncil_load_skill") {
		t.Error("section does not say how to load a skill")
	}
	if i, j := strings.Index(a, "- alpha — first"), strings.Index(a, "- zeta — last"); i < 0 || j < 0 || i > j {
		t.Errorf("not sorted by name:\n%s", a)
	}
	rev, _ := SkillsSection([]Skill{in[1], in[0]}, 0)
	if rev != a {
		t.Error("input order changed the output")
	}
}

func TestSkillsSectionCapIsDeclared(t *testing.T) {
	var skills []Skill
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		skills = append(skills, Skill{Name: n, Description: strings.Repeat("w", 100)})
	}
	full, _ := SkillsSection(skills, 0)
	budget := prompt.EstimateTokens(full) * 9 / 10
	capped, ok := SkillsSection(skills, budget)
	if !ok {
		t.Fatal("no section")
	}
	if !strings.Contains(capped, "[the skills list was cut here") {
		t.Fatalf("a capped listing must say so:\n%s", capped)
	}
	if strings.Contains(capped, "- f —") || !strings.Contains(capped, "- a —") {
		t.Errorf("cut must fall on a line boundary, keeping the first skills whole:\n%s", capped)
	}
	if !strings.Contains(capped, namesLead) {
		t.Errorf("the skills that lost their description must be named:\n%s", capped)
	}
	if capped == full {
		t.Error("cap changed nothing")
	}
	again, _ := SkillsSection(skills, budget)
	if again != capped {
		t.Error("capped output is not deterministic")
	}
}

func FuzzParseFrontmatter(f *testing.F) {
	for _, s := range []string{
		"---\nname: a\ndescription: d\n---\nbody",
		"---\r\nname: \"a\"\r\ndescription: 'b''c'\r\n---\r\n",
		"\ufeff---\nname: a\n---",
		"---\ndescription: >\n x\n---",
		"---\nname: \"unterminated\n---",
		"", "---", "---\n", "name: a\n---",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		fields, body, err := ParseFrontmatter(in)
		if err != nil {
			if fields != nil || body != "" {
				t.Fatalf("error with results: %v %q", fields, body)
			}
			return
		}
		for k, v := range fields {
			if k != "name" && k != "description" {
				t.Fatalf("unexpected key %q", k)
			}
			for _, r := range v {
				if forbiddenRune(r) {
					t.Fatalf("value %q carries U+%04X", v, r)
				}
			}
			if strings.ContainsAny(v, "\n\r") || v != strings.Join(strings.Fields(v), " ") {
				t.Fatalf("value %q is not a normalised single line", v)
			}
		}
		if len(body) > len(in) {
			t.Fatalf("body longer than input")
		}
		// And what loadSkill would put in the always-on prompt.
		if s, reason := loadSkill("foo", stubRead(in)); reason == "" {
			for _, r := range s.Name + s.Description {
				if forbiddenRune(r) {
					t.Fatalf("accepted skill carries U+%04X", r)
				}
			}
		}
	})
}
