//go:build !unix

package proc

import "os/exec"

// ConfigureGroup is a no-op where process groups are not available. WaitDelay
// still bounds the call, so the timeout holds; only the orphaned grandchild
// survives, which is the lesser of the two failures.
func ConfigureGroup(cmd *exec.Cmd) {}
