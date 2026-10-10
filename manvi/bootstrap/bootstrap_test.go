package bootstrap

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// changed reports whether an Ensure call altered the working tree.
func changed(r Report) bool { return len(r.CreatedDirs) > 0 || len(r.AddedRules) > 0 }

// allRules lists every managed rule, sorted.
func allRules() []string {
	var all []string
	for _, section := range Sections {
		all = append(all, section.Rules...)
	}
	sort.Strings(all)
	return all
}

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

// gitRepo is a fresh git working tree. The managed rules are only written
// inside one, so every test about what lands in .gitignore starts here.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH; .gitignore is only written inside a git working tree")
	}
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	runGit(t, root, "init", "-q")
	return root
}

// runGit runs one git command in dir and fails the test if it does.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	// #nosec G204 -- the binary is the literal "git"; the arguments are this test's own.
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// TestEnsureScaffoldsAFreshRepository is the base case: a directory the harness
// has never run in comes back with somewhere to write and rules that keep what
// it writes out of a commit.
func TestEnsureScaffoldsAFreshRepository(t *testing.T) {
	root := gitRepo(t)

	report := Ensure(root, options())
	if len(report.Failures) > 0 {
		t.Fatalf("failures on a fresh repository: %v", report.Failures)
	}
	if !changed(report) {
		t.Fatal("the report says nothing changed in a directory that had neither the state dir nor an ignore file")
	}
	if got := report.CreatedDirs; len(got) != 1 || got[0] != ".devcouncil" {
		t.Fatalf("CreatedDirs = %q, want [.devcouncil]", got)
	}
	if info, err := os.Stat(filepath.Join(root, ".devcouncil")); err != nil || !info.IsDir() {
		t.Fatalf("the state directory was reported created but is not there: %v", err)
	}

	content := read(t, filepath.Join(root, ".gitignore"))
	for _, rule := range allRules() {
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
	root := gitRepo(t)
	Ensure(root, options())
	before := read(t, filepath.Join(root, ".gitignore"))

	report := Ensure(root, options())
	if len(report.Failures) > 0 {
		t.Fatalf("failures on the second run: %v", report.Failures)
	}
	if changed(report) {
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
	root := gitRepo(t)
	path := filepath.Join(root, ".gitignore")
	// No trailing newline, and one rule the harness would otherwise add.
	existing := "# mine\nbuild/\n.devcouncil/*"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
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
	if strings.Contains(content, ".devcouncil/*\n# ") {
		t.Fatalf("a heading was appended straight onto the last unterminated line:\n%s", content)
	}
	if n := strings.Count(content, "\n.devcouncil/*"); n != 1 {
		t.Fatalf(".devcouncil/* appears %d times; a rule already in the file was added again:\n%s", n, content)
	}
	for _, rule := range report.AddedRules {
		if rule == ".devcouncil/*" {
			t.Error("the report claims to have added a rule that was already there")
		}
	}
	if !strings.Contains(content, "build/") {
		t.Error("the operator's own rule was dropped")
	}
	if !strings.Contains(content, "!.devcouncil/config.yaml") {
		t.Error("the config negation was not added")
	}
}

// TestEnsureRecognisesARuleUnderAnyHeading: the check is against the file's
// rules, not against a marker block, so an operator who moved a rule into their
// own section does not get it written back a second time.
func TestEnsureRecognisesARuleUnderAnyHeading(t *testing.T) {
	root := gitRepo(t)
	path := filepath.Join(root, ".gitignore")
	if err := os.WriteFile(path, []byte("# whatever I call it\n  .devcouncil/*  \n"), 0o600); err != nil {
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
	root := gitRepo(t)
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
	root := gitRepo(t)
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

// TestNoGitignoreOutsideAGitWorkingTree: a directory git does not manage has no
// use for ignore rules, and a .gitignore created there is a file the operator
// never asked for in a directory that may not be theirs. The state directory is
// still made — the command that needed it still runs — and the report says
// why the ignore file was left alone, once, beside the directory it created.
func TestNoGitignoreOutsideAGitWorkingTree(t *testing.T) {
	root := t.TempDir()
	report := Ensure(root, options())
	if len(report.Failures) > 0 {
		t.Fatalf("failures: %v", report.Failures)
	}
	if _, err := os.Stat(filepath.Join(root, ".gitignore")); !os.IsNotExist(err) {
		t.Fatalf("a .gitignore was created outside a git working tree (stat error: %v)", err)
	}
	if len(report.AddedRules) > 0 {
		t.Fatalf("the report claims rules were added: %v", report.AddedRules)
	}
	if !report.Untracked {
		t.Error("a directory outside any git working tree was not reported untracked")
	}
	if info, err := os.Stat(filepath.Join(root, ".devcouncil")); err != nil || !info.IsDir() {
		t.Fatalf("the state directory was not created: %v", err)
	}
	if lines := strings.Join(report.Lines(), "\n"); !strings.Contains(lines, ".gitignore left alone") {
		t.Errorf("the report does not say why .gitignore was not written:\n%s", lines)
	}
	// Said beside the directory it created, not on every later run.
	if lines := Ensure(root, options()).Lines(); len(lines) != 0 {
		t.Errorf("a run that changed nothing reported %q", lines)
	}

	// An empty .git directory is not a repository either; git is what decides.
	fake := t.TempDir()
	if err := os.Mkdir(filepath.Join(fake, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	if !Ensure(fake, options()).Untracked {
		t.Error("an empty .git directory was taken for a working tree")
	}
	if _, err := os.Stat(filepath.Join(fake, ".gitignore")); !os.IsNotExist(err) {
		t.Fatal("a .gitignore was created beside an empty .git directory")
	}

	if Ensure(gitRepo(t), options()).Untracked {
		t.Error("a git working tree was reported untracked")
	}
}

// TestManagedRulesAreTheHarnessStateOnly: every rule the harness maintains is
// about its own state directory. Repository-wide patterns — *.sqlite, *.db,
// .claude*, .cursor/, logs/ — are decisions about the operator's files, and
// they hid fixtures and checked-in agent settings in repositories that never
// asked for them.
func TestManagedRulesAreTheHarnessStateOnly(t *testing.T) {
	for _, rule := range allRules() {
		if !strings.Contains(rule, ".devcouncil/") {
			t.Errorf("managed rule %q is not about the harness state directory", rule)
		}
	}
}

// TestManagedRulesNeverIgnoreATrackedFile builds a repository that commits the
// files the old repository-wide rules swallowed, and asks git whether any
// tracked file is ignored afterwards.
func TestManagedRulesNeverIgnoreATrackedFile(t *testing.T) {
	root := gitRepo(t)
	for _, rel := range []string{
		"CLAUDE.md", "AGENTS.md",
		".claude/settings.json", ".cursor/rules/style.mdc", ".agents/notes.md",
		"testdata/fixture.sqlite", "testdata/app.db", "logs/README.md", ".env.example",
		".devcouncil/config.yaml",
	} {
		writeFile(t, filepath.Join(root, rel), "x\n")
	}
	runGit(t, root, "add", "-A")
	runGit(t, root, "commit", "-qm", "fixture")

	report := Ensure(root, options())
	if len(report.Failures) > 0 {
		t.Fatalf("failures: %v", report.Failures)
	}
	if len(report.AddedRules) == 0 {
		t.Fatal("nothing was added to a repository with no managed rules")
	}
	if hidden := strings.TrimSpace(runGit(t, root, "ls-files", "--cached", "--ignored", "--exclude-standard")); hidden != "" {
		t.Fatalf("tracked files are ignored after Ensure:\n%s\n.gitignore:\n%s", hidden, read(t, filepath.Join(root, ".gitignore")))
	}
	if !ignored(t, root, ".devcouncil/state.sqlite") {
		t.Error("the harness store is not ignored")
	}
}

// TestARuleThatWouldHideATrackedFileIsWithheld: a repository that commits
// something under .devcouncil/ besides config.yaml — DevCouncil itself commits
// its plugin there — would have it ignored by ".devcouncil/*". That rule is
// left out and the report says which tracked file stopped it, so the operator
// decides how to keep their state out of a commit instead of the harness
// deciding their file stops being visible.
func TestARuleThatWouldHideATrackedFileIsWithheld(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, filepath.Join(root, ".devcouncil", "config.yaml"), "a: 1\n")
	writeFile(t, filepath.Join(root, ".devcouncil", "policy.yaml"), "b: 2\n")
	// Below a directory the rule would exclude, which git reports differently
	// from a file directly under it.
	writeFile(t, filepath.Join(root, ".devcouncil", "plugin", "agents", "reviewer.md"), "c\n")
	runGit(t, root, "add", "-A")
	runGit(t, root, "commit", "-qm", "fixture")

	report := Ensure(root, options())
	if len(report.Failures) > 0 {
		t.Fatalf("failures: %v", report.Failures)
	}
	if len(report.Withheld) != 1 || report.Withheld[0].Rule != ".devcouncil/*" {
		t.Fatalf("Withheld = %+v, want .devcouncil/* alone", report.Withheld)
	}
	want := []string{".devcouncil/plugin/agents/reviewer.md", ".devcouncil/policy.yaml"}
	if got := report.Withheld[0].Tracked; !slices.Equal(got, want) {
		t.Fatalf("Withheld names %q, want the tracked %q", got, want)
	}
	if lines := strings.Join(report.Lines(), "\n"); !strings.Contains(lines, ".devcouncil/policy.yaml") {
		t.Errorf("the report does not name the tracked file that stopped the rule:\n%s", lines)
	}
	// Its negations mean nothing without it, so they are not written either.
	if _, err := os.Stat(filepath.Join(root, ".gitignore")); !os.IsNotExist(err) {
		t.Fatalf("a .gitignore was written with nothing in it worth writing:\n%s", read(t, filepath.Join(root, ".gitignore")))
	}
	if hidden := strings.TrimSpace(runGit(t, root, "ls-files", "--cached", "--ignored", "--exclude-standard")); hidden != "" {
		t.Fatalf("tracked files are ignored after Ensure:\n%s", hidden)
	}
}

// TestARootBelowTheTopLevelIsCheckedAgainstItsOwnFiles: the project root can be
// a directory inside a repository — one that carries its own .devcouncil — and
// the rules written there are anchored to it, not to the top level. A check
// that evaluated them from the top would pass the very rule that hides a file
// below the root.
func TestARootBelowTheTopLevelIsCheckedAgainstItsOwnFiles(t *testing.T) {
	top := gitRepo(t)
	root := filepath.Join(top, "svc")
	writeFile(t, filepath.Join(root, ".devcouncil", "rules.yaml"), "x\n")
	runGit(t, top, "add", "-A")
	runGit(t, top, "commit", "-qm", "fixture")

	report := Ensure(root, options())
	if len(report.Failures) > 0 {
		t.Fatalf("failures: %v", report.Failures)
	}
	if report.Untracked {
		t.Fatal("a directory inside a working tree was reported untracked")
	}
	if len(report.Withheld) != 1 || report.Withheld[0].Rule != ".devcouncil/*" {
		t.Fatalf("Withheld = %+v, want .devcouncil/* — it would hide svc/.devcouncil/rules.yaml", report.Withheld)
	}
	if _, err := os.Stat(filepath.Join(top, ".gitignore")); !os.IsNotExist(err) {
		t.Fatal("a .gitignore was written at the top level rather than at the project root")
	}
}

// TestARelocatedStoreIsIgnoredByItsOwnPath: with the repository-wide *.sqlite
// rule gone, an operator who pointed MANVI_STORE_DB or MANVI_GRAPH inside the
// repository still has that file — and the database's journal files — kept out
// of a commit, by its own anchored path and nothing wider.
func TestARelocatedStoreIsIgnoredByItsOwnPath(t *testing.T) {
	root := gitRepo(t)
	opts := options()
	opts.ArtifactPaths = []string{
		filepath.Join(root, "data", "state.sqlite"),
		filepath.Join(root, "data", "code_graph.json"),
		filepath.Join(root, ".devcouncil", "harness-grants.json"), // covered already
		filepath.Join(t.TempDir(), "elsewhere.sqlite"),            // not expressible
	}
	report := Ensure(root, opts)
	if len(report.Failures) > 0 {
		t.Fatalf("failures: %v", report.Failures)
	}
	for _, rel := range []string{"data/state.sqlite", "data/state.sqlite-wal", "data/state.sqlite-shm", "data/state.sqlite-journal", "data/code_graph.json"} {
		if !ignored(t, root, rel) {
			t.Errorf("%s is not ignored:\n%s", rel, read(t, filepath.Join(root, ".gitignore")))
		}
	}
	for _, rel := range []string{"data/fixture.sqlite", "sub/data/state.sqlite", "data/code_graph.json.md"} {
		if ignored(t, root, rel) {
			t.Errorf("%s is ignored; the rule is wider than the harness's own file", rel)
		}
	}
	content := read(t, filepath.Join(root, ".gitignore"))
	if strings.Contains(content, "elsewhere") || strings.Contains(content, "harness-grants") {
		t.Fatalf("a rule was written for a path the existing rules cover or cannot express:\n%s", content)
	}
	if changed(Ensure(root, opts)) {
		t.Fatal("the second run with relocated artifacts changed something")
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
	root := gitRepo(t)
	opts := Options{
		StateDir:      filepath.Join("var", "manvi"),
		ArtifactPaths: []string{filepath.Join(root, "data", "state.sqlite"), filepath.Join(root, "data", "code_graph.json")},
		Gitignore:     true,
	}
	report := Ensure(root, opts)
	if len(report.AddedRules) <= 6 {
		t.Fatalf("only %d rules were added; the summary needs more than six to say anything", len(report.AddedRules))
	}

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
	if !strings.Contains(ignoreLine, fmt.Sprintf("%d ignore rule(s)", len(report.AddedRules))) {
		t.Fatalf("the line does not count what was added:\n%s", ignoreLine)
	}
}

// TestARelocatedStateDirIsIgnoredToo. MANVI_STATE_DIR moves the directory the
// harness fills with an index and a grant ledger; the fixed rules only name the
// default one, so without this the setting quietly reintroduces the commit the
// fixed rule exists to prevent.
func TestARelocatedStateDirIsIgnoredToo(t *testing.T) {
	root := gitRepo(t)
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
	if changed(Ensure(root, Options{StateDir: filepath.Join("var", "manvi"), Gitignore: true})) {
		t.Fatal("the second run with a relocated state directory changed something")
	}
}

// TestTheDefaultStateDirIsNotWrittenTwice: ".devcouncil/*" already covers it,
// and a second rule for the same directory reads as two different decisions.
func TestTheDefaultStateDirIsNotWrittenTwice(t *testing.T) {
	root := gitRepo(t)
	Ensure(root, options())
	if n := strings.Count(read(t, filepath.Join(root, ".gitignore")), ".devcouncil"); n != 3 {
		t.Fatalf(".devcouncil appears %d times, want the 3 DevCouncil rules only:\n%s",
			n, read(t, filepath.Join(root, ".gitignore")))
	}
}

// TestAStateDirOutsideTheRepositoryAddsNoRule: a path .gitignore cannot express
// must not become a rule that looks like it works.
func TestAStateDirOutsideTheRepositoryAddsNoRule(t *testing.T) {
	root := gitRepo(t)
	outside := filepath.Join(t.TempDir(), "state")
	Ensure(root, Options{StateDir: outside, Gitignore: true})

	content := read(t, filepath.Join(root, ".gitignore"))
	if strings.Contains(content, "..") || strings.Contains(content, outside) {
		t.Fatalf("a rule was written for a directory outside the repository:\n%s", content)
	}
}

// TestARepositoryThatAlreadyKeepsItsStateIgnoredIsLeftAlone: DevCouncil's own
// .gitignore ignores a .devcouncil/ at any depth (nested ones are generated
// caches) and re-includes only the root one, anchored. Ensure decides a rule is
// missing by its text, so an unanchored "!.devcouncil/" read as missing there,
// was appended last, and — the last matching rule winning — re-included every
// nested cache directory the file had been written to keep out.
func TestARepositoryThatAlreadyKeepsItsStateIgnoredIsLeftAlone(t *testing.T) {
	root := gitRepo(t)
	path := filepath.Join(root, ".gitignore")
	var b strings.Builder
	b.WriteString(".devcouncil/*\n!.devcouncil/config.yaml\n.devcouncil/\n!/.devcouncil/\n")
	before := b.String()
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}

	report := Ensure(root, options())
	if len(report.Failures) > 0 {
		t.Fatalf("failures: %v", report.Failures)
	}
	if len(report.AddedRules) > 0 {
		t.Fatalf("rules were appended to a file that already carries them: %v", report.AddedRules)
	}
	if after := read(t, path); after != before {
		t.Fatalf(".gitignore changed:\n%s", after)
	}
}

// TestNestedStateDirectoriesStayIgnored asks git itself, because what a rule
// means depends on its anchoring and on its place in the file, and a test that
// only compared text would pass with the rule that broke this.
func TestNestedStateDirectoriesStayIgnored(t *testing.T) {
	root := gitRepo(t)
	// An operator who ignores every generated .devcouncil/, at any depth.
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".devcouncil/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	Ensure(root, options())

	if !ignored(t, root, "rust/dc-grep/.devcouncil/dcgrep/index.bin") {
		t.Errorf("a nested .devcouncil/ cache is no longer ignored:\n%s", read(t, filepath.Join(root, ".gitignore")))
	}
	if !ignored(t, root, ".devcouncil/codeintel/graph.bin") {
		t.Error("the repository's own state is not ignored")
	}
	if ignored(t, root, ".devcouncil/config.yaml") {
		t.Error("the committable config is ignored")
	}
}

// ignored asks git whether rel would be ignored in root, tracked or not.
func ignored(t *testing.T, root, rel string) bool {
	t.Helper()
	// #nosec G204 -- the binary is the literal "git"; root and rel are this test's own.
	err := exec.CommandContext(t.Context(), "git", "-C", root, "check-ignore", "-q", "--no-index", rel).Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return false
	default:
		t.Fatalf("git check-ignore %s: %v", rel, err)
		return false
	}
}

// writeFile creates path, and its parents, with content.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
