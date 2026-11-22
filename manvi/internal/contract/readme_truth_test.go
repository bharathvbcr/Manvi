package contract

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/gate"
	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// The README states how Manvi relates to DevCouncil and how many native tools
// it ships. Both were wrong at once: it said Manvi reaches every DevCouncil
// component across a process boundary, "linking none", while manvi/go.mod
// requires DevCouncil's backend/go_orchestrator and imports its Go packages;
// and its badge said 51 tools, its status line 54 (52 with no skills) and its
// bullet 54 (53 with none), against a registry of 54 / 53.
//
// The wording is the owner's design: the Go packages are linked natively and
// pinned, the Rust components are child processes over JSON on stdio, and
// gusset is the one in-process exception. The guard below reads the import
// graph, so a package that gets linked but not named fails here.

const devcouncilGoModule = "github.com/bharathvbcr/DevCouncil/backend/go_orchestrator"

// linkedDevCouncilPackages walks Manvi's non-test Go files and returns the
// import path, relative to DevCouncil's module, of every package they import.
func linkedDevCouncilPackages(t *testing.T) []string {
	t.Helper()
	gomod := readDoc(t, filepath.Join(moduleRoot, "go.mod"))
	if !regexp.MustCompile(`(?m)^\s*(?:require\s+)?` + regexp.QuoteMeta(devcouncilGoModule) + ` `).MatchString(gomod) {
		t.Fatalf("go.mod no longer requires %s: the README's linking statement is now the stale one", devcouncilGoModule)
	}
	seen := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(moduleRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "testdata", "vendor", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			ip, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if rest, ok := strings.CutPrefix(ip, devcouncilGoModule+"/"); ok {
				seen[rest] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", moduleRoot, err)
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	if len(out) < 5 {
		t.Fatalf("found only %d linked DevCouncil packages (%v); the walk is examining almost nothing", len(out), out)
	}
	return out
}

// linkingParagraphs returns every README paragraph that states the link:
// the ones saying go.mod requires backend/go_orchestrator.
func linkingParagraphs(readme string) []string {
	var out []string
	for _, para := range regexp.MustCompile(`\n\s*\n`).Split(readme, -1) {
		if strings.Contains(para, "requires `backend/go_orchestrator`") {
			out = append(out, para)
		}
	}
	return out
}

// TestReadmeDoesNotClaimLinkingNoDevCouncil holds the README to the import
// graph: it must not say Manvi links nothing, and its linking paragraph must
// name, as a code span, the last path element of every DevCouncil package a
// non-test Manvi file imports.
func TestReadmeDoesNotClaimLinkingNoDevCouncil(t *testing.T) {
	readme := readDoc(t, filepath.Join(docRoot, "README.md"))
	if regexp.MustCompile("(?i)linking none").MatchString(readme) {
		t.Error("README.md says Manvi links none of DevCouncil; manvi/go.mod requires DevCouncil's backend/go_orchestrator and imports its Go packages")
	}
	paras := linkingParagraphs(readme)
	if len(paras) == 0 {
		t.Fatal("README.md has no paragraph saying go.mod requires `backend/go_orchestrator`; restate the Go/Rust split")
	}
	for _, pkg := range linkedDevCouncilPackages(t) {
		elem := path.Base(pkg)
		span := regexp.MustCompile("`(?:[A-Za-z0-9_]+/)*" + regexp.QuoteMeta(elem) + "(?:/\\*)?`")
		for _, para := range paras {
			if !span.MatchString(para) {
				t.Errorf("README.md linking paragraph does not name %q (imported as %s/%s by a non-test file): %.80q...", elem, devcouncilGoModule, pkg, para)
			}
		}
	}
}

// observedToolCounts builds the registry the way harness.go does (devcouncil.New
// then Register into a fresh tool pipeline) for a repository with no skills and
// again for one with a skill, and counts what each pipeline exposes. The
// catalogue is the registry's own list, which must agree with the skill case.
func observedToolCounts(t *testing.T) (catalogue, noSkills int) {
	t.Helper()
	open := func(root string) int {
		r, err := devcouncil.New(devcouncil.Deps{
			Store: &store.Client{},
			Gate:  &gate.Gate{},
			Root:  root,
			Work:  &devcouncil.WorkBinding{},
		})
		if err != nil {
			t.Fatalf("build native registry: %v", err)
		}
		pipe := tools.NewRegistry(bus.New())
		if err := r.Register(pipe); err != nil {
			t.Fatalf("register: %v", err)
		}
		catalogue = len(r.Tools())
		return len(pipe.Schemas())
	}
	bare := t.TempDir()
	withSkill := t.TempDir()
	skillDir := filepath.Join(withSkill, ".agents", "skills", "go-review")
	if err := os.MkdirAll(skillDir, 0o750); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: go-review\ndescription: d\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	noSkills = open(bare)
	withSkills := open(withSkill)
	if withSkills != noSkills+1 {
		t.Fatalf("a repository with a skill exposes %d tools, one with none %d; want exactly one more (devcouncil_load_skill)", withSkills, noSkills)
	}
	if withSkills != catalogue {
		t.Fatalf("the skills-present pipeline exposes %d tools but the registry catalogue lists %d", withSkills, catalogue)
	}
	return withSkills, noSkills
}

// TestReadmeNativeToolCountsMatchRegistry checks every native-tool count the
// README and the other documents that restate it give, wherever they give it,
// against the registry observed at test time. Each pattern's capture groups
// are the catalogue count, then the no-skills count where it states one.
func TestReadmeNativeToolCountsMatchRegistry(t *testing.T) {
	catalogue, noSkills := observedToolCounts(t)

	sites := []struct {
		file    string
		name    string
		pattern string
		want    []int
	}{
		{"README.md", "badge", `badge/tools-(\d+)_native`, []int{catalogue}},
		{"README.md", "native tools count", `(?i)\b(\d+) native tools`, []int{catalogue}},
		{"README.md", "diagram node", `Tools\["(\d+) Native Tools"\]`, []int{catalogue}},
		{"README.md", "no-skills count", `(\d+) in a repository with no skills`, []int{noSkills}},
		{"docs/WORKBENCH.md", "native tools", `(\d+) native tools, (\d+) with no skills`, []int{catalogue, noSkills}},
		{"docs/04-visual-architecture-guide.html", "tools node", `the (\d+) native tools \((\d+) without repo skills\)`, []int{catalogue, noSkills}},
		{".github/METADATA.md", "description", `(\d+) native tools \((\d+) with no skills\)`, []int{catalogue, noSkills}},
		{"docs/README.md", "topic slug", `\x60(\d+)-tools\x60`, []int{catalogue}},
		{"docs/README.md", "tools reference", `all (\d+) native tools \((\d+) in a repository with no skills\)`, []int{catalogue, noSkills}},
	}
	for _, s := range sites {
		doc := readDoc(t, filepath.Join(docRoot, s.file))
		ms := regexp.MustCompile(s.pattern).FindAllStringSubmatch(doc, -1)
		if len(ms) == 0 {
			t.Errorf("%s: no %s matched %q; the sentence was reworded, re-point this guard", s.file, s.name, s.pattern)
			continue
		}
		for _, m := range ms {
			for i, want := range s.want {
				got, err := strconv.Atoi(m[i+1])
				if err != nil {
					t.Fatalf("%s: %q is not a number: %v", s.file, m[i+1], err)
				}
				if got != want {
					t.Errorf("%s %s states %d, the registry has %d (pattern %q)", s.file, s.name, got, want, s.pattern)
				}
			}
		}
	}
}
