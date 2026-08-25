package mcp

import (
	"os"
	"os/exec"
	"testing"
)

// --- E5: the leaf package stopped compiling off unix -------------------------
//
// client.go referenced syscall.SysProcAttr.Setpgid and syscall.Kill directly.
// Neither exists on every GOOS, so a package with no other platform dependency
// — no cgo, no build tags, nothing but stdlib — refused to build anywhere but
// unix. devcouncil had already hit this and solved it with exec_unix.go /
// exec_other.go; mcp now mirrors that split in procgroup_unix.go /
// procgroup_other.go.
//
// The wider module is unix-only for other reasons and that is fine. What must
// not happen is a NEW portability break in a leaf package, and the only check
// that catches one is an actual cross-compile: the tags are invisible to every
// test that runs on this host.
func TestTheMCPPackageBuildsOffUnix(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		// Deliberately not a skip. A check that could not run must never
		// report what a check that ran and passed reports.
		t.Fatalf("the Go toolchain is required to cross-compile this package: %v", err)
	}
	for _, target := range []struct{ goos, goarch string }{
		{"windows", "amd64"},
		{"linux", "amd64"},
		{"linux", "arm64"},
	} {
		t.Run(target.goos+"/"+target.goarch, func(t *testing.T) {
			cmd := exec.Command("go", "build", "-o", os.DevNull, ".")
			cmd.Env = append(os.Environ(),
				"GOOS="+target.goos, "GOARCH="+target.goarch, "CGO_ENABLED=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("GOOS=%s GOARCH=%s go build ./mcp/ failed: %v\n%s",
					target.goos, target.goarch, err, out)
			}
		})
	}
}
