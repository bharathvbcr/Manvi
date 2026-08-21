package repomap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The artifact is a file on disk written by a binary from another repository,
// and the gate decides scope from it. Everything here is about the ways it can
// be wrong while still parsing: derived from an index that has since moved on,
// written by an analysis that did not finish, or emitted in a vocabulary this
// build no longer recognises. Each of those loads cleanly today.

// writeGraph renders a graph document to a temp file.
func writeGraph(t *testing.T, doc map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "code_graph.json")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// stamped is graphFixture's document with a provenance block, so a test can
// vary one field of it.
func stamped(generation int, analysis string, extra map[string]any) map[string]any {
	rust := map[string]any{
		"generation_id":               generation,
		"analysis_status":             analysis,
		"edge_endpoints_without_node": 0,
		"duplicate_edges_dropped":     0,
	}
	for k, v := range extra {
		rust[k] = v
	}
	return map[string]any{
		"schema_version": 2,
		"meta":           map[string]any{"devmap_rust": rust},
		"nodes": []map[string]any{
			{"id": "manvi/gate/gate.go", "kind": "file", "path": "manvi/gate/gate.go", "area": "manvi/gate"},
			{"id": "manvi/gate/gate.go::EvaluateWrite", "kind": "method", "path": "manvi/gate/gate.go", "area": "manvi/gate"},
			{"id": "manvi/policy/file.go", "kind": "file", "path": "manvi/policy/file.go", "area": "manvi/policy"},
		},
		"edges": []map[string]any{
			{"source": "manvi/gate/gate.go::EvaluateWrite", "target": "manvi/policy/file.go",
				"kind": "calls", "confidence": ConfidenceExtracted},
		},
	}
}

// TestAnArtifactBehindItsIndexIsReported is the one that was live on this
// repository: the index stood at generation 4 with 4,249 symbols while
// `.devcouncil/code_graph.json` carried generation 2 and 2,713, and `manvi map
// status` printed both on adjacent lines as though they described one graph.
// The gate decides scope from the artifact and the navigation tools answer from
// the index, so the two halves of one answer were describing repositories two
// generations apart with nothing anywhere saying so.
func TestAnArtifactBehindItsIndexIsReported(t *testing.T) {
	m, err := Load(writeGraph(t, stamped(2, "ok", nil)))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Provenance().GenerationID; got != 2 {
		t.Fatalf("the artifact stamps its generation; read %d, want 2", got)
	}
	// The index has moved on two generations since this artifact was written.
	notes := m.DisagreementsWith(4, 4249)
	if len(notes) == 0 {
		t.Fatal("an artifact two generations behind its index must be reported, " +
			"not loaded as though it described the current tree")
	}
	joined := strings.Join(notes, " ")
	if !strings.Contains(joined, "2") || !strings.Contains(joined, "4") {
		t.Fatalf("the report must name both generations so an operator can see the gap: %q", joined)
	}
}

// TestAnArtifactMatchingItsIndexIsSilent guards the other direction. A check
// that fires on a current artifact would be turned off within a week.
func TestAnArtifactMatchingItsIndexIsSilent(t *testing.T) {
	m, err := Load(writeGraph(t, stamped(4, "ok", nil)))
	if err != nil {
		t.Fatal(err)
	}
	if notes := m.DisagreementsWith(4, 3); len(notes) != 0 {
		t.Fatalf("an artifact derived from the index it is compared against must be silent, got %v", notes)
	}
}

// TestAnUnstampedArtifactIsUnverifiedNotAgreed covers the producer that writes
// no generation. The honest answer is that the check could not run — which must
// never render the same as a check that ran and agreed.
func TestAnUnstampedArtifactIsUnverifiedNotAgreed(t *testing.T) {
	doc := stamped(0, "ok", nil)
	delete(doc, "meta")
	m, err := Load(writeGraph(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	notes := m.DisagreementsWith(4, 3)
	if len(notes) == 0 {
		t.Fatal("an artifact carrying no generation cannot be shown to match the index; " +
			"that is unverified, and unverified must be reported")
	}
	if !strings.Contains(strings.Join(notes, " "), "unverified") {
		t.Fatalf("the report must say the check could not run: %v", notes)
	}
}

// TestAPartialAnalysisIsReported: the producer says whether its analysis
// completed, in a field the consumer never read. A graph written by an analysis
// that gave up part way still parses, still has nodes, and answers every
// adjacency question with confidence.
func TestAPartialAnalysisIsReported(t *testing.T) {
	m, err := Load(writeGraph(t, stamped(4, "partial", nil)))
	if err != nil {
		t.Fatal(err)
	}
	degraded := strings.Join(m.Degraded(), " ")
	if !strings.Contains(degraded, "partial") {
		t.Fatalf("an analysis that did not complete must ride on the map that came out of it: %q", degraded)
	}
}

// TestAMovedConfidenceVocabularyIsNotSilentlyZeroAdjacency is assertShape's
// argument applied to the artifact. Only edges marked `extracted` create
// adjacency. If the producer renames that value, every coupling edge is skipped
// as unresolved and the map loads with no adjacency at all — a gate that denies
// every neighbour-scope write, reporting a healthy map while doing it.
//
// The distinction that makes this checkable: a repository where the analyser
// genuinely resolved nothing is possible, but one where it resolved nothing
// *and* there were coupling edges to resolve means the word changed.
func TestAMovedConfidenceVocabularyIsNotSilentlyZeroAdjacency(t *testing.T) {
	doc := stamped(4, "ok", nil)
	doc["edges"] = []map[string]any{
		{"source": "manvi/gate/gate.go::EvaluateWrite", "target": "manvi/policy/file.go",
			"kind": "calls", "confidence": "resolved"},
		{"source": "manvi/policy/file.go", "target": "manvi/gate/gate.go::EvaluateWrite",
			"kind": "imports", "confidence": "resolved"},
	}
	m, err := Load(writeGraph(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	if m.AreNeighbors("manvi/gate", "manvi/policy") {
		t.Fatal("fixture error: a renamed confidence must not create adjacency")
	}
	degraded := strings.Join(m.Degraded(), " ")
	if degraded == "" {
		t.Fatal("every coupling edge in the graph was rejected as unresolved, which is a " +
			"vocabulary this build no longer reads — the resulting empty adjacency must not " +
			"be reported as a repository with no coupling")
	}
	if !strings.Contains(degraded, "resolved") {
		t.Fatalf("the report must name the value it did not understand: %q", degraded)
	}
}

// TestEdgesWithNoNodeAreReported: the producer counts the edges whose endpoints
// are not in the graph it wrote, and this package skips exactly those edges
// when computing adjacency. On this repository that was 349 of them. Each is a
// coupling that might have made two areas neighbours, dropped silently.
func TestEdgesWithNoNodeAreReported(t *testing.T) {
	m, err := Load(writeGraph(t, stamped(4, "ok", map[string]any{"edge_endpoints_without_node": 349})))
	if err != nil {
		t.Fatal(err)
	}
	degraded := strings.Join(m.Degraded(), " ")
	if !strings.Contains(degraded, "349") {
		t.Fatalf("edges whose endpoints are absent from the graph are adjacency this map "+
			"cannot see, and the producer counted them: %q", degraded)
	}
}

// TestAGraphWithNoAreasIsReported covers the other half of the vocabulary. If
// `area` moves, every node falls back to its directory and the map still
// answers — plausibly, and differently from the graph the producer meant.
func TestAGraphWithNoAreasIsReported(t *testing.T) {
	doc := stamped(4, "ok", nil)
	nodes := doc["nodes"].([]map[string]any)
	for _, n := range nodes {
		delete(n, "area")
	}
	m, err := Load(writeGraph(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	degraded := strings.Join(m.Degraded(), " ")
	if !strings.Contains(degraded, "area") {
		t.Fatalf("no node declared an area, so every area here is this build's own guess "+
			"at one, and that must be said: %q", degraded)
	}
}

// TestAnUnsupportedSchemaLoadsButSaysSo. Refusing an unknown schema outright
// would brick every session on an additive producer bump, which is the wrong
// trade for a harness. Loading it silently is the wrong trade for a gate.
func TestAnUnsupportedSchemaLoadsButSaysSo(t *testing.T) {
	doc := stamped(4, "ok", nil)
	doc["schema_version"] = 99
	m, err := Load(writeGraph(t, doc))
	if err != nil {
		t.Fatalf("an unknown schema must still load: %v", err)
	}
	degraded := strings.Join(m.Degraded(), " ")
	if !strings.Contains(degraded, "99") {
		t.Fatalf("the schema this build was not written against must be named: %q", degraded)
	}
}
