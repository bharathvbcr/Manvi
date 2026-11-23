//go:build unix

package serve

import (
	"os"
	"syscall"
)

// identOf is the identity of the directory info describes; canon is the
// fallback when the platform stat carries no device and inode.
func identOf(info os.FileInfo, canon string) rootIdent {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return rootIdent{dev: uint64(st.Dev), ino: uint64(st.Ino)} //nolint:gosec,unconvert // Stat_t field widths differ per GOOS (Ino is already uint64 on darwin, Dev is not); the numbers are opaque identifiers, never arithmetic
	}
	return rootIdent{path: canon}
}
