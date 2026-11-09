//go:build unix

package codingagent

import (
	"os"
	"syscall"
)

// homeDirProblem reports why the resolved HOME cannot anchor seat routing, or
// "" when it can: it must be owned by the effective user and not writable by
// group or others (the sticky bit does not make a world-writable directory
// private to its owner for this purpose).
func homeDirProblem(resolved string) string {
	info, err := os.Stat(resolved)
	if err != nil {
		return homeUnusable
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return homeUnusable
	}
	if int64(st.Uid) != int64(os.Geteuid()) {
		return homeNotOwned
	}
	if info.Mode().Perm()&0o022 != 0 {
		return homeWorldWrtable
	}
	return ""
}
