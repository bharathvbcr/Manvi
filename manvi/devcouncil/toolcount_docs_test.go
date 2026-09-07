package devcouncil

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/tools"
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

// TestToolsReferenceSpecifiesEveryTool holds the document to the whole
// registry, in both directions.
//
// It replaces a guard that policed a disclaimer. The document used to specify
// the first eight categories and merely tabulate the other five, and that
// guard's job was to make sure the fourteen unspecified tools were at least
// *named* as unspecified — so a reader could tell "not documented" from "does
// not exist". All fourteen are specified now, the disclaimer is gone, and the
// weaker invariant went with it.
//
// What replaces it is the stronger one: every tool the registry ships has a
// row of its own under "Detailed Tool Specifications", and every row there
// names a tool that exists. A row is the unit because a bare mention in prose
// is what the old disclaimer was made of, and prose is exactly what drifted.
//
// Failing here means a tool was added, removed, or renamed. Write the row, do
// not weaken the test.
func TestToolsReferenceSpecifiesEveryTool(t *testing.T) {
	body := readFile(t, filepath.Join(repoRoot(t), "docs/TOOLS_REFERENCE.md"))

	const marker = "## Detailed Tool Specifications"
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("docs/TOOLS_REFERENCE.md: %q section is gone", marker)
	}
	spec := body[i:]
	if end := strings.Index(spec, "\n## Direct CLI Tool Invocation"); end > 0 {
		spec = spec[:end]
	}

	// The first cell of a specification row, and nothing else. Matching the
	// name anywhere in the section would count a cross-reference in a
	// paragraph as a specification, which is the drift this guard exists to
	// catch.
	// The name cell and the access cell together. Access is captured because a
	// row that names a write tool as read-only is worse than no row at all:
	// it is a permission claim, and a reader budgeting risk from this table
	// would be reading a number the registry does not agree with.
	rowName := regexp.MustCompile("(?m)^\\|\\s*`((?:devcouncil|mcp)_[a-z_0-9]+)`\\s*\\|\\s*([^|]+?)\\s*\\|")
	specified := map[string]string{}
	for _, m := range rowName.FindAllStringSubmatch(spec, -1) {
		specified[m[1]] = m[2]
	}
	if len(specified) == 0 {
		t.Fatal("docs/TOOLS_REFERENCE.md: no specification rows matched; the table format changed and this guard is now checking nothing")
	}

	// Access, against the registry's own ReadOnly flag.
	r := &Registry{session: &Session{}}
	for _, tl := range r.Tools() {
		documented, ok := specified[tl.Schema.Name]
		if !ok {
			continue // reported as missing below
		}
		want := "Write"
		if tl.ReadOnly {
			want = "Read-only"
		}
		if documented != want {
			t.Errorf("docs/TOOLS_REFERENCE.md: %s is documented as %q, the registry has it as %q",
				tl.Schema.Name, documented, want)
		}
	}

	reg := freshRegistry(t)
	live := map[string]bool{}
	var missing []string
	for _, s := range reg.Schemas() {
		live[s.Name] = true
		if _, ok := specified[s.Name]; !ok {
			missing = append(missing, s.Name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("docs/TOOLS_REFERENCE.md: %d registered tool(s) have no specification row: %v",
			len(missing), missing)
	}

	// The converse. A row for a tool that no longer exists documents a surface
	// the harness does not offer, which is the same lie pointing the other way.
	//
	// devcouncil_fetch_url is the one exception, and it is a real one rather
	// than an allowance: it is registered only when an operator sets
	// MANVI_FETCH_HOSTS, so it is absent from this registry by construction
	// while being a tool a configured harness genuinely offers.
	for name := range specified {
		if name == "devcouncil_fetch_url" {
			continue
		}
		if !live[name] {
			t.Errorf("docs/TOOLS_REFERENCE.md specifies %q, which is not in the registry", name)
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
