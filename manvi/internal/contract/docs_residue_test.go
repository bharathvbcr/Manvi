package contract

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var (
	benchSuiteLoop = regexp.MustCompile(`(?s)bench_ran=0\nfor t in (.*?); do`)
	benchLoopStart = regexp.MustCompile(`(?m)^\s*for t in `)
	benchSuiteName = regexp.MustCompile(`^[\w./-]+\.py$`)
)

// parseBenchSuites reads the suite list out of the bench leg of verify.sh text,
// the artifact that decides how many suites the gate runs. It refuses what it
// cannot count honestly: a missing loop, more than one loop (the second would be
// invisible), and any word that is not a plain .py path (a glob stands for many
// suites, a stray backslash is a broken continuation). exists, when not nil,
// must confirm each suite is a real file under bench/.
func parseBenchSuites(script string, exists func(suite string) bool) ([]string, error) {
	if n := len(benchLoopStart.FindAllString(script, -1)); n != 1 {
		return nil, fmt.Errorf("verify.sh has %d 'for t in' loops, the count reads exactly one", n)
	}
	m := benchSuiteLoop.FindStringSubmatch(script)
	if m == nil {
		return nil, errors.New("verify.sh no longer has the 'bench_ran=0' / 'for t in ...; do' suite loop this test reads")
	}
	suites := strings.Fields(strings.ReplaceAll(m[1], "\\\n", " "))
	for _, s := range suites {
		if !benchSuiteName.MatchString(s) {
			return nil, fmt.Errorf("suite word %q is not a plain .py path, so the count of %d words is not a count of suites", s, len(suites))
		}
		if exists != nil && !exists(s) {
			return nil, fmt.Errorf("suite %q is named in verify.sh and absent from bench/", s)
		}
	}
	return suites, nil
}

func verifyBenchSuites(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(docRoot, "verify.sh"))
	if err != nil {
		t.Fatalf("reading verify.sh: %v", err)
	}
	suites, err := parseBenchSuites(string(raw), func(s string) bool {
		st, err := os.Stat(filepath.Join(docRoot, "bench", s))
		return err == nil && st.Mode().IsRegular()
	})
	if err != nil {
		t.Fatal(err)
	}
	return suites
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
