package contract

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
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
// graph, so a package that gets linked but not named fails here. The tool
// counts are guarded where the registry lives, in
// devcouncil/toolcount_docs_test.go, because only that package may build the
// registry with a work binding.

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
