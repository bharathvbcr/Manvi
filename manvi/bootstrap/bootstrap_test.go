package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func options() Options {
	return Options{StateDir: ".devcouncil", Gitignore: true}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestEnsureScaffoldsAFreshRepository is the base case: a directory the harness
// has never run in comes back with somewhere to write and rules that keep what
// it writes out of a commit.
func TestEnsureScaffoldsAFreshRepository(t *testing.T) {
	root := t.TempDir()

	report := Ensure(root, options())
	if len(report.Failures) > 0 {
		t.Fatalf("failures on a fresh repository: %v", report.Failures)
	}
	if !report.Changed() {
		t.Fatal("the report says nothing changed in a directory that had neither the state dir nor an ignore file")
	}
	if got := report.CreatedDirs; len(got) != 1 || got[0] != ".devcouncil" {
		t.Fatalf("CreatedDirs = %q, want [.devcouncil]", got)
	}
	if info, err := os.Stat(filepath.Join(root, ".devcouncil")); err != nil || !info.IsDir() {
		t.Fatalf("the state directory was reported created but is not there: %v", err)
	}

	content := read(t, filepath.Join(root, ".gitignore"))
	for _, rule := range Rules() {
		if !strings.Contains(content, rule) {
			t.Errorf(".gitignore is missing %q", rule)
		}
	}
	// The negation only means anything after the rule it negates.
	if strings.Index(content, ".devcouncil/*") > strings.Index(content, "!.devcouncil/config.yaml") {
		t.Error("the config negation is written before the rule it negates, so it does nothing")
	}
	if !strings.HasSuffix(content, "\n") {
		t.Error(".gitignore does not end in a newline")
	}
}

// TestEnsureIsIdempotent covers the case that actually runs: every invocation
// after the first. A second call that appended the same block again would make
// the file grow without bound, one command at a time.
func TestEnsureIsIdempotent(t *testing.T) {
	root := t.TempDir()
	Ensure(root, options())
	before := read(t, filepath.Join(root, ".gitignore"))

	report := Ensure(root, options())
	if len(report.Failures) > 0 {
		t.Fatalf("failures on the second run: %v", report.Failures)
	}
	if report.Changed() {
		t.Fatalf("the second run reported changes: dirs=%q rules=%q", report.CreatedDirs, report.AddedRules)
	}
	if len(report.Lines()) != 0 {
		t.Fatalf("the second run printed %q, want silence", report.Lines())
	}
	if after := read(t, filepath.Join(root, ".gitignore")); after != before {
		t.Fatalf(".gitignore changed on a run that reported no changes:\n%s", after)
	}
}

// TestEnsureAddsOnlyWhatIsMissingAndKeepsWhatIsThere: the file belongs to the
// operator. Their rules survive, their rules are not restated, and a file that
// did not end in a newline does not have its last pattern extended by the
// first appended one.
func TestEnsureAddsOnlyWhatIsMissingAndKeepsWhatIsThere(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".gitignore")
	// No trailing newline, and one rule the harness would otherwise add.
	existing := "# mine\nbuild/\n*.log"
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	report := Ensure(root, options())
	if len(report.Failures) > 0 {
		t.Fatalf("failures: %v", report.Failures)
	}
	content := read(t, path)

	if !strings.HasPrefix(content, existing+"\n") {
		t.Fatalf("the operator's content was not preserved verbatim:\n%s", content)
	}
	if strings.Contains(content, "*.log\n# ") {
		t.Fatalf("a heading was appended straight onto the last unterminated line:\n%s", content)
	}
	if n := strings.Count(content, "\n*.log"); n != 1 {
		t.Fatalf("*.log appears %d times; a rule already in the file was added again:\n%s", n, content)
	}
	for _, rule := range report.AddedRules {
		if rule == "*.log" {
			t.Error("the report claims to have added a rule that was already there")
		}
	}
	if !strings.Contains(content, "build/") {
		t.Error("the operator's own rule was dropped")
	}
	if !strings.Contains(content, ".devcouncil/*") {
		t.Error("the state-directory rule was not added")
	}
}

// TestEnsureRecognisesARuleUnderAnyHeading: the check is against the file's
// rules, not against a marker block, so an operator who moved a rule into their
// own section does not get it written back a second time.
func TestEnsureRecognisesARuleUnderAnyHeading(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".gitignore")
	if err := os.WriteFile(path, []byte("# whatever I call it\n  .devcouncil/*  \n"), 0o644); err != nil {
		t.Fatal(err)
	}

	Ensure(root, options())
	if n := strings.Count(read(t, path), ".devcouncil/*"); n != 1 {
		t.Fatalf(".devcouncil/* appears %d times, want 1:\n%s", n, read(t, path))
	}
}

// TestEnsureReportsAFailureAndKeepsGoing: scaffolding is not a gate. A
// .gitignore that cannot be written must not stop the state directory being
// made, and must not be silent about what it could not do.
func TestEnsureReportsAFailureAndKeepsGoing(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".gitignore"), 0o755); err != nil {
		t.Fatal(err)
	}

	report := Ensure(root, options())
	if len(report.Failures) != 1 {
		t.Fatalf("Failures = %v, want exactly the .gitignore failure", report.Failures)
	}
	if !strings.Contains(report.Failures[0].String(), ".gitignore") {
		t.Fatalf("the failure does not name what could not be done: %s", report.Failures[0])
	}
	if lines := strings.Join(report.Lines(), "\n"); !strings.Contains(lines, ".gitignore") {
		t.Fatalf("the failure is not in the printed report:\n%s", lines)
	}
	if info, err := os.Stat(filepath.Join(root, ".devcouncil")); err != nil || !info.IsDir() {
		t.Fatalf("the state directory was skipped because a later step failed: %v", err)
	}
}

// TestEnsureCreatesArtifactParents covers an operator who pointed
// MANVI_STORE_DB or MANVI_GRAPH somewhere other than the state directory.
func TestEnsureCreatesArtifactParents(t *testing.T) {
	root := t.TempDir()
	opts := options()
	opts.ArtifactPaths = []string{
		filepath.Join("var", "state.sqlite"),
		filepath.Join("var", "graph.json"), // same parent: created once, reported once
		filepath.Join(root, "abs", "code_graph.json"),
		"top-level.json", // parent is the root itself
	}

	report := Ensure(root, opts)
	if len(report.Failures) > 0 {
		t.Fatalf("failures: %v", report.Failures)
	}
	want := map[string]bool{".devcouncil": true, "var": true, "abs": true}
	if len(report.CreatedDirs) != len(want) {
		t.Fatalf("CreatedDirs = %q, want exactly %v", report.CreatedDirs, want)
	}
	for _, dir := range report.CreatedDirs {
		if !want[dir] {
			t.Errorf("unexpected directory created: %q", dir)
		}
		if info, err := os.Stat(filepath.Join(root, dir)); err != nil || !info.IsDir() {
			t.Errorf("%s was reported created but is not there: %v", dir, err)
		}
	}
}

// TestEnsureHonoursAnAbsoluteStateDir: MANVI_STATE_DIR may point outside the
// repository, and a path that is already absolute must not be re-rooted.
func TestEnsureHonoursAnAbsoluteStateDir(t *testing.T) {
	root := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "state")

	report := Ensure(root, Options{StateDir: elsewhere})
	if len(report.Failures) > 0 {
		t.Fatalf("failures: %v", report.Failures)
	}
	if info, err := os.Stat(elsewhere); err != nil || !info.IsDir() {
		t.Fatalf("the absolute state directory was not created: %v", err)
	}
	if got := report.CreatedDirs; len(got) != 1 || got[0] != elsewhere {
		t.Fatalf("CreatedDirs = %q, want the absolute path reported as typed", got)
	}
	if _, err := os.Stat(filepath.Join(root, ".gitignore")); !os.IsNotExist(err) {
		t.Fatal("Gitignore was false and the file was written anyway")
	}
}

// TestEnsurePreservesTheFileMode: the append must not widen permissions on a
// file the operator deliberately restricted.
func TestEnsurePreservesTheFileMode(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".gitignore")
	if err := os.WriteFile(path, []byte("build/\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	Ensure(root, options())
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %v, want 0600 — the rewrite changed the file's permissions", got)
	}
}

// TestUntrackedIsReportedOnlyWithoutGit, and only where it changes what an
// operator should conclude: rules written into a directory git does not manage
// are rules that currently govern nothing.
func TestUntrackedIsReportedOnlyWithoutGit(t *testing.T) {
	root := t.TempDir()
	report := Ensure(root, options())
	if !report.Untracked {
		t.Error("a directory with no .git was not reported untracked")
	}
	if !strings.Contains(strings.Join(report.Lines(), "\n"), "no .git here") {
		t.Errorf("the report does not say the rules govern nothing yet:\n%s", strings.Join(report.Lines(), "\n"))
	}

	tracked := t.TempDir()
	if err := os.Mkdir(filepath.Join(tracked, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if Ensure(tracked, options()).Untracked {
		t.Error("a directory with a .git was reported untracked")
	}
}

// TestSectionsCarryNoDuplicates: a rule listed twice would be appended twice on
// the first run, and the file would then look hand-edited.
func TestSectionsCarryNoDuplicates(t *testing.T) {
	seen := map[string]string{}
	for _, section := range Sections {
		for _, rule := range section.Rules {
			if first, ok := seen[rule]; ok {
				t.Errorf("rule %q appears under both %q and %q", rule, first, section.Heading)
			}
			seen[rule] = section.Heading
		}
	}
}

// TestReportSummarisesALongAddition: the first run in a fresh repository adds
// every managed rule, and a line naming all of them is a line nobody reads.
func TestReportSummarisesALongAddition(t *testing.T) {
	report := Ensure(t.TempDir(), options())

	lines := report.Lines()
	var ignoreLine string
	for _, line := range lines {
		if strings.Contains(line, ".gitignore") {
			ignoreLine = line
		}
	}
	if ignoreLine == "" {
		t.Fatalf("no line about .gitignore in %q", lines)
	}
	if !strings.Contains(ignoreLine, "more") {
		t.Fatalf("the whole rule list was printed:\n%s", ignoreLine)
	}
	if !strings.Contains(ignoreLine, ".devcouncil/*") {
		t.Fatalf("the line names nothing it added:\n%s", ignoreLine)
	}
	// The count is the part that has to be exact.
	if !strings.Contains(ignoreLine, fmt.Sprintf("%d ignore rule(s)", len(Rules()))) {
		t.Fatalf("the line does not count what was added:\n%s", ignoreLine)
	}
}

// TestARelocatedStateDirIsIgnoredToo. MANVI_STATE_DIR moves the directory the
// harness fills with an index and a grant ledger; the fixed rules only name the
// default one, so without this the setting quietly reintroduces the commit the
// fixed rule exists to prevent.
func TestARelocatedStateDirIsIgnoredToo(t *testing.T) {
	root := t.TempDir()
	report := Ensure(root, Options{StateDir: filepath.Join("var", "manvi"), Gitignore: true})
	if len(report.Failures) > 0 {
		t.Fatalf("failures: %v", report.Failures)
	}

	content := read(t, filepath.Join(root, ".gitignore"))
	if !strings.Contains(content, "var/manvi/") {
		t.Fatalf("the relocated state directory is not ignored:\n%s", content)
	}
	// And the fixed rules are still there: DevCouncil's own directory is shared
	// with a tool that keeps using it.
	if !strings.Contains(content, ".devcouncil/*") {
		t.Fatal("moving the state directory dropped the DevCouncil rules")
	}
	// Idempotent for the added rule as much as the fixed ones.
	if Ensure(root, Options{StateDir: filepath.Join("var", "manvi"), Gitignore: true}).Changed() {
		t.Fatal("the second run with a relocated state directory changed something")
	}
}

// TestTheDefaultStateDirIsNotWrittenTwice: ".devcouncil/*" already covers it,
// and a second rule for the same directory reads as two different decisions.
func TestTheDefaultStateDirIsNotWrittenTwice(t *testing.T) {
	root := t.TempDir()
	Ensure(root, options())
	if n := strings.Count(read(t, filepath.Join(root, ".gitignore")), ".devcouncil"); n != 3 {
		t.Fatalf(".devcouncil appears %d times, want the 3 DevCouncil rules only:\n%s",
			n, read(t, filepath.Join(root, ".gitignore")))
	}
}

// TestAStateDirOutsideTheRepositoryAddsNoRule: a path .gitignore cannot express
// must not become a rule that looks like it works.
func TestAStateDirOutsideTheRepositoryAddsNoRule(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "state")
	Ensure(root, Options{StateDir: outside, Gitignore: true})

	content := read(t, filepath.Join(root, ".gitignore"))
	if strings.Contains(content, "..") || strings.Contains(content, outside) {
		t.Fatalf("a rule was written for a directory outside the repository:\n%s", content)
	}
}
