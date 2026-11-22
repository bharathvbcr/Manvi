package contract

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// verifyBenchSuites reads the suite list out of the bench leg of verify.sh, the
// artifact that decides how many suites the gate runs.
func verifyBenchSuites(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(docRoot, "verify.sh"))
	if err != nil {
		t.Fatalf("reading verify.sh: %v", err)
	}
	m := regexp.MustCompile(`(?s)bench_ran=0\nfor t in (.*?); do`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("verify.sh no longer has the 'for t in ...; do' bench suite loop this test reads")
	}
	return strings.Fields(strings.ReplaceAll(string(m[1]), "\\\n", " "))
}

// CONTRIBUTING.md states how many benchmark suites verify.sh runs. It said 10
// when the loop ran 13.
func TestContributingBenchSuiteCountMatchesVerify(t *testing.T) {
	suites := verifyBenchSuites(t)
	if len(suites) < 5 {
		t.Fatalf("read only %d bench suites from verify.sh: %v", len(suites), suites)
	}
	raw, err := os.ReadFile(filepath.Join(docRoot, "CONTRIBUTING.md"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`benchmark rig's (\d+) suites`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("CONTRIBUTING.md no longer states the benchmark suite count this test pins")
	}
	if got, _ := strconv.Atoi(string(m[1])); got != len(suites) {
		t.Fatalf("CONTRIBUTING.md says %d bench suites; verify.sh runs %d: %v", got, len(suites), suites)
	}
}

// ARCHITECTURE.md's listing of the sibling DevCouncil go_orchestrator tree must
// name every top-level package the module's non-test code imports from it.
func TestArchitectureListsEveryImportedOrchestratorPackage(t *testing.T) {
	const prefix = "github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/"
	imported := map[string]bool{}
	tree := os.DirFS(moduleRoot)
	err := fs.WalkDir(tree, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := fs.ReadFile(tree, path)
		if err != nil {
			return err
		}
		for _, m := range regexp.MustCompile(`"`+regexp.QuoteMeta(prefix)+`([a-z_0-9]+)`).FindAllSubmatch(src, -1) {
			imported[string(m[1])] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	if len(imported) < 5 {
		t.Fatalf("found only %d imported orchestrator packages: %v", len(imported), imported)
	}
	raw, err := os.ReadFile(filepath.Join(docRoot, "docs", "ARCHITECTURE.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	for pkg := range imported {
		if !strings.Contains(doc, " "+pkg+"/ ") {
			t.Errorf("docs/ARCHITECTURE.md's go_orchestrator listing omits %s/, which this module imports", pkg)
		}
	}
}
