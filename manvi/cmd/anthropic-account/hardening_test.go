package main

import (
	"path/filepath"
	"strings"
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
			unblockPipe(p)
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
