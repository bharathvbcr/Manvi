package devcouncil

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// This file closes the check-then-act gap between policy evaluation and
// filesystem mutation. The ladder judges a path string at T0; a concurrent
// actor — a subagent fan-out, an allowed background command, anything faster
// than a human approval prompt — could previously swap a directory component
// for a symbolic link during the window, and the kernel would happily resolve
// the swapped chain at open time, writing the payload wherever the link
// points.
//
// The defence is identity pinning: at T0 every component's device/inode pair
// is captured alongside the resolved path. At syscall time the opened file
// descriptor's own identity (fstat — immune to later path games) must match
// what T0 recorded, or the write refuses. A swap therefore fails closed
// instead of silently redirecting.
//
// Residual, documented limits: an attacker who can win a race between the T0
// identity capture and the single open() syscall can still forge consistency;
// closing that requires openat2(RESOLVE_BENEATH)-class semantics, which the
// Go standard library does not expose portably (darwin has no exported
// Openat at all). The practical window shrinks from "as long as a human
// approval dialog is open" to microseconds.

// componentIdentity is the filesystem identity of one path component.
type componentIdentity struct {
	dev, ino uint64
}

func identityOf(fi fs.FileInfo) (componentIdentity, bool) {
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return componentIdentity{}, false
	}
	return componentIdentity{uint64(sys.Dev), uint64(sys.Ino)}, true
}

// symlinkRefusal reports that traversal hit a symbolic link where the pinned
// expectation required the literal directory or file.
type symlinkRefusal struct {
	component string
	during    string
}

func (e *symlinkRefusal) Error() string {
	return fmt.Sprintf(
		"refusing to %s through %q: it is a symbolic link, and writing through links is how a "+
			"contained path becomes an uncontained one", e.during, e.component)
}

// pinnedTarget is the T0 snapshot of where a relative path is supposed to
// land, taken while policy holds its view of the world.
type pinnedTarget struct {
	root string
	rel  string
	// physical is the resolved absolute path all syscalls go through.
	physical string
	// resolvedDirs[i] and dirIDs[i] pair each directory along the verified
	// chain (root first) with its T0 identity. Components past firstMissing
	// did not exist at pin time and are created only at write time.
	resolvedDirs []string
	dirIDs       []componentIdentity
	firstMissing int
	// targetExisted records whether the leaf existed at pin time; if it did,
	// its identity must still match when we open it.
	targetExisted bool
	targetIdent   componentIdentity
}

// pinWriteTarget resolves rel under root the way the policy layer understood
// it — following any symlinks once, here, under containment review — and
// snapshots the identities the later write will be held to. Call it BEFORE
// escalation prompts, so the clock the attacker races against is not the
// human one.
//
// Components that do not exist yet are tolerated: writeFile legitimately
// creates directories, so the snapshot records how far the existing chain
// reaches and Write creates the missing tail only after re-verifying every
// recorded ancestor.
func pinWriteTarget(root, rel string) (*pinnedTarget, error) {
	clean := path.Clean(filepath.ToSlash(rel))
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || clean == "." {
		return nil, fmt.Errorf("path %q is not a contained repository-relative path", rel)
	}

	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolving repository root: %w", err)
	}

	parts := strings.Split(clean, "/")
	pinned := &pinnedTarget{
		root:         rootResolved,
		rel:          clean,
		resolvedDirs: []string{rootResolved},
	}
	rootFi, err := os.Stat(rootResolved)
	if err != nil {
		return nil, fmt.Errorf("inspecting repository root: %w", err)
	}
	rootID, ok := identityOf(rootFi)
	if !ok {
		return nil, fmt.Errorf("platform cannot identify directories; refusing to write %q", rel)
	}
	pinned.dirIDs = append(pinned.dirIDs, rootID)

	// Walk the directory components that exist, resolving symlinks
	// deliberately and checking containment of every intermediate result, so
	// a link planted mid-chain cannot carry us out even before the leaf.
	current := rootResolved
	firstMissing := len(parts) - 1
	for i, part := range parts[:len(parts)-1] {
		next := filepath.Join(current, part)
		resolved, err := filepath.EvalSymlinks(next)
		if err != nil {
			if os.IsNotExist(err) {
				firstMissing = i
				break
			}
			return nil, fmt.Errorf("resolving %q: %w", next, err)
		}
		if !containedUnder(rootResolved, resolved) {
			return nil, fmt.Errorf("directory component %q resolves outside the repository", part)
		}
		fi, err := os.Stat(resolved)
		if err != nil {
			return nil, fmt.Errorf("inspecting %q: %w", resolved, err)
		}
		id, ok := identityOf(fi)
		if !ok || !fi.IsDir() {
			return nil, fmt.Errorf("path component %q of %q is not a directory", part, rel)
		}
		pinned.resolvedDirs = append(pinned.resolvedDirs, resolved)
		pinned.dirIDs = append(pinned.dirIDs, id)
		current = resolved
	}

	// Physical location: the verified chain plus whatever was missing.
	tail := parts[firstMissing:]
	pinned.firstMissing = firstMissing
	pinned.physical = filepath.Join(append([]string{current}, tail...)...)

	if firstMissing == len(parts)-1 {
		// The leaf's own directory exists; the leaf itself must never be a
		// symlink. (Distinct names, not re-used err: a shadowed error once
		// made every missing file look like an existing one.)
		leafFi, leafErr := os.Lstat(pinned.physical)
		if leafErr != nil && !os.IsNotExist(leafErr) {
			return nil, fmt.Errorf("inspecting %q: %w", pinned.physical, leafErr)
		}
		if leafErr == nil && leafFi.Mode()&fs.ModeSymlink != 0 {
			return nil, &symlinkRefusal{component: parts[len(parts)-1], during: "write"}
		}
		if leafErr == nil {
			pinned.targetExisted = true
			id, ok := identityOf(leafFi)
			if !ok {
				return nil, fmt.Errorf("platform cannot identify files; refusing to write %q", rel)
			}
			pinned.targetIdent = id
		}
	}
	return pinned, nil
}

// Write stores data at the pinned location, refusing unless every identity
// recorded at pin time still holds at syscall time.
func (p *pinnedTarget) Write(data []byte, perm fs.FileMode) error {
	// Re-verify every ancestor that existed at pin time: the kernel is about
	// to traverse them again, and this is the last chance to catch a swap.
	if err := p.verifyChain(); err != nil {
		return err
	}
	// Create the tail that was missing at pin time — under the verified
	// prefix, after its identity has just been confirmed.
	//
	// Every segment is inspected after the Mkdir, and that inspection is the
	// point of this loop rather than an extra precaution. os.Mkdir reports
	// EEXIST for a path that is already a *symlink*, and treating EEXIST as
	// "fine, it is there" walked straight through one: a component planted
	// between the pin and the write became the parent of the file, and
	// O_NOFOLLOW below guards only the leaf. Nothing else covered it either —
	// when the very first component is missing, resolvedDirs holds only the
	// root, so verifyChain re-verifies the root and nothing between it and the
	// target. The write landed wherever the link pointed, and writeFile pins
	// *before* the policy ladder and before the blocking human approval, so
	// the window was the whole approval dialog rather than the microseconds
	// this file's header claims.
	if p.firstMissing < len(strings.Split(p.rel, "/"))-1 {
		base := p.resolvedDirs[len(p.resolvedDirs)-1]
		for _, seg := range strings.Split(p.rel, "/")[p.firstMissing : len(strings.Split(p.rel, "/"))-1] {
			base = filepath.Join(base, seg)
			if err := os.Mkdir(base, 0o755); err != nil && !os.IsExist(err) {
				return fmt.Errorf("creating directory for %q: %w", p.rel, err)
			}
			// Lstat, not Stat: Stat follows the link and would report the
			// directory at the far end as though the component itself were one.
			fi, err := os.Lstat(base)
			if err != nil {
				return fmt.Errorf("inspecting created directory %q: %w", base, err)
			}
			if fi.Mode()&fs.ModeSymlink != 0 {
				return &symlinkRefusal{component: seg, during: "write"}
			}
			if !fi.IsDir() {
				return fmt.Errorf("refusing to write %q: path component %q is not a directory", p.rel, seg)
			}
			if !containedUnder(p.root, base) {
				return fmt.Errorf("refusing to write %q: directory component %q resolves outside the repository",
					p.rel, seg)
			}
			id, ok := identityOf(fi)
			if !ok {
				return fmt.Errorf("platform cannot identify directories; refusing to write %q", p.rel)
			}
			// Recorded so the create-case verifyChain after the open covers the
			// segments this loop just made, not merely the prefix that existed
			// when the target was pinned.
			p.resolvedDirs = append(p.resolvedDirs, base)
			p.dirIDs = append(p.dirIDs, id)
		}
	}

	fd, err := syscall.Open(p.physical,
		syscall.O_WRONLY|syscall.O_CREAT|syscall.O_TRUNC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC,
		uint32(perm))
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return &symlinkRefusal{component: p.rel, during: "write"}
		}
		return fmt.Errorf("opening %q: %w", p.rel, err)
	}
	f := os.NewFile(uintptr(fd), p.physical)
	defer f.Close()

	// What did we actually just open? fstat reads the descriptor, not the
	// path, so a concurrent rename cannot confuse it.
	opened, err := f.Stat()
	if err != nil {
		return fmt.Errorf("inspecting opened %q: %w", p.rel, err)
	}
	if !opened.Mode().IsRegular() {
		return fmt.Errorf("refusing to write %q: not a regular file", p.rel)
	}
	openedID, ok := identityOf(opened)
	if !ok {
		return fmt.Errorf("platform cannot identify files; refusing to write %q", p.rel)
	}

	if p.targetExisted {
		if openedID != p.targetIdent {
			return fmt.Errorf(
				"refusing to write %q: the file at that path changed identity between "+
					"policy evaluation and open (was inode %d, opened inode %d)",
				p.rel, p.targetIdent.ino, openedID.ino)
		}
	} else {
		// Create case: the file did not exist at pin time, so the thing we
		// opened must have been created by our own O_CREAT at the verified
		// location. Its directory must still be the pinned one.
		if err := p.verifyChain(); err != nil {
			return err
		}
	}

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("writing %q: %w", p.rel, err)
	}
	// Best-effort durability: the content is in the page cache either way,
	// but a crash immediately after a reported success should not leave a
	// zero-length file behind.
	_ = f.Sync()
	return nil
}

// verifyChain re-stats every recorded directory along the resolved chain and
// requires each to still be the very directory captured at pin time.
func (p *pinnedTarget) verifyChain() error {
	for i, dir := range p.resolvedDirs {
		fi, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("directory %q of %q vanished before the write: %w", dir, p.rel, err)
		}
		id, ok := identityOf(fi)
		if !ok {
			return fmt.Errorf("platform cannot identify directories; refusing to use %q", dir)
		}
		if id != p.dirIDs[i] {
			return fmt.Errorf(
				"refusing to use %q: directory component %q changed identity between policy "+
					"evaluation and open (possible symlink swap; was inode %d, found inode %d)",
				p.rel, dir, p.dirIDs[i].ino, id.ino)
		}
	}
	return nil
}

// verifyDirectory is the single-parent form kept for the leaf-only checks.
func (p *pinnedTarget) verifyDirectory() error {
	parent := filepath.Dir(p.physical)
	fi, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("the directory of %q vanished before the write: %w", p.rel, err)
	}
	id, ok := identityOf(fi)
	if !ok {
		return fmt.Errorf("platform cannot identify directories; refusing to write %q", p.rel)
	}
	last := len(p.dirIDs) - 1
	if id != p.dirIDs[last] {
		return fmt.Errorf(
			"refusing to write %q: its directory changed identity between policy evaluation "+
				"and open (possible symlink swap; was inode %d, found inode %d)",
			p.rel, p.dirIDs[last].ino, id.ino)
	}
	return nil
}

// Read reads the whole file at the pinned location through the same identity
// verification as Write: reading through a swapped directory is an
// exfiltration channel, not merely a wrong result. A limit of 0 means
// unlimited.
func (p *pinnedTarget) Read(limit int64) ([]byte, error) {
	if !p.targetExisted {
		return nil, fmt.Errorf("file %s does not exist", p.rel)
	}
	if err := p.verifyChain(); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(p.physical, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, &symlinkRefusal{component: p.rel, during: "read"}
		}
		return nil, fmt.Errorf("opening %q: %w", p.rel, err)
	}
	f := os.NewFile(uintptr(fd), p.physical)
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspecting opened %q: %w", p.rel, err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing to read %q: not a regular file", p.rel)
	}
	openedID, ok := identityOf(st)
	if !ok {
		return nil, fmt.Errorf("platform cannot identify files; refusing to read %q", p.rel)
	}
	if openedID != p.targetIdent {
		return nil, fmt.Errorf(
			"refusing to read %q: the file changed identity between evaluation and open "+
				"(was inode %d, opened inode %d)", p.rel, p.targetIdent.ino, openedID.ino)
	}
	if limit > 0 && st.Size() > limit {
		return nil, fmt.Errorf("refusing to read %q: %d bytes exceeds the %d-byte limit", p.rel, st.Size(), limit)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", p.rel, err)
	}
	return data, nil
}

// ReadPinned pins and then reads in one step, for callers that have no
// deliberation window between evaluation and read.
func ReadPinned(root, rel string, limit int64) ([]byte, error) {
	pinned, err := pinWriteTarget(root, rel)
	if err != nil {
		return nil, err
	}
	return pinned.Read(limit)
}

// RemovePinned deletes the leaf at a pinned location. Unlink never follows a
// final-component symlink, so the leaf itself cannot redirect the operation;
// the directory identity check closes the swapped-parent variant.
func RemovePinned(root, rel string) error {
	pinned, err := pinWriteTarget(root, rel)
	if err != nil {
		return err
	}
	if err := pinned.verifyChain(); err != nil {
		return err
	}
	if err := syscall.Unlink(pinned.physical); err != nil {
		if errors.Is(err, syscall.EISDIR) || errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("refusing to remove %q: it is a directory", rel)
		}
		return fmt.Errorf("removing %q: %w", rel, err)
	}
	return nil
}

// containedUnder reports whether child is root itself or lies beneath it.
func containedUnder(root, child string) bool {
	rel, err := filepath.Rel(root, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// io_ReadAll exists only to keep this file's imports minimal; io.ReadAll is
// the same function.
func io_ReadAll(f *os.File) ([]byte, error) {
	var out []byte
	buf := make([]byte, 32*1024)
	for {
		n, err := f.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			if err.Error() == "EOF" {
				return out, nil
			}
			return out, err
		}
	}
}

// syscall_Mkfifo builds a fifo for the non-regular-file refusal test. Mkfifo
// is not in the portable syscall surface, so it shells out to the platform's
// tool; the test skips when that is unavailable.
func syscall_Mkfifo(path string, mode uint32) error {
	return runMkfifo(path, mode)
}
