//go:build unix && !darwin && !linux

package codingagent

import "errors"

// groupMembers is unsupported on the other unixes: there is no member listing
// here, so the snapshot always fails and the group probe keeps its kill(-pid, 0)
// behaviour, which is to fail closed (a group that answers is the agent's).
func groupMembers(pgid int) ([]groupMember, error) {
	return nil, errors.New("listing process group members is not supported on this platform")
}
