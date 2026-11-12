package devcouncil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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
// or `foo/bar` itself, and `git update-ref` creates a root name for it.
//
// What is read: every regular file (or symlink to one) below the git directory
// of the root, below the common git directory when the root is a linked worktree
// (that is `main-worktree/NAME`), and below every other linked worktree's admin
// directory (`worktrees/<id>/NAME`), except the trees listed in rootFileSkipDirs.
// A symlink to a directory is followed (`.git/hooks -> ../.githooks` is common
// and `lnk/x` resolves through a link), with a cycle check and the bounds
// maxRootWalkFiles and maxRootWalkDepth; a link to one of the skipped trees is
// the same residual as the tree. A file reached through a link is never
// rewritten or removed by a rollback (the target is the user's), only reported.
//
// How a file is read is how git reads a loose ref (parse_loose_ref_contents): an
// object id of the repository's own length (40 hex digits for sha1, 64 for
// sha256) at the start of the file, followed by the end of the file, a NUL or
// white space. Each line of such a file that begins that way is one entry, so
// every parent of a multi-line MERGE_HEAD and every tip of a FETCH_HEAD is
// judged. A file that does not begin that way (config, index, COMMIT_EDITMSG, a
// symbolic `ref: ` file, a hook, a sha256 sum in a sha1 repository) is not a ref
// source for git and is not one here; a symbolic root file names a ref that the
// snapshot already holds. A file that is ref-shaped but cannot be read in full
// (it is larger than maxRootFileBytes, or cannot be opened) is an error: the
// command is not run, and one that appears during a command is reported
// unscanned. An entry whose object does not exist is recorded as absent: it
// cannot be pushed, and it is judged when the object appears (markAbsent).
//
// Not read: files below objects/, logs/ and modules/ of any git directory. git
// resolves `objects/x` and `logs/x` (`git push <remote> logs/x:refs/heads/z`
// delivers), but the first is the object store and the second the reflogs, which
// hold one file per ref and can be large; walking them on every command is not
// a bounded cost. A name that git's own ref-name rules refuse is not resolved by
// git either and is not read.
const (
	maxRootFileBytes = 1 << 20
	// maxRootWalkFiles bounds the files looked at below one git directory, and
	// maxRootWalkDepth the nesting of its directories.
	maxRootWalkFiles = 200000
	maxRootWalkDepth = 32
	// absentMark follows the object id of an entry whose object does not exist.
	absentMark = "?absent"
)

// rootFileSkipDirs are the top-level directories of a git directory that are not
// walked. refs/ is read by for-each-ref, worktrees/ through its admin directories.
var rootFileSkipDirs = map[string]bool{"objects": true, "refs": true, "worktrees": true, "modules": true, "logs": true}

// fileKeyPrefix names a snapshot key that is a file's path; the object ids of
// its second and later lines carry "#<line>".
const fileKeyPrefix = "file:"

// rootFile is one file git resolves as a ref: the object id that starts each of
// its lines that has one, and its bytes.
type rootFile struct {
	path    string
	oids    []string
	raw     []byte
	viaLink bool
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

// entryOID is the object id of a snapshot value, without the absent mark.
func entryOID(v string) string { return strings.TrimSuffix(v, absentMark) }

func isHexString(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// leadingOID is the object id a line begins with: hashLen hex digits followed by
// the end of the line, a NUL or white space, as git reads a loose ref. The null
// id names nothing and is not one.
func leadingOID(line string, hashLen int) (string, bool) {
	if len(line) < hashLen || !isHexString(line[:hashLen]) {
		return "", false
	}
	if len(line) > hashLen {
		switch line[hashLen] {
		case 0, ' ', '\t', '\n', '\v', '\f', '\r':
		default:
			return "", false
		}
	}
	if strings.Trim(line[:hashLen], "0") == "" {
		return "", false
	}
	return strings.ToLower(line[:hashLen]), true
}

func parseRootFile(content string, hashLen int) []string {
	var oids []string
	for i, line := range strings.Split(content, "\n") {
		oid, ok := leadingOID(line, hashLen)
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

// readRootFile returns the object ids and bytes of a file git would resolve as a
// ref, nil when it is not one, and an error when it looks like one and cannot be
// read in full.
func readRootFile(path string, hashLen int) ([]string, []byte, error) {
	f, err := os.Open(path) // #nosec G304 -- a file below a git directory git itself reported.
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil {
		return nil, nil, err
	} else if !fi.Mode().IsRegular() {
		return nil, nil, nil // a FIFO or device would block or lie.
	}
	head := make([]byte, 2*hashLen)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, nil, err
	}
	first, _, _ := strings.Cut(string(head[:n]), "\n")
	if _, ok := leadingOID(first, hashLen); !ok {
		return nil, nil, nil
	}
	rest, err := io.ReadAll(io.LimitReader(f, maxRootFileBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if n+len(rest) > maxRootFileBytes {
		return nil, nil, fmt.Errorf("%s is a ref source larger than %d bytes", path, maxRootFileBytes)
	}
	raw := append(append([]byte{}, head[:n]...), rest...)
	return parseRootFile(string(raw), hashLen), raw, nil
}

// rootWalk is one walk of the files below a git directory.
type rootWalk struct {
	top     string
	hashLen int
	seen    map[string]bool // real paths of the directories entered through a link
	skip    []string        // real paths of the trees that are not read
	count   int
	out     []rootFile
}

func (w *rootWalk) skipped(real string) bool {
	for _, s := range w.skip {
		if real == s || strings.HasPrefix(real, s+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func (w *rootWalk) dir(dir string, depth int, viaLink bool) error {
	if depth > maxRootWalkDepth {
		return fmt.Errorf("%s is nested deeper than %d directories", dir, maxRootWalkDepth)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && dir != w.top {
			return nil
		}
		return err
	}
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		atTop := dir == w.top
		if w.count++; w.count > maxRootWalkFiles {
			return fmt.Errorf("more than %d entries below the git directory", maxRootWalkFiles)
		}
		switch {
		case e.IsDir():
			if atTop && rootFileSkipDirs[e.Name()] {
				continue
			}
			if err := w.dir(p, depth+1, viaLink); err != nil {
				return err
			}
		case e.Type()&fs.ModeSymlink != 0:
			fi, err := os.Stat(p)
			if errors.Is(err, fs.ErrNotExist) {
				continue // a dangling link resolves to nothing.
			}
			if err != nil {
				return err
			}
			switch {
			case fi.IsDir():
				real, err := filepath.EvalSymlinks(p)
				if err != nil {
					return err
				}
				if w.seen[real] || w.skipped(real) {
					continue
				}
				w.seen[real] = true
				if err := w.dir(p, depth+1, true); err != nil {
					return err
				}
			case fi.Mode().IsRegular():
				if err := w.file(p, true); err != nil {
					return err
				}
			}
		case e.Type().IsRegular():
			if atTop && e.Name() == "HEAD" {
				continue // each worktree's HEAD is read through `worktree list`.
			}
			if err := w.file(p, viaLink); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *rootWalk) file(p string, viaLink bool) error {
	oids, raw, err := readRootFile(p, w.hashLen)
	if err != nil {
		return err
	}
	if len(oids) > 0 {
		w.out = append(w.out, rootFile{path: p, oids: oids, raw: raw, viaLink: viaLink})
	}
	return nil
}

// walkRootFiles reads every ref-source file below a git directory (see the
// comment at the top of this file for what is skipped and bounded).
func walkRootFiles(dir string, hashLen int) ([]rootFile, error) {
	w := &rootWalk{top: dir, hashLen: hashLen, seen: map[string]bool{realPath(dir): true}}
	for name := range rootFileSkipDirs {
		w.skip = append(w.skip, realPath(filepath.Join(dir, name)))
	}
	if err := w.dir(dir, 0, false); err != nil {
		return nil, fmt.Errorf("reading the ref-source files in %s: %w", dir, err)
	}
	return w.out, nil
}

// addRootFiles puts each entry of each file into the snapshot.
func addRootFiles(snap *refSnapshot, files []rootFile) {
	for _, f := range files {
		snap.files[f.path] = f.oids
		snap.raw[f.path] = f.raw
		if f.viaLink {
			snap.viaLink[f.path] = true
		}
		for i, oid := range f.oids {
			snap.refs[fileKey(f.path, i)] = oid
		}
	}
}

// markAbsent marks each file entry whose object does not exist, so that it is a
// different value from the same entry once the object does: a command that
// writes the object later (hash-object -w) moves it. An id an ordinary ref
// already names exists; a loose object is found by its path; only the rest goes
// to one `git cat-file --batch-check`.
func (r *Registry) markAbsent(ctx context.Context, snap *refSnapshot, common string) error {
	known := map[string]bool{}
	for k, oid := range snap.refs {
		if filePathOfKey(k) == "" {
			known[oid] = true
		}
	}
	var ask []string
	asked := map[string]bool{}
	for k, oid := range snap.refs {
		if filePathOfKey(k) == "" || known[oid] || asked[oid] {
			continue
		}
		asked[oid] = true
		if _, err := os.Lstat(filepath.Join(common, "objects", oid[:2], oid[2:])); err == nil {
			known[oid] = true
			continue
		}
		ask = append(ask, oid)
	}
	if len(ask) == 0 {
		return nil
	}
	sort.Strings(ask)
	out, err := runGitStdin(ctx, r.deps.Root, strings.Join(ask, "\n")+"\n", "cat-file", "--batch-check")
	if err != nil {
		return fmt.Errorf("checking which objects the ref-source files name exist: %w", err)
	}
	missing := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if oid, ok := strings.CutSuffix(line, " missing"); ok {
			missing[oid] = true
		}
	}
	for k, oid := range snap.refs {
		if filePathOfKey(k) != "" && missing[oid] {
			snap.refs[k] = oid + absentMark
		}
	}
	return nil
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
// now: a file that did not exist before the command is removed, one that did is
// rewritten with the exact bytes it held (a FETCH_HEAD keeps its not-for-merge
// markers). `git update-ref` is not used because git refuses to update
// FETCH_HEAD and MERGE_HEAD by name, refuses most other names, and mishandles
// symbolic root files; the effect is the same delete or update the other refs
// get. A file reached through a symlink, or that is one, is never replaced or
// written through: it is reported. done holds the files already settled in this
// judgement, so that a file with several leaking lines is restored once.
func restoreRootFile(before, after *refSnapshot, key string, g *guardedRef, v *refViolation, done map[string]bool) {
	path := filePathOfKey(key)
	if done[path] {
		g.Restored = true
		return
	}
	if after.raw == nil {
		after.raw = map[string][]byte{}
	}
	refuse := func(why string) {
		g.Problem = why
		v.manual = true
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&fs.ModeSymlink != 0 || after.viaLink[path] {
		refuse("the file is, or is reached through, a symbolic link: it is not replaced or written through, and the leak is still reachable from it")
		return
	}
	hashLen := 0
	if len(after.files[path]) > 0 {
		hashLen = len(after.files[path][0])
	}
	cur, curRaw, err := readRootFile(path, hashLen)
	if err != nil {
		refuse(fmt.Sprintf("the file could not be read to put it back (%v)", err))
		return
	}
	unchanged := sameOIDs(cur, after.files[path])
	if raw, ok := after.raw[path]; ok {
		unchanged = unchanged && bytes.Equal(curRaw, raw)
	}
	if !unchanged {
		refuse("the file changed since it was read; it may have moved since")
		return
	}
	was, wasRaw := before.files[path], before.raw[path]
	if wasRaw != nil && bytes.Equal(wasRaw, curRaw) {
		// The file is what it was; the object it names is what arrived. Putting the
		// file back changes nothing, so the leak stays reachable and is reported.
		refuse("the file held this id before the command and still does: the object it names arrived, so restoring the file removes nothing")
		return
	}
	switch {
	case len(was) == 0:
		err = os.Remove(path) // #nosec G122 -- a file below a git directory, compared with what was scanned just above.
	case wasRaw != nil:
		err = writeRootFile(path, wasRaw)
	case len(was) == 1:
		err = writeRootFile(path, []byte(was[0]+"\n"))
	default:
		refuse("a file that held several object ids before the command cannot be put back without its bytes")
		return
	}
	if err != nil {
		refuse(fmt.Sprintf("the file could not be restored (%v)", err))
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
	delete(after.raw, path)
	if len(was) > 0 {
		after.files[path] = was
		after.raw[path] = wasRaw
		for i := range was {
			after.refs[fileKey(path, i)] = before.refs[fileKey(path, i)]
		}
	}
}

// writeRootFile replaces a file's content with the same mode, through a rename.
func writeRootFile(path string, content []byte) error {
	mode := fs.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp := path + ".guard-tmp"
	if err := os.WriteFile(tmp, content, mode); err != nil { // #nosec G703 -- a file below a git directory.
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
