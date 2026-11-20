//go:build unix

package treefile

import (
	"io/fs"
	"syscall"
)

// nameLinks is the hard-link count the file system reported for the name in the
// same call that gave its identity, so identity and count cannot come from two
// moments. ok is false where the platform does not report it.
func nameLinks(info fs.FileInfo) (n uint64, ok bool) {
	st, isStat := info.Sys().(*syscall.Stat_t)
	if !isStat {
		return 0, false
	}
	return uint64(st.Nlink), true // #nosec G115 -- a link count is not negative
}
