//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

// userConfigLinkCount is how many directory entries name the file at path.
func userConfigLinkCount(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("no link count for %s", path)
	}
	return uint64(st.Nlink), nil
}
