//go:build !windows

package main

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"time"
)

// openRunImage opens for reading without waiting for a writer. A plain open of
// a FIFO blocks until the other end opens, and an --image path is read while
// the arguments are parsed, before any --timeout context exists.
func openRunImage(path string) (*os.File, error) {
	// #nosec G304 -- the operator named this --image path; it is read and sniffed, never executed
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

// refuseImagePath has nothing to refuse by name here: what a path is, is read
// from the file system.
func refuseImagePath(string) error { return nil }

// imageSourceAllowed is a regular file or a pipe. A shell's process
// substitution and /dev/stdin are pipes (/dev/fd/N reports as one), and a pipe
// is read under a bound rather than refused.
func imageSourceAllowed(mode fs.FileMode) bool {
	return mode.IsRegular() || mode&fs.ModeNamedPipe != 0
}

// imagePipePoll is how often a pipe with nothing to read yet is looked at again.
const imagePipePoll = 10 * time.Millisecond

// readImagePipe reads an O_NONBLOCK pipe to its end, for at most wait and no
// further than maxImageBytes+1 bytes (one over, so the caller can tell a
// stream that fits from one that does not).
//
// It reads the descriptor itself rather than through os.File: Go does not put a
// FIFO on the poller on every platform (not on darwin), so a read deadline on
// the File is not available everywhere, and a poll loop is the same on all of
// them. No data yet is EAGAIN while a writer is connected, and end-of-file
// before the first byte while none is: both mean "not yet" until the bound,
// because a named pipe's writer may open after the reader (the open that would
// have waited for it is what is not allowed to wait). End-of-file after data is
// the end of the image.
func readImagePipe(f *os.File, wait time.Duration) ([]byte, error) {
	conn, err := f.SyscallConn()
	if err != nil {
		return nil, err
	}
	var (
		data    []byte
		readErr error
		buf     = make([]byte, 64<<10)
		stop    = time.Now().Add(wait)
	)
	if err := conn.Control(func(fd uintptr) {
		for {
			n, err := syscall.Read(int(fd), buf)
			switch {
			case n > 0:
				data = append(data, buf[:n]...)
				if len(data) > maxImageBytes {
					return
				}
				if !time.Now().Before(stop) {
					readErr = errImagePipeTimeout
					return
				}
				continue
			case err == nil && len(data) > 0:
				return
			case err == nil, errors.Is(err, syscall.EAGAIN), errors.Is(err, syscall.EINTR):
			default:
				readErr = err
				return
			}
			if !time.Now().Before(stop) {
				readErr = errImagePipeTimeout
				return
			}
			time.Sleep(imagePipePoll)
		}
	}); err != nil {
		return nil, err
	}
	return data, readErr
}
