//go:build darwin

package serve

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

// peerCredSupported says this platform can report who is on the other end of a
// unix socket, which socket-served mode requires.
const peerCredSupported = true

// peerUID is the uid the kernel recorded for the peer of conn when it connected
// (LOCAL_PEERCRED). It is the kernel's record, not anything the peer said.
func peerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Xucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED) // #nosec G115 -- a file descriptor fits an int
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	if cred == nil {
		return 0, errors.New("the kernel returned no peer credentials")
	}
	return cred.Uid, nil
}
