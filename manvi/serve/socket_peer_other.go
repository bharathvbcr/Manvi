//go:build !darwin && !linux

package serve

import (
	"errors"
	"net"
)

// peerCredSupported is false where this build has no way to ask the kernel who
// is on the other end of a unix socket. Socket-served mode then refuses to
// start: a socket that authenticates nobody is not a degraded mode, it is a
// local privilege hole.
const peerCredSupported = false

func peerUID(*net.UnixConn) (uint32, error) {
	return 0, errors.New("peer credentials are not available on this platform")
}
