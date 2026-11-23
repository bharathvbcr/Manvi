//go:build unix

package storeguard

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/childenv"
)

// psTimeout bounds one process listing.
const psTimeout = 10 * time.Second

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
	// Bounded like every other exec boundary: a ps that never returns must not
	// hold TestMain. A timeout is an error, which reapLeaked reports as a leak.
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=,stat=,comm=")
	cmd.Env = childenv.Inherit() // ps needs no provider credential
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
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
