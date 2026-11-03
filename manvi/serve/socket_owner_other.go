//go:build !unix

package serve

import "os"

// fileOwner reports nothing off unix: ownership checks fail closed, and
// socket-served mode does not start there anyway (see peerCredSupported).
func fileOwner(os.FileInfo) (uint32, bool) { return 0, false }
