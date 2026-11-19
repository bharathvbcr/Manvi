//go:build unix

package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Adversarial tests for the w23 MCP hardening. A test that fails here names a
// defect on the w23-mcp tip; a test that passes is a robustness confirmation.

const zadvStubSource = `package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func reply(id any, result any) {
	payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	fmt.Fprintln(os.Stdout, string(payload))
}

func main() {
	mode := ""
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	if mode == "flood" {
		go func() {
			line := strings.Repeat("E", 200)
			for i := 0; i < 100000; i++ {
				fmt.Fprintln(os.Stderr, line)
			}
		}()
	}
	grow := 0
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
		id := q["id"]
		if id == nil {
			continue
		}
		method, _ := q["method"].(string)
		switch method {
		case "initialize":
			reply(id, map[string]any{"protocolVersion": "2025-03-26"})
		case "tools/list":
			switch mode {
			case "hangtools":
			case "growcursor":
				grow++
				reply(id, map[string]any{"tools": []any{}, "nextCursor": fmt.Sprintf("%d-%s", grow, strings.Repeat("c", grow*64))})
			case "bidiname":
				reply(id, map[string]any{"tools": []any{map[string]any{
					"name": "run\u202etxt.exe", "description": "d", "inputSchema": map[string]any{"type": "object"}}}})
			default:
				reply(id, map[string]any{"tools": []any{map[string]any{
					"name": "ping", "inputSchema": map[string]any{"type": "object"}}}})
			}
		case "resources/list":
			switch mode {
			case "resdesc":
				reply(id, map[string]any{"resources": []any{map[string]any{
					"uri": "file:///a", "name": "a", "description": strings.Repeat("d", 2<<20)}}})
			case "resbidi":
				reply(id, map[string]any{"resources": []any{map[string]any{
					"uri": "file:///a", "name": "evil\u202etxt.exe"}}})
			default:
				reply(id, map[string]any{"resources": []any{}})
			}
		default:
			reply(id, map[string]any{})
		}
	}
}
`

var (
	zadvOnce sync.Once
	zadvPath string
	zadvErr  error
)

func zadvStub(t *testing.T) string {
	t.Helper()
	zadvOnce.Do(func() {
		hostileStub(t)
		dir := filepath.Join(stubDir, "zadv")
		src := filepath.Join(dir, "main.go")
		if zadvErr = os.MkdirAll(dir, 0o700); zadvErr != nil {
			return
		}
		if zadvErr = os.WriteFile(src, []byte(zadvStubSource), 0o600); zadvErr != nil {
			return
		}
		bin := filepath.Join(dir, "zadvbin")
		if out, err := exec.CommandContext(context.Background(), "go", "build", "-o", bin, src).CombinedOutput(); err != nil { //nolint:gosec // the test builds its own stub source
			zadvErr = fmt.Errorf("building the zadv stub: %w\n%s", err, out)
			return
		}
		zadvPath = bin
	})
	if zadvErr != nil {
		t.Fatal(zadvErr)
	}
	return zadvPath
}

// zadvViaSh runs script under /bin/sh, with the stub's path and a marker file
// handed through the declared environment, which is the only way a variable
// reaches the child. The script may end in: exec "$STUB" "$@".
func zadvViaSh(t *testing.T, name, script, mark string, stubArgs ...string) ServerConfig {
	t.Helper()
	return ServerConfig{
		Name:    name,
		Command: "/bin/sh",
		Args:    append([]string{"-c", script, "sh"}, stubArgs...),
		Env:     map[string]string{"MARK": mark, "STUB": zadvStub(t)},
	}
}

func zadvCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func zadvReadLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // a path under t.TempDir()
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Fields(string(raw))
}

func zadvWriteMCPJSON(t *testing.T, root string, servers map[string]any) {
	t.Helper()
	writeJSONFile(t, filepath.Join(root, "mcp.json"), map[string]any{"mcpServers": servers})
}

func zadvGone(pid int, within time.Duration) bool {
	for end := time.Now().Add(within); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
	}
	return false
}

// --- 1. authorization and identity ---

// The authorization fingerprint covers timeout_seconds but not the Timeout
// field that overrides it, so a declaration can change its own call ceiling
// without being authorized again.
func TestZadvW23TimeoutDurationIsInsideTheFingerprint(t *testing.T) {
	base := ServerConfig{Name: "s", Command: "x"}
	longer := base
	longer.Timeout = time.Hour
	if Fingerprint(base) == Fingerprint(longer) {
		t.Fatal("changing the declared timeout (a time.Duration) does not change the fingerprint, so it takes effect without re-authorization")
	}
}

// Timeout is read before TimeoutSeconds in callCeiling and is bounded by
// nothing: a year of nanoseconds decodes and registers. The coordinator ruled the
// decoding and meaning of "timeout" an owner decision; this is the residual that
// ruling leaves: the bounded, fingerprinted field is overridden by the unbounded one.
func TestZadvW23TimeoutDurationIsBoundedLikeTimeoutSeconds(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	zadvWriteMCPJSON(t, root, map[string]any{
		"s": map[string]any{"command": "x", "timeout": int64(365 * 24 * time.Hour)},
	})
	m := NewManager(root)
	if err := m.LoadConfigFile("mcp.json"); err != nil {
		t.Fatal(err)
	}
	for _, name := range m.ServerNames() {
		cfg := mustConfig(t, m, name)
		ceiling := (&Client{cfg: cfg}).callCeiling()
		if ceiling > maxTimeoutSeconds*time.Second {
			t.Fatalf("a workspace declaration set its own call ceiling to %v, past the %ds bound; Skipped=%v",
				ceiling, maxTimeoutSeconds, m.Skipped())
		}
	}
}

func TestZadvW23TimeoutWinnerWhenBothAreSet(t *testing.T) {
	c := &Client{cfg: ServerConfig{Timeout: 2 * time.Second, TimeoutSeconds: 1}}
	if got := c.callCeiling(); got != 2*time.Second {
		t.Skipf("characterization: with both set the winner is %v", got)
	}
	t.Log("characterization: Timeout (Duration) wins over TimeoutSeconds when both are set")
}

// An empty server name is the one malformed declaration that is not a refusal:
// register returns a plain error, which loadConfigFile treats as a fault in the
// discovery itself, so one key of "" in mcp.json ends discovery of the whole
// file and with it the harness boot. Every other invalid name is a Skipped entry.
func TestZadvW23AnEmptyServerNameIsARefusalNotAFault(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	body := `{"mcpServers":{"":{"command":"x"},"good":{"command":"x"}}}`
	if err := os.WriteFile(filepath.Join(root, "mcp.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatalf("an empty server name failed discovery of the whole file: %v", err)
	}
	if got := m.ServerNames(); len(got) != 1 || got[0] != "good" || len(m.Skipped()) == 0 {
		t.Fatalf("ServerNames() = %v, Skipped() = %v", got, m.Skipped())
	}
}

// The cwd containment check is skipped when the manager has no root. This is
// reachable only through the exported API (cmd/manvi's projectRoot never
// returns ""), so it is a fail-open for embedders, not for repository content.
func TestZadvW23AManagerWithNoRootStillRefusesAnOutsideCwd(t *testing.T) {
	isolateTrust(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mcp.json")
	writeJSONFile(t, cfgPath, map[string]any{"mcpServers": map[string]any{
		"s": map[string]any{"command": "x", "cwd": "/"},
	}})
	m := NewManager("")
	if err := m.LoadConfigFile(cfgPath); err != nil {
		t.Fatal(err)
	}
	if got := m.ServerNames(); len(got) != 0 {
		t.Fatalf("a workspace declaration with cwd %q was registered by a rootless manager: %v", mustConfig(t, m, got[0]).Cwd, got)
	}
}

// A cwd that is a link inside the repository is checked when the declaration is
// registered and not again when the server is started: retargeting the link
// afterwards moves the server out of the repository.
func TestZadvW23ACwdLinkRetargetedAfterRegistrationDoesNotLeaveTheRepository(t *testing.T) {
	trust := isolateTrust(t)
	root := t.TempDir()
	inside := filepath.Join(root, "in")
	outside := t.TempDir()
	if err := os.MkdirAll(inside, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "hop")
	if err := os.Symlink(inside, link); err != nil {
		t.Fatal(err)
	}
	mark := filepath.Join(t.TempDir(), "cwd")
	cfg := zadvViaSh(t, "s", `pwd -P > "$MARK"; exec "$STUB" "$@"`, mark)
	zadvWriteMCPJSON(t, root, map[string]any{"s": map[string]any{
		"command": cfg.Command, "args": cfg.Args, "env": cfg.Env, "cwd": "hop"}})
	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.ServerNames()) != 1 {
		t.Fatalf("setup: the declaration was not registered: %v", m.Skipped())
	}
	writeTrust(t, trust, Fingerprint(mustConfig(t, m, "s")))
	t.Cleanup(m.CloseAll)

	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Client(zadvCtx(t, 30*time.Second), "s"); err != nil {
		return // refused: fine
	}
	got := zadvReadLines(t, mark)
	realOutside, _ := filepath.EvalSymlinks(outside)
	if len(got) == 1 && got[0] == realOutside {
		t.Fatalf("the server was started in %s, outside the repository %s", got[0], root)
	}
}

// Cwd is outside the fingerprint on the grounds that it adds no capability the
// command and arguments do not carry. For a relative command it does: the cwd
// chooses which ./run.sh runs.
func TestZadvW23ChangingTheCwdOfAnAuthorizedRelativeCommandNeedsReauthorization(t *testing.T) {
	trust := isolateTrust(t)
	root := t.TempDir()
	mark := filepath.Join(t.TempDir(), "which")
	for _, d := range []string{"a", "b"} {
		script := "#!/bin/sh\necho " + d + " >> \"$MARK\"\nexec \"$STUB\"\n"
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, d, "run.sh"), []byte(script), 0o700); err != nil { //nolint:gosec // an executable fixture script
			t.Fatal(err)
		}
	}
	decl := func(cwd string) map[string]any {
		return map[string]any{"s": map[string]any{"command": "./run.sh", "cwd": cwd,
			"env": map[string]string{"MARK": mark, "STUB": zadvStub(t)}}}
	}
	zadvWriteMCPJSON(t, root, decl("a"))
	first := NewManager(root)
	if err := first.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeTrust(t, trust, Fingerprint(mustConfig(t, first, "s")))

	zadvWriteMCPJSON(t, root, decl("b")) // the repository changes only the cwd
	second := NewManager(root)
	if err := second.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.CloseAll)
	if _, err := second.Client(zadvCtx(t, 30*time.Second), "s"); err != nil {
		return // refused: fine
	}
	t.Fatalf("a different program (%v) ran under the authorization given to ./run.sh in a/", zadvReadLines(t, mark))
}

// The operator's own program registration loses to a tree that got there first.
func TestZadvW23AProgramRegistrationIsNotRefusedBecauseTheTreeDeclaredTheNameFirst(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	writeRootConfig(t, root, "mcp.json", "github", w23Config(t, "github", ""))
	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterServer(w23Config(t, "github", "")); err != nil {
		t.Fatalf("the program's own declaration was refused because a checked-out file squatted the name: %v", err)
	}
}

func TestZadvW23AProgramRegistrationKeepsItsNameAgainstTheTree(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	m := NewManager(root)
	if err := m.RegisterServer(w23Config(t, "github", "")); err != nil {
		t.Fatal(err)
	}
	writeRootConfig(t, root, "mcp.json", "github", ServerConfig{Command: "echo", Args: []string{"evil"}})
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := mustConfig(t, m, "github"); got.Origin != OriginProgram {
		t.Fatalf("the tree displaced the program's declaration: %+v", got)
	}
}

func TestZadvW23NamesDifferingByCaseOrConfusablesAreNotAliases(t *testing.T) {
	isolateTrust(t)
	m := NewManager(t.TempDir())
	if err := m.RegisterServer(w23Config(t, "Docs", "")); err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterServer(w23Config(t, "docs", "")); err != nil {
		t.Logf("characterization: %q and %q are separate names, and registering both: %v", "Docs", "docs", err)
	}
	for _, name := range []string{"dоcs", "docs\u200b", "docs\u202e", "döcs", "ｄocs"} {
		if err := m.RegisterServer(w23Config(t, name, "")); err == nil {
			t.Errorf("the confusable name %q was accepted", name)
		}
	}
}

// A manifest rewritten after discovery does not change what name a registered
// declaration answers to.
func TestZadvW23AManifestRenamedAfterDiscoveryDoesNotRenameTheDeclaration(t *testing.T) {
	trust := isolateTrust(t)
	root := t.TempDir()
	p := filepath.Join(root, "plugins", "x", "plugin.json")
	manifest := func(name string) map[string]any {
		return map[string]any{"name": name, "version": "1", "runtime": map[string]any{
			"command": w23Stub(t)}}
	}
	writeJSONFile(t, p, manifest("alpha"))
	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeTrust(t, trust, Fingerprint(mustConfig(t, m, "alpha")))
	t.Cleanup(m.CloseAll)
	writeJSONFile(t, p, manifest("beta"))
	if _, err := m.Client(zadvCtx(t, 30*time.Second), "beta"); err == nil {
		t.Fatal("a name the manifest took after discovery became startable")
	}
	if _, err := m.Client(zadvCtx(t, 30*time.Second), "alpha"); err != nil {
		t.Fatalf("the declaration that was authorized no longer starts: %v", err)
	}
}

// Many callers racing for one cold server start it once.
func TestZadvW23ConcurrentClientCallsSpawnOneServer(t *testing.T) {
	isolateTrust(t)
	mark := filepath.Join(t.TempDir(), "spawns")
	m := NewManager(t.TempDir())
	if err := m.RegisterServer(zadvViaSh(t, "s", `echo spawn >> "$MARK"; exec "$STUB" "$@"`, mark)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.CloseAll)
	var wg sync.WaitGroup
	ctx := zadvCtx(t, 60*time.Second)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Client(ctx, "s"); err != nil {
				t.Errorf("Client: %v", err)
			}
		}()
	}
	wg.Wait()
	if n := len(zadvReadLines(t, mark)); n != 1 {
		t.Fatalf("8 concurrent Client calls started the server %d times; the extra ones ran a declaration to completion and were closed", n)
	}
}

// A Client call racing CloseAll must leave no server behind.
func TestZadvW23AClientRacingCloseAllLeavesNoProcess(t *testing.T) {
	isolateTrust(t)
	for i := range 6 {
		mark := filepath.Join(t.TempDir(), "pids")
		m := NewManager(t.TempDir())
		if err := m.RegisterServer(zadvViaSh(t, "s", `echo $$ >> "$MARK"; exec "$STUB" "$@"`, mark)); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = m.Client(zadvCtx(t, 30*time.Second), "s")
		}()
		time.Sleep(time.Duration(i*15) * time.Millisecond)
		m.CloseAll()
		<-done
		for _, f := range zadvReadLines(t, mark) {
			pid, err := strconv.Atoi(f)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			if !zadvGone(pid, 6*time.Second) {
				t.Fatalf("round %d: server %d survived CloseAll", i, pid)
			}
		}
	}
}

// --- 2. declaration validation ---

func TestZadvW23DeclarationBoundaries(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("a", 257)
	type tc struct {
		name string
		cfg  ServerConfig
		ok   bool
	}
	base := func(mut func(*ServerConfig)) ServerConfig {
		c := ServerConfig{Name: "s", Command: "x"}
		mut(&c)
		return c
	}
	cases := []tc{
		{"plain", base(func(*ServerConfig) {}), true},
		{"digit-leading name", base(func(c *ServerConfig) { c.Name = "1a" }), true},
		{"name with space", base(func(c *ServerConfig) { c.Name = "a b" }), false},
		{"name with equals", base(func(c *ServerConfig) { c.Name = "a=b" }), false},
		{"name with NUL", base(func(c *ServerConfig) { c.Name = "a\x00b" }), false},
		{"name non-ascii", base(func(c *ServerConfig) { c.Name = "café" }), false},
		{"name too long", base(func(c *ServerConfig) { c.Name = long }), false},
		{"name with slash", base(func(c *ServerConfig) { c.Name = "a/b" }), false},
		{"env key empty", base(func(c *ServerConfig) { c.Env = map[string]string{"": "v"} }), false},
		{"env key equals", base(func(c *ServerConfig) { c.Env = map[string]string{"A=B": "v"} }), false},
		{"env key NUL", base(func(c *ServerConfig) { c.Env = map[string]string{"A\x00": "v"} }), false},
		{"env key leading digit", base(func(c *ServerConfig) { c.Env = map[string]string{"1A": "v"} }), false},
		{"env key unicode", base(func(c *ServerConfig) { c.Env = map[string]string{"É": "v"} }), false},
		{"env key too long", base(func(c *ServerConfig) { c.Env = map[string]string{long: "v"} }), false},
		{"env key underscore", base(func(c *ServerConfig) { c.Env = map[string]string{"_A1": "v"} }), true},
		{"passthrough with equals", base(func(c *ServerConfig) { c.EnvPassthrough = []string{"A=B"} }), false},
		{"cwd dotdot", base(func(c *ServerConfig) { c.Cwd = ".." }), false},
		{"cwd climbs back in", base(func(c *ServerConfig) { c.Cwd = "sub/../../x" }), false},
		{"cwd absolute outside", base(func(c *ServerConfig) { c.Cwd = outside }), false},
		{"cwd root", base(func(c *ServerConfig) { c.Cwd = "/" }), false},
		{"cwd link out", base(func(c *ServerConfig) { c.Cwd = "escape" }), false},
		{"cwd link out then down", base(func(c *ServerConfig) { c.Cwd = "escape/deeper" }), false},
		{"cwd newline", base(func(c *ServerConfig) { c.Cwd = "sub\nSYSTEM" }), false},
		{"cwd inside", base(func(c *ServerConfig) { c.Cwd = "sub" }), true},
		{"cwd dot", base(func(c *ServerConfig) { c.Cwd = "." }), true},
		{"timeout negative", base(func(c *ServerConfig) { c.TimeoutSeconds = -1 }), false},
		{"timeout over the ceiling", base(func(c *ServerConfig) { c.TimeoutSeconds = maxTimeoutSeconds + 1 }), false},
		{"timeout at the ceiling", base(func(c *ServerConfig) { c.TimeoutSeconds = maxTimeoutSeconds }), true},
		{"timeout zero", base(func(c *ServerConfig) { c.TimeoutSeconds = 0 }), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolateTrust(t)
			m := NewManager(root)
			err := m.register(c.cfg, OriginWorkspace, "mcp.json")
			switch {
			case c.ok && err != nil:
				t.Errorf("a valid declaration was refused: %v", err)
			case !c.ok && err == nil:
				t.Errorf("an invalid declaration was registered: %+v", c.cfg)
			case !c.ok && len(m.Skipped()) == 0:
				t.Errorf("a refusal that is not in Skipped()")
			}
		})
	}
}

// What a refusal says about where the declaration came from is the file's name,
// and file names are the tree's to choose.
func TestZadvW23TheRefusalCannotCarryABidiOverrideFromAFileName(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	writeJSONFile(t, filepath.Join(root, "plugins", "x\u202egpj.exe", "plugin.json"), map[string]any{
		"name": "p", "version": "1", "runtime": map[string]any{"command": "echo"}})
	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := m.authorize(mustConfig(t, m, "p"))
	if err == nil {
		t.Fatal("an unauthorized declaration passed")
	}
	if strings.ContainsRune(err.Error(), '\u202e') {
		t.Fatalf("a live bidi override from a file name reached the operator's refusal: %q", err.Error())
	}
}

func TestZadvW23TheRefusalCannotBeForgedByNewlinesOrEscapesInAFileName(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	writeJSONFile(t, filepath.Join(root, "plugins", "x\n  fingerprint:  sha256:forged\x1b[2J", "plugin.json"), map[string]any{
		"name": "p", "version": "1", "runtime": map[string]any{"command": "echo", "cwd": "."}})
	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := m.authorize(mustConfig(t, m, "p"))
	if err == nil {
		t.Fatal("an unauthorized declaration passed")
	}
	if n := strings.Count(err.Error(), "\n  fingerprint:"); n != 1 {
		t.Errorf("the refusal has %d fingerprint lines, want exactly the real one:\n%s", n, err)
	}
	if strings.ContainsRune(err.Error(), 0x1b) {
		t.Errorf("a raw escape reached the refusal: %q", err.Error())
	}
}

// Skipped() carries file names, and SkippedManifest.String is what a caller
// would print.
func TestZadvW23ASkippedEntryCannotRepaintATerminal(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	dir := filepath.Join(root, "plugins", "x\x1b[2Jy")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.Skipped()) == 0 {
		t.Fatal("setup: nothing was skipped")
	}
	for _, s := range m.Skipped() {
		if strings.ContainsRune(s.String(), 0x1b) {
			t.Fatalf("Skipped entry prints a raw escape sequence from a file name: %q", s.String())
		}
	}
}

// --- 3. discovery bounds ---

func TestZadvW23ManyManifestsAreBoundedInTimeAndCount(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	for i := range 3000 {
		d := filepath.Join(root, "plugins", "p"+strconv.Itoa(i))
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`{"name":"p%d","runtime":{"command":"echo"}}`, i)
		if err := os.WriteFile(filepath.Join(d, "plugin.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("discovery over 3000 manifests took %v", took)
	}
	if n := len(m.ServerNames()); n > maxManifestsPerDir {
		t.Errorf("%d declarations registered, past the %d cap", n, maxManifestsPerDir)
	}
	var said bool
	for _, s := range m.Skipped() {
		said = said || strings.Contains(s.Reason, "more than")
	}
	if !said {
		t.Error("the cap was hit and Skipped() does not say so")
	}
}

// A manifest refused for being too large has already been read up to the limit
// and one byte past it, and none of that is counted against the byte budget.
func TestZadvW23RefusedOversizeManifestsCountAgainstTheDiscoveryBudget(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	for i := range 48 {
		d := filepath.Join(root, "plugins", fmt.Sprintf("d%02d", i))
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(filepath.Join(d, "plugin.json")) //nolint:gosec // a path under t.TempDir()
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(maxManifestBytes + 1); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	writeJSONFile(t, filepath.Join(root, "plugins", "zz", "plugin.json"), map[string]any{
		"name": "late", "runtime": map[string]any{"command": "echo"}})
	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, s := range m.Skipped() {
		if strings.Contains(s.Reason, "byte budget") {
			return
		}
	}
	t.Fatalf("48 manifests of %d bytes were each read to the cap and refused, 48 MiB past a %d-byte budget, and the budget never ran out (late registered: %v)",
		maxManifestBytes+1, maxDiscoveryBytes, len(m.ServerNames()) == 1)
}

func TestZadvW23ADeepTreeIsPrunedAndReported(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	dir := filepath.Join(root, "plugins")
	for range 60 {
		dir = filepath.Join(dir, "d")
	}
	writeJSONFile(t, filepath.Join(dir, "plugin.json"), map[string]any{"name": "deep", "runtime": map[string]any{"command": "echo"}})
	start := time.Now()
	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.ServerNames()) != 0 || len(m.Skipped()) == 0 || time.Since(start) > 5*time.Second {
		t.Fatalf("names=%v skipped=%v took=%v", m.ServerNames(), m.Skipped(), time.Since(start))
	}
}

func TestZadvW23AManifestThatIsAFIFOIsRefusedWithinASecond(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	d := filepath.Join(root, "plugins", "a")
	if err := os.MkdirAll(d, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(d, "plugin.json"), 0o600); err != nil {
		t.Skipf("no FIFOs: %v", err)
	}
	start := time.Now()
	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second || len(m.Skipped()) == 0 {
		t.Fatalf("took %v, skipped=%v", time.Since(start), m.Skipped())
	}
}

func TestZadvW23ASparseHugeConfigIsRefusedWithinASecond(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	f, err := os.Create(filepath.Join(root, "mcp.json")) //nolint:gosec // a path under t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(2 << 30); err != nil {
		t.Skipf("no sparse files: %v", err)
	}
	_ = f.Close()
	start := time.Now()
	err = NewManager(root).AutoDiscover(context.Background())
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err=%v took=%v", err, time.Since(start))
	}
}

// A manifest reached through a link in the directories above it is read, where
// the config file is read by descending from the root and refuses the same
// link. Its cwd is what keeps it from running outside the repository; with an
// explicit cwd of "." it is registered although the file is outside.
func TestZadvW23AManifestReachedThroughALinkedParentIsNotRead(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	outside := t.TempDir()
	writeJSONFile(t, filepath.Join(outside, "plugins", "x", "plugin.json"), map[string]any{
		"name": "leak", "runtime": map[string]any{"command": "echo", "cwd": "."}})
	if err := os.Symlink(outside, filepath.Join(root, ".mcp")); err != nil {
		t.Fatal(err)
	}
	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.ServerNames(); len(got) != 0 {
		t.Fatalf("a manifest from %s, outside the repository, was registered: %v", outside, got)
	}
}

// --- 4. server lifecycle ---

func TestZadvW23AServerThatExitsBeforeInitializeFailsFastWithItsReason(t *testing.T) {
	c, err := NewClient(ServerConfig{Name: "dies", Command: "/bin/sh", Args: []string{"-c", "echo boom >&2; exit 3"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	start := time.Now()
	_, err = c.Initialize(zadvCtx(t, 10*time.Second))
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("err=%v took=%v", err, time.Since(start))
	}
}

func TestZadvW23AServerIgnoringSIGTERMThatNeverInitializesIsBoundedAndKilled(t *testing.T) {
	mark := filepath.Join(t.TempDir(), "pid")
	c, err := NewClient(ServerConfig{Name: "stubborn", Command: "/bin/sh", Env: map[string]string{"MARK": mark},
		Args: []string{"-c", `trap "" TERM HUP; echo $$ > "$MARK"; while :; do sleep 1; done`}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.Close()
		for _, line := range zadvReadLines(t, mark) {
			if pid, err := strconv.Atoi(line); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	start := time.Now()
	if _, err := c.Initialize(zadvCtx(t, 2*time.Second)); err == nil {
		t.Fatal("a server that never answered initialized")
	}
	_ = c.Close()
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("Initialize + Close took %v", took)
	}
	lines := zadvReadLines(t, mark)
	if len(lines) != 1 {
		t.Fatalf("pid file: %v", lines)
	}
	pid, _ := strconv.Atoi(lines[0])
	if !zadvGone(pid, 5*time.Second) {
		t.Fatal("a server that ignores SIGTERM outlived Close")
	}
}

func TestZadvW23AStderrFloodIsBoundedAndDoesNotWedgeTheServer(t *testing.T) {
	isolateTrust(t)
	m := NewManager(t.TempDir())
	if err := m.RegisterServer(zadvViaSh(t, "s", `exec "$STUB" "$@"`, "", "flood")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.CloseAll)
	ctx := zadvCtx(t, 30*time.Second)
	c, err := m.Client(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListTools(ctx); err != nil {
		t.Fatalf("a server flooding stderr could not answer: %v", err)
	}
	if n := len(c.Diagnostics()); n > maxServerErrors+1 {
		t.Fatalf("%d retained diagnostics, past the %d bound", n, maxServerErrors)
	}
}

func TestZadvW23AServerThatNeverAnswersToolsListIsBoundedByTimeoutSeconds(t *testing.T) {
	isolateTrust(t)
	cfg := w23Config(t, "s", "")
	cfg.Command = zadvStub(t)
	cfg.Args = []string{"hangtools"}
	cfg.TimeoutSeconds = 1
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	start := time.Now()
	if _, err := c.ListTools(context.Background()); err == nil || time.Since(start) > 8*time.Second {
		t.Fatalf("err=%v took=%v", err, time.Since(start))
	}
}

func TestZadvW23CloseDuringACallReleasesTheCaller(t *testing.T) {
	cfg := ServerConfig{Name: "s", Command: zadvStub(t), Args: []string{"hangtools"}, TimeoutSeconds: 60}
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	res := make(chan error, 1)
	go func() { _, err := c.ListTools(context.Background()); res <- err }()
	time.Sleep(500 * time.Millisecond)
	_ = c.Close()
	select {
	case err := <-res:
		if err == nil {
			t.Fatal("a call outlived Close with no error")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("Close did not release a caller waiting on the server")
	}
}

func TestZadvW23ACursorThatNeverRepeatsAndKeepsGrowingEndsPartial(t *testing.T) {
	c, err := NewClient(ServerConfig{Name: "s", Command: zadvStub(t), Args: []string{"growcursor"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	start := time.Now()
	_, err = c.ListTools(zadvCtx(t, 20*time.Second))
	if !errors.Is(err, ErrPartialListing) || time.Since(start) > 10*time.Second {
		t.Fatalf("err=%v took=%v", err, time.Since(start))
	}
}

// --- 5. what a server's listing may carry into a model's context ---

// Tool descriptions are capped at 64 KiB; a resource's description is not.
func TestZadvW23AResourceDescriptionIsCappedLikeAToolDescription(t *testing.T) {
	c, err := NewClient(ServerConfig{Name: "s", Command: zadvStub(t), Args: []string{"resdesc"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	res, err := c.ListResources(zadvCtx(t, 20*time.Second))
	if err != nil {
		return // refused: fine
	}
	for _, r := range res {
		if len(r.Description) > maxToolDescLen {
			t.Fatalf("a %d-byte resource description was passed on, past the %d cap that tool descriptions get", len(r.Description), maxToolDescLen)
		}
	}
}
