package contract

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// advParse feeds the real parser a script, without the bench/ existence check.
func advParse(script string) ([]string, error) { return parseBenchSuites(script, nil) }

// The real verify.sh: every token the helper counts is a python suite file that
// exists under bench/, so the count is a count of suites.
func TestAdvB29ResidueBenchTokensAreRealSuites(t *testing.T) {
	suites := verifyBenchSuites(t) // fails the test on a non-.py word or a missing file
	if len(suites) < 5 {
		t.Fatalf("only %d suites parsed", len(suites))
	}
}

// A suite added on a continuation line changes the count.
func TestAdvB29ResidueBenchContinuationSuiteIsCounted(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(docRoot, "verify.sh"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := advParse(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	mut := strings.Replace(string(raw), "batching2/test_batching2.py; do", "batching2/test_batching2.py \\\n         extra/test_new.py; do", 1)
	got, err := advParse(mut)
	if err != nil || len(got) != len(base)+1 {
		t.Errorf("adding a suite on a continuation line: %d -> %d suites (err %v)", len(base), len(got), err)
	}
}

// A glob token is one shell word that expands to many suites: refused.
func TestAdvB29ResidueBenchGlobTokenFailsLoudly(t *testing.T) {
	if toks, err := advParse("bench_ran=0\nfor t in test_a.py batching*/test_*.py; do\n"); err == nil {
		t.Errorf("a glob token was accepted: %v", toks)
	}
}

// A trailing space after the backslash breaks the shell continuation: the lone
// backslash is refused, not counted as a suite.
func TestAdvB29ResidueBenchBrokenContinuationFailsLoudly(t *testing.T) {
	if toks, err := advParse("bench_ran=0\nfor t in test_a.py \\ \n  test_b.py; do\n"); err == nil {
		t.Errorf("a broken continuation was accepted: %v", toks)
	}
}

// A second suite loop beside the first is refused, not silently ignored.
func TestAdvB29ResidueBenchSecondLoopIsNotSilentlyIgnored(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(docRoot, "verify.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := advParse(string(raw)); err != nil {
		t.Fatalf("the real verify.sh must parse: %v", err)
	}
	if toks, err := advParse(string(raw) + "\nfor t in test_z.py; do :; done\n"); err == nil {
		t.Errorf("a second 'for t in' loop was accepted: %v", toks)
	}
}

// A suite file that does not exist under bench/ is refused when the check runs.
func TestAdvB29ResidueBenchMissingSuiteFileIsRefused(t *testing.T) {
	_, err := parseBenchSuites("bench_ran=0\nfor t in a.py b.py; do\n", func(s string) bool { return s == "a.py" })
	if err == nil {
		t.Fatal("a suite absent from bench/ was accepted")
	}
}

// Renaming the loop variable or the bench_ran initialiser fails loudly.
func TestAdvB29ResidueBenchRenamedLoopIsLoud(t *testing.T) {
	for _, s := range []string{
		"bench_ran=0\nfor s in a.py b.py; do\n",
		"bench_total=0\nfor t in a.py b.py; do\n",
		"bench_ran=0\n\nfor t in a.py b.py; do\n",
		"bench_ran=0\nfor t in a.py b.py\ndo\n",
	} {
		if _, err := advParse(s); err == nil {
			t.Errorf("mutated script unexpectedly parsed: %q", s)
		}
	}
}

// The ARCHITECTURE test walks moduleRoot with fs.WalkDir; a missing root
// surfaces as an error to the callback and the test Fatals (HOLDS).
func TestAdvB29ResidueArchitectureWalkFailsOnMissingRoot(t *testing.T) {
	src, err := os.ReadFile("docs_residue_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "t.Skip") {
		t.Error("docs_residue_test.go skips somewhere")
	}
	if !strings.Contains(string(src), "if err != nil {\n\t\t\treturn err") {
		t.Error("the walk callback no longer propagates a walk error")
	}
}

// The listing must be derived from the same listing block, not any " pkg/ "
// elsewhere in the document: every imported package must appear inside the
// go_orchestrator block of the tree diagram.
func TestAdvB29ResidueArchitecturePackagesAreInsideTheListingBlock(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(docRoot, "docs", "ARCHITECTURE.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	i := strings.Index(doc, "../DevCouncil/backend/go_orchestrator/  #")
	if i < 0 {
		t.Fatal("go_orchestrator tree block not found")
	}
	j := strings.Index(doc[i:], "native/")
	if j < 0 {
		t.Fatal("end of block not found")
	}
	block := doc[i : i+j]
	const prefix = "github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/"
	re := regexp.MustCompile(`"` + regexp.QuoteMeta(prefix) + `([a-z_0-9]+)`)
	seen := map[string]bool{}
	tree := os.DirFS(moduleRoot)
	if err := fs.WalkDir(tree, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, err := fs.ReadFile(tree, p)
		if err != nil {
			return err
		}
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			seen[m[1]] = true
		}
		return nil
	}); err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	if len(seen) < 5 {
		t.Fatalf("only %d packages found", len(seen))
	}
	for pkg := range seen {
		if !strings.Contains(block, " "+pkg+"/ ") {
			t.Errorf("%s/ is imported but not inside the go_orchestrator listing block", pkg)
		}
	}
}

// The reverse direction: a package named in the listing that the pinned
// go_orchestrator no longer has goes unnoticed by the change's test.
func TestAdvB29ResidueArchitectureListingNamesOnlyExistingPackages(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(docRoot, "docs", "ARCHITECTURE.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	i := strings.Index(doc, "../DevCouncil/backend/go_orchestrator/  #")
	j := strings.Index(doc[i:], "native/")
	if i < 0 || j < 0 {
		t.Fatal("block not found")
	}
	pinned := filepath.Join(moduleRoot, "..", "..", "DevCouncil", "backend", "go_orchestrator")
	pinnedFS := os.DirFS(pinned)
	if _, err := fs.Stat(pinnedFS, "."); err != nil {
		t.Fatalf("the pinned module dir must be found, never skipped: %v", err)
	}
	for _, m := range regexp.MustCompile(`(?m)^│   ├── ([a-z_0-9]+)/|^│   └── ([a-z_0-9]+)/`).FindAllStringSubmatch(doc[i:i+j], -1) {
		name := m[1] + m[2]
		if st, err := fs.Stat(pinnedFS, name); err != nil || !st.IsDir() {
			t.Errorf("ARCHITECTURE.md lists %s/ but the pinned go_orchestrator has no such directory", name)
		}
	}
}
