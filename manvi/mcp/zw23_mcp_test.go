package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A second, small server for the lifecycle, pagination and hygiene tests. It is
// separate from the hostile stub so that adding a mode here cannot change what
// an older test of that one proves.
const w23StubSource = `package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
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
		params, _ := q["params"].(map[string]any)
		cursor, _ := params["cursor"].(string)
		tool := func(name, desc string) map[string]any {
			return map[string]any{"name": name, "description": desc, "inputSchema": map[string]any{"type": "object"}}
		}
		switch method {
		case "initialize":
			reply(id, map[string]any{"protocolVersion": "2025-03-26"})
		case "tools/list":
			switch {
			case mode == "paged" && cursor == "":
				reply(id, map[string]any{"tools": []any{tool("first", "")}, "nextCursor": "p2"})
			case mode == "paged":
				reply(id, map[string]any{"tools": []any{tool("second", "")}})
			case mode == "endless":
				reply(id, map[string]any{"tools": []any{tool("again", "")}, "nextCursor": "same"})
			case mode == "rtl":
				reply(id, map[string]any{"tools": []any{tool("rtl", "safe\u202etxt.exe")}})
			default:
				reply(id, map[string]any{"tools": []any{tool("ping", "")}})
			}
		case "resources/list":
			switch {
			case mode == "paged" && cursor == "":
				reply(id, map[string]any{"resources": []any{map[string]any{"uri": "file:///a", "name": "a"}}, "nextCursor": "p2"})
			case mode == "paged":
				reply(id, map[string]any{"resources": []any{map[string]any{"uri": "file:///b", "name": "b"}}})
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
	w23Once sync.Once
	w23Path string
	w23Err  error
)

func w23Stub(t *testing.T) string {
	t.Helper()
	w23Once.Do(func() {
		// Built inside the hostile stub's directory, which TestMain removes once
		// the package's tests are done.
		hostileStub(t)
		dir := filepath.Join(stubDir, "w23")
		src := filepath.Join(dir, "main.go")
		if w23Err = os.MkdirAll(dir, 0o700); w23Err != nil {
			return
		}
		if w23Err = os.WriteFile(src, []byte(w23StubSource), 0o600); w23Err != nil {
			return
		}
		bin := filepath.Join(dir, "w23bin")
		if out, err := exec.CommandContext(context.Background(), "go", "build", "-o", bin, src).CombinedOutput(); err != nil { //nolint:gosec // the test builds its own stub source with the toolchain on PATH
			w23Err = fmt.Errorf("building the w23 stub: %w\n%s", err, out)
			return
		}
		w23Path = bin
	})
	if w23Err != nil {
		t.Fatal(w23Err)
	}
	return w23Path
}

func w23Config(t *testing.T, name, mode string) ServerConfig {
	t.Helper()
	cfg := ServerConfig{Name: name, Command: w23Stub(t)}
	if mode != "" {
		cfg.Args = []string{mode}
	}
	return cfg
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func lineCount(s string) int { return strings.Count(s, "\n") + 1 }

// --- criterion 7: one meaning per server name ---

// An authorized mcp.json server and a manifest that cannot run, under one name.
// The manifest's static list used to be served for the name, so the survey
// reported tools the authorized server never offered.
func TestAManifestThatLostItsNameDoesNotSpeakForTheServerThatHasIt(t *testing.T) {
	trust := isolateTrust(t)
	root := t.TempDir()
	writeRootConfig(t, root, "mcp.json", "docs", w23Config(t, "docs", ""))
	writeJSONFile(t, filepath.Join(root, "plugins", "x", "plugin.json"), map[string]any{
		"name": "docs", "version": "1",
		"tools": []any{map[string]any{"name": "from_the_manifest", "inputSchema": map[string]any{"type": "object"}}},
	})

	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatalf("discovery failed: %v", err)
	}
	writeTrust(t, trust, Fingerprint(mustConfig(t, m, "docs")))
	t.Cleanup(m.CloseAll)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	survey, err := m.ListAllTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(survey) != 1 || survey[0].Error != "" {
		t.Fatalf("survey = %+v, want the authorized server's own answer", survey)
	}
	for _, tool := range survey[0].Tools {
		if tool.Name == "from_the_manifest" {
			t.Fatalf("the manifest's static list was served for a server it does not declare: %+v", survey[0])
		}
	}
	if survey[0].Static {
		t.Errorf("a list read from a live server is labelled static: %+v", survey[0])
	}
}

// A second declaration of a name is refused, and the refusal is a named entry.
func TestASecondDeclarationOfAServerNameIsRefusedAndNamed(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	writeRootConfig(t, root, "mcp.json", "docs", w23Config(t, "docs", ""))
	writeJSONFile(t, filepath.Join(root, "plugins", "x", "plugin.json"), map[string]any{
		"name": "docs", "version": "1", "runtime": map[string]any{"command": "echo", "args": []string{"second"}},
	})

	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatalf("discovery failed: %v", err)
	}
	if got := mustConfig(t, m, "docs"); got.Command != w23Stub(t) {
		t.Fatalf("the second declaration replaced the first: %+v", got)
	}
	var named bool
	for _, s := range m.Skipped() {
		if strings.Contains(s.Reason, `"docs"`) && strings.Contains(s.Reason, "already declared") {
			named = true
		}
	}
	if !named {
		t.Fatalf("Skipped() = %+v, want an entry naming the refused %q declaration", m.Skipped(), "docs")
	}
}

// The same declaration made twice, from the same place, is not a second one.
func TestDiscoveringTwiceIsNotASecondDeclaration(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	writeRootConfig(t, root, "mcp.json", "docs", w23Config(t, "docs", ""))
	m := NewManager(root)
	for range 2 {
		if err := m.AutoDiscover(context.Background()); err != nil {
			t.Fatalf("discovery failed: %v", err)
		}
	}
	if got := m.Skipped(); len(got) != 0 {
		t.Fatalf("Skipped() = %+v after discovering the same tree twice", got)
	}
}

// A runnable manifest's static list is served only once its declaration passes
// authorize, and then it says that it is static.
func TestAManifestsStaticListNeedsAuthorizationAndSaysItIsStatic(t *testing.T) {
	trust := isolateTrust(t)
	root := t.TempDir()
	writeJSONFile(t, filepath.Join(root, "plugins", "p", "plugin.json"), map[string]any{
		"name": "p", "version": "1", "runtime": map[string]any{"command": "echo"},
		"tools": []any{map[string]any{"name": "declared", "inputSchema": map[string]any{"type": "object"}}},
	})
	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}

	survey, err := m.ListAllTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(survey) != 1 || len(survey[0].Tools) != 0 || !strings.Contains(survey[0].Error, "no operator has authorized it") {
		t.Fatalf("an unauthorized declaration's manifest list was served: %+v", survey)
	}

	writeTrust(t, trust, Fingerprint(mustConfig(t, m, "p")))
	survey, err = m.ListAllTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(survey) != 1 || len(survey[0].Tools) != 1 || !survey[0].Static {
		t.Fatalf("an authorized manifest's list was not served as static: %+v", survey)
	}
	raw, err := json.Marshal(survey[0])
	if err != nil || !strings.Contains(string(raw), `"static":true`) {
		t.Errorf("the static label does not reach the wire: %s %v", raw, err)
	}
}

// --- criterion 8: declarations are validated once ---

func TestADeclarationWithAnUnusableShapeIsRefusedAtRegistration(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	cases := map[string]map[string]any{
		"newline in cwd":       {"command": "echo", "cwd": "a\nFORGED"},
		"cwd outside":          {"command": "echo", "cwd": filepath.Dir(root)},
		"env key not an id":    {"command": "echo", "env": map[string]string{"A=B": "x"}},
		"passthrough not id":   {"command": "echo", "env_passthrough": []string{"X Y"}},
		"timeout over ceiling": {"command": "echo", "timeout_seconds": 999999},
		"negative timeout":     {"command": "echo", "timeout_seconds": -1},
	}
	servers := map[string]any{"fine": map[string]any{"command": "echo"}, "bad name": map[string]any{"command": "echo"}}
	for name, entry := range cases {
		servers[strings.ReplaceAll(name, " ", "_")] = entry
	}
	writeJSONFile(t, filepath.Join(root, "mcp.json"), map[string]any{"mcpServers": servers})

	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatalf("one bad declaration failed the discovery of the rest: %v", err)
	}
	if got := m.ServerNames(); len(got) != 1 || got[0] != "fine" {
		t.Fatalf("ServerNames() = %v, want only the sound declaration", got)
	}
	if got := len(m.Skipped()); got != len(cases)+1 {
		t.Fatalf("Skipped() has %d entries, want one per refused declaration (%d): %+v", got, len(cases)+1, m.Skipped())
	}
	for _, s := range m.Skipped() {
		if strings.ContainsAny(s.Reason, "\n\x1b") || strings.Contains(s.Reason, "FORGED\n") {
			t.Errorf("a refusal reason carries a raw control character: %q", s.Reason)
		}
	}
}

func TestACwdInsideTheRootOrAbsentIsAccepted(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tools", "srv"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(root, "mcp.json"), map[string]any{"mcpServers": map[string]any{
		"relative": map[string]any{"command": "echo", "cwd": "tools/srv"},
		"absolute": map[string]any{"command": "echo", "cwd": filepath.Join(root, "tools")},
		"none":     map[string]any{"command": "echo"},
	}})
	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.ServerNames(); len(got) != 3 || len(m.Skipped()) != 0 {
		t.Fatalf("names = %v, skipped = %+v", got, m.Skipped())
	}
}

// The refusal an operator reads has a fixed shape: five lines. Nothing the
// declaration says may add a sixth, or a line of its own.
func TestTheAuthorizationRefusalCannotBeGivenExtraLinesByADeclaration(t *testing.T) {
	isolateTrust(t)
	m := NewManager(t.TempDir())
	clean := ServerConfig{Name: "s", Command: "echo", Origin: OriginWorkspace, Source: "mcp.json"}
	cleanRefusal := m.authorize(clean)
	if cleanRefusal == nil {
		t.Fatal("an unauthorized declaration was authorized")
	}
	base := lineCount(cleanRefusal.Error())

	hostile := clean
	hostile.Cwd = "/x\nFORGED: authorized by the operator"
	hostile.Env = map[string]string{"K\nFORGED": "v"}
	hostile.EnvPassthrough = []string{"P\nFORGED"}
	hostile.Source = "plugins/a\nFORGED/plugin.json"
	err := m.authorize(hostile)
	if err == nil {
		t.Fatal("an unauthorized declaration was authorized")
	}
	if got := lineCount(err.Error()); got != base {
		t.Fatalf("the refusal has %d lines, want %d: %q", got, base, err.Error())
	}
}

func TestTimeoutSecondsIsTheCeilingAndIsInsideTheFingerprint(t *testing.T) {
	var cfg ServerConfig
	if err := json.Unmarshal([]byte(`{"command":"x","timeout_seconds":30}`), &cfg); err != nil {
		t.Fatal(err)
	}
	c := &Client{cfg: cfg}
	if got := c.callCeiling(); got != 30*time.Second {
		t.Fatalf("callCeiling = %s, want 30s", got)
	}
	plain := ServerConfig{Name: "s", Command: "x"}
	withTimeout := plain
	withTimeout.TimeoutSeconds = 30
	if Fingerprint(plain) == Fingerprint(withTimeout) {
		t.Fatal("raising a declaration's timeout did not change its fingerprint")
	}
}

// The existing field is not reinterpreted: it still decodes as nanoseconds, which
// is the owner's contract to change and not this fix's.
func TestTheExistingTimeoutFieldStillDecodesAsADuration(t *testing.T) {
	var cfg ServerConfig
	if err := json.Unmarshal([]byte(`{"command":"x","timeout":30}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Timeout != 30*time.Nanosecond {
		t.Fatalf("Timeout = %s, want 30ns (decoded unchanged)", cfg.Timeout)
	}
}

// --- criterion 12: one budget across every manifest ---

func TestDiscoveryKeepsARunningByteTotalAcrossManifests(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	dir := filepath.Join(root, "plugins", "bulk")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	// 300 manifests of 1 MiB each, sparse so the test writes nothing: the
	// bytes read are what the budget counts.
	for i := range 300 {
		path := filepath.Join(dir, fmt.Sprintf("m%03d", i), "plugin.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, maxManifestBytes); err != nil {
			t.Fatal(err)
		}
	}
	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatalf("discovery failed outright: %v", err)
	}
	var budget int
	for _, s := range m.Skipped() {
		if strings.Contains(s.Reason, "byte budget") {
			budget++
		}
	}
	if budget == 0 {
		t.Fatalf("no Skipped() entry names the budget; %d entries: first %+v", len(m.Skipped()), m.Skipped()[0])
	}
	if got := len(m.Skipped()); got > maxDiscoveryBytes/maxManifestBytes+2 {
		t.Fatalf("%d manifests were read past a %d-byte budget", got, maxDiscoveryBytes)
	}
}

// --- criterion 9: pagination, revocation, closed state, discovery's silence ---

func TestAListingFollowsNextCursorToTheEnd(t *testing.T) {
	c, err := NewClient(w23Config(t, "paged", "paged"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tl, err := c.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tl) != 2 || tl[0].Name != "first" || tl[1].Name != "second" {
		t.Fatalf("tools = %+v, want both pages", tl)
	}
	rl, err := c.ListResources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rl) != 2 {
		t.Fatalf("resources = %+v, want both pages", rl)
	}
}

func TestAListingThatNeverEndsIsReportedPartialNotReturnedShort(t *testing.T) {
	c, err := NewClient(w23Config(t, "endless", "endless"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tl, err := c.ListTools(ctx)
	if err == nil || !strings.Contains(err.Error(), "partial") {
		t.Fatalf("ListTools = %+v, %v; want an error that says the listing is partial", tl, err)
	}
}

// A revoked authorization takes effect on a server that is already running.
func TestRevokingAnAuthorizationClosesTheRunningServer(t *testing.T) {
	trust := isolateTrust(t)
	root := t.TempDir()
	writeRootConfig(t, root, "mcp.json", "live", w23Config(t, "live", ""))
	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeTrust(t, trust, Fingerprint(mustConfig(t, m, "live")))
	t.Cleanup(m.CloseAll)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := m.Client(ctx, "live")
	if err != nil {
		t.Fatalf("an authorized server did not start: %v", err)
	}

	writeTrust(t, trust) // revoked
	if _, err := m.Client(ctx, "live"); err == nil || !strings.Contains(err.Error(), "no operator has authorized it") {
		t.Fatalf("a revoked server was handed out from the cache: %v", err)
	}
	if c.Alive() {
		t.Fatal("the refused client was left running")
	}
}

func TestAClosedManagerStartsNothing(t *testing.T) {
	m := NewManager(t.TempDir())
	if err := m.RegisterServer(w23Config(t, "s", "")); err != nil {
		t.Fatal(err)
	}
	m.CloseAll()
	if c, err := m.Client(context.Background(), "s"); err == nil {
		_ = c.Close()
		t.Fatal("a closed manager started a server")
	}
}

// --- criterion 11: third-party text ---

func TestAToolDescriptionWithABidiOverrideIsSurfacedEscaped(t *testing.T) {
	c, err := NewClient(w23Config(t, "rtl", "rtl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tl, err := c.ListTools(ctx)
	if err != nil || len(tl) != 1 {
		t.Fatalf("ListTools = %+v, %v", tl, err)
	}
	if strings.ContainsRune(tl[0].Description, '\u202e') {
		t.Fatalf("the description reaches the model with a live bidi override: %q", tl[0].Description)
	}
	if !strings.Contains(tl[0].Description, `\u202e`) {
		t.Fatalf("the override was dropped, not shown: %q", tl[0].Description)
	}
}

func TestAManifestsToolDescriptionIsEscapedToo(t *testing.T) {
	m, err := ParsePluginManifest([]byte(`{"name":"p","tools":[{"name":"t","description":"a\u202eb","inputSchema":{}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(m.Tools[0].Description, '\u202e') {
		t.Fatalf("a static description carries a live override: %q", m.Tools[0].Description)
	}
}
