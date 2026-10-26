package devcouncil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// The commit-time check on content.
//
// secretPaths and aliasedPaths judge the names a commit stages. A credential
// does not have to arrive under its own: `cp .env notes.txt` followed by a
// stage and a commit puts the file in history as notes.txt, past the secret
// patterns and past .gitignore. What identifies the file is its bytes, so the
// commit is also judged by them. A staged blob is refused when
//
//   - its object id is the id of a secret-path file (an exact copy, whatever it
//     is now called) — the file as the index, HEAD and the working tree hold it,
//   - what it adds contains a value assigned in one of those files (an edited
//     copy), or
//   - what it adds contains a credential this process knows (a provider key, a
//     forwarded token — Deps.KnownSecret).
//
// What a text blob adds is read from the staged patch's `+` lines. Git does not
// print a binary's bytes there, so a binary — by git's own verdict, attributes
// included — is read whole from the object store in bounded windows.
//
// It fails closed, and every refusal names a remedy the agent can perform or
// says who can: anything that cannot be read, listed, hashed or scanned within
// its bounds is a refusal that says the check did not run, never a clean pass.
// A secret file that is gone from the working tree is still judged by the
// blobs git holds for it, a working-tree entry that is not a regular file (a
// symlink, a directory, a FIFO) is judged by those blobs alone, and neither is
// a failure.
//
// Residuals, accepted and named: a modified binary whose HEAD version already
// held a value is refused on every touch, since its bytes still hold it; an
// untracked, ignored secret file that was deleted is nothing the check can
// enumerate; and KnownSecret is asked once per window, not per line, because
// each call re-arms the credential scrubber.

const (
	// maxSecretFiles bounds how many secret-path files one commit check hashes.
	// A tree holding more than this is not one whose credentials this check
	// can enumerate, and says so.
	maxSecretFiles = 512
	// minSecretValueLen matches the credential scrubber's floor: shorter values
	// match ordinary prose and protect nothing.
	minSecretValueLen = 8
)

// assignedValue reads `KEY=value` and `export KEY=value` lines, the shape of
// .env files, shell profiles and most credential configs.
var assignedValue = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_.-]*)\s*[=:]\s*(.+?)\s*$`)

// credentialKeyHint is what makes an assignment's value a credential rather
// than configuration. A .env also says NODE_ENV=development, and refusing every
// commit that mentions "development" would be a check people route around.
var credentialKeyHint = regexp.MustCompile(`(?i)key|token|secret|pass|pwd|credential|auth|private|dsn|database_url|conn`)

// looksCredential reports whether an assigned value is worth matching: its key
// names a credential, or the value is long and mixes letters with digits, which
// is what generated secrets look like and configuration does not.
func looksCredential(key, value string) bool {
	if len(value) < minSecretValueLen {
		return false
	}
	if credentialKeyHint.MatchString(key) {
		return true
	}
	if len(value) < 16 {
		return false
	}
	var letter, digit bool
	for _, c := range value {
		switch {
		case c >= '0' && c <= '9':
			digit = true
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			letter = true
		}
	}
	return letter && digit
}

// stagedLeak is one staged path that carries a credential, and how it was
// recognised. It never carries the credential.
type stagedLeak struct {
	Path   string   `json:"path"`
	Reason []string `json:"matched"`
}

// secretFileSet is what the commit check knows of the secret-path files: the
// git blob ids of every version of them, and the values they assign.
type secretFileSet struct {
	blobs  map[string]string // object id -> path
	values map[string]string // value -> path
}

// trackedBlob is one version of a tracked secret-path file that git holds.
type trackedBlob struct {
	id, source string
}

// literalSpecs spells paths as pathspecs that match themselves: a file named
// `we*rd` is a path, not a glob.
func literalSpecs(paths []string) []string {
	specs := make([]string, len(paths))
	for i, p := range paths {
		specs[i] = ":(literal)" + p
	}
	return specs
}

// headExists reports whether HEAD names a commit. An unborn branch is a state,
// not a failure; any other way `rev-parse` can fail is.
func (r *Registry) headExists(ctx context.Context) (bool, error) {
	res := runProc(ctx, func() procSpec {
		spec := gitSpec(r.deps.Root, "", []string{"rev-parse", "--verify", "--quiet", "HEAD"})
		spec.stdoutLimit = maxGitStderrBytes
		spec.stderrLimit = maxGitStderrBytes
		return spec
	}())
	switch {
	case res.timedOut:
		return false, errors.New("git rev-parse HEAD: timed out")
	case res.failedToRun():
		return false, fmt.Errorf("git rev-parse HEAD: %w", res.err)
	case res.exitCode == 0:
		return true, nil
	case res.exitCode == 1 && len(strings.TrimSpace(string(res.stderr))) == 0:
		return false, nil
	}
	return false, fmt.Errorf("git rev-parse HEAD: exit %d: %s", res.exitCode, strings.TrimSpace(string(res.stderr)))
}

// trackedSecretBlobs finds every version of each secret-path file that git
// holds — the index's and HEAD's — keyed by path. Only regular blobs: a symlink
// is its target's name, not bytes that hold a value, and a submodule is a
// commit id.
func (r *Registry) trackedSecretBlobs(ctx context.Context, secret []string) (map[string][]trackedBlob, error) {
	isSecret := make(map[string]bool, len(secret))
	for _, p := range secret {
		isSecret[p] = true
	}
	found := map[string][]trackedBlob{}
	take := func(listing, source string, withType bool) error {
		for _, rec := range strings.Split(listing, "\x00") {
			if rec == "" {
				continue
			}
			meta, path, ok := strings.Cut(rec, "\t")
			fields := strings.Fields(meta)
			if !ok || len(fields) != 3 {
				return fmt.Errorf("git answered an unreadable record for %s: %q", source, rec)
			}
			if !isSecret[path] {
				continue
			}
			mode, id := fields[0], fields[1]
			if withType {
				if fields[1] != "blob" {
					continue
				}
				id = fields[2]
			}
			if mode != "100644" && mode != "100755" {
				continue
			}
			found[path] = append(found[path], trackedBlob{id: id, source: source})
		}
		return nil
	}

	specs := literalSpecs(secret)
	staged, err := runGit(ctx, r.deps.Root, append([]string{"ls-files", "-s", "-z", "--"}, specs...)...)
	if err != nil {
		return nil, fmt.Errorf("reading the index versions of the secret-path files: %w", err)
	}
	if err := take(staged, "the index", false); err != nil {
		return nil, err
	}
	born, err := r.headExists(ctx)
	if err != nil {
		return nil, err
	}
	if born {
		head, err := runGit(ctx, r.deps.Root, append([]string{"ls-tree", "-r", "-z", "HEAD", "--"}, specs...)...)
		if err != nil {
			return nil, fmt.Errorf("reading the HEAD versions of the secret-path files: %w", err)
		}
		if err := take(head, "HEAD", true); err != nil {
			return nil, err
		}
	}
	return found, nil
}

func (r *Registry) collectSecretFiles(ctx context.Context) (secretFileSet, error) {
	set := secretFileSet{blobs: map[string]string{}, values: map[string]string{}}

	// Tracked and untracked-not-ignored files, then ignored ones. Ignored
	// directories are listed collapsed so a node_modules is one entry, not a
	// million; a collapsed directory is entered only when its own name is under
	// a secret pattern (secrets/).
	listings := [][]string{
		{"ls-files", "-z", "--cached", "--others", "--exclude-standard"},
		{"ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory"},
	}
	seen := map[string]bool{}
	var candidates []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			candidates = append(candidates, p)
		}
	}
	for _, args := range listings {
		out, err := runGit(ctx, r.deps.Root, args...)
		if err != nil {
			return set, fmt.Errorf("listing files: %w", err)
		}
		for _, p := range strings.Split(out, "\x00") {
			if !strings.HasSuffix(p, "/") {
				add(p)
				continue
			}
			hit, err := secretPaths(r.matcher(), []string{p + "x"})
			if err != nil {
				return set, err
			}
			if len(hit) == 0 {
				continue
			}
			walkErr := filepath.WalkDir(filepath.Join(r.deps.Root, filepath.FromSlash(p)),
				func(path string, d fs.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if d.Type().IsRegular() {
						if rel, relErr := filepath.Rel(r.deps.Root, path); relErr == nil {
							add(filepath.ToSlash(rel))
						}
					}
					if len(candidates) > maxSecretFiles*4 {
						return fs.SkipAll
					}
					return nil
				})
			if walkErr != nil {
				return set, fmt.Errorf("walking %s: %w", p, walkErr)
			}
		}
	}

	secret, err := secretPaths(r.matcher(), candidates)
	if err != nil {
		return set, err
	}
	if len(secret) > maxSecretFiles {
		return set, fmt.Errorf("the tree holds %d files under secret paths, more than the %d this check can enumerate",
			len(secret), maxSecretFiles)
	}
	if len(secret) == 0 {
		return set, nil
	}

	// What git holds for them: the index's and HEAD's versions, whose ids need
	// no hashing and whose bytes do not depend on the working tree.
	tracked, err := r.trackedSecretBlobs(ctx, secret)
	if err != nil {
		return set, err
	}

	// The working-tree copy, when there is one. A file gone from the working
	// tree is judged by the blobs above; so is an entry that is not a regular
	// file (a symlink stores its target's name, and a directory or FIFO has no
	// bytes to hold a value — and reading a FIFO would block). Any other
	// failure to look is a refusal: the copy may be there and unread.
	var worktree []string
	for _, p := range secret {
		info, err := os.Lstat(filepath.Join(r.deps.Root, filepath.FromSlash(p)))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return set, fmt.Errorf("could not stat the working-tree copy of the secret-path file %s, so its values were not scanned "+
				"(the harness user must be able to reach it; fix its permissions outside this session): %w", p, err)
		case !info.Mode().IsRegular():
			continue
		}
		worktree = append(worktree, p)
	}

	// Every version's id is an exact-copy match. The working-tree copies are
	// hashed because an unstaged edit's exact copy matches neither the index nor
	// HEAD.
	wtID := map[string]string{}
	if len(worktree) > 0 {
		out, err := runGit(ctx, r.deps.Root, append([]string{"hash-object", "--"}, worktree...)...)
		if err != nil {
			return set, fmt.Errorf("hashing secret-path files: %w", err)
		}
		ids := strings.Fields(out)
		if len(ids) != len(worktree) {
			return set, fmt.Errorf("hash-object answered %d ids for %d files", len(ids), len(worktree))
		}
		for i, id := range ids {
			set.blobs[id] = worktree[i]
			wtID[worktree[i]] = id
		}
	}

	limits := r.scan.limits.resolved()
	values := &valueScan{set: &set, limits: limits}
	var items []batchItem
	read := map[string]bool{}
	for _, p := range secret {
		for _, b := range tracked[p] {
			set.blobs[b.id] = p
			if !read[b.id] {
				read[b.id] = true
				items = append(items, batchItem{id: b.id, label: p})
			}
		}
	}
	// HEAD's version, the index's version and the working tree's, each judged
	// for its own values: staging an edit that drops a key from .env must not
	// let a staged copy of that key's old value through.
	if err := r.streamBlobs(ctx, items, func(item batchItem) blobSink { return values.sink(item.label) }); err != nil {
		return set, err
	}
	for _, p := range worktree {
		if read[wtID[p]] {
			continue
		}
		if err := r.readValues(values, p, filepath.Join(r.deps.Root, filepath.FromSlash(p))); err != nil {
			return set, err
		}
	}
	return set, nil
}

// stagedEntry is one path the index changes, from `diff --cached --raw`.
type stagedEntry struct {
	path, mode, id, status string
}

// content reports whether the entry adds bytes a scan can read: not a deletion
// (nothing is added), not a submodule (a commit id, not a blob).
func (e stagedEntry) content() bool {
	return e.status != "D" && e.mode != "160000" && strings.Trim(e.id, "0") != ""
}

// stagedBinaries is the set of staged paths git itself calls binary: numstat
// prints "-\t-" for them, a verdict that already folds in gitattributes
// (`binary`, `-diff`) and diff.<driver>.binary — which is why this is asked of
// git and not guessed from the bytes.
func (r *Registry) stagedBinaries(ctx context.Context) (map[string]bool, error) {
	out, err := runGit(ctx, r.deps.Root, "diff", "--cached", "--numstat", "-z", "--no-renames", "--no-textconv", "--no-ext-diff")
	if err != nil {
		return nil, fmt.Errorf("classifying the staged files as text or binary: %w", err)
	}
	binary := map[string]bool{}
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		parts := strings.SplitN(rec, "\t", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("git answered an unreadable numstat record: %q", rec)
		}
		if parts[0] == "-" && parts[1] == "-" {
			binary[parts[2]] = true
		}
	}
	return binary, nil
}

// stagedContentLeaks judges what is staged by its content. A nil result with a
// nil error means the check ran and found nothing; the notes it returns are
// degradations that did not change that answer (see below), for the caller to
// report.
func (r *Registry) stagedContentLeaks(ctx context.Context) ([]stagedLeak, []string, error) {
	set, err := r.collectSecretFiles(ctx)
	if err != nil {
		return nil, nil, err
	}

	leaks := map[string]map[string]bool{}
	note := func(path, reason string) {
		if leaks[path] == nil {
			leaks[path] = map[string]bool{}
		}
		leaks[path][reason] = true
	}
	var notes []string

	raw, err := runGit(ctx, r.deps.Root, "diff", "--cached", "--raw", "-z", "--no-renames", "--no-abbrev")
	if err != nil {
		return nil, nil, fmt.Errorf("reading the staged blobs: %w", err)
	}
	// ":<mode> <mode> <old> <new> <status>\0<path>\0" repeated.
	var staged []stagedEntry
	fields := strings.Split(raw, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(meta) < 5 {
			continue
		}
		if source, ok := set.blobs[meta[3]]; ok {
			note(fields[i+1], "identical to "+source)
		}
		staged = append(staged, stagedEntry{path: fields[i+1], mode: meta[1], id: meta[3], status: meta[4]})
	}

	binary, err := r.stagedBinaries(ctx)
	if err != nil {
		return nil, nil, err
	}

	// The prefixes are spelled out because diff.noprefix and diff.mnemonicPrefix
	// are repository configuration: with either set, git writes no "b/" and the
	// name below was never found, which skipped the whole file's content.
	//
	// There is no --text: it would make git print a binary's bytes as added
	// lines, and a patch of that size is refused by the capture cap, which
	// turned deleting or renaming a large binary into a refused commit. A text
	// blob's added lines are here; a binary is read from the object store below.
	patch, capNote, err := runGitCapped(ctx, r.deps.Root, maxGitCaptureBytes, "diff", "--cached", "--no-renames",
		"--no-color", "--no-ext-diff", "--no-textconv", "--src-prefix=a/", "--dst-prefix=b/", "-U0")
	if err != nil {
		return nil, nil, fmt.Errorf("reading the staged patch: %w", err)
	}
	patchWhole := capNote == ""
	if patchWhole {
		if err := scanAddedLines(patch, func(path, added string) {
			for value, source := range set.values {
				if strings.Contains(added, value) {
					note(path, "contains a value assigned in "+source)
				}
			}
			if r.deps.KnownSecret != nil && r.deps.KnownSecret(added) {
				note(path, "contains a credential this process holds")
			}
		}); err != nil {
			return nil, nil, err
		}
	}

	// Whole blobs: every added or modified binary and, when the text patch was
	// cut by its cap, every added or modified text blob too — a superset of the
	// `+` lines it lost, so nothing past the cut goes unjudged. Deletions add
	// nothing; with --no-renames the added half of a rename is an addition.
	var items []batchItem
	for _, e := range staged {
		if e.content() && (binary[e.path] || !patchWhole) {
			items = append(items, batchItem{id: e.id, label: e.path})
		}
	}
	if err := r.scanBlobs(ctx, set, items, note); err != nil {
		return nil, nil, err
	}
	if !patchWhole && len(items) > 0 {
		notes = append(notes, fmt.Sprintf("the staged patch is over the %d-byte capture cap (%s), so the staged blobs were scanned whole instead of by their added lines",
			maxGitCaptureBytes, capNote))
	}

	var out []stagedLeak
	for path, reasons := range leaks {
		leak := stagedLeak{Path: path}
		for reason := range reasons {
			leak.Reason = append(leak.Reason, reason)
		}
		sort.Strings(leak.Reason)
		out = append(out, leak)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, notes, nil
}

// scanBlobs searches whole staged blobs for the values the check holds, in
// bounded windows. With nothing to look for there is nothing to read.
func (r *Registry) scanBlobs(ctx context.Context, set secretFileSet, items []batchItem, hit func(path, reason string)) error {
	if len(items) == 0 {
		return nil
	}
	var needles []windowNeedle
	longest := 0
	for value, source := range set.values {
		needles = append(needles, windowNeedle{reason: "contains a value assigned in " + source, bytes: []byte(value)})
		longest = max(longest, len(value))
	}
	if r.deps.KnownSecret == nil && len(needles) == 0 {
		return nil
	}
	if r.deps.KnownSecret != nil {
		known := defaultKnownSecretMaxLen
		if r.deps.KnownSecretMaxLen != nil {
			known = r.deps.KnownSecretMaxLen()
		}
		longest = max(longest, known)
	}
	limits := r.scan.limits.resolved()
	if limits.scanTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limits.scanTimeout)
		defer cancel()
	}
	return r.streamBlobs(ctx, items, func(item batchItem) blobSink {
		return &windowSink{
			ctx: ctx, label: item.label, chunk: limits.chunkBytes, overlap: max(longest-1, 0),
			values: needles, known: r.deps.KnownSecret, hit: hit,
		}
	})
}

// contentRefusal refuses a commit whose staged content is a credential under
// another name. It carries the secret rung's rule id, like aliasedRefusal,
// because that is the rung the rename defeats.
func contentRefusal(leaks []stagedLeak) tools.Result {
	paths := make([]string, 0, len(leaks))
	for _, l := range leaks {
		paths = append(paths, l.Path)
	}
	payload := map[string]any{
		"operation": "commit",
		"paths":     paths,
		"matched":   leaks,
		"rule":      string(policy.RuleSecretPath),
		"severity":  string(policy.Hard),
		"reason": "these staged files carry a credential under a name the secret-path check does not " +
			"match: their content is, or contains, what a secret-path file or a known credential holds. " +
			"Committing them puts it in history. Unstage them, remove the credential, and retry.",
	}
	data, _ := json.Marshal(payload)
	return tools.Result{
		Text:     string(data),
		IsError:  true,
		Blocked:  true,
		Rule:     string(policy.RuleSecretPath),
		Severity: string(policy.Hard),
	}
}

// hunkHeader reads the line counts out of "@@ -a[,b] +c[,d] @@".
var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,(\d+))? \+\d+(?:,(\d+))? @@`)

// scanAddedLines calls visit with every added line of a -U0 patch and the path
// it was added to. It fails closed: an added line it cannot attribute to a path
// is an error, because that is content nothing judged.
//
// Two things in a patch are easy to misread as a file header. A name git
// C-quotes (a quote, a backslash, a control character) arrives as
// `+++ "b/..."`, and an added line whose own text begins "++ " arrives as
// `+++ ...` — so lines inside a hunk are told apart by the counts the hunk
// header promised, never by their spelling.
func scanAddedLines(patch string, visit func(path, added string)) error {
	var path string
	var haveFile bool
	oldLeft, newLeft := 0, 0
	for _, line := range strings.Split(patch, "\n") {
		if oldLeft > 0 || newLeft > 0 {
			switch {
			case strings.HasPrefix(line, "+"):
				newLeft--
				if !haveFile {
					return fmt.Errorf("the staged patch adds a line to a file whose name could not be read, so it was not checked")
				}
				visit(path, line[1:])
			case strings.HasPrefix(line, "-"):
				oldLeft--
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "diff --git "):
			path, haveFile = "", false
		case strings.HasPrefix(line, "+++ "):
			name, ok := unquotePatchName(strings.TrimPrefix(line, "+++ "))
			switch {
			case name == "/dev/null":
				path, haveFile = "", false
			case ok && strings.HasPrefix(name, "b/"):
				path, haveFile = strings.TrimPrefix(name, "b/"), true
			default:
				path, haveFile = "", false
			}
		case strings.HasPrefix(line, "@@"):
			m := hunkHeader.FindStringSubmatch(line)
			if m == nil {
				return fmt.Errorf("the staged patch has a hunk header this check cannot read: %q", line)
			}
			oldLeft, newLeft = hunkCount(m[1]), hunkCount(m[2])
		}
	}
	return nil
}

// hunkCount is a hunk header's line count, which git omits when it is 1.
func hunkCount(raw string) int {
	if raw == "" {
		return 1
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return n
}

// unquotePatchName undoes git's C-style quoting of a header name and the tab it
// appends to a name that contains a space.
func unquotePatchName(raw string) (string, bool) {
	raw = strings.TrimSuffix(raw, "\t")
	if !strings.HasPrefix(raw, `"`) {
		return raw, true
	}
	name, err := strconv.Unquote(raw)
	return name, err == nil
}
