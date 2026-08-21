package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A configuration file that exists and cannot be parsed must be reported.
//
// Discovery swallowed it: every load was `_ = m.LoadConfigFile(p)`, so a
// mistyped comma in .devcouncil/mcp.json produced exactly the same manager as
// no file at all — no servers, no error, nothing anywhere saying the operator's
// declaration had been dropped.
func TestAMalformedConfigIsReportedNotSwallowed(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll(filepath.Join(dir, ".devcouncil"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".devcouncil", "mcp.json"),
		[]byte(`{"mcpServers": {"broken": {"command": "cat",}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewManager(dir)
	if err := m.AutoDiscover(context.Background()); err == nil {
		t.Fatalf("discovery reported success over an unparseable .devcouncil/mcp.json, "+
			"and registered %d servers — a dropped declaration is indistinguishable from no declaration",
			len(m.ServerNames()))
	}
}
