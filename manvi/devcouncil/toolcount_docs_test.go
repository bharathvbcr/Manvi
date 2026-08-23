package devcouncil

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"manvi/tools"
)

// TestDocumentedToolCountMatchesRegistry pins every documented native-tool
// count to the registry itself.
//
// This guard exists because the counts drifted three ways at once: the README
// status line said 37, the README architecture diagram said 23, and
// docs/TOOLS_REFERENCE.md said 37 over a category table that summed to 30 —
// while the registry shipped 44. A count in prose is a claim, and nothing was
// checking it, so every tool added after the docs were written made all four
// numbers quietly wronger.
//
// Failing here means a tool was added or removed. Update the prose, do not
// weaken the test.
func TestDocumentedToolCountMatchesRegistry(t *testing.T) {
	want := registryToolCount(t)

	root := repoRoot(t)
	// Every doc location that states the count, and the pattern that finds it.
	// Adding a new count to the docs means adding it here too.
	sites := []struct {
		file    string
		pattern string
	}{
		{"README.md", `(\d+) native tools \(including a native git integration`},
		{"README.md", `Tools\["(\d+) Native Tools"\]`},
		{"README.md", `\| All (\d+) native tools in Go and Rust \|`},
		{"docs/README.md", `Category summary of all (\d+) native tools`},
		{"docs/CLI_AND_CONFIGURATION.md", `List all (\d+) native tools`},
		{"docs/TOOLS_REFERENCE.md", `natively in Go and Rust — \*\*(\d+) tools\*\*`},
		{"docs/TOOLS_REFERENCE.md", `sum to the (\d+) tools ` + "`manvi tools`" + ` reports`},
	}

	for _, s := range sites {
		body := readFile(t, filepath.Join(root, s.file))
		m := regexp.MustCompile(s.pattern).FindStringSubmatch(body)
		if m == nil {
			t.Errorf("%s: no count matched %q — the sentence stating the tool count was reworded or removed; re-point this guard at it", s.file, s.pattern)
			continue
		}
		got, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("%s: %q is not a number: %v", s.file, m[1], err)
		}
		if got != want {
			t.Errorf("%s: documents %d native tools, registry has %d (pattern %q)", s.file, got, want, s.pattern)
		}
	}
}

// TestToolsReferenceCategoryTableSumsToRegistry checks the category table's
// arithmetic, not just its prose total. The table summed to 30 against a
// claimed 37 for long enough that both numbers were wrong and neither was
// checkable from the other.
func TestToolsReferenceCategoryTableSumsToRegistry(t *testing.T) {
	want := registryToolCount(t)
	body := readFile(t, filepath.Join(repoRoot(t), "docs/TOOLS_REFERENCE.md"))

	const marker = "## Tool Category Summary"
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("docs/TOOLS_REFERENCE.md: %q section is gone", marker)
	}
	table := body[i:]
	if end := strings.Index(table, "\n## "); end > 0 {
		table = table[:end]
	}

	rows := regexp.MustCompile(`(?m)^\|\s*\*\*(.+?)\*\*\s*\|\s*(\d+)\s*\|`).FindAllStringSubmatch(table, -1)
	if len(rows) == 0 {
		t.Fatal("docs/TOOLS_REFERENCE.md: category table has no bolded rows with counts")
	}
	sum := 0
	for _, r := range rows {
		n, err := strconv.Atoi(r[2])
		if err != nil {
			t.Fatalf("category %q: count %q is not a number: %v", r[1], r[2], err)
		}
		sum += n
	}
	if sum != want {
		t.Errorf("docs/TOOLS_REFERENCE.md category table sums to %d across %d categories, registry has %d tools", sum, len(rows), want)
	}
}

// TestToolsReferenceNamesEveryUnspecifiedTool keeps the coverage disclaimer
// honest. The document specifies some tools in full and only tabulates the
// rest; the disclaimer names the unspecified ones. If a tool is added and
// documented nowhere, the disclaimer must grow to name it — otherwise a
// reader cannot tell the difference between "not documented" and "does not
// exist", which is the same non-cheating invariant the harness itself keeps.
func TestToolsReferenceNamesEveryUnspecifiedTool(t *testing.T) {
	body := readFile(t, filepath.Join(repoRoot(t), "docs/TOOLS_REFERENCE.md"))

	const marker = "**Specification coverage is not yet complete.**"
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatal("docs/TOOLS_REFERENCE.md: the coverage disclaimer is gone; if every tool is now specified, delete this test with that change")
	}
	disclaimer := body[i:]
	if end := strings.Index(disclaimer, "\n\n"); end > 0 {
		disclaimer = disclaimer[:end]
	}
	// The body of the document, excluding the disclaimer, is where a tool
	// counts as specified.
	specified := body[:i] + body[i+len(disclaimer):]

	named := map[string]bool{}
	for _, m := range regexp.MustCompile("`((?:devcouncil|mcp)_[a-z_]+)`").FindAllStringSubmatch(disclaimer, -1) {
		named[m[1]] = true
	}

	reg := freshRegistry(t)
	var undocumented []string
	for _, s := range reg.Schemas() {
		if strings.Contains(specified, s.Name) {
			continue
		}
		if !named[s.Name] {
			undocumented = append(undocumented, s.Name)
		}
	}
	if len(undocumented) > 0 {
		t.Errorf("docs/TOOLS_REFERENCE.md: %d tool(s) neither specified nor named in the coverage disclaimer: %v",
			len(undocumented), undocumented)
	}
	// The converse: a tool named as unspecified that no longer exists is a
	// disclaimer nobody pruned.
	live := map[string]bool{}
	for _, s := range reg.Schemas() {
		live[s.Name] = true
	}
	for name := range named {
		if !live[name] {
			t.Errorf("docs/TOOLS_REFERENCE.md: coverage disclaimer names %q, which is not in the registry", name)
		}
	}
}

func freshRegistry(t *testing.T) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry(nil)
	r := &Registry{session: &Session{}}
	for _, tl := range r.Tools() {
		if err := reg.Register(tl); err != nil {
			t.Fatalf("register %s: %v", tl.Schema.Name, err)
		}
	}
	return reg
}

func registryToolCount(t *testing.T) int {
	t.Helper()
	return len(freshRegistry(t).Schemas())
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// manvi/devcouncil -> manvi -> repo root
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "README.md")); err != nil {
		t.Fatalf("repo root %s has no README.md: %v", root, err)
	}
	return root
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return fmt.Sprintf("%s", b)
}
