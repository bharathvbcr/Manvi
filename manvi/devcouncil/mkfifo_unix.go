//go:build unix

package devcouncil

import "syscall"

func runMkfifo(path string, mode uint32) error {
	return syscall.Mkfifo(path, mode)
}
