package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestManagedMcpServersResolvesConfiguredExecutables(t *testing.T) {
	dir := t.TempDir()
	gp := filepath.Join(dir, "gitpulse-mcp")
	dm := filepath.Join(dir, "devmap")
	nonExec := filepath.Join(dir, "non-exec-devmap")
	for _, p := range []string{gp, dm} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // executable stand-in in t.TempDir
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(nonExec, []byte("not executable"), 0o644); err != nil { //nolint:gosec // non-executable test stand-in in t.TempDir
		t.Fatal(err)
	}

	t.Setenv("PATH", t.TempDir())
	t.Setenv("MANVI_GITPULSE_MCP_BINARY", gp)
	t.Setenv("MANVI_MAP_BINARY", dm)

	got := managedMcpServers()
	if len(got) != 2 {
		t.Fatalf("expected 2 MCP servers, got %+v", got)
	}
	if got[0].Name != "gitpulse" || got[0].Command != gp || len(got[0].Args) != 0 {
		t.Fatalf("unexpected gitpulse server: %+v", got[0])
	}
	if got[1].Name != "devmap" || got[1].Command != dm || !reflect.DeepEqual(got[1].Args, []string{"mcp"}) {
		t.Fatalf("unexpected devmap server: %+v", got[1])
	}

	// Missing or non-executable overrides are omitted rather than failing startup.
	t.Setenv("MANVI_GITPULSE_MCP_BINARY", filepath.Join(dir, "missing-gitpulse-mcp"))
	t.Setenv("MANVI_MAP_BINARY", nonExec)
	if none := managedMcpServers(); len(none) != 0 {
		t.Fatalf("expected missing/non-exec binaries to be skipped, got %+v", none)
	}
}
