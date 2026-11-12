//go:build unix

package storeguard

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// settle bounds how long an abandoned start, which a client closes
// asynchronously, gets to finish ending before the children count as leaked.
const settle = 5 * time.Second

// reapLeaked waits up to settle for this process's dcstore children to
// disappear. Any that remain are returned and sent SIGTERM; they are this
// process's own children, so no other session's process is touched. A process
// listing that cannot be read is reported as a leak: a check that could not run
// must not look like one that passed.
func reapLeaked() []child {
	deadline := time.Now().Add(settle)
	for {
		kids, err := dcstoreChildren(os.Getpid())
		if err != nil {
			return []child{{pid: 0, command: "process listing failed: " + err.Error()}}
		}
		if len(kids) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			for _, k := range kids {
				_ = syscall.Kill(k.pid, syscall.SIGTERM)
			}
			return kids
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// dcstoreChildren lists the live processes whose parent is ppid and whose
// executable is named dcstore.
func dcstoreChildren(ppid int) ([]child, error) {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,stat=,comm=").Output()
	if err != nil {
		return nil, err
	}
	var kids []child
	for _, line := range bytes.Split(out, []byte("\n")) {
		f := strings.Fields(string(line))
		if len(f) < 4 {
			continue
		}
		pid, perr := strconv.Atoi(f[0])
		parent, qerr := strconv.Atoi(f[1])
		if perr != nil || qerr != nil || parent != ppid {
			continue
		}
		if strings.HasPrefix(f[2], "Z") { // exited, awaiting reap: not a live process
			continue
		}
		command := strings.Join(f[3:], " ")
		if filepath.Base(command) == "dcstore" {
			kids = append(kids, child{pid: pid, command: command})
		}
	}
	return kids, nil
}
