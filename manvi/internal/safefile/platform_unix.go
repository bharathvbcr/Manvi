//go:build !windows

package safefile

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

const noFollowFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK | syscall.O_CLOEXEC

func Stat(name string) (fs.FileInfo, error)  { return os.Stat(name) }
func Lstat(name string) (fs.FileInfo, error) { return os.Lstat(name) }

func validateHandle(f *os.File) error {
	_, err := f.Stat()
	return err
}

// LinkCount inspects the open handle, including aliases created after pinning.
func LinkCount(f *os.File) (uint64, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	info, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("platform cannot count file links")
	}
	//nolint:unconvert // Nlink widths differ between Darwin and Linux.
	return uint64(info.Nlink), nil
}
