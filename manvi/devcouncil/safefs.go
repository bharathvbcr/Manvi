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
// than a human approval prompt — could otherwise swap a directory component
// for a symbolic link during the window, and the kernel would happily resolve
// the swapped chain at open time, writing the payload wherever the link
// points.
//
// The defence has two halves, and both are needed.
//
//  1. Identity pinning. At T0 every existing component's device/inode pair is
//     captured. At syscall time each of those components must still be the
//     very same object, or the operation refuses.
//
//  2. Descriptor traversal. At syscall time the path is walked one component
//     at a time through directory file descriptors (os.Root, which is openat2
//     RESOLVE_BENEATH on Linux and an openat/O_NOFOLLOW walk elsewhere) rather
//     than handed to the kernel as a string. Nothing is ever re-resolved from
//     the root by name after it has been verified, so there is no interval in
//     which a verified name can be pointed somewhere else before it is used.
//
// Half 1 alone was not enough, and its failure was not theoretical: identities
// can only be pinned for components that exist at T0, and writeFile
// legitimately creates missing directories. A request for "newdir/payload"
// pinned nothing but the repository root, created "newdir" at write time with
// no verification at all, and then opened the leaf by full path — so replacing
// "newdir" with a symbolic link during the approval dialog redirected the write
// out of the repository and the harness reported success. Components this
// operation creates are now verified exactly like pinned ones: created through
// the parent's descriptor, re-opened through that same descriptor, required to
// be a real directory (a link found where we expected our own new directory is
// a refusal, not something to follow), and used only as a descriptor
// thereafter.
//
// After the leaf is open, a final pass walks the held descriptors back up and
// requires each child to still be the entry its parent names — that catches a
// component renamed out from under the walk, which identity comparison alone
// cannot see because a rename preserves the inode.
//
// Residual, documented limits:
//
//   - Once a descriptor is held, a rename of that directory follows the
//     descriptor. The post-open pass detects any such move that completes
//     before the leaf is open; a move that races the pass itself is not
//     detectable this way. The window is microseconds of syscalls, never the
//     length of an approval dialog.
//   - os.Root refuses absolute symbolic links outright, even ones that resolve
//     back inside the repository. Such a link mid-path is now a refusal where
//     it used to be followed. That is a deliberate fail-closed narrowing.
//   - Symbolic links between two locations inside the repository are still
//     followed for components that existed at pin time, because that is the
//     tree policy evaluated; they cannot escape, since the walk is confined
//     beneath the root.

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
	// parts is rel split on "/"; the last element is the leaf.
	parts []string
	// dirIDs[0] is the root's identity and dirIDs[i+1] is parts[i]'s, for the
	// components that existed at pin time. len(dirIDs) == firstMissing+1.
	dirIDs       []componentIdentity
	firstMissing int
	// targetExisted records whether the leaf existed at pin time; if it did,
	// its identity must still match when we open it.
	targetExisted bool
	targetIdent   componentIdentity
}

// pinnedDirID returns the T0 identity of directory component parts[i], and
// whether that component existed at pin time at all.
func (p *pinnedTarget) pinnedDirID(i int) (componentIdentity, bool) {
	if i < 0 || i >= p.firstMissing {
		return componentIdentity{}, false
	}
	return p.dirIDs[i+1], true
}

// pinWriteTarget resolves rel under root the way the policy layer understood
// it — following any symlinks once, here, under containment review — and
// snapshots the identities the later write will be held to. Call it BEFORE
// escalation prompts, so the clock the attacker races against is not the
// human one.
//
// Components that do not exist yet are tolerated: writeFile legitimately
// creates directories, so the snapshot records how far the existing chain
// reaches. The missing tail is not trusted for being absent — it is created
// and verified at write time, through descriptors, by openVerifiedParent.
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
		root:  rootResolved,
		rel:   clean,
		parts: parts,
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
		pinned.dirIDs = append(pinned.dirIDs, id)
		current = resolved
	}

	// The resolved leaf location: the verified chain plus whatever was
	// missing. It is inspected here and then deliberately forgotten — every
	// later syscall goes through descriptors, never through a whole path,
	// because a whole path is exactly what an attacker can re-point.
	pinned.firstMissing = firstMissing
	physical := filepath.Join(append([]string{current}, parts[firstMissing:]...)...)

	if firstMissing == len(parts)-1 {
		// The leaf's own directory exists; the leaf itself must never be a
		// symlink. (Distinct names, not re-used err: a shadowed error once
		// made every missing file look like an existing one.)
		leafFi, leafErr := os.Lstat(physical)
		if leafErr != nil && !os.IsNotExist(leafErr) {
			return nil, fmt.Errorf("inspecting %q: %w", physical, leafErr)
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

// verifiedChain is the descriptor chain from the repository root down to the
// leaf's parent directory. Every element was verified as it was opened, and
// every element is a file descriptor: no name in it can be re-pointed.
type verifiedChain struct {
	// dirs[0] is the repository root; dirs[len-1] is the leaf's parent.
	dirs []*os.Root
	// ids[i] is the fstat identity of dirs[i] as opened.
	ids []componentIdentity
	// op is the verb used in refusal messages ("write", "read", "remove").
	op string
	// rel is the repository-relative path, for messages.
	rel string
}

func (c *verifiedChain) parent() *os.Root { return c.dirs[len(c.dirs)-1] }

func (c *verifiedChain) Close() {
	for _, d := range c.dirs {
		if d != nil {
			d.Close()
		}
	}
}

// stillLinked re-checks, from the descriptors themselves, that every directory
// in the chain is still the entry its parent names. Identity comparison alone
// cannot see a rename — the inode survives it — but a renamed directory is no
// longer reachable under the name policy approved, and following a descriptor
// that has been moved out of the repository would be an escape. Lstat, not
// Stat: a symbolic link left behind under the old name must not resolve back
// to the directory it replaced.
func (c *verifiedChain) stillLinked(parts []string) error {
	rootFi, err := os.Lstat(c.rootPath())
	if err != nil {
		return fmt.Errorf("refusing to %s %q: the repository root vanished mid-operation: %w", c.op, c.rel, err)
	}
	rootID, ok := identityOf(rootFi)
	if !ok {
		return fmt.Errorf("platform cannot identify directories; refusing to %s %q", c.op, c.rel)
	}
	if rootID != c.ids[0] {
		return fmt.Errorf(
			"refusing to %s %q: the repository root changed identity mid-operation "+
				"(was inode %d, found inode %d)", c.op, c.rel, c.ids[0].ino, rootID.ino)
	}
	for i := 1; i < len(c.dirs); i++ {
		name := parts[i-1]
		fi, err := c.dirs[i-1].Lstat(name)
		if err != nil {
			return fmt.Errorf(
				"refusing to %s %q: directory component %q was unlinked or renamed mid-operation: %w",
				c.op, c.rel, name, err)
		}
		id, ok := identityOf(fi)
		if !ok {
			return fmt.Errorf("platform cannot identify directories; refusing to %s %q", c.op, c.rel)
		}
		if id != c.ids[i] {
			return fmt.Errorf(
				"refusing to %s %q: directory component %q changed identity or became a symbolic "+
					"link mid-operation (was inode %d, found inode %d)",
				c.op, c.rel, name, c.ids[i].ino, id.ino)
		}
	}
	return nil
}

func (c *verifiedChain) rootPath() string { return c.dirs[0].Name() }

// openVerifiedParent walks from the repository root to the leaf's parent
// directory through file descriptors, verifying every component — the ones
// pinned at T0 against their pinned identity, and the ones this call has to
// create against the fact that we created them and that what we re-open is a
// real directory rather than something planted in the window.
//
// create says whether missing directory components may be created; reads and
// removals pass false, so a vanished chain is a refusal rather than a
// resurrection.
//
// The caller owns the returned chain and must Close it.
func (p *pinnedTarget) openVerifiedParent(op string, create bool) (*verifiedChain, error) {
	root, err := os.OpenRoot(p.root)
	if err != nil {
		return nil, fmt.Errorf("opening the repository root to %s %q: %w", op, p.rel, err)
	}
	chain := &verifiedChain{dirs: []*os.Root{root}, op: op, rel: p.rel}

	rootFi, err := root.Stat(".")
	if err != nil {
		chain.Close()
		return nil, fmt.Errorf("inspecting the repository root to %s %q: %w", op, p.rel, err)
	}
	rootID, ok := identityOf(rootFi)
	if !ok {
		chain.Close()
		return nil, fmt.Errorf("platform cannot identify directories; refusing to %s %q", op, p.rel)
	}
	if rootID != p.dirIDs[0] {
		chain.Close()
		return nil, fmt.Errorf(
			"refusing to %s %q: the repository root changed identity between policy evaluation "+
				"and open (was inode %d, found inode %d)", op, p.rel, p.dirIDs[0].ino, rootID.ino)
	}
	chain.ids = append(chain.ids, rootID)

	for i, part := range p.parts[:len(p.parts)-1] {
		next, id, err := p.enterDir(chain.parent(), i, part, op, create)
		if err != nil {
			chain.Close()
			return nil, err
		}
		chain.dirs = append(chain.dirs, next)
		chain.ids = append(chain.ids, id)
	}
	return chain, nil
}

// enterDir opens one directory component through its parent's descriptor and
// returns it only if it is provably the directory this operation is entitled
// to enter.
func (p *pinnedTarget) enterDir(parent *os.Root, i int, name, op string, create bool) (*os.Root, componentIdentity, error) {
	var zero componentIdentity
	expected, fromPin := p.pinnedDirID(i)

	if !fromPin {
		// This component did not exist when policy looked. It gets no trust
		// from the pin, so it gets all of its trust from being created here
		// and inspected through the parent descriptor.
		if !create {
			return nil, zero, fmt.Errorf(
				"refusing to %s %q: directory component %q does not exist", op, p.rel, name)
		}
		if err := parent.Mkdir(name, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, zero, fmt.Errorf("creating directory %q for %q: %w", name, p.rel, err)
		}
		// EEXIST is tolerated — a concurrent legitimate write may have created
		// the same directory — but only if what is there is a real directory.
		// A symbolic link at a name we intended to create ourselves is an
		// attacker, not a race between two honest writers.
		li, err := parent.Lstat(name)
		if err != nil {
			return nil, zero, fmt.Errorf(
				"refusing to %s %q: inspecting the directory %q we just created: %w", op, p.rel, name, err)
		}
		if li.Mode()&fs.ModeSymlink != 0 {
			return nil, zero, &symlinkRefusal{component: name, during: op}
		}
		if !li.IsDir() {
			return nil, zero, fmt.Errorf(
				"refusing to %s %q: path component %q is not a directory", op, p.rel, name)
		}
		linkID, ok := identityOf(li)
		if !ok {
			return nil, zero, fmt.Errorf("platform cannot identify directories; refusing to %s %q", op, p.rel)
		}
		// From here on this component is held to the identity we just
		// established for it, exactly as a pinned one is held to T0's.
		expected = linkID
	}

	next, err := parent.OpenRoot(name)
	if err != nil {
		return nil, zero, fmt.Errorf(
			"refusing to %s %q: directory component %q could not be entered as the directory policy "+
				"evaluated — it changed identity or became a symbolic link: %w", op, p.rel, name, err)
	}
	fi, err := next.Stat(".")
	if err != nil {
		next.Close()
		return nil, zero, fmt.Errorf("inspecting directory component %q of %q: %w", name, p.rel, err)
	}
	if !fi.IsDir() {
		next.Close()
		return nil, zero, fmt.Errorf(
			"refusing to %s %q: path component %q is not a directory", op, p.rel, name)
	}
	id, ok := identityOf(fi)
	if !ok {
		next.Close()
		return nil, zero, fmt.Errorf("platform cannot identify directories; refusing to %s %q", op, p.rel)
	}
	// The descriptor's own fstat — immune to any path game — must be the
	// directory we verified a moment ago.
	if id != expected {
		next.Close()
		when := "between policy evaluation and open"
		if !fromPin {
			when = "between being created and being entered"
		}
		return nil, zero, fmt.Errorf(
			"refusing to %s %q: directory component %q changed identity %s "+
				"(possible symlink swap; was inode %d, found inode %d)",
			op, p.rel, name, when, expected.ino, id.ino)
	}
	return next, id, nil
}

// leafName is the final component of the pinned path.
func (p *pinnedTarget) leafName() string { return p.parts[len(p.parts)-1] }

// Write stores data at the pinned location, refusing unless every component of
// the path — pinned or created by this very call — is provably the one policy
// evaluated at the moment the leaf is opened.
func (p *pinnedTarget) Write(data []byte, perm fs.FileMode) error {
	chain, err := p.openVerifiedParent("write", true)
	if err != nil {
		return err
	}
	defer chain.Close()

	leaf := p.leafName()
	// O_NONBLOCK so a FIFO planted at the target cannot park the harness
	// forever; O_NOFOLLOW as belt and braces where the platform honours it
	// (os.Root resolves in-root links itself, so the refusal below is what
	// actually carries the guarantee). No O_TRUNC: truncation happens after
	// the opened file has been proved to be the pinned one, so a refused
	// write never destroys the bystander it refused to write to.
	flags := os.O_WRONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK | syscall.O_CLOEXEC
	if p.targetExisted {
		// The file existed at pin time. It must still exist and still be that
		// same file; re-creating it would paper over a deletion race.
		if err := p.verifyLeafIdentity(chain, leaf); err != nil {
			return err
		}
	} else {
		// It did not exist at pin time, so this call must be the one that
		// creates it. O_EXCL makes that provable, and it is also what stops a
		// symbolic link planted at the name during the window from being
		// followed: O_CREAT|O_EXCL refuses an existing link outright rather
		// than creating a file at the far end of it.
		flags |= os.O_CREATE | os.O_EXCL
	}

	f, err := chain.parent().OpenFile(leaf, flags, perm)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return &symlinkRefusal{component: p.rel, during: "write"}
		}
		if !p.targetExisted && errors.Is(err, fs.ErrExist) {
			return fmt.Errorf(
				"refusing to write %q: it did not exist when policy evaluated it but something "+
					"was created at that name before the write (possible symlink swap)", p.rel)
		}
		return fmt.Errorf("opening %q: %w", p.rel, err)
	}
	defer f.Close()

	openedID, err := p.checkOpened(f, chain, leaf, "write")
	if err != nil {
		return err
	}
	if p.targetExisted && openedID != p.targetIdent {
		return fmt.Errorf(
			"refusing to write %q: the file at that path changed identity between "+
				"policy evaluation and open (was inode %d, opened inode %d)",
			p.rel, p.targetIdent.ino, openedID.ino)
	}

	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncating %q: %w", p.rel, err)
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

// checkOpened proves that the descriptor just opened is a regular file that is
// still the entry named leaf in the verified parent, and that the whole
// directory chain was intact at that moment.
func (p *pinnedTarget) checkOpened(f *os.File, chain *verifiedChain, leaf, op string) (componentIdentity, error) {
	var zero componentIdentity
	// What did we actually just open? fstat reads the descriptor, not the
	// path, so a concurrent rename cannot confuse it.
	opened, err := f.Stat()
	if err != nil {
		return zero, fmt.Errorf("inspecting opened %q: %w", p.rel, err)
	}
	if !opened.Mode().IsRegular() {
		return zero, fmt.Errorf("refusing to %s %q: not a regular file", op, p.rel)
	}
	openedID, ok := identityOf(opened)
	if !ok {
		return zero, fmt.Errorf("platform cannot identify files; refusing to %s %q", op, p.rel)
	}
	// The open resolved a name; prove the name still denotes this exact file
	// in the verified directory, and that nothing in the chain moved while we
	// were opening it.
	li, err := chain.parent().Lstat(leaf)
	if err != nil {
		return zero, fmt.Errorf("refusing to %s %q: inspecting the opened entry: %w", op, p.rel, err)
	}
	if li.Mode()&fs.ModeSymlink != 0 {
		return zero, &symlinkRefusal{component: leaf, during: op}
	}
	linkID, ok := identityOf(li)
	if !ok {
		return zero, fmt.Errorf("platform cannot identify files; refusing to %s %q", op, p.rel)
	}
	if linkID != openedID {
		return zero, fmt.Errorf(
			"refusing to %s %q: the name resolved to a different file than the one opened "+
				"(named inode %d, opened inode %d)", op, p.rel, linkID.ino, openedID.ino)
	}
	if err := chain.stillLinked(p.parts); err != nil {
		return zero, err
	}
	return openedID, nil
}

// verifyLeafIdentity refuses early when the pinned leaf is already gone or has
// been replaced, so the failure names the swap rather than a bare ENOENT.
func (p *pinnedTarget) verifyLeafIdentity(chain *verifiedChain, leaf string) error {
	li, err := chain.parent().Lstat(leaf)
	if err != nil {
		return fmt.Errorf(
			"refusing to write %q: the file policy evaluated is no longer there: %w", p.rel, err)
	}
	if li.Mode()&fs.ModeSymlink != 0 {
		return &symlinkRefusal{component: leaf, during: "write"}
	}
	id, ok := identityOf(li)
	if !ok {
		return fmt.Errorf("platform cannot identify files; refusing to write %q", p.rel)
	}
	if id != p.targetIdent {
		return fmt.Errorf(
			"refusing to write %q: the file at that path changed identity between policy "+
				"evaluation and open (was inode %d, found inode %d)",
			p.rel, p.targetIdent.ino, id.ino)
	}
	return nil
}

// Read reads the whole file at the pinned location through the same descriptor
// traversal and identity verification as Write: reading through a swapped
// directory is an exfiltration channel, not merely a wrong result. A limit of
// 0 means unlimited.
func (p *pinnedTarget) Read(limit int64) ([]byte, error) {
	if !p.targetExisted {
		return nil, fmt.Errorf("file %s does not exist", p.rel)
	}
	chain, err := p.openVerifiedParent("read", false)
	if err != nil {
		return nil, err
	}
	defer chain.Close()

	leaf := p.leafName()
	f, err := chain.parent().OpenFile(leaf,
		os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, &symlinkRefusal{component: p.rel, during: "read"}
		}
		return nil, fmt.Errorf("opening %q: %w", p.rel, err)
	}
	defer f.Close()

	openedID, err := p.checkOpened(f, chain, leaf, "read")
	if err != nil {
		return nil, err
	}
	if openedID != p.targetIdent {
		return nil, fmt.Errorf(
			"refusing to read %q: the file changed identity between evaluation and open "+
				"(was inode %d, opened inode %d)", p.rel, p.targetIdent.ino, openedID.ino)
	}
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspecting opened %q: %w", p.rel, err)
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

// RemovePinned deletes the leaf at a pinned location. The removal is issued
// against the verified parent's descriptor and never follows a final-component
// symlink, so neither the leaf nor any directory above it can redirect it.
func RemovePinned(root, rel string) error {
	pinned, err := pinWriteTarget(root, rel)
	if err != nil {
		return err
	}
	chain, err := pinned.openVerifiedParent("remove", false)
	if err != nil {
		return err
	}
	defer chain.Close()

	leaf := pinned.leafName()
	li, err := chain.parent().Lstat(leaf)
	if err != nil {
		return fmt.Errorf("removing %q: %w", rel, err)
	}
	if li.IsDir() {
		return fmt.Errorf("refusing to remove %q: it is a directory", rel)
	}
	if pinned.targetExisted {
		id, ok := identityOf(li)
		if !ok {
			return fmt.Errorf("platform cannot identify files; refusing to remove %q", rel)
		}
		if id != pinned.targetIdent {
			return fmt.Errorf(
				"refusing to remove %q: the file at that path changed identity between policy "+
					"evaluation and removal (was inode %d, found inode %d)",
				rel, pinned.targetIdent.ino, id.ino)
		}
	}
	if err := chain.stillLinked(pinned.parts); err != nil {
		return err
	}
	if err := chain.parent().Remove(leaf); err != nil {
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

// syscall_Mkfifo builds a fifo for the non-regular-file refusal test. Mkfifo
// is not in the portable syscall surface, so it shells out to the platform's
// tool; the test skips when that is unavailable.
func syscall_Mkfifo(path string, mode uint32) error {
	return runMkfifo(path, mode)
}
