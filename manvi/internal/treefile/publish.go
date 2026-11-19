package treefile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/safefile"

	"github.com/bharathvbcr/Manvi/manvi/internal/filelock"
	"github.com/bharathvbcr/Manvi/manvi/internal/syncdir"
)

// PublishOptions say how Publish names and settles a file.
type PublishOptions struct {
	// Perm is the mode of the new file. The process umask still applies unless
	// ExactPerm is set.
	Perm fs.FileMode
	// ExactPerm chmods the staged file to Perm, for a file that replaces one
	// whose mode must survive.
	ExactPerm bool
	// Replace publishes by rename, replacing a regular file at the name. Without
	// it the name is published by link and an existing name is fs.ErrExist.
	Replace bool
	// SyncFile flushes the staged file. Nil is (*os.File).Sync. It is an option
	// so a test can make the flush fail.
	SyncFile func(*os.File) error
	// SyncDir flushes dir after the name lands. Nil is syncdir.Root.
	SyncDir func(dir *os.Root) error
}

// stageName is the temporary sibling a publish builds: ".<name>.<16 hex>.tmp".
// A crash between create and publish leaves one, and it is never a name a
// caller published.
func stageName(name string) (string, error) {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("treefile: naming a temporary file for %s: %w", name, err)
	}
	return "." + name + "." + hex.EncodeToString(nonce[:]) + ".tmp", nil
}

// Publish writes data so that name never holds half of it: the bytes go to a
// synced temporary sibling in dir, which is then renamed (Replace) or linked
// onto name, and the directory is synced so the name survives a crash. name is
// one segment of dir. The stage is created O_EXCL and no-follow, and the final
// name is never opened for writing, so a link planted there is replaced, not
// written through.
//
// A failure of the last step returns an error that wraps ErrPublishedUnsynced:
// by then the name is published and the write is neither undone nor a failure.
func Publish(dir *os.Root, name string, data []byte, o PublishOptions) (result error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return refusal(name, "is not a single path segment")
	}
	if o.Perm == 0 {
		o.Perm = 0o644
	}
	syncFile := o.SyncFile
	if syncFile == nil {
		syncFile = func(f *os.File) error { return f.Sync() }
	}
	syncDir := o.SyncDir
	if syncDir == nil {
		syncDir = syncdir.Root
	}
	if o.Replace {
		if info, err := dir.Lstat(name); err == nil && !info.Mode().IsRegular() {
			return refusal(name, "is not a regular file, so it will not be replaced")
		}
	}

	var (
		stage string
		f     *os.File
		open  error
	)
	for attempt := 0; attempt < 16; attempt++ {
		var err error
		if stage, err = stageName(name); err != nil {
			return err
		}
		f, open = safefile.OpenNoFollow(dir, stage, os.O_WRONLY|os.O_CREATE|os.O_EXCL, o.Perm)
		if !errors.Is(open, fs.ErrExist) {
			break
		}
	}
	if open != nil {
		return fmt.Errorf("treefile: creating a temporary file for %s: %w", name, open)
	}
	defer func() {
		// A rename consumed the stage; after a link it is still there.
		if err := dir.Remove(stage); err != nil && !errors.Is(err, fs.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}()

	if o.ExactPerm {
		if err := f.Chmod(o.Perm); err != nil {
			return errors.Join(fmt.Errorf("treefile: setting the mode of %s: %w", name, err), f.Close())
		}
	}
	if n, err := f.Write(data); err != nil || n != len(data) {
		if err == nil {
			err = errors.New("short write")
		}
		return errors.Join(fmt.Errorf("treefile: writing %s: %w", name, err), f.Close())
	}
	if err := syncFile(f); err != nil {
		return errors.Join(fmt.Errorf("treefile: syncing %s: %w", name, err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("treefile: closing %s: %w", name, err)
	}

	if o.Replace {
		if err := dir.Rename(stage, name); err != nil {
			return fmt.Errorf("treefile: publishing %s: %w", name, err)
		}
	} else if err := dir.Link(stage, name); err != nil {
		return fmt.Errorf("treefile: publishing %s: %w", name, err)
	}

	if err := syncDir(dir); err != nil {
		return fmt.Errorf("treefile: %s is published but syncing its directory failed, so it may not survive a crash: %w: %w",
			name, ErrPublishedUnsynced, err)
	}
	return nil
}

// Lock takes the exclusive cross-process lock held on the file name in dir,
// waiting until ctx ends. The lock file is opened through dir with no link
// followed, created if need be, and never removed: a removed lock file lets two
// processes lock two inodes of one name. The kernel drops the lock when its
// holder exits, so a lock file's age says nothing about whether it is held and
// is never consulted. A platform with no cross-process lock returns
// filelock.ErrUnsupported; the caller refuses rather than write unlocked.
func Lock(ctx context.Context, dir *os.Root, name string) (unlock func(), err error) {
	for attempt := 0; attempt < lockAttempts; attempt++ {
		f, err := openLockFile(dir, name)
		if err != nil {
			return nil, fmt.Errorf("treefile: opening lock %s: %w", name, err)
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			if err == nil {
				err = refusal(name, "is not a regular file, so it cannot be a lock")
			}
			return nil, errors.Join(err, f.Close())
		}
		unlock, err := filelock.LockFile(ctx, f)
		if err != nil {
			return nil, err
		}
		// A lock is held on an inode, and the name may have been deleted or
		// renamed over while this waited: then it excludes nobody who opens the
		// name now. Held, the handle is compared with the name; if they are not
		// the same file the lock is released and taken again on the current one.
		named, lerr := dir.Lstat(name)
		held, herr := f.Stat()
		if lerr == nil && herr == nil && os.SameFile(named, held) {
			return unlock, nil
		}
		unlock()
	}
	return nil, fmt.Errorf("treefile: lock %s kept being replaced while it was taken", name)
}

// lockAttempts bounds how often Lock retakes a lock file that was replaced
// under it.
const lockAttempts = 16

// openLockFile opens the lock file, creating it when it is not there. Creation
// is exclusive and separate from the open: several writers meet at a lock
// file's first use, and an O_CREATE open that races another creator of the same
// name can fail with a spurious ENOENT, which is not an answer about the name.
// Each of those races ends with the file existing, so the pair is retried a
// bounded number of times.
func openLockFile(dir *os.Root, name string) (*os.File, error) {
	var last error
	for attempt := 0; attempt < 16; attempt++ {
		f, err := safefile.OpenNoFollow(dir, name, os.O_RDWR, 0)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		f, err = safefile.OpenNoFollow(dir, name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, fs.ErrExist) && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		last = err
	}
	return nil, last
}
