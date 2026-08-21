package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseOpenPluginManifest(t *testing.T) {
	raw := `{
		"schema_version": "1.0",
		"name": "sqlite-tool",
		"version": "1.2.0",
		"description": "Query and inspect SQLite databases",
		"author": "DevCouncil",
		"runtime": {
			"type": "stdio",
			"command": "sqlite3",
			"args": ["state.sqlite"]
		},
		"capabilities": ["tools", "resources"],
		"tools": [
			{
				"name": "execute_query",
				"description": "Run SQL query on database",
				"inputSchema": {"type":"object","properties":{"sql":{"type":"string"}},"required":["sql"]}
			}
		]
	}`

	manifest, err := ParsePluginManifest([]byte(raw))
	if err != nil {
		t.Fatalf("parsing manifest: %v", err)
	}

	if manifest.Name != "sqlite-tool" {
		t.Errorf("expected name 'sqlite-tool', got %q", manifest.Name)
	}
	if manifest.SchemaVersion != "1.0" {
		t.Errorf("expected schema '1.0', got %q", manifest.SchemaVersion)
	}
	if len(manifest.Tools) != 1 || manifest.Tools[0].Name != "execute_query" {
		t.Errorf("expected 1 tool 'execute_query', got %+v", manifest.Tools)
	}

	cfg, err := manifest.ToServerConfig()
	if err != nil {
		t.Fatalf("converting to server config: %v", err)
	}
	if cfg.Command != "sqlite3" || len(cfg.Args) != 1 || cfg.Args[0] != "state.sqlite" {
		t.Errorf("unexpected server config: %+v", cfg)
	}
}

func TestDiscoverOpenPlugins(t *testing.T) {
	tmpDir := t.TempDir()
	pluginDir := filepath.Join(tmpDir, "plugins", "my-plugin")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}

	manifestPath := filepath.Join(pluginDir, "plugin.json")
	content := `{
		"name": "custom-tester",
		"version": "0.1.0",
		"description": "Test runner plugin",
		"runtime": {
			"command": "go",
			"args": ["test"]
		}
	}`
	if err := os.WriteFile(manifestPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	plugins, skipped, err := DiscoverPlugins(tmpDir)
	if err != nil {
		t.Fatalf("discovering plugins: %v", err)
	}
	// A well-formed manifest must not land in the skipped report; otherwise
	// the report would be noise and would stop being read.
	if len(skipped) != 0 {
		t.Fatalf("a valid manifest was reported as unreadable: %+v", skipped)
	}

	if len(plugins) != 1 {
		t.Fatalf("expected 1 discovered plugin, got %d", len(plugins))
	}
	if plugins[0].Name != "custom-tester" {
		t.Errorf("expected 'custom-tester', got %q", plugins[0].Name)
	}
}

func TestManagerConfigFile(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "mcp.json")
	content := `{
		"mcpServers": {
			"mock-server": {
				"command": "cat"
			}
		}
	}`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(tmpDir)
	if err := mgr.LoadConfigFile(cfgPath); err != nil {
		t.Fatalf("loading config: %v", err)
	}

	names := mgr.ServerNames()
	if len(names) != 1 || names[0] != "mock-server" {
		t.Errorf("expected ['mock-server'], got %+v", names)
	}
}

func TestMCP20ProtocolTypes(t *testing.T) {
	req := Request{
		JSONRPC: JSONRPCVersion,
		ID:      1,
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"test_tool","arguments":{"x":1}}`),
		Meta:    map[string]any{"sessionless": true},
	}

	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(raw), `"jsonrpc":"2.0"`) {
		t.Errorf("missing JSON-RPC version in %s", string(raw))
	}
	if !strings.Contains(string(raw), `"_meta"`) {
		t.Errorf("missing MCP 2.0 stateless metadata in %s", string(raw))
	}

	resp := Response{
		JSONRPC: JSONRPCVersion,
		ID:      1,
		Result:  json.RawMessage(`{"content":[{"type":"text","text":"hello"}]}`),
	}
	rawResp, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rawResp), `"content"`) {
		t.Errorf("missing content in response %s", string(rawResp))
	}
}
