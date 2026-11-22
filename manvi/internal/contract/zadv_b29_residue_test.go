package contract

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// advBenchLoop is the regexp and tokenizer of verifyBenchSuites, copied because
// that helper reads the real verify.sh and cannot be fed a mutated script.
func advBenchLoop(script string) ([]string, bool) {
	m := regexp.MustCompile(`(?s)bench_ran=0\nfor t in (.*?); do`).FindStringSubmatch(script)
	if m == nil {
		return nil, false
	}
	return strings.Fields(strings.ReplaceAll(m[1], "\\\n", " ")), true
}

var advSuiteName = regexp.MustCompile(`^[A-Za-z0-9_./-]+\.py$`)

// The real verify.sh: every token the helper counts is a python suite file that
// exists under bench/, so the count is a count of suites.
func TestAdvB29ResidueBenchTokensAreRealSuites(t *testing.T) {
	suites := verifyBenchSuites(t)
	for _, s := range suites {
		if !advSuiteName.MatchString(s) {
			t.Errorf("verifyBenchSuites counted %q, which is not a .py suite name", s)
		}
		if _, err := os.Stat(filepath.Join(docRoot, "bench", s)); err != nil {
			t.Errorf("suite %q counted from verify.sh does not exist in bench/: %v", s, err)
		}
	}
}

// A suite added on a continuation line changes the count (HOLDS expected).
func TestAdvB29ResidueBenchContinuationSuiteIsCounted(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(docRoot, "verify.sh"))
	if err != nil {
		t.Fatal(err)
	}
	base, ok := advBenchLoop(string(raw))
	if !ok {
		t.Fatal("loop not found")
	}
	mut := strings.Replace(string(raw), "batching2/test_batching2.py; do", "batching2/test_batching2.py \\\n         extra/test_new.py; do", 1)
	got, ok := advBenchLoop(mut)
	if !ok || len(got) != len(base)+1 {
		t.Errorf("adding a suite on a continuation line: %d -> %d suites", len(base), len(got))
	}
}

// A glob token is one shell word that expands to many suites. The count treats
// it as one and passes silently with a wrong number.
func TestAdvB29ResidueBenchGlobTokenFailsLoudly(t *testing.T) {
	script := "bench_ran=0\nfor t in test_a.py batching*/test_*.py; do\n"
	toks, ok := advBenchLoop(script)
	if !ok {
		t.Fatal("loop not found")
	}
	for _, s := range toks {
		if strings.ContainsAny(s, "*?[") {
			t.Errorf("the suite-list parser counts glob token %q as one suite though the shell expands it to many (tokens=%v)", s, toks)
		}
	}
}

// A trailing space after the backslash breaks the shell continuation, but the
// parser's "\\\n" replace does not match it, so the lone backslash is counted as a suite.
func TestAdvB29ResidueBenchBrokenContinuationFailsLoudly(t *testing.T) {
	script := "bench_ran=0\nfor t in test_a.py \\ \n  test_b.py; do\n"
	toks, _ := advBenchLoop(script)
	for _, s := range toks {
		if !advSuiteName.MatchString(s) {
			t.Errorf("token %q (from a broken continuation) is counted as a suite; tokens=%v", s, toks)
		}
	}
}

// A second suite loop added beside the first is invisible: the regexp reads
// only the first, so the count agrees with the doc while verify.sh runs more.
func TestAdvB29ResidueBenchSecondLoopIsNotSilentlyIgnored(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(docRoot, "verify.sh"))
	if err != nil {
		t.Fatal(err)
	}
	n := len(regexp.MustCompile(`(?m)^for t in .*\.py`).FindAllString(string(raw), -1))
	if n != 1 {
		t.Fatalf("verify.sh has %d 'for t in ...py' loops", n)
	}
	mut := string(raw) + "\nfor t in test_z.py; do :; done\n"
	toks, _ := advBenchLoop(mut)
	loops := len(regexp.MustCompile(`(?m)^for t in .*\.py`).FindAllString(mut, -1))
	if loops > 1 && len(toks) == len(verifyBenchSuites(t)) {
		t.Errorf("a second 'for t in' suite loop (%d loops) leaves the parsed count at %d: the doc test would still pass", loops, len(toks))
	}
}

// Renaming the loop variable or the bench_ran initialiser fails loudly (HOLDS).
func TestAdvB29ResidueBenchRenamedLoopIsLoud(t *testing.T) {
	for _, s := range []string{
		"bench_ran=0\nfor s in a.py b.py; do\n",
		"bench_total=0\nfor t in a.py b.py; do\n",
		"bench_ran=0\n\nfor t in a.py b.py; do\n",
		"bench_ran=0\nfor t in a.py b.py\ndo\n",
	} {
		if _, ok := advBenchLoop(s); ok {
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
	filepath.WalkDir(moduleRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, _ := os.ReadFile(p)
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			seen[m[1]] = true
		}
		return nil
	})
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
	if _, err := os.Stat(pinned); err != nil {
		t.Fatalf("the pinned module dir must be found, never skipped: %v", err)
	}
	for _, m := range regexp.MustCompile(`(?m)^│   ├── ([a-z_0-9]+)/|^│   └── ([a-z_0-9]+)/`).FindAllStringSubmatch(doc[i:i+j], -1) {
		name := m[1] + m[2]
		if st, err := os.Stat(filepath.Join(pinned, name)); err != nil || !st.IsDir() {
			t.Errorf("ARCHITECTURE.md lists %s/ but the pinned go_orchestrator has no such directory", name)
		}
	}
}
