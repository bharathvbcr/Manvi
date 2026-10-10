package devcouncil

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
//   - its object id is the id of a file the working tree holds under a secret
//     path (an exact copy, whatever it is now called),
//   - an added line contains a value assigned in one of those files (an edited
//     copy), or
//   - an added line contains a credential this process knows (a provider key, a
//     forwarded token — Deps.KnownSecret).
//
// It fails closed: anything that cannot be read, listed or hashed is a refusal
// that says the check did not run, never a clean pass.

const (
	// maxSecretFileBytes bounds one secret-path file read for its values.
	maxSecretFileBytes = 256 << 10
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

// secretFileSet lists the files the working tree holds under a secret path,
// with their git blob ids and the values they assign.
type secretFileSet struct {
	files  []string
	blobs  map[string]string // object id -> path
	values map[string]string // value -> path
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
	for _, p := range secret {
		info, err := os.Lstat(filepath.Join(r.deps.Root, filepath.FromSlash(p)))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		set.files = append(set.files, p)
	}
	if len(set.files) == 0 {
		return set, nil
	}

	args := append([]string{"hash-object", "--"}, set.files...)
	out, err := runGit(ctx, r.deps.Root, args...)
	if err != nil {
		return set, fmt.Errorf("hashing secret-path files: %w", err)
	}
	ids := strings.Fields(out)
	if len(ids) != len(set.files) {
		return set, fmt.Errorf("hash-object answered %d ids for %d files", len(ids), len(set.files))
	}
	for i, id := range ids {
		set.blobs[id] = set.files[i]
	}

	for _, p := range set.files {
		info, err := os.Stat(filepath.Join(r.deps.Root, filepath.FromSlash(p)))
		if err != nil || info.Size() > maxSecretFileBytes {
			continue
		}
		data, err := os.ReadFile(filepath.Join(r.deps.Root, filepath.FromSlash(p)))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			m := assignedValue.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			value := strings.Trim(m[2], `"'`)
			if looksCredential(m[1], value) {
				set.values[value] = p
			}
		}
	}
	return set, nil
}

// stagedContentLeaks judges what is staged by its content. A nil result with a
// nil error means the check ran and found nothing.
func (r *Registry) stagedContentLeaks(ctx context.Context) ([]stagedLeak, error) {
	set, err := r.collectSecretFiles(ctx)
	if err != nil {
		return nil, err
	}

	leaks := map[string]map[string]bool{}
	note := func(path, reason string) {
		if leaks[path] == nil {
			leaks[path] = map[string]bool{}
		}
		leaks[path][reason] = true
	}

	raw, err := runGit(ctx, r.deps.Root, "diff", "--cached", "--raw", "-z", "--no-renames", "--no-abbrev")
	if err != nil {
		return nil, fmt.Errorf("reading the staged blobs: %w", err)
	}
	// ":<mode> <mode> <old> <new> <status>\0<path>\0" repeated.
	fields := strings.Split(raw, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(meta) < 5 {
			continue
		}
		if source, ok := set.blobs[meta[3]]; ok {
			note(fields[i+1], "identical to "+source)
		}
	}

	patch, err := runGit(ctx, r.deps.Root, "diff", "--cached", "--no-renames", "--no-color", "-U0")
	if err != nil {
		return nil, fmt.Errorf("reading the staged patch: %w", err)
	}
	var current string
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ b/"):
			current = strings.TrimSuffix(strings.TrimPrefix(line, "+++ b/"), "\t")
		case strings.HasPrefix(line, "+++ "):
			current = ""
		case strings.HasPrefix(line, "+") && current != "":
			added := line[1:]
			for value, source := range set.values {
				if strings.Contains(added, value) {
					note(current, "contains a value assigned in "+source)
				}
			}
			if r.deps.KnownSecret != nil && r.deps.KnownSecret(added) {
				note(current, "contains a credential this process holds")
			}
		}
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
	return out, nil
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
