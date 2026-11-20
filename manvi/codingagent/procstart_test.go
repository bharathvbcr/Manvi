//go:build linux || darwin

package codingagent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestProcessStartIsStableForALiveProcess(t *testing.T) {
	first, err := ProcessStart(os.Getpid())
	if err != nil || first <= 0 {
		t.Fatalf("ProcessStart(self) = %d, %v", first, err)
	}
	if second, err := ProcessStart(os.Getpid()); err != nil || second != first {
		t.Fatalf("ProcessStart(self) changed: %d then %d (%v)", first, second, err)
	}
}

func TestProcessStartTellsAReapedProcessFromALiveOne(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sleep", "30") //nolint:gosec // G204: literal command and argument
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	alive, err := ProcessStart(pid)
	if err != nil || alive <= 0 {
		t.Fatalf("ProcessStart(live child) = %d, %v", alive, err)
	}
	if self, _ := ProcessStart(os.Getpid()); self == alive {
		t.Fatalf("a child and its parent have the same start %d", alive)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait() // reaped; the kill's exit status is not under test
	if _, err := ProcessStart(pid); !errors.Is(err, ErrNoProcess) {
		t.Fatalf("ProcessStart(reaped child) = %v, want ErrNoProcess", err)
	}
}

func TestProcessZombieSeesAnExitedUnreapedChild(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "exit 0") //nolint:gosec // G204: literal command
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	deadline := time.Now().Add(10 * time.Second)
	for {
		zombie, err := ProcessZombie(pid)
		if err != nil {
			t.Fatalf("ProcessZombie(exited child) = %v", err)
		}
		if zombie {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the exited, unreaped child never read as a zombie")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if self, err := ProcessZombie(os.Getpid()); err != nil || self {
		t.Fatalf("ProcessZombie(self) = %v, %v", self, err)
	}
	_ = cmd.Wait() // reaped
	if _, err := ProcessZombie(pid); !errors.Is(err, ErrNoProcess) {
		t.Fatalf("ProcessZombie(reaped child) = %v, want ErrNoProcess", err)
	}
}

func TestProcessStartRefusesPidsThatAreNeverProcesses(t *testing.T) {
	for _, pid := range []int{0, -1} {
		if _, err := ProcessStart(pid); !errors.Is(err, ErrNoProcess) {
			t.Errorf("ProcessStart(%d) = %v, want ErrNoProcess", pid, err)
		}
	}
}
