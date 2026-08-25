package contract

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The documentation is a declaration layer too, and it was the one nothing
// checked.
//
// This package already refuses to let a declared flag, schema property or role
// field be inert. Prose is the same kind of promise with none of the same
// enforcement: a sentence stating a count compiles no matter what the count is,
// and it goes on reading correctly forever after the thing it counts has moved.
//
// That is not hypothetical here either. A single audit of this repository's own
// docs found five drifted claims: the README status line said 37 native tools
// and its architecture diagram said 23, against a registry of 44; the tool
// reference claimed 37 over a category table that summed to 30; the hardening
// ledger recorded the fnmatch fixture growing to "776 cases" when 776 was the
// line count and 775 the case count; two documents called the policy ladder
// 5-Tier while the README called it six ordered checks and COMPARISON.md's own
// parenthetical named six rungs. The commit immediately before that audit was
// titled "correct the parity corpus and native tool counts against the
// repository" — it corrected the corpus and left every tool count wrong.
//
// The common shape is that each number was true when written and nothing
// re-read it. So these tests re-read them, on every run, against the artifact
// that decides the answer.
//
// docRoot is the repository, one level above the Go module.
const docRoot = moduleRoot + "/.."

// docFiles is every document these checks govern: the README a visitor reads
// first, and the reference set under docs/.
func docFiles(t *testing.T) []string {
	t.Helper()
	out := []string{filepath.Join(docRoot, "README.md")}
	entries, err := os.ReadDir(filepath.Join(docRoot, "docs"))
	if err != nil {
		t.Fatalf("reading docs/: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			out = append(out, filepath.Join(docRoot, "docs", e.Name()))
		}
	}
	if len(out) < 5 {
		t.Fatalf("found only %d documents; the doc checks are examining almost nothing", len(out))
	}
	return out
}

func readDoc(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// rel renders a path the way a reader would cite it, so a failure names the
// file they have to open.
func rel(path string) string {
	if r, err := filepath.Rel(docRoot, path); err == nil {
		return r
	}
	return path
}

// countCases counts the real rows of a parity fixture: comments and blank
// lines are not cases.
//
// This distinction is the entire content of one of the drifted claims. A TSV
// with a header has one more line than it has cases, and a number obtained by
// piping it through `wc -l` is off by exactly that header.
func countCases(t *testing.T, name string) int {
	t.Helper()
	body := readDoc(t, filepath.Join(docRoot, "testdata", name))
	n := 0
	for _, ln := range strings.Split(body, "\n") {
		s := strings.TrimSpace(ln)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		n++
	}
	if n == 0 {
		t.Fatalf("%s holds no cases; the parity claims are being checked against an empty fixture", name)
	}
	return n
}

// TestParityCountsInProseMatchTheFixtures pins every stated case count.
//
// The counts appear in six places across four documents, in three different
// phrasings, and they are the numbers most often quoted out of this repository
// — including onto a CV. One of them was wrong by exactly one header row.
func TestParityCountsInProseMatchTheFixtures(t *testing.T) {
	glob := countCases(t, "fnmatch-parity.tsv")
	command := countCases(t, "command-parity.tsv")
	total := glob + command

	// Each pattern captures a number that must equal the named quantity. A
	// pattern that matches nothing is itself a failure: the sentence was
	// reworded and this guard stopped watching it.
	checks := []struct {
		pattern *regexp.Regexp
		want    int
		what    string
	}{
		{regexp.MustCompile(`(\d+)-case CPython parity fixture`), glob, "glob cases"},
		{regexp.MustCompile(`fnmatch-parity\.tsv<br/>\((\d+) cases\)`), glob, "glob cases"},
		{regexp.MustCompile(`fnmatch-parity\.tsv<br/>(\d+) cases`), glob, "glob cases"},
		{regexp.MustCompile(`all (\d+) glob test cases`), glob, "glob cases"},
		{regexp.MustCompile(`# (\d+) glob test cases`), glob, "glob cases"},
		{regexp.MustCompile(`command-parity\.tsv<br/>(\d+) cases`), command, "command cases"},
		{regexp.MustCompile(`\*\*([\d,]+) parity cases\*\*`), total, "total parity cases"},
		{regexp.MustCompile(`\((\d+) → (\d+) cases\)`), glob, "post-regeneration glob cases"},
	}

	seen := map[string]int{}
	for _, path := range docFiles(t) {
		body := readDoc(t, path)
		for _, c := range checks {
			for _, m := range c.pattern.FindAllStringSubmatch(body, -1) {
				// The last capture group is always the current count; the
				// "683 → 776" shape carries a historical value first.
				raw := m[len(m)-1]
				got, err := strconv.Atoi(strings.ReplaceAll(raw, ",", ""))
				if err != nil {
					t.Errorf("%s: %q is not a number", rel(path), raw)
					continue
				}
				seen[c.what]++
				if got != c.want {
					t.Errorf("%s: prose says %d %s, the fixture holds %d (matched %q)",
						rel(path), got, c.what, c.want, m[0])
				}
			}
		}
	}

	for _, want := range []string{"glob cases", "command cases", "total parity cases"} {
		if seen[want] == 0 {
			t.Errorf("no document states the %s count any more; either the claim was removed "+
				"(delete its pattern here) or it was reworded past this guard", want)
		}
	}
}

// mermaidBlocks returns every fenced mermaid diagram in a document, with the
// line the fence opened on so a failure is navigable.
func mermaidBlocks(body string) []struct {
	Line int
	Body string
} {
	var out []struct {
		Line int
		Body string
	}
	lines := strings.Split(body, "\n")
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "```mermaid" {
			continue
		}
		start := i
		var block []string
		for i++; i < len(lines) && strings.TrimSpace(lines[i]) != "```"; i++ {
			block = append(block, lines[i])
		}
		out = append(out, struct {
			Line int
			Body string
		}{start + 1, strings.Join(block, "\n")})
	}
	return out
}

// TestMermaidDiagramsAreWellFormed catches a diagram that will not render.
//
// A broken diagram is worse than a missing one: GitHub replaces it with a
// parse error, in the middle of the architecture section, on the page that is
// now this project's front door. The README shipped exactly that — one edge
// written `B4 -- "Yes" => Granted[...]` among nineteen siblings that all used
// `-->`. Mermaid has no `=>`, so the whole flowchart failed, and nothing in
// the build had any opinion about it.
//
// This is a structural check, not a parser: it looks for the malformations
// that actually occur when hand-editing these blocks.
func TestMermaidDiagramsAreWellFormed(t *testing.T) {
	// Mermaid's edge vocabulary differs by diagram type, and a check that
	// ignores that produces noise instead of findings: `->>` is a correct
	// sequence-diagram arrow and a nonsense flowchart one.
	//
	// `=>` is the one form no diagram type accepts, which is why the README's
	// broken edge was written with it. A bare `->` is valid in sequence
	// diagrams and invalid in flowcharts and state diagrams.
	arrowFree := regexp.MustCompile(`(^|[^-=<>])=>`)
	bareArrow := regexp.MustCompile(`(^|[^-=<>.])->(?:[^->]|$)`)

	total := 0
	for _, path := range docFiles(t) {
		body := readDoc(t, path)
		for _, blk := range mermaidBlocks(body) {
			total++
			kind := diagramKind(blk.Body)
			for j, ln := range strings.Split(blk.Body, "\n") {
				line := blk.Line + 1 + j
				code := ln
				// Labels and node text may legitimately contain anything.
				if i := strings.Index(code, "%%"); i >= 0 {
					code = code[:i]
				}
				code = stripQuoted(code)
				if m := arrowFree.FindString(code); m != "" {
					t.Errorf("%s:%d: %q is not a mermaid edge operator in any diagram type; "+
						"the diagram will not render\n    %s",
						rel(path), line, strings.TrimSpace(m), strings.TrimSpace(ln))
				}
				if kind != "sequence" {
					if m := bareArrow.FindString(code); m != "" {
						t.Errorf("%s:%d: bare %q is not an edge in a %s diagram (did you mean \"-->\"?)\n    %s",
							rel(path), line, strings.TrimSpace(m), kind, strings.TrimSpace(ln))
					}
				}
			}
		}
	}
	if total < 20 {
		t.Fatalf("only %d mermaid blocks found; this check is not looking at the diagrams", total)
	}
	t.Logf("checked %d mermaid diagrams", total)
}

// diagramKind names the mermaid dialect a block is written in, because the
// legal edge operators depend on it.
func diagramKind(body string) string {
	for _, ln := range strings.Split(body, "\n") {
		s := strings.TrimSpace(ln)
		if s == "" || strings.HasPrefix(s, "%%") {
			continue
		}
		switch {
		case strings.HasPrefix(s, "sequenceDiagram"):
			return "sequence"
		case strings.HasPrefix(s, "stateDiagram"):
			return "state"
		case strings.HasPrefix(s, "flowchart"), strings.HasPrefix(s, "graph"):
			return "flowchart"
		}
		return "unknown"
	}
	return "unknown"
}

// stripQuoted blanks the inside of double-quoted labels so their contents —
// which may hold arrows, brackets and arbitrary prose — cannot be mistaken for
// diagram syntax.
func stripQuoted(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		if r == '"' {
			in = !in
			b.WriteRune(r)
			continue
		}
		if in {
			if r == '\n' {
				b.WriteRune(r)
			} else {
				b.WriteRune(' ')
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Deliberately not checked: bracket balance.
//
// The obvious next check — that every `[`, `(` and `{` in a diagram closes —
// cannot be made correct without a real parser, and a wrong version of it is
// worse than nothing. Mermaid edge labels are free text, so this repository's
// own input-decoder flowchart legitimately contains `-- Followed by [ -->`,
// `-- [A, [B, [C, [D -->` and `-- [200~ ... [201~ -->`, and sequence-diagram
// messages legitimately contain `(CUP: \x1b[y;xH)`. A balance check flags all
// five as broken diagrams. Failures that fire on correct input are how a suite
// teaches people to skip its output, which costs more than the class it catches.
//
// The arrow checks above stay because they are exact: `=>` is wrong in every
// dialect, and a bare `->` is wrong in every dialect that is not a sequence
// diagram. Both were verified against every block in this repository.

// TestPolicyLadderRungCountIsConsistent pins the prose to the diagram.
//
// Two documents described the ladder as "5-Tier" while the README described it
// as "six ordered checks", and COMPARISON.md's own parenthetical listed six
// named rungs immediately after calling it five. Nobody was wrong on purpose;
// a rung was added and one sentence was updated.
//
// The diagram is the arbiter because it is the thing a reader counts.
func TestPolicyLadderRungCountIsConsistent(t *testing.T) {
	readme := readDoc(t, filepath.Join(docRoot, "README.md"))

	var ladder string
	for _, blk := range mermaidBlocks(readme) {
		if strings.Contains(blk.Body, "Within repo root?") {
			ladder = blk.Body
			break
		}
	}
	if ladder == "" {
		t.Fatal("the evaluation-ladder diagram is gone from the README; this check examined nothing")
	}

	// Decision nodes are the rungs: B1{...} .. Bn{...}.
	rungs := map[string]bool{}
	for _, m := range regexp.MustCompile(`\b(B\d+)\s*\{`).FindAllStringSubmatch(ladder, -1) {
		rungs[m[1]] = true
	}
	if len(rungs) == 0 {
		t.Fatal("no decision nodes found in the ladder diagram; this check examined nothing")
	}

	words := map[string]int{
		"one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
		"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
	}

	// The README's own sentence.
	m := regexp.MustCompile(`resolve through (\w+) ordered checks`).FindStringSubmatch(readme)
	if m == nil {
		t.Error("README no longer states how many ordered checks the ladder has")
	} else if got := words[strings.ToLower(m[1])]; got != len(rungs) {
		t.Errorf("README says %q ordered checks; the diagram draws %d decision nodes", m[1], len(rungs))
	}

	// And every other document that puts a number on the ladder.
	tier := regexp.MustCompile(`(?i)\*?\*?(\d+)[- ](?:tier|rung)s? policy ladder`)
	found := 0
	for _, path := range docFiles(t) {
		for _, mm := range tier.FindAllStringSubmatch(readDoc(t, path), -1) {
			found++
			got, _ := strconv.Atoi(mm[1])
			if got != len(rungs) {
				t.Errorf("%s: calls it a %s-rung ladder; the README diagram draws %d",
					rel(path), mm[1], len(rungs))
			}
		}
	}
	if found == 0 {
		t.Error("no document numbers the policy ladder any more; if that is deliberate, drop this half of the check")
	}
}

// TestOutcomeStateCountIsConsistent does the same for the five outcome states,
// which are stated as a word, as a digit, as a table and as a state diagram.
func TestOutcomeStateCountIsConsistent(t *testing.T) {
	readme := readDoc(t, filepath.Join(docRoot, "README.md"))

	var states []string
	for _, blk := range mermaidBlocks(readme) {
		if !strings.Contains(blk.Body, "stateDiagram") {
			continue
		}
		for _, m := range regexp.MustCompile(`Evaluated\s*-->\s*(\w+)\s*:`).FindAllStringSubmatch(blk.Body, -1) {
			states = append(states, m[1])
		}
	}
	if len(states) == 0 {
		t.Fatal("the outcome-state diagram is gone from the README; this check examined nothing")
	}
	sort.Strings(states)

	words := map[string]int{"four": 4, "five": 5, "six": 6, "seven": 7}
	if m := regexp.MustCompile(`### (\w+) Outcome States`).FindStringSubmatch(readme); m != nil {
		if got := words[strings.ToLower(m[1])]; got != len(states) {
			t.Errorf("README heading says %q Outcome States; the diagram defines %d (%v)",
				m[1], len(states), states)
		}
	} else {
		t.Error("the README no longer heads a section with the outcome-state count")
	}

	digit := regexp.MustCompile(`(\d+) outcome states`)
	for _, path := range docFiles(t) {
		for _, m := range digit.FindAllStringSubmatch(readDoc(t, path), -1) {
			got, _ := strconv.Atoi(m[1])
			if got != len(states) {
				t.Errorf("%s: says %s outcome states; the README diagram defines %d",
					rel(path), m[1], len(states))
			}
		}
	}

	// Each state must also have a row in the semantics table, or the table is
	// describing a set the diagram no longer matches.
	for _, s := range states {
		if !strings.Contains(readme, "| **"+s+"** |") {
			t.Errorf("outcome state %q has no row in the README semantics table", s)
		}
	}
}

// TestEveryRelativeDocLinkResolves keeps the front door from 404-ing.
//
// Relative links are the one kind this repository can check without a network,
// and they are also the kind that rot silently when a file is renamed. On a
// private repo a dead link is an annoyance; on a public one it is the first
// impression.
func TestEveryRelativeDocLinkResolves(t *testing.T) {
	link := regexp.MustCompile(`\[([^\]]*)\]\(([^)\s]+)\)`)
	checked := 0
	for _, path := range docFiles(t) {
		body := readDoc(t, path)
		base := filepath.Dir(path)
		for _, m := range link.FindAllStringSubmatch(body, -1) {
			target := m[2]
			if i := strings.Index(target, "#"); i >= 0 {
				target = target[:i]
			}
			if target == "" ||
				strings.HasPrefix(target, "http://") ||
				strings.HasPrefix(target, "https://") ||
				strings.HasPrefix(target, "mailto:") {
				continue
			}
			checked++
			if _, err := os.Stat(filepath.Join(base, target)); err != nil {
				t.Errorf("%s: link [%s](%s) points at nothing", rel(path), m[1], m[2])
			}
		}
	}
	if checked < 20 {
		t.Fatalf("only %d relative links checked; this guard is not looking at the docs", checked)
	}
	t.Logf("resolved %d relative links", checked)
}

// TestEveryCLISubcommandIsDocumented pins the reference to the dispatch.
//
// A command that exists and is documented nowhere is invisible; a command that
// is documented and does not exist is a lie a reader discovers at the shell.
// The dispatch switch is the arbiter for both directions.
func TestEveryCLISubcommandIsDocumented(t *testing.T) {
	main := readDoc(t, filepath.Join(docRoot, "manvi", "cmd", "manvi", "main.go"))

	// Only the top-level dispatch, which runs from `switch args[0]` to its
	// default. Anchoring on the literal keeps this from silently drifting onto
	// some other switch if the file is reorganised.
	start := strings.Index(main, "switch args[0] {")
	if start < 0 {
		t.Fatal("the top-level command switch was not found; this check examined nothing")
	}
	body := main[start:]
	if end := strings.Index(body, "\n\tdefault:"); end > 0 {
		body = body[:end]
	}
	var commands []string
	for _, m := range regexp.MustCompile(`case "([a-z]+)":`).FindAllStringSubmatch(body, -1) {
		commands = append(commands, m[1])
	}
	if len(commands) < 10 {
		t.Fatalf("only %d subcommands parsed out of the dispatch; this check examined almost nothing", len(commands))
	}

	ref := readDoc(t, filepath.Join(docRoot, "docs", "CLI_AND_CONFIGURATION.md"))
	documented := map[string]bool{}
	for _, m := range regexp.MustCompile("`manvi ([a-z]+)").FindAllStringSubmatch(ref, -1) {
		documented[m[1]] = true
	}

	var missing []string
	for _, c := range commands {
		if !documented[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("docs/CLI_AND_CONFIGURATION.md documents no `manvi <cmd>` line for: %v", missing)
	}

	live := map[string]bool{}
	for _, c := range commands {
		live[c] = true
	}
	// Sub-verbs (`manvi map build`, `manvi lease list`) are documented under
	// their parent, so only flag a documented word that is neither.
	for name := range documented {
		if !live[name] && !strings.Contains(body, `"`+name+`"`) {
			t.Errorf("docs/CLI_AND_CONFIGURATION.md documents `manvi %s`, which the dispatch does not handle", name)
		}
	}
	t.Logf("checked %d subcommands against the reference", len(commands))
}

// eventFieldPattern matches one struct field line in either the source or the
// documented copy of it: an indented exported name, a type, and a json tag.
// Comment lines and blank lines do not match, and neither does the `type …`
// line itself, which is not indented.
var eventFieldPattern = regexp.MustCompile("(?m)^[ \t]+([A-Z][A-Za-z0-9]*)[ \t]+[^/\\s][^`]*`json:\"([^\",]+)")

// eventFields pulls the field-name-to-JSON-name mapping out of one rendering
// of the Event struct, from the opening brace to the first closing brace at
// the start of a line.
func eventFields(t *testing.T, body, what string) map[string]string {
	t.Helper()
	start := strings.Index(body, "type Event struct {")
	if start < 0 {
		t.Fatalf("%s: no `type Event struct {` found; this check examined nothing", what)
	}
	block := body[start:]
	if end := strings.Index(block, "\n}"); end > 0 {
		block = block[:end]
	}
	fields := map[string]string{}
	for _, m := range eventFieldPattern.FindAllStringSubmatch(block, -1) {
		fields[m[1]] = m[2]
	}
	if len(fields) < 15 {
		t.Fatalf("%s: parsed only %d fields out of the Event struct; this check examined almost nothing", what, len(fields))
	}
	return fields
}

// TestDocumentedEventFieldsMatchTheStruct pins the wire's documented shape to
// the wire.
//
// The NDJSON face is a contract with programs — a CI job, a benchmark, an
// editor plugin — and this block is where that contract is written down for a
// reader who is not going to open event.go. It had already drifted the whole
// way: the documented struct named Timestamp, Turn, Step, Grantor and a
// `Degraded string`, none of which exist, and omitted Agent, Detail, Arguments,
// Grantable, GrantedBy, Weakened, ApprovalID and TaskID, all of which do. Every
// one of those was true of some earlier version and nothing re-read it.
//
// Both names are checked, because both are load-bearing and they fail
// differently: the Go name is what a contributor greps for, and the JSON name
// is what a consumer's parser keys on.
func TestDocumentedEventFieldsMatchTheStruct(t *testing.T) {
	source := eventFields(t,
		readDoc(t, filepath.Join(docRoot, "manvi", "ui", "event.go")), "manvi/ui/event.go")
	documented := eventFields(t,
		readDoc(t, filepath.Join(docRoot, "docs", "TUI_AND_EVENT_SUBSYSTEM.md")),
		"docs/TUI_AND_EVENT_SUBSYSTEM.md")

	for name, jsonName := range source {
		switch got, ok := documented[name]; {
		case !ok:
			t.Errorf("ui.Event has field %s and docs/TUI_AND_EVENT_SUBSYSTEM.md does not document it", name)
		case got != jsonName:
			t.Errorf("ui.Event.%s is on the wire as %q; the docs say %q", name, jsonName, got)
		}
	}
	for name := range documented {
		if _, ok := source[name]; !ok {
			t.Errorf("docs/TUI_AND_EVENT_SUBSYSTEM.md documents ui.Event.%s, which does not exist", name)
		}
	}
	t.Logf("checked %d documented event fields against the struct", len(source))
}

// TestDocumentedExitCodesMatchTheDispatch pins the table CI branches on.
//
// The reference listed 0, 1 and 2 while the dispatch had grown 3, 4 and 5 —
// each added because a turn that ended badly was exiting 0 and being read as
// success, and each documented nowhere. A CI step written from that table
// treats an unknown status as a generic failure, which is the safe direction
// only by luck; the harness added those codes precisely so a caller could tell
// the cases apart, and a caller cannot act on a code it was never told about.
func TestDocumentedExitCodesMatchTheDispatch(t *testing.T) {
	main := readDoc(t, filepath.Join(docRoot, "manvi", "cmd", "manvi", "main.go"))
	start := strings.Index(main, "func statusFor(")
	if start < 0 {
		t.Fatal("statusFor was not found; this check examined nothing")
	}
	body := main[start:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	live := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\t\treturn (\d+)$`).FindAllStringSubmatch(body, -1) {
		live[m[1]] = true
	}
	if len(live) < 4 {
		t.Fatalf("parsed only %d exit codes out of statusFor; this check examined almost nothing", len(live))
	}

	ref := readDoc(t, filepath.Join(docRoot, "docs", "CLI_AND_CONFIGURATION.md"))
	table := ref[strings.Index(ref, "## Headless Exit Codes"):]
	if cut := strings.Index(table, "\n---"); cut > 0 {
		table = table[:cut]
	}
	documented := map[string]bool{}
	for _, m := range regexp.MustCompile("(?m)^\\| `(\\d+)` \\|").FindAllStringSubmatch(table, -1) {
		documented[m[1]] = true
	}

	for code := range live {
		if !documented[code] {
			t.Errorf("manvi exits %s and docs/CLI_AND_CONFIGURATION.md does not document that code", code)
		}
	}
	for code := range documented {
		if !live[code] && code != "0" {
			t.Errorf("docs/CLI_AND_CONFIGURATION.md documents exit %s, which the dispatch never returns", code)
		}
	}
	t.Logf("checked %d exit codes against the dispatch", len(documented))
}
