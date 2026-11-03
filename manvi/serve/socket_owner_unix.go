//go:build unix

package serve

import (
	"os"
	"syscall"
)

// fileOwner is the uid that owns the file, and whether this platform could say.
func fileOwner(info os.FileInfo) (uint32, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, false
	}
	return st.Uid, true
}
