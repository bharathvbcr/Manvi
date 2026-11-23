//go:build unix

package main

import (
	"bufio"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// zombieLeaderWithLiveMember starts a group leader that exits at once while a
// member it forked stays alive, and that nobody waits for: the leader is a
// zombie of this process, the member is a live process of the leader's group.
// It returns the leader and the member's pid.
func zombieLeaderWithLiveMember(t *testing.T) (*exec.Cmd, int) {
	t.Helper()
	// #nosec G204 -- a literal shell command.
	leader := exec.CommandContext(t.Context(), "sh", "-c", "sleep 300 </dev/null >/dev/null 2>&1 & echo $!")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := leader.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-leader.Process.Pid, syscall.SIGKILL)
		_ = leader.Wait()
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	member, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	// The leader has printed and is exiting; wait until it is a zombie.
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, _ := exec.CommandContext(t.Context(), "/bin/ps", "-o", "stat=", "-p", strconv.Itoa(leader.Process.Pid)).Output() // #nosec G204 -- literal.
		if strings.HasPrefix(strings.TrimSpace(string(st)), "Z") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the leader %d never became a zombie", leader.Process.Pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !zadvAlive(t, member) {
		t.Fatal("the member is not alive; the scenario did not arise")
	}
	return leader, member
}

// A group leader that has exited but not been reaped is a zombie row, and its
// group can still hold live members: the sweep must end those, not skip the
// whole group because its leader is already dead.
func TestEndLeftoverChildrenKillsLiveMembersOfAZombieLeadersGroup(t *testing.T) {
	_, member := zombieLeaderWithLiveMember(t)
	_ = endLeftoverChildren()
	deadline := time.Now().Add(10 * time.Second)
	for zadvAlive(t, member) {
		if time.Now().After(deadline) {
			t.Fatalf("member %d of a zombie leader's group was left running by the sweep", member)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
