package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A manifest that cannot be read must be reported, not dropped.
//
// It used to be discarded by `if err == nil`, which made a plugin with a
// trailing comma in its manifest indistinguishable from a plugin that was never
// installed. The operator's only symptom was a tool that never appeared, with
// nothing anywhere saying why.
func TestAMalformedManifestIsReportedNotDropped(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".devcouncil", "plugins", "broken")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"),
		[]byte(`{"name":"broken",}`), 0o644); err != nil {
		t.Fatal(err)
	}

	plugins, skipped, err := DiscoverPlugins(filepath.Join(root, ".devcouncil", "plugins"))
	if err != nil {
		t.Fatalf("discovery failed outright: %v", err)
	}
	if len(plugins) != 0 {
		t.Fatalf("an unparseable manifest was loaded as a plugin: %+v", plugins)
	}
	if len(skipped) != 1 {
		t.Fatalf("skipped = %+v, want the broken manifest reported", skipped)
	}
	if !strings.Contains(skipped[0].Path, "plugin.json") {
		t.Errorf("the report does not name the file: %+v", skipped[0])
	}
	if skipped[0].Reason == "" {
		t.Error("the report does not say why the file could not be read")
	}
}

// A malformed manifest must not stop the run, and must not stop a good plugin
// beside it from loading. Discovery searches by filename under guessed-at
// directories, so an unrelated mcp.json in somebody's tree cannot be fatal.
func TestOneBrokenManifestDoesNotHideAGoodOne(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, ".devcouncil", "plugins")
	for name, body := range map[string]string{
		"broken/plugin.json": `not json at all`,
		"good/plugin.json": `{"name":"good","version":"1.0.0",
			"runtime":{"command":"echo","args":["hi"]}}`,
	} {
		path := filepath.Join(base, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	plugins, skipped, err := DiscoverPlugins(base)
	if err != nil {
		t.Fatalf("one broken manifest failed the whole discovery: %v", err)
	}
	if len(plugins) != 1 || plugins[0].Name != "good" {
		t.Fatalf("the good plugin did not load: %+v", plugins)
	}
	if len(skipped) != 1 {
		t.Fatalf("skipped = %+v, want exactly the broken one", skipped)
	}
}

// The manager must carry the report, so a caller can surface it.
func TestTheManagerRemembersWhatItCouldNotRead(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".devcouncil", "plugins", "bad")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{`), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatalf("discovery failed: %v", err)
	}
	if got := m.Skipped(); len(got) != 1 {
		t.Fatalf("Skipped() = %+v, want the unreadable manifest", got)
	}
}

// A clean tree reports nothing skipped — the report must mean something.
func TestACleanTreeSkipsNothing(t *testing.T) {
	m := NewManager(t.TempDir())
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatalf("discovery failed: %v", err)
	}
	if got := m.Skipped(); len(got) != 0 {
		t.Fatalf("Skipped() = %+v on a tree with no manifests", got)
	}
}
