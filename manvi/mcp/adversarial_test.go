package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

	// The template is expanded by substitution rather than by fmt.Sprintf: the
	// scripted defects below build strings of their own, and a stray verb in
	// the generated source would be eaten by the outer format call.
	script := strings.NewReplacer(
		"__MARKERDIR__", strconv.Quote(markerDir),
		"__MARKER__", strconv.Quote("mcp-died-"+defect),
		"__DEFECT__", strconv.Quote(defect),
	).Replace(`package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const defect = __DEFECT__

func main() {
	marker := filepath.Join(__MARKERDIR__, __MARKER__)
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
		case method == "notifications/initialized" && defect == "stdin_stall":
			// Stop draining stdin without dying. The client's next frame is far
			// larger than the pipe buffer, so its write blocks past
			// writeTimeout and then lands anyway once this sleep ends.
			time.Sleep(9 * time.Second)
		case strings.Contains(method, "tools/list") && defect == "oversize_then_ok":
			os.Stdout.WriteString(strings.Repeat("x", 17*1024*1024) + "\n")
			reply(id, map[string]any{"tools": []any{}})
		case strings.Contains(method, "tools/list") && defect == "stderr_oversize":
			// One line past the retained-diagnostic bound, then enough further
			// stderr to fill the 64 KiB pipe buffer, and only then the answer.
			// A reader that stops on the first line never reaches the answer.
			os.Stderr.WriteString(strings.Repeat("y", 2*1024*1024) + "\n")
			for i := 0; i < 500; i++ {
				os.Stderr.WriteString("noise " + strconv.Itoa(i) +
					strings.Repeat(".", 200) + "\n")
			}
			reply(id, map[string]any{"tools": []any{}})
		case strings.Contains(method, "tools/call") && defect == "stdout_dies":
			// End stdout without answering and stay alive holding stdin open:
			// a client whose read loop ended is not a process that exited. The
			// marker is written only on stdin EOF, so its presence is proof
			// somebody closed this child down rather than abandoning it.
			os.Stdout.Close()
			io.Copy(io.Discard, r)
			os.WriteFile(marker, []byte("stdin closed"), 0o644)
			os.Exit(0)
		case strings.Contains(method, "tools/call") && defect == "die_midcall":
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
	var wire any = id
	if defect == "string_id" {
		// JSON-RPC 2.0 permits a string id, and only requires the server to
		// echo back what it was sent.
		wire = strconv.Itoa(id)
	}
	payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": wire, "result": result})
	fmt.Fprintln(os.Stdout, string(payload))
}
`)
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

// A replaced client must be closed, not abandoned.
//
// Client() replaces a cached client whose stdout ended, but it used to
// overwrite the map entry and drop the corpse on the floor. Nothing then closed
// its stdin, nothing Wait()ed it, and it was no longer in m.clients for
// CloseAll to reach — so a server that ends stdout while still running leaked a
// live process, three pipes and a goroutine on every respawn.
func TestTheClientAManagerReplacesIsClosed(t *testing.T) {
	cfg := buildFakeMCPServer(t, "stdout_dies")
	mgr := NewManager(t.TempDir())
	if err := mgr.RegisterServer(cfg); err != nil {
		t.Fatal(err)
	}
	defer mgr.CloseAll()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	corpse, err := mgr.Client(ctx, "fake")
	if err != nil {
		t.Fatal(err)
	}
	// Drive it into the scripted death: the server ends stdout without
	// answering, but keeps running with stdin open.
	if _, err := corpse.CallTool(ctx, "ping", nil); err == nil {
		t.Fatal("expected the call to witness the end of the server's stdout")
	}
	select {
	case <-corpse.Done():
	case <-ctx.Done():
		t.Fatal("the read loop never ended")
	}

	replacement, err := mgr.Client(ctx, "fake")
	if err != nil {
		t.Fatalf("the manager did not respawn: %v", err)
	}
	if replacement == corpse {
		t.Fatal("the manager handed back the dead client")
	}
	if !corpse.closed.Load() {
		t.Fatal("the replaced client was never closed: its stdin is still open, " +
			"its child was never reaped, and it is no longer in m.clients for CloseAll to reach")
	}

	// Close() closes stdin and waits for the child, so by now the abandoned
	// process must have seen EOF and exited. The marker is written on that path
	// only.
	markerDir := filepath.Join(filepath.Dir(cfg.Command), "markers")
	marker := filepath.Join(markerDir, "mcp-died-stdout_dies")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the abandoned server process is still running: nothing closed its stdin")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// One oversized stderr line must not stop stderr draining forever.
//
// bufio.Scanner stops permanently on ErrTooLong rather than skipping the token,
// so the child's stderr pipe filled, the child blocked in write(2) before it
// could answer, and every later call ran out its timeout. ErrTooLong was also
// filtered out of the diagnostics, so a reader that had stopped looked exactly
// like a reader that ran clean.
func TestAnOversizedStderrLineDoesNotWedgeTheServer(t *testing.T) {
	cfg := buildFakeMCPServer(t, "stderr_oversize")
	cfg.Timeout = 15 * time.Second
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// No deadline on the context: cfg.Timeout is what bounds the call, and it
	// is what the wedge used to burn.
	ctx := context.Background()
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}

	var res ListToolsResult
	if err := c.Call(ctx, "tools/list", map[string]any{}, &res); err != nil {
		t.Fatalf("the server never answered after one oversized stderr line; "+
			"stderr draining stopped and the child blocked writing: %v", err)
	}

	diags := c.Diagnostics()
	skipped := false
	for _, d := range diags {
		if strings.Contains(d, "stderr line skipped") {
			skipped = true
		}
	}
	if !skipped {
		t.Fatalf("a dropped stderr line left no diagnostic, so a reader that could not "+
			"read reports what a reader that read everything reports: %d entries retained", len(diags))
	}
}

// A write that outlived its bound must not be reported as a clean failure.
//
// The orphaned goroutine finishes the write the moment the child resumes
// reading, so the server receives and executes a request whose caller was told
// it had never been sent — and a retry runs a non-idempotent tool twice. The
// outcome is genuinely unknown at this layer, so it is named as unknown and the
// connection is torn down rather than left to splice the next frame into the
// abandoned one.
func TestATimedOutWriteIsNotReportedAsACleanFailure(t *testing.T) {
	cfg := buildFakeMCPServer(t, "stdin_stall")
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}

	// Larger than any pipe buffer, so the write cannot complete while the
	// child is not reading.
	blob := strings.Repeat("z", 4<<20)
	callErr := c.Call(ctx, "tools/call", map[string]any{
		"name": "ping", "arguments": map[string]any{"blob": blob},
	}, nil)
	if callErr == nil {
		t.Fatal("expected the stalled write to be reported")
	}

	var uncertain *ErrWriteUncertain
	if !errors.As(callErr, &uncertain) {
		t.Fatalf("a request that may already have been delivered and executed was reported "+
			"as a plain write failure, which a caller may safely retry: %v", callErr)
	}
	if !c.closed.Load() {
		t.Fatal("the connection was left open with an abandoned write in flight; " +
			"the next frame can splice into the middle of it")
	}
	if err := c.Call(ctx, "tools/list", map[string]any{}, nil); err == nil {
		t.Fatal("the poisoned connection still accepted a frame")
	}
}

// A response whose id is a JSON string must be matched, not dropped.
//
// JSON-RPC 2.0 permits a string id and only requires the server to echo the one
// it was sent. Dropping those responses made every call to such a server run
// out its full timeout with nothing recorded, so a server answering in a legal
// shape was indistinguishable from a server that never answered.
func TestAStringIDResponseIsMatched(t *testing.T) {
	cfg := buildFakeMCPServer(t, "string_id")
	cfg.Timeout = 15 * time.Second
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx := context.Background()
	start := time.Now()
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("a server echoing a string id was never matched (%s elapsed): %v",
			time.Since(start).Round(time.Millisecond), err)
	}

	var res ListToolsResult
	if err := c.Call(ctx, "tools/list", map[string]any{}, &res); err != nil {
		t.Fatalf("tools/list against a string-id server: %v", err)
	}
}

// A response with an id this client cannot correlate must leave a diagnostic.
func TestAnUnmatchableIDIsRecorded(t *testing.T) {
	c := &Client{
		cfg:     ServerConfig{Name: "fake"},
		pending: make(map[int64]chan *Response),
		doneCh:  make(chan struct{}),
		stdout:  io.NopCloser(strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":\"not-a-number\",\"result\":{}}\n")),
	}
	c.readLoop()

	found := false
	for _, d := range c.Diagnostics() {
		if strings.Contains(d, "not-a-number") {
			found = true
		}
	}
	if !found {
		t.Fatalf("an uncorrelatable response was dropped in silence: %+v", c.Diagnostics())
	}
}

// A plugin whose manifest declares no way to run must be reported.
//
// Its ToServerConfig error was discarded, so it landed in m.plugins and nowhere
// else: absent from ServerNames, from ListAllTools, and from Skipped. It
// vanished on every channel at once — the exact symptom the Skipped machinery
// exists to eliminate.
func TestAPluginWithNoRuntimeCommandIsReportedNotDropped(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".devcouncil", "plugins", "static-only")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"static-only","version":"1.0.0",
		"description":"declares tools but nothing to run",
		"runtime":{"type":"http"},
		"tools":[{"name":"lookup","description":"never reachable"}]}`
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatalf("discovery failed: %v", err)
	}
	if names := m.ServerNames(); len(names) != 0 {
		t.Fatalf("a manifest with no runtime command was registered as a server: %+v", names)
	}

	skipped := m.Skipped()
	if len(skipped) != 1 {
		t.Fatalf("Skipped() = %+v, want the unrunnable manifest reported; "+
			"an operator otherwise has no channel at all on which this plugin exists", skipped)
	}
	if !strings.Contains(skipped[0].Path, "plugin.json") {
		t.Errorf("the report does not name the file: %+v", skipped[0])
	}
	if !strings.Contains(skipped[0].Reason, "runtime command") {
		t.Errorf("the report does not say why it was dropped: %+v", skipped[0])
	}
}

// Discover's write to m.skipped must be locked like every other field.
//
// It was the one unguarded write on Manager, racing the locked read in
// Skipped(). Latent today — the only production caller runs once at startup —
// but a one-line inconsistency in an otherwise fully-locked type. This test
// only distinguishes the two builds under -race.
func TestDiscoverDoesNotRaceSkipped(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".devcouncil", "plugins", "bad")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{`), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewManager(root)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := m.AutoDiscover(context.Background()); err != nil {
				t.Errorf("discovery failed: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			_ = m.Skipped()
		}()
	}
	wg.Wait()

	if len(m.Skipped()) == 0 {
		t.Fatal("the unreadable manifest was not recorded at all")
	}
}
