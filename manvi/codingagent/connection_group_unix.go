//go:build unix

package codingagent

import (
	"errors"
	"syscall"
)

// groupAlive says whether the process group led by pid still has a member.
// kill(-pid, 0) delivers no signal and answers only ESRCH (no such group) or
// success; EPERM means the group exists with a member this process may not
// signal, so it counts as alive, and so does a zombie awaiting its parent. Only
// ESRCH is "gone": a probe that cannot say must not read as an empty group.
//
// The group is the one startConnection created with Setpgid, so it is addressed
// by the agent's pid and the number cannot be handed to another group while any
// member exists. A descendant that called setsid or setpgid has left the group
// and is not seen here; nothing about the group can find it, and close() does
// not try to. Its children that stayed in the group are seen, and a zombie
// among them keeps the answer true until its parent reaps it or exits, which
// kill(2) cannot tell from a running member.
func groupAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return !errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH)
}
