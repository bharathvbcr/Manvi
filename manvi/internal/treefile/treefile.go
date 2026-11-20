// Package treefile is the one owner of how Manvi touches files whose location a
// repository can influence: a directory under a project root, a bounded read of
// one regular file, an atomic publish into a directory, and the lock that
// orders writers of one entry.
//
// A repository is not trusted to be a plain tree. It can commit any path as a
// symbolic link, and a hard link or a FIFO looks like a file until it is read.
// So nothing here resolves a path string twice: a directory is opened as an
// *os.Root after its entry has been inspected, the root is compared with what
// was inspected, and every later read, stage, link, rename and sync goes through
// that handle. Containment is a property of the handle, not of a string checked
// earlier.
//
// The package starts from an anchor the caller trusts (the project root, or an
// absolute directory an operator named on purpose) and checks only what lies
// below it. Checking from the filesystem root would refuse every temporary
// directory on a host where /tmp is itself a link.
package treefile

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/safefile"

	"github.com/bharathvbcr/Manvi/manvi/internal/filelock"
)

var (
	// ErrRefused marks every refusal made for what an entry is: a symbolic link,
	// a directory where a file was wanted, a hard-linked or non-regular file, an
	// entry that changed identity between inspection and open.
	ErrRefused = errors.New("refused")
	// ErrTooLarge marks a file over the caller's limit. It is also a refusal.
	ErrTooLarge = errors.New("over the size limit")
	// ErrPublishedUnsynced reports a write that is committed whose settling is
	// not confirmed: the name was published and either the sync of its directory
	// failed or the temporary file the link left behind could not be removed. It
	// is not a failure and must not be retried as one; the caller that reads it
	// as "not written" gets ErrExist on the retry or a second revision.
	ErrPublishedUnsynced = errors.New("published, durability unconfirmed")
)

func refusal(name, why string) error {
	return fmt.Errorf("%w: %q %s", ErrRefused, name, why)
}

// Open opens the directory anchor itself, which the caller trusts. It is
// os.OpenRoot, named here so that a caller has one import for the whole walk.
func Open(anchor string) (*os.Root, error) {
	return os.OpenRoot(anchor)
}

// Child opens the directory name directly below parent. name is one path
// segment. With create non-zero a missing directory is made with that mode,
// through parent, never by path. The entry must be a real directory, not a link
// and not a file, and the opened root must be the directory that was inspected:
// a swap between the two is refused rather than followed.
func Child(parent *os.Root, name string, create fs.FileMode) (*os.Root, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return nil, refusal(name, "is not a single path segment")
	}
	if create != 0 {
		if err := parent.Mkdir(name, create); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	named, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if named.Mode()&fs.ModeSymlink != 0 {
		return nil, refusal(name, "is a symbolic link")
	}
	if !named.IsDir() {
		return nil, refusal(name, "is not a directory")
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := child.Stat(".")
	if err != nil {
		return nil, errors.Join(err, child.Close())
	}
	after, err := parent.Lstat(name)
	if err != nil {
		return nil, errors.Join(err, child.Close())
	}
	if after.Mode()&fs.ModeSymlink != 0 || !os.SameFile(named, opened) || !os.SameFile(after, opened) {
		return nil, errors.Join(refusal(name, "changed while it was being opened"), child.Close())
	}
	return child, nil
}

// descend opens the directory rel below root, one verified Child at a time. rel
// is relative and local ("." is root itself, as a handle of its own); create is
// the mode for directories that do not exist yet, or zero to require them. The
// returned root is the caller's to close; root is left open.
func descend(root *os.Root, rel string, create fs.FileMode) (*os.Root, error) {
	return descendWith(root, rel, func(parent *os.Root, part string) (*os.Root, error) {
		return Child(parent, part, create)
	})
}

// descendWith is descend with the step that opens each component supplied.
func descendWith(root *os.Root, rel string, child func(*os.Root, string) (*os.Root, error)) (*os.Root, error) {
	rel = filepath.Clean(rel)
	if rel == "." {
		return root.OpenRoot(".")
	}
	if !filepath.IsLocal(rel) {
		return nil, refusal(rel, "leaves the directory it is resolved under")
	}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		next, err := child(current, part)
		if current != root {
			err = errors.Join(err, current.Close())
		}
		if err != nil {
			return nil, err
		}
		current = next
	}
	return current, nil
}

// OpenDir opens the trusted anchor and descends rel below it (see descend).
func OpenDir(anchor, rel string, create fs.FileMode) (*os.Root, error) {
	root, err := Open(anchor)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return descend(root, rel, create)
}

// aliasLockWait bounds the wait for another creator of a directory in the same
// parent.
const aliasLockWait = 10 * time.Second

// ChildAliasSafe is Child with create for a store whose names must not alias: a
// directory that does not exist exactly is made only after CheckAlias has passed
// it, and the check and the make happen under the exclusive kernel lock on
// lockName in parent (Lock, the one cross-process lock this package has), so two
// creators of two spellings of one name, in this process or another, have one
// winner and one ErrAliased. lockName must not be a name the store lists or
// reads. A directory that is already there under exactly this spelling is opened
// without the lock.
func ChildAliasSafe(parent *os.Root, name, lockName string, create fs.FileMode) (*os.Root, error) {
	if create == 0 {
		return Child(parent, name, 0)
	}
	if _, exact, err := scanAlias(parent, name); err != nil {
		return nil, err
	} else if exact {
		return Child(parent, name, create)
	}
	ctx, cancel := context.WithTimeout(context.Background(), aliasLockWait)
	defer cancel()
	unlock, err := lockParent(ctx, parent, lockName)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := CheckAlias(parent, name); err != nil {
		return nil, err
	}
	return Child(parent, name, create)
}

// lockParent takes the exclusive kernel lock that orders the creators of
// subdirectories of parent: on the directory itself where the kernel allows that
// (flock on a directory leaves no file behind, and a store that owns nothing but
// its entries keeps owning nothing else), and otherwise on the file lockName
// through Lock. Both are internal/filelock, the one cross-process lock there is.
func lockParent(ctx context.Context, parent *os.Root, lockName string) (func(), error) {
	d, err := parent.Open(".")
	if err != nil {
		return nil, err
	}
	unlock, err := filelock.LockFile(ctx, d)
	if err == nil {
		return unlock, nil
	}
	if ctx.Err() != nil {
		return nil, err
	}
	return Lock(ctx, parent, lockName)
}

// DescendAliasSafe is descend below root whose created components are made with
// ChildAliasSafe, locking on lockName in each parent that gains a directory.
// root itself is not locked or checked: it is the caller's anchor.
func DescendAliasSafe(root *os.Root, rel string, create fs.FileMode, lockName string) (*os.Root, error) {
	return descendWith(root, rel, func(parent *os.Root, part string) (*os.Root, error) {
		return ChildAliasSafe(parent, part, lockName, create)
	})
}

// ReadOptions bound one read. Limit must be positive: a read with no bound is
// not offered.
type ReadOptions struct {
	// Limit is the most bytes the file may hold.
	Limit int64
	// SingleLink refuses a file with more than one hard link. O_NOFOLLOW does not
	// see a hard link to a file outside the tree, so a store whose entries have
	// one name asks for it. A count of zero is not a refusal: the file was
	// replaced by rename after it was opened, and the handle is a consistent
	// snapshot of the previous version. Publish uses an atomic no-replace rename
	// where the OS has one, so a published entry has one name; where it links
	// instead, the one extra link a publish in progress has is accepted only when it
	// is the entry's own stage sibling (see ownStageLink).
	SingleLink bool
}

// openRetries bounds the retries of an open that raced a rename-replace.
const openRetries = 3

// ownStageLink reports whether the second hard link of f, which has n links, is
// this store's own stage for it: a sibling of name in its directory called
// ".<name>.<16 hex>.tmp" that is the same file. A publish by link, where the OS has
// no atomic no-replace rename, has exactly this second name for the moment before
// it removes the stage, however slow the directory sync in between. A link to a file
// outside the tree makes a count of 3 or more, because the file's own name is
// outside, and one with no stage sibling is not a publish, so both are refused.
func ownStageLink(root *os.Root, name string, f *os.File, n uint64) bool {
	if n != 2 {
		return false
	}
	dirPath, base := filepath.Split(filepath.FromSlash(name))
	dirPath = filepath.Clean(dirPath)
	d, err := root.Open(dirPath)
	if err != nil {
		return false
	}
	names, err := d.Readdirnames(maxAliasScan + 1)
	_ = d.Close()
	if err != nil || len(names) > maxAliasScan {
		return false
	}
	held, err := f.Stat()
	if err != nil {
		return false
	}
	prefix := "." + base + "."
	for _, cand := range names {
		if len(cand) != len(prefix)+16+len(".tmp") || !strings.HasPrefix(cand, prefix) || !strings.HasSuffix(cand, ".tmp") {
			continue
		}
		if _, err := hex.DecodeString(cand[len(prefix) : len(prefix)+16]); err != nil {
			continue
		}
		if info, err := root.Lstat(filepath.Join(dirPath, cand)); err == nil && os.SameFile(info, held) {
			return true
		}
	}
	return false
}

// ErrUnstable marks a name that kept being replaced while it was being opened,
// so no handle that was the name's current file could be taken. It is neither a
// refusal of what the entry is nor an absence: a caller may try again.
var ErrUnstable = errors.New("changed while it was being opened")

// unstableOpenError is the answer for a regular file that was replaced under
// every attempt to open it.
func unstableOpenError(name string) error {
	return fmt.Errorf("treefile: %q was replaced %d times while it was being opened, so it was not read: %w",
		name, openRetries+1, ErrUnstable)
}

// openFailure classifies a failure of the no-follow open that is not an absence.
// A permission failure and an exhausted descriptor table are facts about this
// process and this host, not about what the entry is, and calling them refusals
// told an operator the repository was hostile when it was the host that was
// short of descriptors. Everything else the open refuses (a link met on the way,
// an entry that changed identity, a parent that is not a directory) is the
// entry's own doing.
func openFailure(name string, err error) error {
	switch {
	case errors.Is(err, fs.ErrPermission),
		errors.Is(err, syscall.EMFILE), errors.Is(err, syscall.ENFILE),
		errors.Is(err, syscall.ENOMEM), errors.Is(err, syscall.EIO):
		return fmt.Errorf("treefile: opening %q: %w", name, err)
	}
	return fmt.Errorf("%w: %q (a symbolic link, or not an ordinary path): %w", ErrRefused, name, err)
}

// openRegular opens name below root without following any link, FIFO-safe
// (O_NONBLOCK) and checks the handle, not a separate lstat: it must be a
// regular file, within the link and size policy.
func openRegular(root *os.Root, name string, o ReadOptions) (*os.File, error) {
	if o.Limit <= 0 {
		return nil, errors.New("treefile: a read needs a positive size limit")
	}
	for attempt := 0; ; {
		f, err := safefile.OpenNoFollow(root, filepath.FromSlash(name), os.O_RDONLY, 0)
		if err != nil {
			// os.Root answers "does not exist" for a dangling link it resolved
			// itself. A name present as a link is a refusal, not an absence, so
			// that case must not wrap fs.ErrNotExist. A name that is a regular
			// file when looked at again was replaced by rename between the open
			// and the look: that is a race, not a refusal, and the open is
			// retried, and reported as a race if it keeps losing.
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, openFailure(name, err)
			}
			info, lerr := root.Lstat(filepath.FromSlash(name))
			switch {
			case errors.Is(lerr, fs.ErrNotExist):
				return nil, err
			case lerr == nil && info.Mode().IsRegular():
				if attempt < openRetries {
					attempt++
					continue
				}
				return nil, unstableOpenError(name)
			case lerr == nil && info.Mode()&fs.ModeSymlink != 0:
				return nil, refusal(name, "(a dangling symbolic link): "+err.Error())
			case lerr == nil:
				return nil, refusal(name, "is not a regular file or a link it can follow: "+err.Error())
			}
			return nil, openFailure(name, errors.Join(err, lerr))
		}
		info, err := f.Stat()
		if err != nil {
			return nil, errors.Join(fmt.Errorf("treefile: inspecting %q: %w", name, err), f.Close())
		}
		if !info.Mode().IsRegular() {
			return nil, errors.Join(refusal(name, "is not a regular file"), f.Close())
		}
		if o.SingleLink {
			n, err := safefile.LinkCount(f)
			if err != nil {
				return nil, errors.Join(fmt.Errorf("treefile: counting the links of %q: %w", name, err), f.Close())
			}
			if n > 1 && !ownStageLink(root, name, f, n) {
				// The publisher may have finished between the count and the
				// look, which leaves one link.
				if again, err := safefile.LinkCount(f); err != nil || again > 1 {
					return nil, errors.Join(refusal(name, fmt.Sprintf("has %d hard links, so it may be a file outside the tree", n)), f.Close())
				}
			}
		}
		if info.Size() > o.Limit {
			return nil, errors.Join(fmt.Errorf("%w: %q is %d bytes, over the %d-byte limit: %w", ErrRefused, name, info.Size(), o.Limit, ErrTooLarge), f.Close())
		}
		return f, nil
	}
}

// Check opens name as ReadFile would and applies every refusal, without reading
// the body.
func Check(root *os.Root, name string, o ReadOptions) error {
	f, err := openRegular(root, name, o)
	if err != nil {
		return err
	}
	return f.Close()
}

// ReadFile reads the regular file name below root, through no link, within
// o.Limit. A name that is absent altogether wraps fs.ErrNotExist; every other
// failure to read an ordinary file wraps ErrRefused or names its cause.
func ReadFile(root *os.Root, name string, o ReadOptions) (data []byte, err error) {
	f, err := openRegular(root, name, o)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	data, err = io.ReadAll(io.LimitReader(f, o.Limit+1))
	if err != nil {
		return nil, fmt.Errorf("treefile: reading %q: %w", name, err)
	}
	if int64(len(data)) > o.Limit {
		return nil, fmt.Errorf("%w: %q grew past the %d-byte limit: %w", ErrRefused, name, o.Limit, ErrTooLarge)
	}
	return data, nil
}
