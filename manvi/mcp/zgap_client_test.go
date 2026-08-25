package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildDeafServer compiles a server that never reads its stdin. Real stdio, no
// network: the pipe fills, and a write into it parks exactly as it would
// against a server that has stopped draining.
func buildDeafServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	bin := filepath.Join(dir, "deaf")
	const script = `package main

import "time"

func main() { time.Sleep(120 * time.Second) }
`
	if err := os.WriteFile(src, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("building the deaf server: %v\n%s", err, out)
	}
	return bin
}

// --- a timed-out stdin write left the stream unaccounted for -----------------
//
// writeFrame bounds the write by giving up on it, not by cancelling it: the
// goroutine is still inside stdin.Write when writeFrame returns, and it may
// have already delivered part of the frame into the pipe. Releasing writerMu
// at that point let the next call append a second frame behind a partial
// first — two JSON documents spliced into one line, which the server reads as
// one malformed request and which no reply ever arrives for. It also leaked a
// goroutine and its buffer per timed-out call, with nothing bounding how many.
func TestATimedOutStdinWriteRetiresTheConnection(t *testing.T) {
	c, err := NewClient(ServerConfig{Name: "deaf", Command: buildDeafServer(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// Comfortably past any pipe buffer, so the write cannot complete.
	big := strings.Repeat("x", 4<<20)

	start := time.Now()
	first := c.Notify("noop", map[string]any{"payload": big})
	firstTook := time.Since(start)
	if first == nil {
		t.Fatal("a write to a server that never reads its stdin reported success")
	}
	if firstTook < writeTimeout {
		t.Fatalf("the write returned after %s, before the %s bound; this test is not "+
			"exercising a wedge", firstTook, writeTimeout)
	}

	start = time.Now()
	second := c.Notify("noop", map[string]any{"payload": "small"})
	secondTook := time.Since(start)
	if second == nil {
		t.Fatal("a second frame was written onto a stream that may already carry a partial one")
	}
	if !strings.Contains(second.Error(), "retired") {
		t.Errorf("the second refusal does not say the connection was retired, so a caller "+
			"cannot tell a wedged stream from a slow one: %v", second)
	}
	// Refused rather than attempted: an attempt would have parked for another
	// full writeTimeout behind the same unread pipe.
	if secondTook > time.Second {
		t.Errorf("the second write waited %s, so it was attempted rather than refused; "+
			"that is one more stranded goroutine and one more stranded buffer", secondTook)
	}
}

// --- resources/read had no ceiling ------------------------------------------
//
// Every other reply from a server is capped: tools/list, resources/list, the
// frame reader itself. A resource read was not, and it is the reply that goes
// straight into a model's context with its size chosen entirely by the server.
func TestAnOversizedResourceReadIsRefusedRatherThanReturned(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	bin := filepath.Join(dir, "loud")
	script := `package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func main() {
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
		id, ok := q["id"]
		if !ok {
			continue
		}
		method, _ := q["method"].(string)
		var result map[string]any
		switch method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18"}
		case "resources/read":
			result = map[string]any{"contents": []any{map[string]any{
				"uri": "file:///huge", "text": strings.Repeat("z", 2*1024*1024)}}}
		default:
			result = map[string]any{}
		}
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		fmt.Fprintln(os.Stdout, string(payload))
	}
}
`
	if err := os.WriteFile(src, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("building the loud server: %v\n%s", err, out)
	}

	c, err := NewClient(ServerConfig{Name: "loud", Command: bin})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	contents, err := c.ReadResource(ctx, "file:///huge")
	if err == nil {
		t.Fatalf("a %d-byte resource came back with no cap and no complaint (%d parts)",
			2<<20, len(contents))
	}
	if contents != nil {
		t.Error("the refusal still handed back contents")
	}
	if !strings.Contains(err.Error(), "refused rather than truncated") {
		t.Errorf("the refusal does not say it declined to trim: %v", err)
	}
}
