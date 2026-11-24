//go:build !windows

package devcouncil

import "path/filepath"

// realDir is the directory path names with every symbolic link followed.
func realDir(path string) (string, error) { return filepath.EvalSymlinks(path) }
