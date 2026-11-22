package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Adversarial pass on b26-creds: the marker reader and the inherited env.
// Every test here PASSES when the change holds.

func advB26Repo(t *testing.T) (home, repo string) {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home = resolved
	repo = filepath.Join(home, "proj")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	return home, repo
}

func TestAdvB26CredsMarkerThatIsADirectoryIsRefused(t *testing.T) {
	home, repo := advB26Repo(t)
	if err := os.Mkdir(filepath.Join(repo, markerName), 0o750); err != nil {
		t.Fatal(err)
	}
	_, name, err := findMarkerWithin(t, repo, home)
	if err == nil {
		t.Fatalf("a directory marker was honoured as account %q", name)
	}
}

func TestAdvB26CredsMarkerThatIsASocketIsRefused(t *testing.T) {
	// sun_path is about 104 bytes on darwin, so the tree lives under a short /tmp dir.
	dir, err := os.MkdirTemp("/tmp", "advb26")
	if err != nil {
		t.Skipf("no short temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	home, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(home, "p")
	if err := os.Mkdir(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", filepath.Join(repo, markerName))
	if err != nil {
		t.Skipf("cannot bind a unix socket here: %v", err)
	}
	defer func() { _ = l.Close() }()
	_, name, err := findMarkerWithin(t, repo, home)
	if err == nil {
		t.Fatalf("a socket marker was honoured as account %q", name)
	}
}

func TestAdvB26CredsMarkerSymlinkLoopAndDanglingAreRefused(t *testing.T) {
	for _, kind := range []string{"loop", "dangling"} {
		t.Run(kind, func(t *testing.T) {
			home, repo := advB26Repo(t)
			marker := filepath.Join(repo, markerName)
			target := marker + ".x"
			if kind == "dangling" {
				target = filepath.Join(t.TempDir(), "gone")
			} else if err := os.Symlink(marker, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, marker); err != nil {
				t.Fatal(err)
			}
			_, name, err := findMarkerWithin(t, repo, home)
			if err == nil {
				t.Fatalf("a %s symlink marker produced account %q and no error: the repository's symlink was walked past", kind, name)
			}
		})
	}
}

// TestAdvB26CredsMarkerThroughASymlinkedParentIsRefused: O_NOFOLLOW only guards
// the last path component. A repository directory that is itself a symlink
// leads the read to a marker that does not sit in the repository.
func TestAdvB26CredsMarkerThroughASymlinkedParentIsRefused(t *testing.T) {
	home, repo := advB26Repo(t)
	elsewhere := t.TempDir()
	write(t, filepath.Join(elsewhere, markerName), "alt\n")
	link := filepath.Join(repo, "sub")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	_, name, err := findMarkerWithin(t, link, home)
	if err == nil && name != "" {
		t.Fatalf("a marker reached through a symlinked parent directory was honoured as %q (it sits outside the tree the operator is in)", name)
	}
}

// TestAdvB26CredsOversizedMarkerIsBoundedAndRefused: a sparse 4 GiB marker must
// neither be slurped nor honoured.
func TestAdvB26CredsOversizedMarkerIsBoundedAndRefused(t *testing.T) {
	home, repo := advB26Repo(t)
	marker := filepath.Join(repo, markerName)
	// #nosec G304 -- marker is under this test's t.TempDir().
	f, err := os.Create(marker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("alt "); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(4 << 30); err != nil {
		t.Skipf("cannot make a sparse file: %v", err)
	}
	_ = f.Close()
	start := time.Now()
	_, name, err := findMarkerWithin(t, repo, home)
	if err == nil {
		t.Fatalf("an oversized marker was honoured as %q", name)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("reading an oversized marker took %v: the read is not bounded", time.Since(start))
	}
}

// TestAdvB26CredsMarkerSwapRaceNeverFollowsOrBlocks flips the marker path between
// a regular file, a symlink to a file naming a different account, and a named
// pipe while findMarker runs. Whatever it returns must be the regular file's
// account or an error, never the symlink's target, and it must never block.
func TestAdvB26CredsMarkerSwapRaceNeverFollowsOrBlocks(t *testing.T) {
	home, repo := advB26Repo(t)
	marker := filepath.Join(repo, markerName)
	evil := filepath.Join(t.TempDir(), "evil")
	write(t, evil, "evil-account\n")
	good := filepath.Join(repo, "good")
	write(t, good, "good-account\n")
	pipe := filepath.Join(repo, "pipe")
	mkfifo(t, pipe)
	write(t, marker, "good-account\n")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tmp := marker + ".swap"
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Remove(tmp)
			switch i % 3 {
			case 0:
				_ = os.Symlink(evil, tmp)
			case 1:
				_ = os.Symlink(pipe, tmp)
			default:
				_ = os.Symlink(good, tmp)
			}
			_ = os.Rename(tmp, marker)
		}
	}()
	defer func() { close(stop); wg.Wait() }()

	for i := 0; i < 1500; i++ {
		_, name, err := findMarkerWithin(t, repo, home, pipe)
		if err == nil && name == "evil-account" {
			t.Fatalf("iteration %d: the swapped-in symlink's target was honoured", i)
		}
	}
}

// TestAdvB26CredsOnlyBaseURLIsAddedToTheStripSet pins exactly what the change
// strips and what it keeps. Manvi's own anthropic adapter reads
// ANTHROPIC_BASE_URL (cmd/manvi/providers.go) and no other ANTHROPIC_* endpoint
// variable; names other CLIs read are logged, not asserted.
func TestAdvB26CredsStripSetIsExactlyCredentialsPlusBaseURL(t *testing.T) {
	names, err := anthropicEnvNames()
	if err != nil {
		t.Fatal(err)
	}
	base := []string{
		"PATH=/usr/bin", "ANTHROPIC_BASE_URL=https://gw.invalid", "ANTHROPIC_BASE_URL=https://gw2.invalid",
		"ANTHROPIC_AUTH_TOKEN=tok", "ANTHROPIC_API_KEY=k", "ANTHROPIC_API_URL=https://api-url.invalid",
		"ANTHROPIC_UNIX_SOCKET=/tmp/s", "ANTHROPIC_CUSTOM_HEADERS=X: y",
		"ANTHROPIC_BEDROCK_BASE_URL=https://b.invalid", "ANTHROPIC_VERTEX_BASE_URL=https://v.invalid",
		"ANTHROPIC_FOUNDRY_BASE_URL=https://f.invalid", "HTTPS_PROXY=http://proxy.invalid:3128",
	}
	env := envMap(childEnv(base, names, account{Name: "main"}, "sk-chosen", nil, ""))
	for _, gone := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN"} {
		if _, ok := env[gone]; ok {
			t.Errorf("%s survived", gone)
		}
	}
	if env["ANTHROPIC_API_KEY"] != "sk-chosen" {
		t.Errorf("the chosen key was lost: %q", env["ANTHROPIC_API_KEY"])
	}
	for _, other := range []string{
		"ANTHROPIC_API_URL", "ANTHROPIC_UNIX_SOCKET", "ANTHROPIC_CUSTOM_HEADERS",
		"ANTHROPIC_BEDROCK_BASE_URL", "ANTHROPIC_VERTEX_BASE_URL", "ANTHROPIC_FOUNDRY_BASE_URL", "HTTPS_PROXY",
	} {
		if v, ok := env[other]; ok {
			t.Logf("inherited %s=%q reaches the child (not read by manvi; unverified for other CLIs)", other, v)
		}
	}
}

func TestAdvB26CredsKeepBaseURLKeepsOnlyTheBaseURL(t *testing.T) {
	h := newHarness(t, "main\n")
	if code := h.run("", "exec", "--keep-base-url", "--", "manvi"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	env := envMap(h.launched.env)
	if env["ANTHROPIC_BASE_URL"] != "https://example.invalid" {
		t.Errorf("--keep-base-url dropped the value: %q", env["ANTHROPIC_BASE_URL"])
	}
	for _, k := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY_FALLBACK", "ANTHROPIC_WORKSPACE_ID_FALLBACK"} {
		if v, ok := env[k]; ok && strings.Contains(v, "inherited") {
			t.Errorf("--keep-base-url let inherited %s=%q through", k, v)
		}
	}
	if env["ANTHROPIC_API_KEY"] == "sk-ant-inherited" {
		t.Error("--keep-base-url let the inherited key through")
	}
}

// TestAdvB26CredsMissingRepoDirIsNotAHardError records a behaviour change: the
// walk now resolves each directory, so a --repo that does not exist is an
// error where it used to be "no marker". Logged, not asserted: either is
// defensible, but it is new.
func TestAdvB26CredsMissingRepoDirBehaviour(t *testing.T) {
	h := newHarness(t, "main\n")
	code := h.run("", "which", "--repo", filepath.Join(h.home, "does", "not", "exist"))
	t.Logf("which --repo <missing>: exit %d stderr=%q", code, h.stderr.String())
	_ = syscall.ENOENT
}
