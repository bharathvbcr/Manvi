package repomap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"manvi/dc"
	"manvi/policy"
)

// graphFixture writes a small code graph with two coupled areas and one
// isolated one.
func graphFixture(t *testing.T) string {
	t.Helper()
	g := map[string]any{
		"schema_version": 2,
		"nodes": []map[string]any{
			{"id": "manvi/gate/gate.go", "kind": "file", "path": "manvi/gate/gate.go", "area": "manvi/gate"},
			{"id": "manvi/gate/gate.go::EvaluateWrite", "kind": "method", "path": "manvi/gate/gate.go", "area": "manvi/gate"},
			{"id": "manvi/policy/file.go", "kind": "file", "path": "manvi/policy/file.go", "area": "manvi/policy"},
			{"id": "manvi/policy/file.go::FileGate", "kind": "struct", "path": "manvi/policy/file.go", "area": "manvi/policy"},
			{"id": "docs/notes.md", "kind": "file", "path": "docs/notes.md", "area": "docs"},
		},
		"edges": []map[string]any{
			{"source": "manvi/gate/gate.go::EvaluateWrite", "target": "manvi/policy/file.go::FileGate",
				"kind": "calls", "confidence": ConfidenceExtracted},
			{"source": "manvi/gate/gate.go", "target": "manvi/gate/gate.go::EvaluateWrite",
				"kind": "contains", "confidence": ConfidenceExtracted},
			// An unresolved call from gate into docs. It must not make them
			// neighbours: the analyser declined to say where this call goes.
			{"source": "manvi/gate/gate.go::EvaluateWrite", "target": "docs/notes.md",
				"kind": "calls", "confidence": "ambiguous"},
		},
	}
	raw, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "code_graph.json")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestAdjacencyIsDerivedFromEdgesNotFromTheStubbedField is the reason this
// package computes rather than reads. The artifact's `neighbors` field is a
// literal `[]` in one producer and computed in another, and a consumer cannot
// tell the two apart.
func TestAdjacencyIsDerivedFromEdgesNotFromTheStubbedField(t *testing.T) {
	m, err := Load(graphFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if !m.AreNeighbors("manvi/gate", "manvi/policy") {
		t.Fatal("a call from gate into policy must make them neighbours")
	}
	// Symmetric: a reference in one direction is a coupling in both, and the
	// gate's question has no direction.
	if !m.AreNeighbors("manvi/policy", "manvi/gate") {
		t.Fatal("adjacency must be symmetric")
	}
	// The fixture has a call edge from gate to docs, but the analyser marked it
	// ambiguous — it could not determine which symbol the call reaches.
	// Widening an agent's write scope on the strength of a guess is exactly
	// backwards, so it must not create adjacency.
	if m.AreNeighbors("manvi/gate", "docs") {
		t.Fatal("an ambiguous edge widened the neighbourhood; a scope decision must rest on evidence, not a guess")
	}
	if m.Stats().AmbiguousSkipped != 1 {
		t.Fatalf("ambiguous_skipped = %d, want the excluded edge counted so the narrowing is visible",
			m.Stats().AmbiguousSkipped)
	}
	if !m.AreNeighbors("docs", "docs") {
		t.Fatal("an area is trivially its own neighbour")
	}
}

// TestAreaForPathFallsBackToTheDeepestIndexedAncestor: a file created during
// this turn is by definition not in the index, and that is exactly the write
// the neighbour rung is asked about.
func TestAreaForPathFallsBackToTheDeepestIndexedAncestor(t *testing.T) {
	m, err := Load(graphFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if area, ok := m.AreaForPath("manvi/gate/gate.go"); !ok || area != "manvi/gate" {
		t.Fatalf("exact hit = %q/%v", area, ok)
	}
	// Never indexed, created this turn.
	if area, ok := m.AreaForPath("manvi/gate/brand_new.go"); !ok || area != "manvi/gate" {
		t.Fatalf("new file in a known area = %q/%v; the neighbour rung would degrade on the writes it exists to judge", area, ok)
	}
	if _, ok := m.AreaForPath("wholly/unknown/x.go"); ok {
		t.Fatal("an unknown area must be reported as unknown, not guessed")
	}
	if _, ok := m.AreaForPath(""); ok {
		t.Fatal("an empty path must not resolve")
	}
}

// TestAnEmptyGraphIsAnErrorNotAnEmptyMap: a Map with no files answers "unknown"
// to everything, which is correct and useless. The caller needs to be told to
// build the index, not handed a confident blindness.
func TestAnEmptyGraphIsAnErrorNotAnEmptyMap(t *testing.T) {
	p := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(p, []byte(`{"nodes":[],"edges":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("an empty graph loaded as a usable map")
	}
	if _, err := Load(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("a missing artifact loaded")
	}
	if err := os.WriteFile(p, []byte(`not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("a non-graph file loaded as a graph")
	}
}

// TestLoadIfPresentReturnsNilForAnUnbuiltIndex: nil is meaningful downstream —
// the policy layer records repo_map.unavailable rather than answering "no".
func TestLoadIfPresentReturnsNilForAnUnbuiltIndex(t *testing.T) {
	m, err := LoadIfPresent(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil || m != nil {
		t.Fatalf("m = %v, err = %v; an absent artifact must be (nil, nil)", m, err)
	}
}

// TestTheGateStopsDegradingOnceTheMapIsPresent is the unification, asserted end
// to end through the real policy ladder. Before this package the neighbour rung
// could not run and every unplanned write carried repo_map.unavailable.
func TestTheGateStopsDegradingOnceTheMapIsPresent(t *testing.T) {
	m, err := Load(graphFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	task := &dc.Task{
		ID:           "TASK-1",
		PlannedFiles: []dc.PlannedFile{{Path: "manvi/gate/gate.go", AllowedChange: dc.ChangeModify}},
	}

	without := policy.FileGate{Root: root, HardRules: true, AllowNeighbors: true}
	blind := without.EvaluateFileChange("manvi/policy/file.go", task, dc.OpWrite, false)
	if len(blind.Degraded) == 0 {
		t.Fatal("with no map, an unplanned write must record that the neighbour check could not run")
	}

	with := policy.FileGate{Root: root, HardRules: true, AllowNeighbors: true, Subsystems: m}
	seeing := with.EvaluateFileChange("manvi/policy/file.go", task, dc.OpWrite, false)
	if len(seeing.Degraded) != 0 {
		t.Fatalf("with a map, the neighbour check ran; degraded = %v", seeing.Degraded)
	}
	if seeing.Action != policy.Allow {
		t.Fatalf("policy is a declared neighbour of gate, so the write is in scope: %+v", seeing)
	}

	// And an unrelated area is still refused — with a real reason, not a
	// degradation.
	far := with.EvaluateFileChange("docs/notes.md", task, dc.OpWrite, false)
	if far.Action != policy.Deny || far.Rule != policy.RuleUnplannedScope {
		t.Fatalf("an unrelated subsystem must still be denied: %+v", far)
	}
	if len(far.Degraded) != 0 {
		t.Fatalf("a denial reached with a working map is not degraded: %v", far.Degraded)
	}
}

// TestASymbolNodeNeverOverwritesAFileNodesArea guards an ordering bug: nodes
// arrive in arbitrary order, and a symbol that happens to sort later must not
// relabel the file it lives in.
func TestASymbolNodeNeverOverwritesAFileNodesArea(t *testing.T) {
	g := map[string]any{
		"nodes": []map[string]any{
			{"id": "a.go::Sym", "kind": "func", "path": "a.go", "area": "wrong"},
			{"id": "a.go", "kind": "file", "path": "a.go", "area": "right"},
			{"id": "a.go::Other", "kind": "func", "path": "a.go", "area": "alsowrong"},
		},
		"edges": []map[string]any{},
	}
	raw, _ := json.Marshal(g)
	p := filepath.Join(t.TempDir(), "g.json")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if area, _ := m.AreaForPath("a.go"); area != "right" {
		t.Fatalf("area = %q, want the file node to win regardless of order", area)
	}
}

// TestOnlyCouplingEdgeKindsCreateAdjacency: `contains` and `member_of` are
// relations inside one file. Admitting them would say nothing but would make
// the relation mean something different from what its name claims.
func TestOnlyCouplingEdgeKindsCreateAdjacency(t *testing.T) {
	g := map[string]any{
		"nodes": []map[string]any{
			{"id": "a/x.go", "kind": "file", "path": "a/x.go", "area": "a"},
			{"id": "b/y.go", "kind": "file", "path": "b/y.go", "area": "b"},
		},
		"edges": []map[string]any{
			{"source": "a/x.go", "target": "b/y.go", "kind": "contains", "confidence": ConfidenceExtracted},
			{"source": "a/x.go", "target": "b/y.go", "kind": "member_of", "confidence": ConfidenceExtracted},
		},
	}
	raw, _ := json.Marshal(g)
	p := filepath.Join(t.TempDir(), "g.json")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if m.AreNeighbors("a", "b") {
		t.Fatal("a structural containment edge created a dependency relation")
	}
}

// TestPermissiveIsReported: on a densely coupled repository the neighbour rule
// approaches "allow anything", and an operator must be able to see that before
// relying on it rather than discovering it from a write that should have been
// refused.
func TestPermissiveIsReported(t *testing.T) {
	nodes := []map[string]any{}
	edges := []map[string]any{}
	for _, area := range []string{"a", "b", "c", "d"} {
		nodes = append(nodes, map[string]any{
			"id": area + "/f.go", "kind": "file", "path": area + "/f.go", "area": area,
		})
	}
	for _, other := range []string{"b", "c", "d"} {
		edges = append(edges, map[string]any{
			"source": "a/f.go", "target": other + "/f.go",
			"kind": "calls", "confidence": ConfidenceExtracted,
		})
	}
	raw, _ := json.Marshal(map[string]any{"nodes": nodes, "edges": edges})
	p := filepath.Join(t.TempDir(), "g.json")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	s := m.Stats()
	if s.MaxDegree != 3 || s.WidestArea != "a" {
		t.Fatalf("stats = %+v, want area a with three neighbours", s)
	}
	if !s.Permissive() {
		t.Fatalf("stats = %+v; an area adjacent to every other must report as permissive", s)
	}
}
