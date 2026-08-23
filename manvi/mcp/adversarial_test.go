package mcp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildFakeMCPServer compiles a tiny JSON-RPC server that speaks enough of the
// protocol for Initialize, tools/list and tools/call, with a configurable
// defect injected. Speaking real stdio keeps the client path identical to
// production; the defects are ones a well-behaved pipe still carries.
func buildFakeMCPServer(t *testing.T, defect string) ServerConfig {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	bin := filepath.Join(dir, "fakeserver")
	// Per-run marker directory: a marker left in the shared temp root made
	// the second run of a test skip its own scripted death.
	markerDir := filepath.Join(dir, "markers")
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	script := fmt.Sprintf(`package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)


func main() {
	marker := filepath.Join(%q, %q)
	r := bufio.NewReader(os.Stdin)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		var q map[string]any
		if json.Unmarshal([]byte(line), &q) != nil {
			continue
		}
		id := 0
		if f, ok := q["id"].(float64); ok {
			id = int(f)
		}
		method, _ := q["method"].(string)
		switch {
		case method == "initialize":
			reply(id, map[string]any{"protocolVersion": "2025-06-18"})
		case strings.Contains(method, "tools/list") && %q == "oversize_then_ok":
			os.Stdout.WriteString(strings.Repeat("x", 17*1024*1024) + "\n")
			reply(id, map[string]any{"tools": []any{}})
		case strings.Contains(method, "tools/call") && %q == "die_midcall":
			if _, err := os.Stat(marker); os.IsNotExist(err) {
				os.WriteFile(marker, []byte("died"), 0o644)
				os.Exit(0)
			}
			reply(id, map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}})
		case strings.Contains(method, "tools/call"):
			reply(id, map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}})
		case strings.Contains(method, "tools/list"):
			reply(id, map[string]any{"tools": []any{}})
		}
	}
}

func reply(id int, result map[string]any) {
	payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	fmt.Fprintln(os.Stdout, string(payload))
}
`, markerDir, "mcp-died-"+defect, defect, defect)
	if err := os.WriteFile(src, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", bin, src)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building fake server: %v\n%s", err, out)
	}
	return ServerConfig{Name: "fake", Command: bin}
}

// TestAnOversizedFrameDoesNotKillTheSession pins the fix: one 17 MiB frame
// from a broken server used to end the read loop permanently — scanner.Err was
// never checked — and every later call failed with "server exited
// unexpectedly" until process restart. The oversized line is now drained and
// skipped, and the session continues.
func TestAnOversizedFrameDoesNotKillTheSession(t *testing.T) {
	cfg := buildFakeMCPServer(t, "oversize_then_ok")
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var first ListToolsResult
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(ctx, "tools/list", map[string]any{}, &first); err != nil {
		t.Fatalf("the good response behind an oversized frame was lost: %v", err)
	}
	var second ListToolsResult
	if err := c.Call(ctx, "tools/list", map[string]any{}, &second); err != nil {
		t.Fatalf("a call after an oversized frame failed; the session did not survive: %v", err)
	}
	if !c.Alive() {
		t.Fatal("client reported dead after surviving an oversized frame")
	}
}

// TestManagerReplacesADeadClient: a server that dies mid-call used to stay
// cached forever (`closed` was set only by Close), so every later call failed
// until process restart. The manager now notices the corpse and respawns a
// fresh connection.
func TestManagerReplacesADeadClient(t *testing.T) {
	cfg := buildFakeMCPServer(t, "die_midcall")
	mgr := NewManager(t.TempDir())
	if err := mgr.RegisterServer(cfg); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := mgr.CallTool(ctx, "fake", "ping", nil); err == nil {
		t.Fatal("expected the first call to witness the server's death")
	}

	// The second attempt must not be answered by the corpse. The respawned
	// fake skips its own death marker past the first call, so a live reply is
	// proof of a fresh connection.
	result, err := mgr.CallTool(ctx, "fake", "ping", nil)
	if err != nil {
		t.Fatalf("the manager did not replace the dead client: %v", err)
	}
	found := false
	for _, block := range result.Content {
		if strings.Contains(fmt.Sprint(block), "ok") {
			found = true
		}
	}
	if !found {
		t.Fatalf("respawned server did not answer: %+v", result)
	}
}
