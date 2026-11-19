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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/safefile"
)

var (
	// ErrRefused marks every refusal made for what an entry is: a symbolic link,
	// a directory where a file was wanted, a hard-linked or non-regular file, an
	// entry that changed identity between inspection and open.
	ErrRefused = errors.New("refused")
	// ErrTooLarge marks a file over the caller's limit. It is also a refusal.
	ErrTooLarge = errors.New("over the size limit")
	// ErrPublishedUnsynced reports a write that is committed whose durability is
	// not confirmed: the name was published and the sync of its directory
	// failed. It is not a failure and must not be retried as one; the caller
	// that reads it as "not written" gets ErrExist on the retry or a second
	// revision.
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

// Descend opens the directory rel below root, one verified Child at a time. rel
// is relative and local ("." is root itself, as a handle of its own); create is
// the mode for directories that do not exist yet, or zero to require them. The
// returned root is the caller's to close; root is left open.
func Descend(root *os.Root, rel string, create fs.FileMode) (*os.Root, error) {
	rel = filepath.Clean(rel)
	if rel == "." {
		return root.OpenRoot(".")
	}
	if !filepath.IsLocal(rel) {
		return nil, refusal(rel, "leaves the directory it is resolved under")
	}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		next, err := Child(current, part, create)
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

// OpenDir opens the trusted anchor and descends rel below it (see Descend).
func OpenDir(anchor, rel string, create fs.FileMode) (*os.Root, error) {
	root, err := Open(anchor)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return Descend(root, rel, create)
}

// ReadOptions bound one read. Limit must be positive: a read with no bound is
// not offered.
type ReadOptions struct {
	// Limit is the most bytes the file may hold.
	Limit int64
	// SingleLink refuses a file with more than one hard link. O_NOFOLLOW does not
	// see a hard link to a file outside the tree, so a store whose entries have
	// exactly one name asks for it. A store that publishes by link and then
	// removes its stage must not, or a read racing that window is refused.
	SingleLink bool
}

// openRegular opens name below root without following any link, FIFO-safe
// (O_NONBLOCK) and checks the handle, not a separate lstat: it must be a
// regular file, within the link and size policy.
func openRegular(root *os.Root, name string, o ReadOptions) (*os.File, error) {
	if o.Limit <= 0 {
		return nil, errors.New("treefile: a read needs a positive size limit")
	}
	f, err := safefile.OpenNoFollow(root, filepath.FromSlash(name), os.O_RDONLY, 0)
	if err != nil {
		// os.Root answers "does not exist" for a dangling link it resolved
		// itself. A name present as anything is a refusal, not an absence, so
		// that case must not wrap fs.ErrNotExist.
		if errors.Is(err, fs.ErrNotExist) {
			if _, lerr := root.Lstat(filepath.FromSlash(name)); errors.Is(lerr, fs.ErrNotExist) {
				return nil, err
			}
			return nil, refusal(name, "(a dangling symbolic link): "+err.Error())
		}
		return nil, fmt.Errorf("%w: %q (a symbolic link, or not an ordinary path): %w", ErrRefused, name, err)
	}
	info, err := f.Stat()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("treefile: inspecting %q: %w", name, err), f.Close())
	}
	if !info.Mode().IsRegular() {
		return nil, errors.Join(refusal(name, "is not a regular file"), f.Close())
	}
	if o.SingleLink {
		links, err := safefile.LinkCount(f)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("treefile: counting the links of %q: %w", name, err), f.Close())
		}
		if links != 1 {
			return nil, errors.Join(refusal(name, fmt.Sprintf("has %d hard links, so it may be a file outside the tree", links)), f.Close())
		}
	}
	if info.Size() > o.Limit {
		return nil, errors.Join(fmt.Errorf("%w: %q is %d bytes, over the %d-byte limit: %w", ErrRefused, name, info.Size(), o.Limit, ErrTooLarge), f.Close())
	}
	return f, nil
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
