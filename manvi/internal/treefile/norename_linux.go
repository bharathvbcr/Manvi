//go:build linux

package treefile

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames stage to name within dir, failing with EEXIST when the
// name is taken, in one atomic step (renameat2 RENAME_NOREPLACE), so the file is
// never reachable under two names. errNoAtomicRename means this kernel or file
// system does not do it.
// hasAtomicRename says Publish renames without replacing, so a published entry
// never has a second link. See stillLinked.
const hasAtomicRename = true

func renameNoReplace(dir *os.Root, stage, name string) error {
	d, err := dir.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	err = unix.Renameat2(int(d.Fd()), stage, int(d.Fd()), name, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) {
		return errNoAtomicRename
	}
	return err
}
