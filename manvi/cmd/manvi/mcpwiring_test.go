package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"manvi/flags"
	"manvi/tools"
)

// mcp.enabled was declared and read by nothing: MCP servers were discovered and
// registered on every run, and there was no way to turn them off.
func TestMCPDisabledRefusesRatherThanAnsweringAsThoughItWereOn(t *testing.T) {
	t.Chdir(t.TempDir())
	reg := registryWith(t, map[string]string{flags.MCPEnabled: "false"})
	_, pipeline, err := nativeToolsWith(reg, nil)
	if err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}

	res := pipeline.Run(context.Background(), tools.Call{
		ID: "c1", Name: "mcp_list_tools", Arguments: json.RawMessage(`{}`),
	})
	if !res.IsError {
		t.Fatalf("mcp.enabled=false and mcp_list_tools answered %q — "+
			"a subsystem that was switched off must not report the same result as one that ran", res.Text)
	}
	if !strings.Contains(res.Text, flags.MCPEnabled) {
		t.Errorf("the refusal does not name the setting responsible: %q", res.Text)
	}
}

// mcp.config declared a path and nothing read it: discovery hardcoded
// .devcouncil/mcp.json, mcp.json and .mcp.json, so pointing the setting
// anywhere else registered nothing and said nothing.
func TestTheConfiguredMCPPathIsRead(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	cfg := filepath.Join(dir, "config", "servers.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(`{"mcpServers":{"declared-server":{"command":"cat"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := registryWith(t, map[string]string{flags.MCPConfig: "config/servers.json"})
	_, pipeline, err := nativeToolsWith(reg, nil)
	if err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}

	// Asked about the server the configured file declares. Whether it can be
	// spawned is not the question — whether it was registered at all is.
	res := pipeline.Run(context.Background(), tools.Call{
		ID: "c1", Name: "mcp_list_resources",
		Arguments: json.RawMessage(`{"server_name":"declared-server"}`),
	})
	if strings.Contains(res.Text, "is not registered") {
		t.Fatalf("%s named a file declaring declared-server and the harness never read it: %q",
			flags.MCPConfig, res.Text)
	}
}

// A configured path that is not there is an operator's mistake, and it has to
// arrive as one. Swallowed, it becomes "no servers" — the same answer a correct
// configuration with nothing in it gives.
func TestAMissingConfiguredMCPPathIsRefused(t *testing.T) {
	t.Chdir(t.TempDir())
	reg := registryWith(t, map[string]string{flags.MCPConfig: "config/typo.json"})
	if _, _, err := nativeToolsWith(reg, nil); err == nil {
		t.Fatalf("%s named a file that does not exist and the harness started anyway", flags.MCPConfig)
	} else if !strings.Contains(err.Error(), "config/typo.json") {
		t.Errorf("the refusal does not name the path: %v", err)
	}
}
