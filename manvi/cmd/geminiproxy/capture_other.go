//go:build !unix

package main

import "os"

// noFollow has no equivalent flag here; the regular-file check in openCapture
// still refuses a device or directory.
const noFollow = 0

// ownedByCurrentUser cannot be decided from FileInfo on this platform, so it
// does not refuse.
func ownedByCurrentUser(os.FileInfo) bool { return true }
