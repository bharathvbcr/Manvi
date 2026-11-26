package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestManagedMcpServersResolvesConfiguredExecutables(t *testing.T) {
	dir := t.TempDir()
	wl := filepath.Join(dir, exe("waylode-mcp"))
	gp := filepath.Join(dir, exe("gitpulse-mcp"))
	dm := filepath.Join(dir, exe("devmap"))
	nonExec := filepath.Join(dir, "non-exec-devmap")
	for _, p := range []string{wl, gp, dm} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // executable stand-in in t.TempDir
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(nonExec, []byte("not executable"), 0o644); err != nil { //nolint:gosec // non-executable test stand-in in t.TempDir
		t.Fatal(err)
	}

	t.Setenv("PATH", t.TempDir())
	t.Setenv("MANVI_WAYLODE_MCP_BINARY", wl)
	t.Setenv("MANVI_GITPULSE_MCP_BINARY", "")
	t.Setenv("MANVI_MAP_BINARY", dm)

	got := managedMcpServers("edit")
	if len(got) != 2 {
		t.Fatalf("expected 2 MCP servers, got %+v", got)
	}
	if got[0].Name != "waylode" || got[0].Command != wl || len(got[0].Args) != 0 {
		t.Fatalf("unexpected waylode server: %+v", got[0])
	}
	if got[1].Name != "devmap" || got[1].Command != dm || !reflect.DeepEqual(got[1].Args, []string{"mcp"}) {
		t.Fatalf("unexpected devmap server: %+v", got[1])
	}

	// Missing or non-executable overrides are omitted rather than failing startup,
	// and an explicit MANVI_WAYLODE_MCP_BINARY opt-out must not fall back to
	// MANVI_GITPULSE_MCP_BINARY or gitpulse-mcp.
	t.Setenv("MANVI_WAYLODE_MCP_BINARY", filepath.Join(dir, "missing-waylode-mcp"))
	t.Setenv("MANVI_GITPULSE_MCP_BINARY", gp)
	t.Setenv("MANVI_MAP_BINARY", nonExec)
	if none := managedMcpServers("edit"); len(none) != 0 {
		t.Fatalf("expected missing/non-exec binaries to be skipped without fallback, got %+v", none)
	}
}

func TestManagedMcpServersFallsBackToLegacyGitpulseMcp(t *testing.T) {
	dir := t.TempDir()
	gp := filepath.Join(dir, exe("gitpulse-mcp"))
	dm := filepath.Join(dir, exe("devmap"))
	for _, p := range []string{gp, dm} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // executable stand-in in t.TempDir
			t.Fatal(err)
		}
	}

	t.Setenv("PATH", t.TempDir())
	t.Setenv("MANVI_WAYLODE_MCP_BINARY", "")
	t.Setenv("MANVI_GITPULSE_MCP_BINARY", gp)
	t.Setenv("MANVI_MAP_BINARY", dm)

	got := managedMcpServers("edit")
	if len(got) != 2 {
		t.Fatalf("expected 2 MCP servers via fallback, got %+v", got)
	}
	if got[0].Name != "waylode" || got[0].Command != gp || len(got[0].Args) != 0 {
		t.Fatalf("expected legacy binary registered under server name waylode, got %+v", got[0])
	}

	// Legacy opt-out via MANVI_GITPULSE_MCP_BINARY drops the server when
	// MANVI_WAYLODE_MCP_BINARY is unset, even if waylode-mcp is on PATH.
	pathDir := t.TempDir()
	onPath := filepath.Join(pathDir, exe("waylode-mcp"))
	if err := os.WriteFile(onPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // executable stand-in in t.TempDir
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)
	t.Setenv("MANVI_GITPULSE_MCP_BINARY", filepath.Join(dir, "missing-gitpulse-mcp"))
	got = managedMcpServers("edit")
	if len(got) != 1 || got[0].Name != "devmap" {
		t.Fatalf("expected legacy opt-out to drop waylode server, got %+v", got)
	}

	// With both env vars unset, PATH prefers waylode-mcp and falls back to gitpulse-mcp.
	t.Setenv("MANVI_GITPULSE_MCP_BINARY", "")
	got = managedMcpServers("edit")
	if len(got) != 2 || got[0].Name != "waylode" || resolvedPath(t, got[0].Command) != resolvedPath(t, onPath) {
		t.Fatalf("expected PATH waylode-mcp, got %+v", got)
	}

	onlyLegacyDir := t.TempDir()
	legacyOnPath := filepath.Join(onlyLegacyDir, exe("gitpulse-mcp"))
	if err := os.WriteFile(legacyOnPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // executable stand-in in t.TempDir
		t.Fatal(err)
	}
	t.Setenv("PATH", onlyLegacyDir)
	got = managedMcpServers("edit")
	if len(got) != 2 || got[0].Name != "waylode" || resolvedPath(t, got[0].Command) != resolvedPath(t, legacyOnPath) {
		t.Fatalf("expected PATH fallback to gitpulse-mcp under server name waylode, got %+v", got)
	}
}

// The devmap MCP server has its own opt-out: MANVI_DEVMAP_MCP_BINARY wins over
// MANVI_MAP_BINARY, so the server can be turned off (a non-executable path) while
// Manvi keeps navigating with MANVI_MAP_BINARY, and waylode is unaffected.
func TestManagedMcpDevmapOptOutLeavesTheNavigationBinaryAlone(t *testing.T) {
	dir := t.TempDir()
	wl := filepath.Join(dir, exe("waylode-mcp"))
	dm := filepath.Join(dir, exe("devmap"))
	other := filepath.Join(dir, exe("devmap-mcp-only"))
	off := filepath.Join(dir, "off")
	for _, p := range []string{wl, dm, other} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // executable stand-in in t.TempDir
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(off, []byte("not executable"), 0o644); err != nil { //nolint:gosec // non-executable test stand-in in t.TempDir
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("MANVI_WAYLODE_MCP_BINARY", wl)
	t.Setenv("MANVI_GITPULSE_MCP_BINARY", "")
	t.Setenv("MANVI_MAP_BINARY", dm)

	t.Setenv("MANVI_DEVMAP_MCP_BINARY", off)
	got := managedMcpServers("edit")
	if len(got) != 1 || got[0].Name != "waylode" {
		t.Fatalf("a non-executable MANVI_DEVMAP_MCP_BINARY must drop only the devmap server, got %+v", got)
	}
	if v := os.Getenv("MANVI_MAP_BINARY"); v != dm {
		t.Fatalf("the opt-out must not touch MANVI_MAP_BINARY: %q", v)
	}

	t.Setenv("MANVI_DEVMAP_MCP_BINARY", other)
	got = managedMcpServers("edit")
	if len(got) != 2 || got[1].Name != "devmap" || got[1].Command != other {
		t.Fatalf("MANVI_DEVMAP_MCP_BINARY must pick the devmap server's binary, got %+v", got)
	}

	t.Setenv("MANVI_DEVMAP_MCP_BINARY", "")
	got = managedMcpServers("edit")
	if len(got) != 2 || got[1].Command != dm {
		t.Fatalf("an empty MANVI_DEVMAP_MCP_BINARY is unset and falls back to MANVI_MAP_BINARY, got %+v", got)
	}
}

// An inspect session is attached only the read-only devmap server; every other
// mode keeps both.
func TestManagedMcpInspectSessionsGetOnlyDevmap(t *testing.T) {
	dir := t.TempDir()
	wl := filepath.Join(dir, exe("waylode-mcp"))
	dm := filepath.Join(dir, exe("devmap"))
	for _, p := range []string{wl, dm} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // executable stand-in in t.TempDir
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("MANVI_WAYLODE_MCP_BINARY", wl)
	t.Setenv("MANVI_GITPULSE_MCP_BINARY", "")
	t.Setenv("MANVI_MAP_BINARY", dm)
	if got := managedMcpServers("inspect"); len(got) != 1 || got[0].Name != "devmap" {
		t.Fatalf("inspect must get only devmap, got %+v", got)
	}
	for _, mode := range []string{"ask", "edit", "auto_review", "preapproved", "bypass"} {
		if got := managedMcpServers(mode); len(got) != 2 {
			t.Fatalf("%s: want waylode and devmap, got %+v", mode, got)
		}
	}
}
