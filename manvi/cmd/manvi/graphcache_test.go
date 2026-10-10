package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTestGraph(t *testing.T, path string, generation string) {
	t.Helper()
	doc := `{"schema_version":2,"meta":{"devmap_rust":{"generation_id":` + generation + `,"analysis_status":"ok"}},
"nodes":[{"id":"a/x.go","kind":"file","path":"a/x.go","area":"a"},
{"id":"b/y.go","kind":"file","path":"b/y.go","area":"b"}],
"edges":[{"source":"a/x.go","target":"b/y.go","kind":"calls","confidence":"extracted"}]}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The cache may only answer for the file it read. A rebuilt graph — even one
// the same size, written within the same second — has to be read again, and a
// deleted one has to read as absent, or a new session would navigate against
// a graph that no longer exists.
func TestLoadGraphReusesOnlyAnUnchangedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "code_graph.json")

	if m, err := loadGraph(path); err != nil || m != nil {
		t.Fatalf("absent graph: %v, %v", m, err)
	}

	writeTestGraph(t, path, "2")
	first, err := loadGraph(path)
	if err != nil || first == nil {
		t.Fatalf("loading: %v, %v", first, err)
	}
	again, err := loadGraph(path)
	if err != nil || again != first {
		t.Fatalf("an unchanged graph was loaded again (%p, %p, %v)", first, again, err)
	}

	// Same size, new contents and a new stamp.
	writeTestGraph(t, path, "3")
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := loadGraph(path)
	if err != nil || rebuilt == first {
		t.Fatalf("a rebuilt graph was served from the cache (%v)", err)
	}
	if got := rebuilt.Provenance().GenerationID; got != 3 {
		t.Fatalf("rebuilt graph reports generation %d, want 3", got)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if m, err := loadGraph(path); err != nil || m != nil {
		t.Fatalf("a deleted graph still loaded: %v, %v", m, err)
	}
}
