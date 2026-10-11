// Package bootstrap prepares a repository for the harness: the state directory
// the harness writes into, and the ignore rules that keep that state out of a
// commit.
//
// It runs ahead of every command that opens the store or writes state, rather
// than behind an init subcommand, because both failures it prevents are silent
// ones. A missing state directory makes the store unopenable, so every lease
// check refuses and every write that needs one is blocked for a reason that has
// nothing to do with policy. An unignored state directory puts a multi-megabyte
// index, a grant ledger, and a code graph into someone's next commit. Neither
// announces itself at the moment it starts being true.
//
// It does not run for anything else. A command that only reads — help, a
// version, the flag table, the logo, a typo — has no state to protect, and an
// agent probing someone's checkout with one must leave that checkout exactly as
// it found it. Which commands those are is decided where they are dispatched,
// in cmd/manvi.
//
// What it writes is held to two limits. The ignore rules are about the
// harness's own files and nothing wider, and none of them may ignore a file the
// repository tracks: the .gitignore is the operator's, and a rule that hides
// their checked-in settings or fixtures is the harness deciding something about
// files it does not own. Outside a git working tree it writes no .gitignore at
// all, because there is nothing for one to govern.
//
// Nothing here is a gate, and nothing here is allowed to stop the command the
// operator actually typed. Scaffolding that refused to let the harness start
// would be a worse failure than the one it prevents. What it must never do is
// hide a failure, which is why Failures is part of the report rather than an
// error the caller can drop: a step that could not run is reported as such,
// never omitted, because an omitted step reads as one that worked.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/childenv"
)

// Section is one heading in the managed part of .gitignore, with the rules
// filed under it.
type Section struct {
	Heading string
	Rules   []string
}

// Sections are the rules the harness maintains: its own state directory, and
// nothing else.
//
// They were transcribed from DevCouncil's Python ensure_gitignore, which also
// carried four repository-wide sections — local AI agent directories (.claude*,
// .cursor/, …), *.sqlite and *.db, logs and temporary files, and environment and
// cache directories. Those were dropped, not made opt-in, and the decision is
// recorded here because it is the one a reader will want to reverse:
//
//   - They are hygiene for a repository, not protection for this harness. The
//     harness's databases, index, graph and ledger all live in the state
//     directory, which the rules below cover; an artifact moved elsewhere is
//     covered by its own path (see sectionsFor). Nothing the harness writes
//     needed *.sqlite.
//   - They were wrong for the repositories they landed in. ".claude*" and
//     ".cursor/" hid checked-in agent settings, "*.sqlite" and "*.db" hid test
//     fixtures, "logs/" hid a documented directory — and they landed from any
//     command, a help probe included, in checkouts that never asked.
//   - Opt-in would need a setting, and the settings catalogue belongs to
//     DevCouncil. A repository that wants those rules can write them; they are
//     one line each, and they are the operator's decision to make.
//
// DevCouncil's own .gitignore is the reference for the rules that remain, and a
// rule that differs from it by a character is a rule Ensure reads as missing and
// appends — where, being last, it wins.
//
// DevCouncil also ignores AGENTS.md and CLAUDE.md because `devmap` regenerates
// them. This harness generates neither, so ignoring them here would hide a
// hand-written file rather than a derived one.
var Sections = []Section{
	{
		// The negations are what keep DevCouncil's committable config
		// committable while the rest of the directory — the store, the index,
		// the grant ledger, the code graph — stays out of the history.
		//
		// The directory negation is anchored to the root. It exists so that
		// config.yaml stays reachable when something ignores .devcouncil/
		// wholesale, but an unanchored "!.devcouncil/" re-includes a
		// .devcouncil/ at every depth — the index caches the tools leave
		// beside a subdirectory they were pointed at — and, appended after an
		// operator's own ".devcouncil/", it overrode exactly the rule written
		// to keep those out. DevCouncil's own .gitignore anchors it the same
		// way, which is also what lets Ensure recognise it there.
		Heading: "DevCouncil local state",
		Rules: []string{
			".devcouncil/*",
			"!/.devcouncil/",
			"!.devcouncil/config.yaml",
		},
	},
}

// Options configures one Ensure.
type Options struct {
	// StateDir is the harness's own directory. Relative paths are resolved
	// against the root; an empty value means the state directory is not
	// managed, which is not a case any caller in this repository wants.
	StateDir string
	// ArtifactPaths are files the harness will write. Their parent directories
	// are created, so an operator who pointed MANVI_STORE_DB or MANVI_GRAPH at
	// a path outside the state directory does not get a failure from sqlite
	// about a directory the harness could have made — and one that lands
	// inside the repository but outside the state directory is ignored by its
	// own path.
	ArtifactPaths []string
	// Gitignore, when false, leaves .gitignore untouched.
	Gitignore bool
}

// Failure is one step that could not be completed, and why.
type Failure struct {
	What string
	Err  error
}

func (f Failure) String() string { return f.What + ": " + f.Err.Error() }

// Report is what Ensure did, in the terms a caller has to be able to print.
type Report struct {
	// Root is the directory that was scaffolded.
	Root string
	// CreatedDirs holds the directories that did not exist before this call,
	// relative to Root where they are under it.
	CreatedDirs []string
	// AddedRules holds the ignore rules appended by this call. An empty slice
	// after a successful run means the file already carried all of them.
	AddedRules []string
	// Untracked reports that Root is not inside a git working tree, so
	// .gitignore was left alone: there is nothing for it to govern.
	Untracked bool
	// Failures holds every step that could not run.
	Failures []Failure
	// Withheld holds the managed rules left out of .gitignore because they
	// would have ignored a file the repository tracks.
	Withheld []Withheld
}

// Withheld is one managed rule that was not written, and the tracked files it
// would have hidden.
type Withheld struct {
	Rule    string
	Tracked []string
}

// Lines renders the report for a human, one fact per line, and returns nothing
// when there is nothing to say. A run that changed nothing and failed at
// nothing is the ordinary case, and printing "already initialised" on every
// command is noise that trains an operator to stop reading the line above it.
func (r Report) Lines() []string {
	var lines []string
	for _, dir := range r.CreatedDirs {
		lines = append(lines, "created "+dir)
	}
	if r.Untracked && len(r.CreatedDirs) > 0 {
		// Said only beside the directory this run created, so the note lands
		// once, next to the change it explains, rather than on every later run.
		lines = append(lines, "not a git working tree — .gitignore left alone")
	}
	if n := len(r.AddedRules); n > 0 {
		lines = append(lines, fmt.Sprintf("added %d ignore rule(s) to .gitignore: %s",
			n, summarise(r.AddedRules, 6)))
	}
	for _, w := range r.Withheld {
		// Said on every run that would have written it, because the state it
		// describes persists: the harness's files are not being kept out of a
		// commit, and only the operator can decide how they should be.
		lines = append(lines, fmt.Sprintf("left %s out of .gitignore: it would ignore tracked %s",
			w.Rule, summarise(w.Tracked, 3)))
	}
	for _, f := range r.Failures {
		lines = append(lines, f.String())
	}
	return lines
}

// summarise names the first few of a list and counts the rest. A first run in a
// fresh repository adds every managed rule, and a single line naming fifty of
// them is a line nobody reads — including on the run where it says something
// unexpected. What changed exactly is one `git diff .gitignore` away.
func summarise(items []string, limit int) string {
	if len(items) <= limit {
		return strings.Join(items, " ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(items[:limit], " "), len(items)-limit)
}

// Ensure scaffolds root, and reports what it did.
//
// It returns no error. Every failure it can hit is one the harness can continue
// past — the command the operator typed will fail on its own terms if it needed
// what could not be made — and a failure returned here would be reported as if
// the harness itself had refused to run.
func Ensure(root string, opts Options) Report {
	report := Report{Root: root}

	for _, dir := range directories(root, opts) {
		if _, err := os.Stat(dir); err == nil {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			report.Failures = append(report.Failures, Failure{"the state directory could not be created", err})
			continue
		}
		report.CreatedDirs = append(report.CreatedDirs, display(root, dir))
	}

	if opts.Gitignore {
		result, err := ensureGitignore(root, sectionsFor(root, opts))
		if err != nil {
			report.Failures = append(report.Failures, Failure{".gitignore could not be updated", err})
		}
		report.AddedRules = result.added
		report.Withheld = result.withheld
		report.Untracked = result.untracked
	}
	return report
}

// sectionsFor is the managed rule set for one repository: the fixed sections,
// plus the harness's own files wherever the fixed ones do not cover them.
//
// The added rules are the whole point of the exercise for an operator who
// moved MANVI_STATE_DIR, MANVI_STORE_DB or MANVI_GRAPH. Without them the
// harness creates a database or a graph, and leaves it for the next commit —
// the exact failure the fixed ".devcouncil/*" rule exists to prevent,
// reintroduced by the setting that was supposed to relocate it. Each is named by
// its own path, which is what replaced the repository-wide "*.sqlite".
func sectionsFor(root string, opts Options) []Section {
	var rules []string
	if rule := stateDirRule(root, opts.StateDir); rule != "" {
		rules = append(rules, rule)
	}
	for _, path := range opts.ArtifactPaths {
		rules = append(rules, artifactRules(root, opts.StateDir, path)...)
	}
	if len(rules) == 0 {
		return Sections
	}
	// A fresh slice: appending to the package-level one would let a repository
	// with a moved state directory mutate what every later caller maintains.
	return append(append([]Section{}, Sections...),
		Section{Heading: "Harness state outside .devcouncil", Rules: dedupe(rules)})
}

// stateDirRule is the ignore rule for a relocated state directory, or empty
// when there is nothing to add: a directory outside the repository cannot be
// ignored from inside it, and the default is already covered.
func stateDirRule(root, stateDir string) string {
	rel, ok := under(root, stateDir)
	if !ok {
		return ""
	}
	rule := rel + "/"
	for _, section := range Sections {
		for _, existing := range section.Rules {
			// The default state directory is covered by the DevCouncil rules,
			// which are written as a glob rather than a directory.
			if existing == rule || strings.TrimSuffix(existing, "/*") == strings.TrimSuffix(rule, "/") {
				return ""
			}
		}
	}
	return rule
}

// sqliteJournals are the files SQLite writes beside a database, which go
// wherever the database goes.
var sqliteJournals = []string{"-wal", "-shm", "-journal"}

// artifactRules are the ignore rules for one artifact the harness writes, or
// none when the state directory's rules already cover it or the path is outside
// the repository. They are anchored, so a file of the same name anywhere else in
// the tree — a fixture, say — is not swept up with it.
func artifactRules(root, stateDir, path string) []string {
	rel, ok := under(root, path)
	if !ok {
		return nil
	}
	if stateDir != "" {
		if _, inState := under(stateDirAbs(root, stateDir), path); inState {
			return nil
		}
	}
	rules := []string{"/" + rel}
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".sqlite", ".sqlite3", ".db":
		for _, suffix := range sqliteJournals {
			rules = append(rules, "/"+rel+suffix)
		}
	}
	return rules
}

// stateDirAbs resolves the state directory against the root.
func stateDirAbs(root, stateDir string) string {
	if filepath.IsAbs(stateDir) {
		return filepath.Clean(stateDir)
	}
	return filepath.Join(root, stateDir)
}

// under reports path relative to dir, slash-separated, when path is strictly
// below dir. Relative paths are resolved against dir.
func under(dir, path string) (string, bool) {
	if path == "" {
		return "", false
	}
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(dir, abs)
	}
	rel, err := filepath.Rel(dir, abs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// dedupe drops repeated rules, keeping the first of each.
func dedupe(rules []string) []string {
	seen := map[string]bool{}
	out := rules[:0:0]
	for _, rule := range rules {
		if !seen[rule] {
			seen[rule] = true
			out = append(out, rule)
		}
	}
	return out
}

// directories collects the directories to ensure, in a stable order and
// without duplicates: the state directory, and the parent of every artifact
// path that was configured somewhere else.
func directories(root string, opts Options) []string {
	seen := map[string]bool{}
	var dirs []string
	add := func(dir string) {
		if dir == "" {
			return
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(root, dir)
		}
		dir = filepath.Clean(dir)
		// The root itself is the repository the operator is standing in. It
		// exists, and creating it would mean an artifact path escaped upward.
		if dir == filepath.Clean(root) || seen[dir] {
			return
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}

	add(opts.StateDir)
	for _, path := range opts.ArtifactPaths {
		add(filepath.Dir(path))
	}
	return dirs
}

// display renders a path the way an operator typed it: relative to the root
// when it is under it, absolute when it is not.
func display(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}

// gitignoreResult is what ensureGitignore did.
type gitignoreResult struct {
	added     []string
	withheld  []Withheld
	untracked bool
}

// gitTimeout bounds every git call one Ensure makes, together. Scaffolding
// runs before the command the operator typed, and a git that hangs — a lock, a
// network filesystem — must not hang that command with it.
const gitTimeout = 10 * time.Second

// ensureGitignore appends whatever managed rules are missing, and reports them.
//
// Missing is decided against every non-empty line already in the file, not
// against a marker block, so a rule an operator wrote themselves is never
// duplicated and a block they edited is never rewritten. Nothing is removed and
// nothing is reordered: this file is the operator's, and the harness only ever
// adds to the end of it.
//
// Git is consulted only when something is missing, which after the first run
// in a repository is never: whether this is a working tree at all, and which
// tracked files the additions would hide. A rule that would hide one is
// withheld rather than written.
func ensureGitignore(root string, sections []Section) (gitignoreResult, error) {
	var result gitignoreResult
	path := filepath.Join(root, ".gitignore")

	var content string
	// An existing file keeps its mode; the harness only adds to it.
	mode := os.FileMode(0o644)
	info, statErr := os.Stat(path)
	if statErr == nil {
		if info.IsDir() {
			return result, fmt.Errorf("%s is a directory", path)
		}
		mode = info.Mode().Perm()
		data, err := os.ReadFile(path)
		if err != nil {
			return result, err
		}
		content = string(data)
	} else if !os.IsNotExist(statErr) {
		return result, statErr
	}

	present := map[string]bool{}
	for _, line := range strings.Split(content, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			present[trimmed] = true
		}
	}
	write := map[string]bool{}
	for _, section := range sections {
		for _, rule := range section.Rules {
			if !present[rule] {
				write[rule] = true
			}
		}
	}
	if len(write) == 0 {
		return result, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()

	inTree, err := inWorkTree(ctx, root)
	if err != nil {
		return result, err
	}
	if !inTree {
		result.untracked = true
		return result, nil
	}

	withheld, err := withholdTracked(ctx, root, content, sections, present, write)
	if err != nil {
		return result, err
	}
	result.withheld = withheld

	text, added := render(content, sections, write)
	if len(added) == 0 {
		return result, nil
	}
	if err := writeAtomic(path, text, mode); err != nil {
		return result, err
	}
	result.added = added
	return result, nil
}

// render is content with the rules in write appended under their headings, and
// the rules it appended.
func render(content string, sections []Section, write map[string]bool) (string, []string) {
	var (
		chunks []string
		added  []string
	)
	for _, section := range sections {
		var rules []string
		for _, rule := range section.Rules {
			if write[rule] {
				rules = append(rules, rule)
			}
		}
		if len(rules) == 0 {
			continue
		}
		chunks = append(chunks, "# "+section.Heading+"\n"+strings.Join(rules, "\n"))
		added = append(added, rules...)
	}
	if len(chunks) == 0 {
		return content, nil
	}

	var b strings.Builder
	b.WriteString(content)
	if content != "" {
		if !strings.HasSuffix(content, "\n") {
			// Without this the first managed rule is appended to whatever the
			// operator's last line was, silently changing a pattern they wrote.
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	b.WriteString(strings.Join(chunks, "\n\n"))
	b.WriteString("\n")
	return b.String(), added
}

// withholdTracked removes from write every rule that would make git ignore a
// file the repository tracks, and reports what each one would have hidden.
//
// Git decides, not a reimplementation of its matching: what a rule means
// depends on its anchoring, on the negations after it, and on where the file
// sits relative to the top level, and a matcher that disagreed with git about
// any of those would pass the rule it exists to stop. The candidate file is
// evaluated whole, against the files tracked below root, and compared with the
// operator's file as it stands — a tracked file their own rules already ignore
// is theirs, not something this call did.
//
// Rules are tried one at a time, in order, and a rule is withheld only when
// leaving it out hides fewer tracked files. If anything is still hidden after
// every rule has been tried, the whole addition is refused: writing it would
// break the one promise this function exists to keep.
func withholdTracked(ctx context.Context, root, content string, sections []Section, present, write map[string]bool) ([]Withheld, error) {
	baseline, err := trackedIgnored(ctx, root, content)
	if err != nil {
		return nil, err
	}
	hidden := func() ([]string, error) {
		text, _ := render(content, sections, write)
		ignored, err := trackedIgnored(ctx, root, text)
		if err != nil {
			return nil, err
		}
		var newly []string
		for _, file := range ignored {
			if !slices.Contains(baseline, file) {
				newly = append(newly, file)
			}
		}
		return newly, nil
	}

	newly, err := hidden()
	if err != nil || len(newly) == 0 {
		return nil, err
	}

	var withheld []Withheld
	for _, section := range sections {
		for _, rule := range section.Rules {
			if len(newly) == 0 {
				break
			}
			if !write[rule] || strings.HasPrefix(rule, "!") {
				continue
			}
			delete(write, rule)
			after, err := hidden()
			if err != nil {
				return nil, err
			}
			if len(after) >= len(newly) {
				write[rule] = true
				continue
			}
			var stopped []string
			for _, file := range newly {
				if !slices.Contains(after, file) {
					stopped = append(stopped, file)
				}
			}
			withheld = append(withheld, Withheld{Rule: rule, Tracked: stopped})
			newly = after
		}
	}
	if len(newly) > 0 {
		return nil, fmt.Errorf("the managed rules would ignore tracked %s, and no single rule could be left out to prevent it", summarise(newly, 3))
	}

	// A negation re-includes what a rule beside it excluded. With every
	// exclusion in its section withheld and none already in the file, it
	// re-includes nothing, and writing it would leave a line in the operator's
	// file that explains a rule that is not there.
	for _, section := range sections {
		active := false
		for _, rule := range section.Rules {
			if !strings.HasPrefix(rule, "!") && (present[rule] || write[rule]) {
				active = true
				break
			}
		}
		if active {
			continue
		}
		for _, rule := range section.Rules {
			delete(write, rule)
		}
	}
	return withheld, nil
}

// removeCheckFile is os.Remove, replaceable by a test that needs the cleanup
// to fail: no file mode stops the owner removing a file it just created.
var removeCheckFile = os.Remove

// trackedIgnored lists the files tracked below root that a .gitignore holding
// content, placed at root, would ignore.
//
// The content goes into a temporary file beside the real one and git is told
// to read that name in place of .gitignore, so the rules are evaluated exactly
// as they would be once written — anchored at root, negations included —
// without the operator's file being touched to find out.
//
// The check file is created in the operator's own work tree, so failing to
// remove it is reported with the answer: a stray .gitignore-check-* left there
// without a word is exactly the kind of change this package promises not to
// make silently.
func trackedIgnored(ctx context.Context, root, content string) (tracked []string, err error) {
	if strings.TrimSpace(content) == "" {
		return nil, nil
	}
	tmp, err := os.CreateTemp(root, ".gitignore-check-*")
	if err != nil {
		return nil, err
	}
	name := tmp.Name()
	defer func() {
		if rmErr := removeCheckFile(name); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("removing the ignore-rule check file %s: %w", name, rmErr))
		}
	}()
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close() // the write failed; that is the error worth returning
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}

	out, err := git(ctx, root, "ls-files", "-z", "--cached", "--ignored",
		"--exclude-per-directory="+filepath.Base(name))
	if err != nil {
		return nil, fmt.Errorf("listing the tracked files the rules would ignore: %w", err)
	}
	var files []string
	for _, file := range strings.Split(string(out), "\x00") {
		if file != "" {
			files = append(files, file)
		}
	}
	sort.Strings(files)
	return files, nil
}

// inWorkTree reports whether root is inside a git working tree.
//
// Git answers rather than a stat of root/.git: the project root can sit below
// the top level, and an empty .git directory is not a repository. "Not a
// repository" is the one failure that is an answer; any other — no git on PATH,
// an ownership refusal, a timeout — is returned, because a check that could not
// run must not read as one that said no.
func inWorkTree(ctx context.Context, root string) (bool, error) {
	out, err := git(ctx, root, "rev-parse", "--is-inside-work-tree")
	var exit *exec.ExitError
	if errors.As(err, &exit) && strings.Contains(string(exit.Stderr), "not a git repository") {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("could not tell whether %s is a git working tree: %w", root, err)
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

// git runs one git command in dir and returns its stdout. The C locale is
// forced so the one message inWorkTree reads is the one it looks for.
func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	// #nosec G204 -- the binary is the literal "git"; every argument is a literal, the project root, or a file name this package created.
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	// Built, not inherited: a hook or filter in the repository runs with it.
	cmd.Env = childenv.Git("LC_ALL=C")
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && len(exit.Stderr) > 0 {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exit.Stderr)))
	}
	return out, err
}

// writeAtomic replaces path in one step.
//
// The file being replaced is the operator's, and it is usually longer than what
// the harness contributes to it. A truncating write that fails halfway — a full
// disk, a signal — would leave them with a .gitignore that is neither the old
// one nor the new one, and the harness would have destroyed content it did not
// author to add three lines it did.
func writeAtomic(path, content string, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".gitignore-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)

	// On each failure below, the step that failed is the error worth
	// returning; the close that follows it is cleanup, and the deferred
	// remove takes the half-written file either way.
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	// Sync before publish: without this a crash right after the rename can
	// leave the file present but empty — the exact truncated-.gitignore state
	// that silently disables the rules it carries. The session store's
	// link-and-sync protocol is the reference here.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	// And sync the directory: syncing the file puts its bytes on disk; only
	// the directory sync guarantees the NAME is there after a crash.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
