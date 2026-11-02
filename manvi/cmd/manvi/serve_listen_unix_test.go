//go:build darwin || linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/serve"
)

// The server is meant to outlive the program that started it. The two things
// that ended a detached Go process were its starter's death closing the pipe it
// logs to (the next write raises SIGPIPE, which kills a process that writes to
// fd 1 or 2) and its terminal closing (SIGHUP). Both are sent here to the real
// binary, and it must still answer afterwards.
func TestServeListenSurvivesItsStartersStderrClosingAndAHangup(t *testing.T) {
	binary := e2eBinary(t)
	dir, err := os.MkdirTemp("", "mvl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	t.Cleanup(cancel)
	// #nosec G204 -- the binary is the one this package built into its own temp
	// directory; every argument is a literal except the temp socket path.
	cmd := exec.CommandContext(ctx, binary, "serve", "--listen", sock, "--idle-exit", "10m")
	cmd.Env = append(os.Environ(), "MANVI_HARNESS_INIT_ENABLED=false")
	cmd.Dir = dir
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = writer
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close() // the child holds its own copy
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill() // the process this test started; Wait reaps it
		}
	})

	lines := bufio.NewScanner(reader)
	ready := make(chan string, 1)
	go func() {
		for lines.Scan() {
			if strings.Contains(lines.Text(), "listening on") {
				ready <- lines.Text()
				return
			}
		}
		ready <- ""
	}()
	select {
	case line := <-ready:
		if !strings.Contains(line, sock) || !strings.Contains(line, "owner manvi-") {
			t.Fatalf("the server did not announce itself: %q", line)
		}
	case err := <-exited:
		t.Fatalf("the server exited before it listened: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the server never announced that it was listening")
	}
	// The starter is gone: nothing reads the server's stderr any more.
	_ = reader.Close()

	hello := func(conn net.Conn, r *bufio.Reader, id string) serve.Response {
		t.Helper()
		if _, err := conn.Write([]byte(`{"id":"` + id + `","op":"hello","params":{"protocol":1}}` + "\n")); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		line, err := r.ReadBytes('\n')
		if err != nil {
			t.Fatalf("hello %s: %v", id, err)
		}
		var resp serve.Response
		if err := json.Unmarshal(line, &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}
	first, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	firstReader := bufio.NewReader(first)
	if !hello(first, firstReader, "a").OK {
		t.Fatal("the first client was not served")
	}

	// A takeover is logged to stderr. With the pipe's reader gone, an unignored
	// SIGPIPE would end the process here.
	second, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	secondReader := bufio.NewReader(second)
	if _, err := second.Write([]byte(`{"id":"t","op":"serve.takeover"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	_ = second.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := secondReader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	var took serve.Response
	if err := json.Unmarshal(line, &took); err != nil || !took.OK {
		t.Fatalf("takeover: %s %v", line, err)
	}
	// And the terminal closing.
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	if !hello(second, secondReader, "b").OK {
		t.Fatal("the server stopped answering after SIGPIPE and SIGHUP")
	}
	select {
	case err := <-exited:
		t.Fatalf("the server ended: %v", err)
	default:
	}

	// A signal asks it to stop, and it leaves no socket behind.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("SIGTERM ended it with %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the server did not stop on SIGTERM")
	}
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Fatalf("the socket outlived the server: %v", err)
	}
}
