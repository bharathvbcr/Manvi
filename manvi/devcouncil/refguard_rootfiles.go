package devcouncil

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Ref sources that are plain files in a git directory.
//
// git resolves a name by trying, among other places, the file of that name in
// the git directory. Whatever sits there with an object id as the first word of
// its first line is a ref to it: `git rev-parse` reads it and `git push
// <remote> NAME:refs/heads/x` sends it. That is not limited to the names git
// itself keeps (ORIG_HEAD, FETCH_HEAD, MERGE_HEAD, CHERRY_PICK_HEAD, REVERT_HEAD,
// BISECT_HEAD, AUTO_MERGE, REBASE_HEAD): a command can create `MYLEAK_HEAD`, `x`
// or `foo/bar` itself, and `git update-ref` creates a root name for it. The
// matrix is in the card's report; this file is the guard's answer to it.
//
// What is read: every regular file (or symlink to one) below the git directory
// of the root, below the common git directory when the root is a linked worktree
// (that is `main-worktree/NAME`), and below every other linked worktree's admin
// directory (`worktrees/<id>/NAME`), except the trees listed in rootFileSkipDirs.
// Each line of such a file that begins with an object id is one entry, so every
// parent of a multi-line MERGE_HEAD and every tip of a FETCH_HEAD is judged. A
// file whose first line does not begin with an object id (config, index,
// COMMIT_EDITMSG, a symbolic `ref: ` file, a hook) is not a ref source for git
// and is not one here; a symbolic root file names a ref that the snapshot already
// holds. A file that is ref-shaped but cannot be read in full (it is larger than
// maxRootFileBytes, or cannot be opened) is an error: the command is not run, and
// one that appears during a command is reported unscanned.
//
// Not read, and so still open: files below objects/, logs/ and modules/ of any
// git directory. git resolves `objects/x` and `logs/x` (`git push <remote>
// logs/x:refs/heads/z` delivers), but the first is the object store and the
// second the reflogs, which hold one file per ref and can be large; walking them
// on every command is not a bounded cost. A name that git's own ref-name rules
// refuse is not resolved by git either and is not read.
const maxRootFileBytes = 1 << 20

// rootFileSkipDirs are the top-level directories of a git directory that are not
// walked. refs/ is read by for-each-ref, worktrees/ through its admin directories.
var rootFileSkipDirs = map[string]bool{"objects": true, "refs": true, "worktrees": true, "modules": true, "logs": true}

// fileKeyPrefix names a snapshot key that is a file's path; the object ids of
// its second and later lines carry "#<line>".
const fileKeyPrefix = "file:"

// rootFile is one file git resolves as a ref, with the object id that starts
// each of its lines that has one.
type rootFile struct {
	path string
	oids []string
}

func fileKey(path string, i int) string {
	if i == 0 {
		return fileKeyPrefix + path
	}
	return fileKeyPrefix + path + "#" + strconv.Itoa(i+1)
}

// filePathOfKey is the file a snapshot key names, or "" when it names a ref.
func filePathOfKey(key string) string {
	rest, ok := strings.CutPrefix(key, fileKeyPrefix)
	if !ok {
		return ""
	}
	if i := strings.LastIndex(rest, "#"); i >= 0 {
		if _, err := strconv.Atoi(rest[i+1:]); err == nil {
			return rest[:i]
		}
	}
	return rest
}

func isHexString(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// leadingOID is the object id a line begins with: 40 or 64 hex digits followed
// by the end of the line or white space, as git reads a loose ref. The null id
// names nothing and is not one.
func leadingOID(line string) (string, bool) {
	for _, n := range []int{40, 64} {
		if len(line) < n || !isHexString(line[:n]) {
			continue
		}
		if len(line) > n && line[n] != ' ' && line[n] != '\t' && line[n] != '\r' {
			continue
		}
		if strings.Trim(line[:n], "0") == "" {
			return "", false
		}
		return strings.ToLower(line[:n]), true
	}
	return "", false
}

func parseRootFile(content string) []string {
	var oids []string
	for i, line := range strings.Split(content, "\n") {
		oid, ok := leadingOID(line)
		if !ok {
			if i == 0 {
				return nil
			}
			continue
		}
		oids = append(oids, oid)
	}
	return oids
}

// readRootFile returns the object ids of a file git would resolve as a ref, nil
// when it is not one, and an error when it looks like one and cannot be read.
func readRootFile(path string, d fs.DirEntry) ([]string, error) {
	if d != nil && d.Type()&fs.ModeSymlink != 0 {
		fi, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if !fi.Mode().IsRegular() {
			return nil, nil
		}
	} else if d != nil && !d.Type().IsRegular() {
		return nil, nil
	}
	f, err := os.Open(path) // #nosec G304 -- a file below a git directory git itself reported.
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 128)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	first, _, _ := strings.Cut(string(head[:n]), "\n")
	if _, ok := leadingOID(first); !ok {
		return nil, nil
	}
	rest, err := io.ReadAll(io.LimitReader(f, maxRootFileBytes+1))
	if err != nil {
		return nil, err
	}
	if n+len(rest) > maxRootFileBytes {
		return nil, fmt.Errorf("%s is a ref source larger than %d bytes", path, maxRootFileBytes)
	}
	return parseRootFile(string(head[:n]) + string(rest)), nil
}

// walkRootFiles reads every ref-source file below a git directory (see the
// package comment above for what is skipped).
func walkRootFiles(dir string) ([]rootFile, error) {
	var out []rootFile
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p != dir {
				return nil
			}
			return err
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		if d.IsDir() {
			if rootFileSkipDirs[rel] {
				return fs.SkipDir
			}
			return nil
		}
		if rel == "HEAD" {
			return nil // each worktree's HEAD is read through `worktree list`.
		}
		oids, rerr := readRootFile(p, d)
		if rerr != nil {
			return rerr
		}
		if len(oids) > 0 {
			out = append(out, rootFile{path: p, oids: oids})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading the ref-source files in %s: %w", dir, err)
	}
	return out, nil
}

// addRootFiles puts each entry of each file into the snapshot.
func addRootFiles(snap *refSnapshot, files []rootFile) {
	for _, f := range files {
		snap.files[f.path] = f.oids
		for i, oid := range f.oids {
			snap.refs[fileKey(f.path, i)] = oid
		}
	}
}

func sameOIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// restoreRootFile puts a leaking file back, compare-and-swap on what it holds
// now: a file that did not exist before the command is removed, a single-id file
// that did is rewritten with its old id, and a multi-line file that did exist is
// reported (it cannot be reproduced from the ids alone). `git update-ref` is not
// used because git refuses to update FETCH_HEAD and MERGE_HEAD by name, and
// refuses to name most other files at all; it is the same effect as the delete
// or update the other refs get. done holds the files already settled in this
// judgement, so that a file with several leaking lines is restored once.
func restoreRootFile(before, after *refSnapshot, key string, g *guardedRef, v *refViolation, done map[string]bool) {
	path := filePathOfKey(key)
	if done[path] {
		g.Restored = true
		return
	}
	cur, err := readRootFile(path, nil)
	if err != nil || !sameOIDs(cur, after.files[path]) {
		g.Problem = fmt.Sprintf("the file could not be restored (it changed since it was read, or cannot be read: %v)", err)
		v.manual = true
		return
	}
	was := before.files[path]
	switch len(was) {
	case 0:
		err = os.Remove(path) // #nosec G122 -- a file below a git directory, compared with what was scanned just above.
	case 1:
		err = writeRootFile(path, was[0]+"\n")
	default:
		g.Problem = "a file that held several object ids before the command cannot be put back from the ids alone"
		v.manual = true
		return
	}
	if err != nil {
		g.Problem = fmt.Sprintf("the file could not be restored (%v)", err)
		v.manual = true
		return
	}
	done[path] = true
	g.Restored = true
	for k := range after.refs {
		if filePathOfKey(k) == path {
			delete(after.refs, k)
		}
	}
	delete(after.files, path)
	if len(was) > 0 {
		after.files[path] = was
		for i, oid := range was {
			after.refs[fileKey(path, i)] = oid
		}
	}
}

// writeRootFile replaces a file's content with the same mode, through a rename.
func writeRootFile(path, content string) error {
	mode := fs.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp := path + ".guard-tmp"
	if err := os.WriteFile(tmp, []byte(content), mode); err != nil { // #nosec G703 -- a file below a git directory.
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
