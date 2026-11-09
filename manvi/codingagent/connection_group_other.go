//go:build !unix

package codingagent

import "errors"

// groupAlive is always false here: DevCouncil's proc.ConfigureGroup and
// proc.KillGroup are no-ops off unix, so the agent has no process group to
// probe, and close() keeps its previous behaviour of trusting the direct
// child's Wait. A Windows Job Object that can answer for descendants arrives
// with the DevCouncil pin that provides it.
func groupAlive(pid int) bool { return false }

// groupMembers is never reached off unix: groupAlive is always false, so
// sweepGroup never asks for a snapshot.
func groupMembers(pgid int) ([]groupMember, error) {
	return nil, errors.New("process groups are not supported on this platform")
}
