// Package bootstrap prepares a repository for the harness: the state directory
// the harness writes into, and the ignore rules that keep that state out of a
// commit.
//
// It runs on every invocation rather than behind an init subcommand, because
// both failures it prevents are silent ones. A missing state directory makes
// the store unopenable, so every lease check refuses and every write that needs
// one is blocked for a reason that has nothing to do with policy. An unignored
// state directory puts a multi-megabyte index, a grant ledger, and a code graph
// into someone's next commit. Neither announces itself at the moment it starts
// being true.
//
// Nothing here is a gate, and nothing here is allowed to stop the command the
// operator actually typed. Scaffolding that refused to let the harness start
// would be a worse failure than the one it prevents. What it must never do is
// hide a failure, which is why Failures is part of the report rather than an
// error the caller can drop: a step that could not run is reported as such,
// never omitted, because an omitted step reads as one that worked.
package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Section is one heading in the managed part of .gitignore, with the rules
// filed under it.
type Section struct {
	Heading string
	Rules   []string
}

// Sections are the rules the harness maintains.
//
// They are transcribed from DevCouncil's own ensure_gitignore rather than
// invented, and the wording of each rule is kept identical on purpose: the two
// tools share one .devcouncil/ directory, and a rule that differs by a
// character is a rule each tool re-adds after the other has run, so a
// repository used with both would grow a duplicate block per invocation.
//
// Two entries are deliberately not carried over:
//
//   - DevCouncil ignores AGENTS.md and CLAUDE.md because `dev map` regenerates
//     them. This harness generates neither, so ignoring them here would hide a
//     hand-written file rather than a derived one.
//   - DevCouncil's agent list carries ".conducor/" beside ".conductor/". It is
//     a typo, and writing it into an operator's file would propagate it.
var Sections = []Section{
	{
		// The negations are what keep DevCouncil's committable config
		// committable while the rest of the directory — the store, the index,
		// the grant ledger, the code graph — stays out of the history.
		Heading: "DevCouncil local state",
		Rules: []string{
			".devcouncil/*",
			"!.devcouncil/",
			"!.devcouncil/config.yaml",
		},
	},
	{
		Heading: "Local AI coding agents",
		Rules: []string{
			".agents/",
			".codex/",
			".aider*",
			".gemini/",
			".claude*",
			".cursor/",
			".openhands/",
			".opencode/",
			".conductor/",
			".antigravity/",
			".warp/",
			".grok/",
		},
	},
	{
		Heading: "Secrets and local databases",
		Rules: []string{
			"*.sqlite",
			"*.sqlite-wal",
			"*.sqlite-shm",
			"*.db",
		},
	},
	{
		Heading: "Temporary, log, and dump artifacts",
		Rules: []string{
			"logs/",
			"log/",
			"tmp/",
			"temp/",
			".tmp/",
			".temp/",
			"scratch/",
			"dumps/",
			"dump/",
			"*.tmp",
			"*.temp",
			"*.log",
			"*.dmp",
			"*.dump",
			"*.bak",
			"*.swp",
			"*_results.txt",
			"*_log.txt",
			"*_output.txt",
		},
	},
	{
		Heading: "Environment, dependency, and cache directories",
		Rules: []string{
			"__pycache__/",
			"*.py[cod]",
			".venv/",
			"venv/",
			"node_modules/",
			".env",
			".env.local",
			".env.*",
			"!.env.example",
			".pytest_cache/",
			".mypy_cache/",
			".ruff_cache/",
			".DS_Store",
			"Thumbs.db",
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
	// about a directory the harness could have made.
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
	// Untracked reports that Root is not a git working tree, so the ignore
	// rules are written but currently govern nothing.
	Untracked bool
	// Failures holds every step that could not run.
	Failures []Failure
}

// Changed reports whether this call altered the working tree.
func (r Report) Changed() bool { return len(r.CreatedDirs) > 0 || len(r.AddedRules) > 0 }

// Lines renders the report for a human, one fact per line, and returns nothing
// when there is nothing to say. A run that changed nothing and failed at
// nothing is the ordinary case, and printing "already initialised" on every
// command is noise that trains an operator to stop reading the line above it.
func (r Report) Lines() []string {
	var lines []string
	for _, dir := range r.CreatedDirs {
		lines = append(lines, "created "+dir)
	}
	if n := len(r.AddedRules); n > 0 {
		lines = append(lines, fmt.Sprintf("added %d ignore rule(s) to .gitignore: %s",
			n, summarise(r.AddedRules, 6)))
		if r.Untracked {
			// Said only when something was actually written, so the note lands
			// next to the file it is about rather than on every later run.
			lines = append(lines, "no .git here — those rules take effect when this becomes a repository")
		}
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
		added, err := ensureGitignore(root, sectionsFor(root, opts.StateDir))
		if err != nil {
			report.Failures = append(report.Failures, Failure{".gitignore could not be updated", err})
		}
		report.AddedRules = added
		if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
			report.Untracked = true
		}
	}
	return report
}

// sectionsFor is the managed rule set for one repository: the fixed sections,
// plus the state directory when it is somewhere the fixed ones do not cover.
//
// The added rule is the whole point of the exercise for an operator who moved
// MANVI_STATE_DIR. Without it the harness creates a directory, fills it with an
// index and a grant ledger, and leaves it for the next commit — the exact
// failure the fixed ".devcouncil/*" rule exists to prevent, reintroduced by the
// setting that was supposed to relocate it.
func sectionsFor(root, stateDir string) []Section {
	rule := stateDirRule(root, stateDir)
	if rule == "" {
		return Sections
	}
	// A fresh slice: appending to the package-level one would let a repository
	// with a moved state directory mutate what every later caller maintains.
	return append(append([]Section{}, Sections...),
		Section{Heading: "Harness state directory", Rules: []string{rule}})
}

// stateDirRule is the ignore rule for a relocated state directory, or empty
// when there is nothing to add: a directory outside the repository cannot be
// ignored from inside it, and the default is already covered.
func stateDirRule(root, stateDir string) string {
	if stateDir == "" {
		return ""
	}
	abs := stateDir
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, abs)
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	rule := filepath.ToSlash(rel) + "/"
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

// ensureGitignore appends whatever managed rules are missing, and returns them.
//
// Missing is decided against every non-empty line already in the file, not
// against a marker block, so a rule an operator wrote themselves is never
// duplicated and a block they edited is never rewritten. Nothing is removed and
// nothing is reordered: this file is the operator's, and the harness only ever
// adds to the end of it.
func ensureGitignore(root string, sections []Section) ([]string, error) {
	path := filepath.Join(root, ".gitignore")

	var content string
	info, statErr := os.Stat(path)
	if statErr == nil {
		if info.IsDir() {
			return nil, fmt.Errorf("%s is a directory", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		content = string(data)
	} else if !os.IsNotExist(statErr) {
		return nil, statErr
	}

	present := map[string]bool{}
	for _, line := range strings.Split(content, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			present[trimmed] = true
		}
	}

	var (
		chunks []string
		added  []string
	)
	for _, section := range sections {
		var missing []string
		for _, rule := range section.Rules {
			if !present[rule] {
				missing = append(missing, rule)
			}
		}
		if len(missing) == 0 {
			continue
		}
		chunks = append(chunks, "# "+section.Heading+"\n"+strings.Join(missing, "\n"))
		added = append(added, missing...)
	}
	if len(chunks) == 0 {
		return nil, nil
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

	mode := os.FileMode(0o644)
	if statErr == nil {
		mode = info.Mode().Perm()
	}
	if err := writeAtomic(path, b.String(), mode); err != nil {
		return nil, err
	}
	return added, nil
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

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// Rules lists every managed rule, sorted. It exists for the tests and for any
// caller that has to report what the harness would maintain without writing it.
func Rules() []string {
	var all []string
	for _, section := range Sections {
		all = append(all, section.Rules...)
	}
	sort.Strings(all)
	return all
}
