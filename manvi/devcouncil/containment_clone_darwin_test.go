//go:build darwin

package devcouncil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const (
	cloneHelperSrc = "MANVI_TEST_CLONE_SRC"
	cloneHelperDst = "MANVI_TEST_CLONE_DST"
)

// TestCloneHelperProcess is not a test: the clone tests below run the test
// binary under the seatbelt profile with these variables set, so the clonefile
// call is made from inside the sandbox. Without them it does nothing.
func TestCloneHelperProcess(t *testing.T) {
	src, dst := os.Getenv(cloneHelperSrc), os.Getenv(cloneHelperDst)
	if src == "" || dst == "" {
		t.Skip("not a clone helper invocation")
	}
	if err := unix.Clonefile(src, dst, 0); err != nil {
		fmt.Println("CLONE-FAILED:", err)
		os.Exit(3)
	}
	fmt.Println("CLONE-OK")
	os.Exit(0)
}

// cloneCommand is the shell command that runs the helper to clone src to dst.
func cloneCommand(t *testing.T, src, dst string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	return fmt.Sprintf("%s=%s %s=%s %s -test.run='^TestCloneHelperProcess$' 2>&1",
		cloneHelperSrc, q(src), cloneHelperDst, q(dst), q(exe))
}

// clonefile copies a directory by reference, and the copy's files no longer
// match a denied path. Cloning the secret's parent directory, or the repository
// itself, must be refused like a rename of either is.
func TestContainmentStopsCloneLive(t *testing.T) {
	requireSeatbelt(t)
	fresh := func() string {
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		writeTree(t, root, map[string]string{".env": fakeToken, "cfg/.env": fakeToken, "src/a.go": "package a\n"})
		return root
	}
	// Uncontained, each clone works, so a refusal below is the profile's.
	for name, pair := range map[string][2]string{"parent dir": {"cfg", "cfgc"}, "file": {".env", "envc"}} {
		root := fresh()
		if err := unix.Clonefile(filepath.Join(root, pair[0]), filepath.Join(root, pair[1]), 0); err != nil {
			t.Fatalf("uncontained clone of %s failed (%v): the attack below proves nothing", name, err)
		}
	}
	attacks := map[string]struct{ src, dst string }{
		"clone the parent directory": {"cfg", "cfgc"},
		"clone the file":             {".env", "envc"},
		"clone the nested file":      {"cfg/.env", "nested"},
		"clone the repository root":  {"", "../REPOC"},
	}
	for name, a := range attacks {
		root := fresh()
		src := filepath.Join(root, a.src)
		dst := filepath.Join(root, a.dst)
		out, _ := runContained(t, root, cloneCommand(t, src, dst)+"; cat '"+dst+"/.env' '"+dst+"/cfg/.env' '"+dst+"' 2>&1")
		_ = os.RemoveAll(dst)
		if strings.Contains(out, fakeToken) || strings.Contains(out, "CLONE-OK") {
			t.Errorf("%s: clonefile succeeded under containment: %q", name, out)
		}
	}
	// Ordinary work is untouched by the clone deny.
	root := fresh()
	out, code := runContained(t, root,
		"git init -q . && git add src/a.go && git -c user.email=a@b -c user.name=n commit -qm x && git status --short && git checkout -q -b b && "+
			"cp -R src srccopy && cp -Rc src srcclone && cat srcclone/a.go")
	if code != 0 || !strings.Contains(out, "package a") {
		t.Fatalf("ordinary work under containment failed (exit %d): %s", code, out)
	}
	// A Go build writes its cache and output through the same profile.
	writeTree(t, root, map[string]string{"go.mod": "module scratch\n\ngo 1.21\n", "m.go": "package main\n\nfunc main() { println(\"BUILT\") }\n"})
	out, code = runContained(t, root, "CGO_ENABLED=0 go build -o scratch.bin . && ./scratch.bin 2>&1")
	if code != 0 || !strings.Contains(out, "BUILT") {
		t.Fatalf("go build under containment failed (exit %d): %s", code, out)
	}
	// Cloning a directory that holds no secret, and that is no ancestor of one, still works.
	out, _ = runContained(t, root, cloneCommand(t, filepath.Join(root, "src"), filepath.Join(root, "srcclone2")))
	if !strings.Contains(out, "CLONE-OK") {
		t.Fatalf("cloning an ordinary directory was refused: %s", out)
	}
}
