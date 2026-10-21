package devcouncil

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/harness"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// skillFixture is a Registry with a root and nothing else. The skill tool reads
// the repository and consults no task store, so it is exercised without the
// store process the full fixture seeds.
type skillFixture struct {
	root string
	reg  *Registry
}

func newSkillFixture(t *testing.T) *skillFixture {
	t.Helper()
	root := t.TempDir()
	return &skillFixture{root: root, reg: &Registry{deps: Deps{Root: root}, session: &Session{}}}
}

func (f *skillFixture) call(name string, args any) tools.Result {
	raw, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	if name != "devcouncil_load_skill" {
		panic("skillFixture only dispatches devcouncil_load_skill")
	}
	return f.reg.loadSkill(context.Background(), tools.Call{ID: "c1", Name: name, Arguments: raw})
}

func writeSkillFile(t *testing.T, root, name, content string) {
	t.Helper()
	writeRepoFile(t, root, harness.SkillsDir+"/"+name+"/"+harness.SkillFile, content)
}

func TestLoadSkillReturnsBodyAsToolResult(t *testing.T) {
	f := newSkillFixture(t)
	writeSkillFile(t, f.root, "go-review", "---\nname: go-review\ndescription: Review Go\n---\n# Steps\n1. read\n")
	res := f.call("devcouncil_load_skill", map[string]string{"name": "go-review"})
	if res.IsError {
		t.Fatalf("refused: %q", res.Text)
	}
	if res.Text != "# Steps\n1. read\n" {
		t.Errorf("body = %q", res.Text)
	}
}

func TestLoadSkillBodyIsBounded(t *testing.T) {
	f := newSkillFixture(t)
	writeSkillFile(t, f.root, "long", "---\nname: long\ndescription: d\n---\n"+strings.Repeat("é", maxSkillBodyRunes+500))
	res := f.call("devcouncil_load_skill", map[string]string{"name": "long"})
	if res.IsError {
		t.Fatalf("refused: %q", res.Text)
	}
	if !strings.HasSuffix(res.Text, "… (truncated)") {
		t.Error("a cut body must say it was cut")
	}
	if got := len([]rune(res.Text)); got > maxSkillBodyRunes+len([]rune("… (truncated)")) {
		t.Errorf("body is %d runes", got)
	}
}

func TestLoadSkillRefusesUnknownWithAvailableNames(t *testing.T) {
	f := newSkillFixture(t)
	writeSkillFile(t, f.root, "beta", "---\nname: beta\ndescription: d\n---\nb")
	writeSkillFile(t, f.root, "alpha", "---\nname: alpha\ndescription: d\n---\nb")
	res := f.call("devcouncil_load_skill", map[string]string{"name": "gamma"})
	if !res.IsError || !strings.Contains(res.Text, `no skill named "gamma"`) || !strings.Contains(res.Text, "skills directories are: alpha, beta") {
		t.Errorf("got %v %q", res.IsError, res.Text)
	}
}

func TestLoadSkillRefusesWhenRepositoryHasNone(t *testing.T) {
	f := newSkillFixture(t)
	res := f.call("devcouncil_load_skill", map[string]string{"name": "x"})
	if !res.IsError || !strings.Contains(res.Text, "has no skills") {
		t.Errorf("got %v %q", res.IsError, res.Text)
	}
}

func TestLoadSkillRefusesRejectedWithReason(t *testing.T) {
	f := newSkillFixture(t)
	writeSkillFile(t, f.root, "nodesc", "---\nname: nodesc\n---\nSECRET-BODY")
	writeSkillFile(t, f.root, "mismatch", "---\nname: other\ndescription: d\n---\nSECRET-BODY")
	for name, want := range map[string]string{"nodesc": "no description", "mismatch": `does not match directory "mismatch"`} {
		res := f.call("devcouncil_load_skill", map[string]string{"name": name})
		if !res.IsError || !strings.Contains(res.Text, "was rejected") || !strings.Contains(res.Text, want) {
			t.Errorf("%s: got %v %q", name, res.IsError, res.Text)
		}
		if strings.Contains(res.Text, "SECRET-BODY") {
			t.Errorf("%s: a rejected skill's body was returned", name)
		}
	}
}

func TestLoadSkillRefusesTraversalNames(t *testing.T) {
	f := newSkillFixture(t)
	writeRepoFile(t, f.root, "outside/SKILL.md", "---\nname: x\ndescription: d\n---\nLEAK")
	for _, name := range []string{"../../outside", "a/b", ".hidden", "Upper", "", ".agents/skills/x"} {
		res := f.call("devcouncil_load_skill", map[string]string{"name": name})
		if !res.IsError || !strings.Contains(res.Text, "not a skill name") || strings.Contains(res.Text, "LEAK") {
			t.Errorf("%q: got %v %q", name, res.IsError, res.Text)
		}
	}
}

func TestLoadSkillRefusesSymlinkEscape(t *testing.T) {
	f := newSkillFixture(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, harness.SkillFile), []byte("---\nname: evil\ndescription: d\n---\nLEAK"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.root, harness.SkillsDir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.root, harness.SkillsDir, "evil")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	res := f.call("devcouncil_load_skill", map[string]string{"name": "evil"})
	if !res.IsError || strings.Contains(res.Text, "LEAK") {
		t.Errorf("got %v %q", res.IsError, res.Text)
	}
}

// Read-only sub-agent children are given the registry's ReadOnly subset, and
// the lean local profile keeps every tool that is not Extended. The loader must
// survive both: a child told to follow a skill it cannot load has been handed
// an instruction it cannot obey.
func TestLoadSkillIsReadOnlyCoreTool(t *testing.T) {
	r := &Registry{session: &Session{}}
	for _, tl := range r.Tools() {
		if tl.Schema.Name != "devcouncil_load_skill" {
			continue
		}
		if !tl.ReadOnly || tl.Group != tools.GroupCore || tl.Extended {
			t.Errorf("ReadOnly=%v Group=%q Extended=%v; want read-only, core, not extended", tl.ReadOnly, tl.Group, tl.Extended)
		}
		for _, s := range freshRegistry(t).ReadOnlySchemas() {
			if s.Name == tl.Schema.Name {
				return
			}
		}
		t.Fatal("read-only children do not receive devcouncil_load_skill")
	}
	t.Fatal("devcouncil_load_skill is not registered")
}
