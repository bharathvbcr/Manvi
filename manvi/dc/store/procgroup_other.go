//go:build !unix

package store

import "os/exec"

// configureProcessGroup is a no-op where process groups are not available.
// WaitDelay still bounds the call, so the timeout holds; only the orphaned
// grandchild survives, which is the lesser of the two failures.
func configureProcessGroup(cmd *exec.Cmd) {}
