package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/serve"
)

func TestServeRejectsAmbiguousListenFlagsBeforeStarting(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "s")
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"no path":             {[]string{"--listen"}, "--listen"},
		"relative path":       {[]string{"--listen", "run/s"}, "--listen"},
		"nul in the path":     {[]string{"--listen", abs + "\x00"}, "--listen"},
		"twice":               {[]string{"--listen", abs, "--listen", abs}, "--listen"},
		"idle without listen": {[]string{"--idle-exit", "1m"}, "--idle-exit applies to --listen"},
		"idle without value":  {[]string{"--listen", abs, "--idle-exit"}, "--idle-exit"},
		"idle zero":           {[]string{"--listen", abs, "--idle-exit", "0s"}, "--idle-exit"},
		"idle negative":       {[]string{"--listen", abs, "--idle-exit", "-1m"}, "--idle-exit"},
		"idle not a duration": {[]string{"--listen", abs, "--idle-exit", "5"}, "--idle-exit"},
		"idle past the bound": {[]string{"--listen", abs, "--idle-exit", "25h"}, "--idle-exit"},
		"idle twice":          {[]string{"--listen", abs, "--idle-exit", "1m", "--idle-exit", "1m"}, "--idle-exit"},
	} {
		t.Run(name, func(t *testing.T) {
			err := serveCommand(io.Discard, nil, c.args)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want a refusal naming %q", err, c.want)
			}
			if _, statErr := os.Lstat(abs); statErr == nil {
				t.Fatal("a refused invocation left a socket behind")
			}
		})
	}
}

func TestServeUsageDocumentsTheSocketMode(t *testing.T) {
	var out bytes.Buffer
	if err := serveCommand(&out, nil, []string{"--help"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--listen", "--idle-exit", "0700", "0600", "E_CONTROLLER_BUSY", "serve.takeover", "serve.stop_all", "serve.status", "SIGHUP"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the usage does not mention %q", want)
		}
	}
	// stdio is still the default and is still described first.
	if i, j := strings.Index(out.String(), "NDJSON on stdin/stdout"), strings.Index(out.String(), "--listen serves"); i < 0 || j < 0 || i > j {
		t.Fatalf("the usage describes the socket mode before stdio, or not at all:\n%s", out.String())
	}
}

// serveCommand with --listen serves the protocol on the socket, writes nothing to
// the stdout it was given, survives the signals that would end a detached
// process, and exits cleanly on its idle period.
func TestServeCommandListensOnASocketAndExitsWhenIdle(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("socket-served mode needs peer credentials, which only darwin and linux provide")
	}
	dir, err := os.MkdirTemp("", "mvl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s")
	var stdout bytes.Buffer
	done := make(chan error, 1)
	reg := newTestRegistry(t)
	go func() { done <- serveCommand(&stdout, reg, []string{"--listen", path, "--idle-exit", "400ms"}) }()

	var conn net.Conn
	deadline := time.Now().Add(10 * time.Second)
	for conn == nil {
		c, dialErr := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "unix", path)
		if dialErr == nil {
			conn = c
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server never listened: %v", dialErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := conn.Write([]byte(`{"id":"1","op":"hello","params":{"protocol":1}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var resp serve.Response
	if err := json.Unmarshal(line, &resp); err != nil || !resp.OK {
		t.Fatalf("hello over the socket: %s %v", line, err)
	}
	var hello serve.HelloResult
	if err := json.Unmarshal(resp.Result, &hello); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, op := range hello.Ops {
		found[op] = true
	}
	if !found[serve.OpServeStatus] || !found[serve.OpServeStopAll] || !found[serve.OpServeTakeover] {
		t.Fatalf("the socket server does not advertise its own ops: %v", hello.Ops)
	}
	// Connected, it stays up past the idle period.
	time.Sleep(1200 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("exited with a client attached: %v", err)
	default:
	}
	_ = conn.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("an idle exit is not an error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the server did not exit after its idle period")
	}
	if stdout.Len() != 0 {
		t.Fatalf("socket mode wrote to stdout: %q", stdout.String())
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("the socket outlived the server: %v", err)
	}
}
