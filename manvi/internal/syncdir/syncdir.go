// Package syncdir is the one place Manvi flushes a directory's own entries.
//
// Syncing a file puts its bytes on the disk; only syncing the directory that
// holds it puts its name there. A write that publishes by rename or link and
// stops at the file sync can, after a crash, come back as the previous version
// or as an unreferenced inode. Every durable write in Manvi (session logs, the
// budget ledger, artifacts, the catalog, DevCouncil pinned writes) ends with
// one of the functions here, so the platform rule lives once.
//
// The platform rule: on Unix a directory handle opened read-only is flushed
// with fsync. On Windows it is not flushed at all. A directory handle there
// cannot be flushed (FlushFileBuffers needs write access that a directory
// handle opened by os.Open does not have, and Sync returns an access error),
// and NTFS journals the rename's metadata itself, so asking for the flush
// would fail every write after it had already been published. Platforms
// without either guarantee also do nothing, as the budget ledger's lock_other
// has always done there. See the per-platform files for each reason.
package syncdir

import (
	"errors"
	"os"
)

// File flushes d, an open handle on a directory, so the entries a rename or
// link made in it survive a crash. It reports whatever the flush reports; on a
// platform where a directory cannot be flushed it does nothing and returns nil.
//
// This is the core. The caller opens the handle (os.Open, or os.Root.Open) and
// closes it, so a caller with its own open and error-wrapping policy can use
// it directly; Dir and Root are the open-flush-close wrappers.
func File(d *os.File) error { return flush(d) }

// Dir opens the directory at dir read-only, flushes it and closes it. A
// failure to open is returned, so a directory that is missing is not mistaken
// for one that was flushed. On a platform that cannot flush a directory it
// returns nil without touching the filesystem.
func Dir(dir string) error {
	if !supported {
		return nil
	}
	// #nosec G304 G703 -- every caller passes the directory of a path it has
	// already validated and just published into.
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(File(d), d.Close())
}

// Root is Dir for a directory held as an *os.Root: it flushes the root
// directory itself, never anything a symbolic link inside it names.
func Root(root *os.Root) error {
	if !supported {
		return nil
	}
	d, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(File(d), d.Close())
}
