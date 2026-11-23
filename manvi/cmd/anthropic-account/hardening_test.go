package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestChildEnvStripsAnInheritedBaseURL: ANTHROPIC_BASE_URL decides where the
// chosen organisation's key is sent. A value inherited from the caller (a
// gateway, a proxy someone left exported) would carry the key it was never
// meant to see, so the launcher must not pass it on unless asked.
func TestChildEnvStripsAnInheritedBaseURL(t *testing.T) {
	names, err := anthropicEnvNames()
	if err != nil {
		t.Fatal(err)
	}
	base := []string{"PATH=/usr/bin:/bin", "ANTHROPIC_BASE_URL=https://gateway.invalid"}
	env := envMap(childEnv(base, names, account{Name: "main"}, "sk-ant-chosen", nil, ""))
	if v, ok := env["ANTHROPIC_BASE_URL"]; ok {
		t.Errorf("an inherited ANTHROPIC_BASE_URL survived: %q", v)
	}
	if env["PATH"] != "/usr/bin:/bin" || env["ANTHROPIC_API_KEY"] != "sk-ant-chosen" {
		t.Errorf("unrelated variables or the chosen key were lost: %v", env)
	}
}

// TestKeepBaseURLPassesTheInheritedValueThrough is the flag's other half: an
// operator who runs a gateway on purpose can still say so.
func TestKeepBaseURLPassesTheInheritedValueThrough(t *testing.T) {
	h := newHarness(t, "main\n")
	if code := h.run("", "exec", "--keep-base-url", "--", "manvi"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if h.launched == nil {
		t.Fatal("nothing was launched")
	}
	if got := envMap(h.launched.env)["ANTHROPIC_BASE_URL"]; got != "https://example.invalid" {
		t.Errorf("--keep-base-url: ANTHROPIC_BASE_URL = %q, want the inherited value", got)
	}
	h2 := newHarness(t, "main\n")
	if code := h2.run("", "exec", "--", "manvi"); code != 0 {
		t.Fatalf("exit %d: %s", code, h2.stderr.String())
	}
	if _, ok := envMap(h2.launched.env)["ANTHROPIC_BASE_URL"]; ok {
		t.Error("without --keep-base-url the inherited ANTHROPIC_BASE_URL reached the child")
	}
}

// findMarkerWithin runs findMarker under a deadline, because the failure these
// tests guard against is a hang in open(2) on a named pipe, which no assertion
// after the call could ever report. The pipe is released on cleanup so a build
// that does block does not leak its goroutine into the rest of the suite.
func findMarkerWithin(t *testing.T, dir, home string, pipes ...string) (path, name string, err error) {
	t.Helper()
	t.Cleanup(func() {
		for _, p := range pipes {
			// Holding the pipe open for read and write releases any opener
			// blocked waiting for its other end.
			// #nosec G304 -- p is a pipe this test created under t.TempDir().
			if f, openErr := os.OpenFile(p, syscall.O_RDWR|syscall.O_NONBLOCK, 0); openErr == nil {
				_ = f.Close()
			}
		}
	})
	type result struct {
		path, name string
		err        error
	}
	done := make(chan result, 1)
	go func() {
		p, n, e := findMarker(dir, home)
		done <- result{p, n, e}
	}()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.path, r.name, r.err
	case <-timer.C:
		t.Fatal("findMarker did not return within 3s: it blocked reading a marker that is not a regular file")
		return "", "", nil
	}
}

func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("cannot create a named pipe here: %v", err)
	}
}

func TestAMarkerThatIsASymlinkToAPipeDoesNotBlockTheLauncher(t *testing.T) {
	home := t.TempDir()
	repo := filepath.Join(home, "proj")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	pipe := filepath.Join(t.TempDir(), "pipe")
	mkfifo(t, pipe)
	marker := filepath.Join(repo, markerName)
	if err := os.Symlink(pipe, marker); err != nil {
		t.Fatal(err)
	}
	_, name, err := findMarkerWithin(t, repo, home, pipe)
	if err == nil {
		t.Fatalf("a symlinked marker was honoured (account %q); it must be refused", name)
	}
	if !strings.Contains(err.Error(), marker) {
		t.Errorf("the refusal does not name the marker: %v", err)
	}
}

func TestAMarkerThatIsAPipeDoesNotBlockTheLauncher(t *testing.T) {
	home := t.TempDir()
	repo := filepath.Join(home, "proj")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(repo, markerName)
	mkfifo(t, marker)
	_, _, err := findMarkerWithin(t, repo, home, marker)
	if err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("a pipe marker: err = %v, want a refusal saying it is not a regular file", err)
	}
}

func TestAMarkerThatIsASymlinkToARegularFileIsRefused(t *testing.T) {
	home := t.TempDir()
	repo := filepath.Join(home, "proj")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	write(t, target, "alt\n")
	if err := os.Symlink(target, filepath.Join(repo, markerName)); err != nil {
		t.Fatal(err)
	}
	_, _, err := findMarkerWithin(t, repo, home)
	if err == nil {
		t.Fatal("a marker reached through a symlink was honoured; the marker chooses the billed organisation and must be read where it sits")
	}
	if !errors.Is(err, syscall.ELOOP) && !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("err = %v, want the refusal to say the marker is a symbolic link", err)
	}
}

// TestTheWalkStopsAtHomeEvenWhenHomeIsReachedThroughASymlink: the walk used to
// compare two textual paths, so a working directory spelled through a symlink
// to home never equalled home and the walk read the marker in home itself,
// which the registry's default is supposed to own.
func TestTheWalkStopsAtHomeEvenWhenHomeIsReachedThroughASymlink(t *testing.T) {
	realHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(realHome, markerName), "alt\n")
	if err := os.MkdirAll(filepath.Join(realHome, "Code"), 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "homelink")
	if err := os.Symlink(realHome, link); err != nil {
		t.Fatal(err)
	}
	path, name, err := findMarkerWithin(t, filepath.Join(link, "Code"), realHome)
	if err != nil || path != "" || name != "" {
		t.Fatalf("a marker in home was honoured through a symlink: path=%q name=%q err=%v", path, name, err)
	}
}

// TestAMissingRepoDirectoryMeansNoMarker: a --repo that does not exist holds no
// marker, which is an answer and not an error; the walk goes on to the
// directories above it.
func TestAMissingRepoDirectoryMeansNoMarker(t *testing.T) {
	h := newHarness(t, "main\nalt\n")
	missing := filepath.Join(h.home, "proj", "not", "there")
	if code := h.run("", "which", "--json", "--repo", missing); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "\"account\":\"main\"") {
		t.Fatalf("a missing directory did not fall back to the default: %s", h.stdout.String())
	}
	write(t, filepath.Join(h.home, "proj", markerName), "alt\n")
	if code := h.run("", "which", "--json", "--repo", missing); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "\"account\":\"alt\"") {
		t.Fatalf("the marker in an existing ancestor of a missing directory was not found: %s", h.stdout.String())
	}
}
