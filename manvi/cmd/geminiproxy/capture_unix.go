//go:build unix

package main

import (
	"os"
	"syscall"
)

// noFollow makes opening a path that is a symlink fail instead of opening its
// target.
const noFollow = syscall.O_NOFOLLOW

// ownedByCurrentUser reports whether info names a file this user owns.
func ownedByCurrentUser(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}
