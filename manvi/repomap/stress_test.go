package repomap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The graph is a file written by a binary from another repository, and the
// write gate decides scope from what this package makes of it. It is therefore
// treated as untrusted input: nothing in it may panic this package, and no
// shape of it may produce an adjacency relation that is asymmetric, reflexive
// through the root, or different on a second read of the same bytes.

// load writes a document and loads it, returning nil when it was rejected.
func load(t *testing.T, doc any) *Map {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "g.json")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Load(p)
	if err != nil {
		return nil
	}
	return m
}

// checkInvariants asserts the properties the gate depends on, for any map.
func checkInvariants(t *testing.T, m *Map) {
	t.Helper()
	if m == nil {
		return
	}
	areas := map[string]bool{}
	for _, a := range m.areaOf {
		areas[a] = true
	}
	for a := range m.adjacent {
		areas[a] = true
	}

	for a := range areas {
		// Adjacency is symmetric by construction, and the gate relies on it:
		// it asks "is the target next door to the plan" without a direction.
		for _, b := range m.Neighbours(a) {
			if !m.AreNeighbors(b, a) {
				t.Fatalf("adjacency is not symmetric: %q → %q but not back", a, b)
			}
		}
		// An area is trivially its own neighbour, but it must never be linked
		// to itself in the relation — that would make MaxDegree, and so the
		// permissive verdict, count a coupling that is not one.
		if m.adjacent[a][a] {
			t.Fatalf("area %q was linked to itself", a)
		}
		// Nothing may panic, whatever the area.
		_ = m.NeighborsArePermissive()
	}

	s := m.Stats()
	for name, v := range map[string]int{
		"Files": s.Files, "Areas": s.Areas, "Edges": s.Edges,
		"Adjacencies": s.Adjacencies, "AmbiguousSkipped": s.AmbiguousSkipped,
		"MaxDegree": s.MaxDegree,
	} {
		if v < 0 {
			t.Fatalf("Stats.%s is negative: %d", name, v)
		}
	}
	if s.MaxDegree > s.Areas && s.Areas > 0 {
		// An area cannot neighbour more areas than exist, and the permissive
		// verdict is computed from exactly this ratio.
		t.Fatalf("MaxDegree %d exceeds Areas %d", s.MaxDegree, s.Areas)
	}
	// The reporting paths take the same untrusted input.
	_ = m.Degraded()
	_ = m.DisagreementsWith(1, 1)
	_ = m.Provenance()
}

// TestPathologicalGraphsNeverPanicAndKeepTheirInvariants.
func TestPathologicalGraphsNeverPanicAndKeepTheirInvariants(t *testing.T) {
	node := func(id, kind, p, area string) map[string]any {
		return map[string]any{"id": id, "kind": kind, "path": p, "area": area}
	}
	edge := func(src, tgt, kind, conf string) map[string]any {
		return map[string]any{"source": src, "target": tgt, "kind": kind, "confidence": conf}
	}

	cases := []struct {
		name string
		doc  map[string]any
	}{
		{"an edge whose endpoints are the same node", map[string]any{
			"nodes": []map[string]any{node("a", "file", "a/x.go", "a")},
			"edges": []map[string]any{edge("a", "a", "calls", ConfidenceExtracted)},
		}},
		{"edges naming nodes that do not exist", map[string]any{
			"nodes": []map[string]any{node("a", "file", "a/x.go", "a")},
			"edges": []map[string]any{edge("ghost1", "ghost2", "calls", ConfidenceExtracted)},
		}},
		{"duplicate node ids disagreeing about their area", map[string]any{
			"nodes": []map[string]any{
				node("dup", "file", "a/x.go", "a"),
				node("dup", "file", "a/x.go", "b"),
			},
			"edges": []map[string]any{},
		}},
		{"a symbol node claiming a file's path", map[string]any{
			"nodes": []map[string]any{
				node("a/x.go::S", "struct", "a/x.go", "zzz"),
				node("a/x.go", "file", "a/x.go", "a"),
			},
			"edges": []map[string]any{},
		}},
		{"paths with traversal segments", map[string]any{
			"nodes": []map[string]any{
				node("t", "file", "../../etc/passwd", ""),
				node("u", "file", "./a/./b.go", ""),
			},
			"edges": []map[string]any{edge("t", "u", "calls", ConfidenceExtracted)},
		}},
		{"absolute and root paths", map[string]any{
			"nodes": []map[string]any{
				node("r", "file", "/", ""),
				node("s", "file", "//", ""),
				node("v", "file", "/abs/x.go", ""),
			},
			"edges": []map[string]any{edge("r", "v", "imports", ConfidenceExtracted)},
		}},
		{"windows separators", map[string]any{
			"nodes": []map[string]any{
				node("w", "file", `manvi\policy\file.go`, `manvi\policy`),
				node("x", "file", `manvi\gate\gate.go`, `manvi\gate`),
			},
			"edges": []map[string]any{edge("w", "x", "calls", ConfidenceExtracted)},
		}},
		{"a very deep path", map[string]any{
			"nodes": []map[string]any{
				node("d", "file", strings.Repeat("deep/", 2000)+"x.go", ""),
			},
			"edges": []map[string]any{},
		}},
		{"unicode and whitespace in paths and areas", map[string]any{
			"nodes": []map[string]any{
				node("u1", "file", "  パッケージ/файл.go  ", "  パッケージ  "),
				node("u2", "file", "emoji-🙈/x.go", "emoji-🙈"),
			},
			"edges": []map[string]any{edge("u1", "u2", "references", ConfidenceExtracted)},
		}},
		{"an edge kind that is not a coupling", map[string]any{
			"nodes": []map[string]any{
				node("a", "file", "a/x.go", "a"), node("b", "file", "b/y.go", "b"),
			},
			"edges": []map[string]any{edge("a", "b", "contains", ConfidenceExtracted)},
		}},
		{"null fields throughout", map[string]any{
			"nodes": []map[string]any{
				{"id": "n", "kind": nil, "path": nil, "area": nil},
			},
			"edges": []map[string]any{
				{"source": nil, "target": nil, "kind": nil, "confidence": nil},
			},
		}},
		{"a meta block of the wrong shape", map[string]any{
			"nodes": []map[string]any{node("a", "file", "a/x.go", "a")},
			"edges": []map[string]any{},
			"meta":  map[string]any{"devmap_rust": map[string]any{"generation_id": 0, "analysis_status": ""}},
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checkInvariants(t, load(t, c.doc))
		})
	}
}

// TestALargeGraphStaysConsistent drives the same invariants at a size where an
// accidental quadratic or an unbounded ancestor walk would show.
func TestALargeGraphStaysConsistent(t *testing.T) {
	const areas, perArea = 120, 40
	var nodes, edges []map[string]any
	for a := 0; a < areas; a++ {
		area := fmt.Sprintf("pkg/sub%d", a)
		for f := 0; f < perArea; f++ {
			id := fmt.Sprintf("%s/f%d.go", area, f)
			nodes = append(nodes, map[string]any{"id": id, "kind": "file", "path": id, "area": area})
		}
		// A ring of couplings, plus one long-range edge, so adjacency is neither
		// empty nor complete.
		next := fmt.Sprintf("pkg/sub%d/f0.go", (a+1)%areas)
		edges = append(edges, map[string]any{
			"source": fmt.Sprintf("%s/f0.go", area), "target": next,
			"kind": "calls", "confidence": ConfidenceExtracted})
		edges = append(edges, map[string]any{
			"source": fmt.Sprintf("%s/f1.go", area), "target": "pkg/sub0/f0.go",
			"kind": "imports", "confidence": "ambiguous"})
	}
	m := load(t, map[string]any{"nodes": nodes, "edges": edges, "schema_version": 2})
	if m == nil {
		t.Fatal("a large well-formed graph must load")
	}
	checkInvariants(t, m)

	s := m.Stats()
	if s.Files != areas*perArea {
		t.Errorf("Files = %d, want %d", s.Files, areas*perArea)
	}
	if s.Areas != areas {
		t.Errorf("Areas = %d, want %d", s.Areas, areas)
	}
	if s.AmbiguousSkipped == 0 {
		t.Error("the unresolved edges must be counted, not silently dropped")
	}
	// A ring gives every area exactly two neighbours, which is nowhere near
	// half the map: the permissive verdict must stay off.
	if m.NeighborsArePermissive() {
		t.Errorf("a ring of %d areas with degree %d is not permissive", s.Areas, s.MaxDegree)
	}
}

// TestTheSameBytesAlwaysProduceTheSameMap. The gate's decisions have to be
// reproducible from the artifact, and Go map iteration is not ordered — a
// verdict that depended on it would be a gate that changes its mind between
// runs on identical input.
func TestTheSameBytesAlwaysProduceTheSameMap(t *testing.T) {
	doc := map[string]any{
		"schema_version": 2,
		"nodes": []map[string]any{
			{"id": "a/x.go", "kind": "file", "path": "a/x.go", "area": "a"},
			{"id": "b/y.go", "kind": "file", "path": "b/y.go", "area": "b"},
			{"id": "c/z.go", "kind": "file", "path": "c/z.go", "area": "c"},
		},
		"edges": []map[string]any{
			{"source": "a/x.go", "target": "b/y.go", "kind": "calls", "confidence": ConfidenceExtracted},
			{"source": "b/y.go", "target": "c/z.go", "kind": "calls", "confidence": ConfidenceExtracted},
			{"source": "a/x.go", "target": "c/z.go", "kind": "calls", "confidence": "ambiguous"},
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "g.json")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	var first string
	for i := 0; i < 40; i++ {
		m, err := Load(p)
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, area := range []string{"a", "b", "c"} {
			got, _ := m.AreaForPath(area + "/x.go")
			fmt.Fprintf(&b, "%s=%s:%v;", area, got, m.Neighbours(area))
		}
		s := m.Stats()
		fmt.Fprintf(&b, "widest=%s|max=%d|adj=%d|skipped=%d",
			s.WidestArea, s.MaxDegree, s.Adjacencies, s.AmbiguousSkipped)
		fmt.Fprintf(&b, "|degraded=%v", m.Degraded())

		if i == 0 {
			first = b.String()
			continue
		}
		if b.String() != first {
			t.Fatalf("load %d differs from the first:\n %s\n %s", i, first, b.String())
		}
	}
}

// TestAmbiguousEdgesNeverCreateAdjacencyAtAnyScale. The narrowing is the whole
// safety argument of the rung: an unresolved edge would let an agent write into
// another subsystem on a resolution the analyser declined to make.
func TestAmbiguousEdgesNeverCreateAdjacencyAtAnyScale(t *testing.T) {
	var nodes, edges []map[string]any
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("area%d/f.go", i)
		nodes = append(nodes, map[string]any{
			"id": id, "kind": "file", "path": id, "area": fmt.Sprintf("area%d", i)})
	}
	for i := 0; i < 200; i++ {
		for _, conf := range []string{"ambiguous", "inferred", "guessed", "", "EXTRACTED", " extracted"} {
			edges = append(edges, map[string]any{
				"source": fmt.Sprintf("area%d/f.go", i), "target": "area0/f.go",
				"kind": "calls", "confidence": conf})
		}
	}
	m := load(t, map[string]any{"nodes": nodes, "edges": edges, "schema_version": 2})
	if m == nil {
		t.Fatal("the graph must load")
	}
	if got := m.Stats().Adjacencies; got != 0 {
		t.Fatalf("unresolved edges created %d adjacencies", got)
	}
	// And the map must say why it is empty rather than presenting a repository
	// with no coupling: nothing in it was understood.
	if len(m.Degraded()) == 0 {
		t.Fatal("a map that rejected every coupling edge must report it")
	}
}

// FuzzLoad drives the parser and the whole derivation with arbitrary bytes.
//
// The properties are the ones a gate cannot do without: it must not panic on
// any input, and whatever it does build must satisfy the adjacency invariants.
// A crash here is a repository that cannot open a session.
func FuzzLoad(f *testing.F) {
	f.Add(`{"schema_version":2,"nodes":[{"id":"a","kind":"file","path":"a/x.go","area":"a"}],"edges":[]}`)
	f.Add(`{"nodes":[{"id":"a","path":"a/x.go","area":"a"},{"id":"b","path":"b/y.go","area":"b"}],` +
		`"edges":[{"source":"a","target":"b","kind":"calls","confidence":"extracted"}]}`)
	f.Add(`{"nodes":[],"edges":[]}`)
	f.Add(`{"nodes":null,"edges":null,"meta":null}`)
	f.Add(`{"meta":{"devmap_rust":{"generation_id":7,"analysis_status":"partial",` +
		`"edge_endpoints_without_node":3}},"nodes":[{"id":"a","path":"a","area":"."}],"edges":[]}`)
	f.Add(`not json at all`)
	f.Add(``)

	f.Fuzz(func(t *testing.T, doc string) {
		p := filepath.Join(t.TempDir(), "g.json")
		if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
			t.Skip()
		}
		m, err := Load(p)
		if err != nil {
			// Rejection is always a legitimate outcome; only a panic is not.
			return
		}
		if m == nil {
			t.Fatal("Load returned neither a map nor an error")
		}
		checkInvariants(t, m)
		// Whatever it holds, asking about a path must terminate and answer
		// consistently with itself.
		for _, probe := range []string{"", ".", "/", "a/b/c.go", "../x", strings.Repeat("x/", 100)} {
			area, ok := m.AreaForPath(probe)
			if !ok && area != "" {
				t.Fatalf("AreaForPath(%q) returned %q while reporting no area", probe, area)
			}
			if again, okAgain := m.AreaForPath(probe); again != area || okAgain != ok {
				t.Fatalf("AreaForPath(%q) is not deterministic: (%q,%v) then (%q,%v)",
					probe, area, ok, again, okAgain)
			}
		}
	})
}
