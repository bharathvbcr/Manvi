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

// The devmap MCP server has its own opt-out: MANVI_DEVMAP_MCP_BINARY wins over
// MANVI_MAP_BINARY, so the server can be turned off (a non-executable path) while
// Manvi keeps navigating with MANVI_MAP_BINARY, and gitpulse is unaffected.
func TestManagedMcpDevmapOptOutLeavesTheNavigationBinaryAlone(t *testing.T) {
	dir := t.TempDir()
	gp := filepath.Join(dir, "gitpulse-mcp")
	dm := filepath.Join(dir, "devmap")
	other := filepath.Join(dir, "devmap-mcp-only")
	off := filepath.Join(dir, "off")
	for _, p := range []string{gp, dm, other} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // executable stand-in in t.TempDir
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(off, []byte("not executable"), 0o644); err != nil { //nolint:gosec // non-executable test stand-in in t.TempDir
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("MANVI_GITPULSE_MCP_BINARY", gp)
	t.Setenv("MANVI_MAP_BINARY", dm)

	t.Setenv("MANVI_DEVMAP_MCP_BINARY", off)
	got := managedMcpServers()
	if len(got) != 1 || got[0].Name != "gitpulse" {
		t.Fatalf("a non-executable MANVI_DEVMAP_MCP_BINARY must drop only the devmap server, got %+v", got)
	}
	if v := os.Getenv("MANVI_MAP_BINARY"); v != dm {
		t.Fatalf("the opt-out must not touch MANVI_MAP_BINARY: %q", v)
	}

	t.Setenv("MANVI_DEVMAP_MCP_BINARY", other)
	got = managedMcpServers()
	if len(got) != 2 || got[1].Name != "devmap" || got[1].Command != other {
		t.Fatalf("MANVI_DEVMAP_MCP_BINARY must pick the devmap server's binary, got %+v", got)
	}

	t.Setenv("MANVI_DEVMAP_MCP_BINARY", "")
	got = managedMcpServers()
	if len(got) != 2 || got[1].Command != dm {
		t.Fatalf("an empty MANVI_DEVMAP_MCP_BINARY is unset and falls back to MANVI_MAP_BINARY, got %+v", got)
	}
}
