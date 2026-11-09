//go:build darwin

package codingagent

import "testing"

// groupMembers answers with the real members of a group, each with a start
// time, and nothing for a group that has none.
func TestGroupMembersListsTheRealMembersWithAStartTime(t *testing.T) {
	stranger := startStranger(t)
	pid := stranger.Process.Pid
	members, err := groupMembers(pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].pid != pid || members[0].start == 0 {
		t.Fatalf("groupMembers(%d) = %+v, want exactly the stranger with a start time", pid, members)
	}
	again, err := groupMembers(pid)
	if err != nil || len(again) != 1 || again[0] != members[0] {
		t.Fatalf("a second listing = %+v, %v; the start time must be stable", again, err)
	}
	empty, err := groupMembers(pid + 1<<20)
	if err != nil || len(empty) != 0 {
		t.Fatalf("groupMembers of an unused number = %+v, %v, want none", empty, err)
	}
}
