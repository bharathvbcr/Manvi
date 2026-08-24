// Package repomap turns the code graph into the answers the policy gate needs.
//
// This is the unification. Before it, three consumers each had their own idea
// of repository structure: the write gate's neighbour rule had none at all and
// reported `repo_map.unavailable` on every unplanned write, the verifier
// classified scope by glob alone, and the code-intelligence tools did not
// exist. All three now read one graph.
//
// # Why this derives adjacency instead of reading it
//
// The obvious implementation is to read `subsystems[].neighbors` from the repo
// map artifact. Two things make that wrong, and both were found by looking at
// the artifact rather than at its documentation:
//
//  1. The Rust producer emits `"neighbors": []` unconditionally — the field is
//     a literal in its manifest writer. The Python producer computes it from
//     cross-area edges. A consumer reading the field cannot tell "this
//     repository has no adjacent subsystems" from "this producer does not
//     compute the field", and the first is a decision while the second is a
//     missing feature. Believing it would make the gate deny neighbour-scope
//     writes with a reason that confidently states the subsystem is not a
//     declared neighbour.
//
//  2. The artifact's two halves used different vocabularies for the same word.
//     `files[].area` is a directory path, while `subsystems[].area` was the
//     graph community id (`community-4`), so the intersection of the two was
//     empty for all 132 files and a lookup joining them returned nothing every
//     time — and "nothing" is indistinguishable from "not adjacent".
//
//     This one is fixed in the producer as of DevCouncil 70ad30e (2026-08-17):
//     both fields now come from one rule, and a subsystem with no file under
//     its area is dropped rather than emitted. It is recorded in the past
//     tense rather than deleted because it is why this package does not join
//     those two fields, and because a consumer that trusted the field would
//     have been silently wrong for as long as it held — which is the reason
//     that outlives the specific defect.
//
// So adjacency is computed here from the edges: if a symbol in one area
// references a symbol in another, those areas are neighbours. That is evidence
// rather than a claim, it uses one vocabulary throughout, and it cannot be
// silently emptied by a producer that stubs a field. Reason 1 alone still
// requires it, and this path reads the code graph's own `nodes[].area` — which
// was a directory prefix before the fix and after it — so neither the defect
// nor its repair changes what this package computes.
package repomap

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
)

// node is the subset of a graph node this package reads.
type node struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	Area      string `json:"area"`
	Community string `json:"community"`
}

type edge struct {
	Source     string `json:"source"`
	Target     string `json:"target"`
	Kind       string `json:"kind"`
	Confidence string `json:"confidence"`
}

// couplingKinds are the edge kinds that mean one area actually depends on
// another. `contains` and `member_of` are structural relations inside a single
// file and never cross an area; listing them would be harmless but misleading
// about what the relation means.
var couplingKinds = map[string]bool{"calls": true, "references": true, "imports": true}

// ConfidenceExtracted marks an edge whose target the analyser resolved rather
// than guessed.
//
// Only these create adjacency, and that is a deliberate narrowing. On this
// repository, edges marked `ambiguous` — the analyser could not determine which
// symbol a call reaches — account for 288 of the 431 linked area pairs, so
// admitting them roughly triples the neighbourhood. The consequence is not
// abstract: an ambiguous edge would let an agent write into another subsystem
// on the strength of a resolution the analyser itself declined to make. A scope
// decision has to rest on evidence, and a guess is the opposite of that.
const ConfidenceExtracted = "extracted"

type graph struct {
	Nodes         []node `json:"nodes"`
	Edges         []edge `json:"edges"`
	SchemaVersion int    `json:"schema_version"`
	Meta          struct {
		// DevmapRust is the producer's own account of the run that wrote this
		// file. Every field here was being decoded and discarded, including
		// the generation stamp that is the only exact way to tell whether the
		// artifact and the index describe the same tree.
		DevmapRust struct {
			GenerationID             int    `json:"generation_id"`
			AnalysisStatus           string `json:"analysis_status"`
			EdgeEndpointsWithoutNode int    `json:"edge_endpoints_without_node"`
			DuplicateEdgesDropped    int    `json:"duplicate_edges_dropped"`
			DuplicateNodeIDsDropped  int    `json:"duplicate_node_ids_dropped"`
		} `json:"devmap_rust"`
	} `json:"meta"`
}

// SupportedSchema is the code-graph schema version this build was written
// against. A different one is reported rather than refused: see Degraded.
const SupportedSchema = 2

// Provenance is what the artifact says about the run that produced it.
//
// It exists because the gate reads this file rather than the index, and a file
// is a thing that persists after the index it came from has moved on. The
// producer stamps enough to detect that; nothing here read the stamp.
type Provenance struct {
	// Stamped reports whether the producer wrote a provenance block at all.
	// Its absence is why GenerationID zero cannot be read as "generation zero".
	Stamped        bool   `json:"stamped"`
	SchemaVersion  int    `json:"schema_version"`
	GenerationID   int    `json:"generation_id"`
	Nodes          int    `json:"nodes"`
	AnalysisStatus string `json:"analysis_status,omitempty"`
	// OrphanEndpoints counts edges the producer wrote whose endpoints are not
	// nodes in the same file. build skips exactly these, so each is a coupling
	// this map cannot see.
	OrphanEndpoints         int `json:"orphan_endpoints"`
	DuplicateEdgesDropped   int `json:"duplicate_edges_dropped"`
	DuplicateNodeIDsDropped int `json:"duplicate_node_ids_dropped"`
}

// Map answers the two questions the neighbour rung asks.
type Map struct {
	// areaOf maps a repo-relative file path to its area.
	areaOf map[string]string
	// adjacent maps an area to the areas it references or is referenced by.
	adjacent map[string]map[string]bool
	// stats describe what was loaded, for the run report.
	stats Stats
	// prov is what the artifact said about its own derivation.
	prov Provenance
	// vocab records what build understood of the producer's wording. See
	// Degraded: a value this build does not recognise reduces the map rather
	// than failing it, and a reduced map that cannot say so is the failure this
	// package exists to prevent.
	vocab vocabulary
}

// vocabulary is what build observed of the producer's field values, kept so
// Degraded can distinguish "the analyser resolved nothing" from "this build
// understood nothing the analyser said".
type vocabulary struct {
	// declaredAreas counts nodes that carried an area of their own.
	declaredAreas int
	// couplingRecognised counts coupling edges whose confidence this build
	// read as resolved.
	couplingRecognised int
	// couplingRejected counts coupling edges skipped for their confidence.
	couplingRejected int
	// rejectedValues are the distinct confidence values that caused those
	// skips, so a report can name what it did not understand.
	rejectedValues map[string]bool
}

// Stats describe a loaded map, so a caller can say what it is reasoning over.
type Stats struct {
	Files       int `json:"files"`
	Areas       int `json:"areas"`
	Edges       int `json:"edges"`
	Adjacencies int `json:"adjacencies"`
	// AmbiguousSkipped counts coupling edges excluded because the analyser did
	// not resolve them. Reported rather than dropped silently: it is the
	// difference between "these areas are not coupled" and "we could not tell",
	// and on a repository where it dominates, the neighbour rule is narrower
	// than an operator might assume.
	AmbiguousSkipped int `json:"ambiguous_skipped"`
	// MaxDegree is the largest number of neighbours any one area has. A value
	// approaching Areas means the rule permits nearly any write, which is a
	// property of the codebase rather than a defect, and an operator should be
	// able to see it before relying on the rule.
	MaxDegree int `json:"max_degree"`
	// WidestArea names that area.
	WidestArea string `json:"widest_area,omitempty"`
}

// Permissive reports whether the neighbour relation covers most of the
// repository, in which case the rule is close to allowing everything.
//
// The ratio used to be guarded by `Areas > 2`, and that clause excluded the two
// graphs that are the *most* permissive of all. For a complete neighbour graph
// MaxDegree is Areas-1, so `MaxDegree*2 >= Areas` holds at every Areas >= 2:
// the clause was the only thing keeping the small total cases quiet, and a
// repository where every area neighbours every other was reported as not
// permissive at two areas and permissive at three — the same relation, opposite
// verdicts, decided by a threshold that had nothing to do with how wide the
// relation was. An allow qualified in one repository and clean in a strictly
// more permissive one is a check reporting that it meant something when it
// could not have.
//
// One area is stronger still, and the ratio cannot express it: an area has no
// neighbours to count when there is nothing to be adjacent to, so the widest
// degree is zero while every indexed file sits in the same subsystem as every
// planned one. It is stated separately rather than folded into the arithmetic,
// because it is a different fact — not "the relation is wide" but "there is no
// relation to be outside of".
//
// Zero areas is not permissive. Nothing is indexed, so the rung answers
// "unknown" rather than "yes", and the policy layer records that as its own
// degradation; calling it permissive would name the wrong defect.
func (s Stats) Permissive() bool {
	if s.Areas <= 0 {
		return false
	}
	if s.Areas == 1 {
		return true
	}
	return s.MaxDegree*2 >= s.Areas
}

// Stats returns what was loaded.
func (m *Map) Stats() Stats { return m.stats }

// Load reads a code graph artifact.
//
// An empty graph is an error rather than an empty map. A Map with no files
// answers "unknown area" to every question, which the policy layer records as a
// degradation — correct but useless — whereas an error at load time tells the
// caller to build the index. The distinction is the difference between a gate
// that knows it is blind and one that reports blindness on every decision.
func Load(graphPath string) (*Map, error) {
	raw, err := os.ReadFile(graphPath)
	if err != nil {
		return nil, fmt.Errorf("repomap: reading %s: %w", graphPath, err)
	}
	var g graph
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, fmt.Errorf("repomap: %s is not a code graph: %w", graphPath, err)
	}
	if len(g.Nodes) == 0 {
		return nil, fmt.Errorf("repomap: %s holds no nodes; the index has not been built", graphPath)
	}
	return build(g), nil
}

func build(g graph) *Map {
	m := &Map{
		areaOf:   map[string]string{},
		adjacent: map[string]map[string]bool{},
		vocab:    vocabulary{rejectedValues: map[string]bool{}},
	}
	rust := g.Meta.DevmapRust
	m.prov = Provenance{
		// A block with no generation in it is the same as no block: what makes
		// the stamp usable is that it names a generation, not that the key
		// existed.
		Stamped:                 rust.GenerationID > 0,
		SchemaVersion:           g.SchemaVersion,
		GenerationID:            rust.GenerationID,
		Nodes:                   len(g.Nodes),
		AnalysisStatus:          rust.AnalysisStatus,
		OrphanEndpoints:         rust.EdgeEndpointsWithoutNode,
		DuplicateEdgesDropped:   rust.DuplicateEdgesDropped,
		DuplicateNodeIDsDropped: rust.DuplicateNodeIDsDropped,
	}

	// nodeArea covers every node, not only files, because edges join symbols.
	nodeArea := make(map[string]string, len(g.Nodes))
	for _, n := range g.Nodes {
		area := normalize(n.Area)
		if area != "" {
			m.vocab.declaredAreas++
		}
		if area == "" {
			// A node with no declared area falls back to its directory, which
			// is the same vocabulary `files[].area` uses. Leaving it empty
			// would silently exclude the node from adjacency.
			area = path.Dir(normalize(n.Path))
		}
		nodeArea[n.ID] = area
		if p := normalize(n.Path); p != "" {
			// A file node is authoritative for its own path; a symbol node
			// only fills a gap, so a file's area is never overwritten by a
			// symbol that happens to sort later.
			if n.Kind == "file" || m.areaOf[p] == "" {
				m.areaOf[p] = area
			}
		}
	}

	areas := map[string]bool{}
	for _, area := range m.areaOf {
		areas[area] = true
	}

	for _, e := range g.Edges {
		if !couplingKinds[e.Kind] {
			continue
		}
		from, okFrom := nodeArea[e.Source]
		to, okTo := nodeArea[e.Target]
		if !okFrom || !okTo || from == "" || to == "" || from == to {
			continue
		}
		if e.Confidence != ConfidenceExtracted {
			m.stats.AmbiguousSkipped++
			m.vocab.couplingRejected++
			if len(m.vocab.rejectedValues) < rejectedValueLimit {
				m.vocab.rejectedValues[e.Confidence] = true
			}
			continue
		}
		m.vocab.couplingRecognised++
		// Adjacency is symmetric. A reference in one direction is a coupling in
		// both, and the gate's question — "is this file near the plan" — does
		// not have a direction.
		m.link(from, to)
		m.link(to, from)
	}

	m.stats.Files = len(m.areaOf)
	m.stats.Areas = len(areas)
	m.stats.Edges = len(g.Edges)
	for area, neighbours := range m.adjacent {
		m.stats.Adjacencies += len(neighbours)
		if len(neighbours) > m.stats.MaxDegree {
			m.stats.MaxDegree = len(neighbours)
			m.stats.WidestArea = area
		}
	}
	return m
}

func (m *Map) link(from, to string) {
	if m.adjacent[from] == nil {
		m.adjacent[from] = map[string]bool{}
	}
	m.adjacent[from][to] = true
}

// AreaForPath returns the subsystem owning a path.
//
// An exact hit is preferred; otherwise the deepest indexed ancestor directory
// answers, so a file created during this turn — which by definition is not in
// the index — still resolves to the area it was created in. Without that
// fallback the neighbour rung would degrade on precisely the writes it exists
// to judge.
func (m *Map) AreaForPath(p string) (string, bool) {
	p = normalize(p)
	if p == "" {
		return "", false
	}
	if area, ok := m.areaOf[p]; ok {
		return area, true
	}
	for dir := path.Dir(p); dir != "." && dir != "/" && dir != ""; dir = path.Dir(dir) {
		if _, known := m.adjacent[dir]; known {
			return dir, true
		}
		// A directory that is some indexed file's area is a real area even if
		// nothing references it.
		for _, area := range m.areaOf {
			if area == dir {
				return dir, true
			}
		}
	}
	return "", false
}

// AreNeighbors reports whether two areas are coupled in the graph.
// NeighborsArePermissive reports whether this map's neighbour relation is wide
// enough that the scope rung is close to allowing everything.
//
// The computation already existed and was reachable only from `manvi map
// status`, which meant the condition was visible to an operator who thought to
// run a diagnostic and invisible in the decision it actually affects.
func (m *Map) NeighborsArePermissive() bool { return m.stats.Permissive() }

func (m *Map) AreNeighbors(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	return m.adjacent[a][b]
}

// Neighbours lists an area's neighbours, sorted, for reporting.
func (m *Map) Neighbours(area string) []string {
	out := make([]string, 0, len(m.adjacent[area]))
	for n := range m.adjacent[area] {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// rejectedValueLimit bounds how many distinct unrecognised confidence values
// one map retains. The values come from another repository's output and the set
// is only ever used to name what was not understood.
const rejectedValueLimit = 8

// Provenance returns what the artifact said about the run that produced it.
func (m *Map) Provenance() Provenance { return m.prov }

// Degraded names, one line each, the ways this map is less than it appears.
//
// Every entry here describes a map that loaded cleanly. That is the whole
// point: a graph that fails to parse announces itself, and the dangerous
// artifact is the one that parses into a confident, smaller answer.
func (m *Map) Degraded() []string {
	var out []string

	if m.prov.SchemaVersion != 0 && m.prov.SchemaVersion != SupportedSchema {
		out = append(out, fmt.Sprintf(
			"the code graph declares schema version %d and this build reads %d; fields it "+
				"renamed or moved are read as absent, and an area or coupling this build "+
				"cannot see is indistinguishable from one that does not exist",
			m.prov.SchemaVersion, SupportedSchema))
	}
	if status := m.prov.AnalysisStatus; status != "" && status != "ok" {
		out = append(out, fmt.Sprintf(
			"the analysis that wrote this graph reported status %q, so it describes as much of "+
				"the repository as that run got through", status))
	}
	if m.vocab.declaredAreas == 0 && m.prov.Nodes > 0 {
		out = append(out, fmt.Sprintf(
			"not one of the %d node(s) declared an area, so every subsystem here is this build's "+
				"own fallback to the containing directory rather than the producer's grouping",
			m.prov.Nodes))
	}
	// The vocabulary check. Rejecting some coupling edges is ordinary — the
	// analyser could not resolve them. Rejecting every single one, when there
	// were some to resolve, means the word this build matches on has moved.
	if m.vocab.couplingRecognised == 0 && m.vocab.couplingRejected > 0 {
		out = append(out, fmt.Sprintf(
			"all %d coupling edge(s) were rejected as unresolved and none carried %q, so this "+
				"map's empty adjacency is a vocabulary this build no longer reads rather than a "+
				"repository with no coupling; the value(s) seen were %s",
			m.vocab.couplingRejected, ConfidenceExtracted, m.rejected()))
	}
	if m.prov.OrphanEndpoints > 0 {
		out = append(out, fmt.Sprintf(
			"%d edge(s) in the graph name an endpoint that is not a node in it; each is a "+
				"coupling this map cannot place, so two areas it joins are not neighbours here",
			m.prov.OrphanEndpoints))
	}
	return out
}

// rejected renders the unrecognised confidence values, sorted so the message is
// stable across runs.
func (m *Map) rejected() string {
	out := make([]string, 0, len(m.vocab.rejectedValues))
	for v := range m.vocab.rejectedValues {
		out = append(out, strconv.Quote(v))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// DisagreementsWith compares this artifact's provenance against the index it
// should have been derived from.
//
// This is checkAgainstIndex's argument moved to the artifact. A build already
// verifies that the graph it analysed is the graph it stored; nothing verified
// that the *file the gate reads* came from that same generation. It is a
// separate file with its own lifetime: `devmap build` advances the index and
// writes no artifact, a manifest that fails after a successful build leaves the
// previous one in place, and either way the next session loads it without
// complaint. On this repository the index stood at generation 4 while the
// artifact carried generation 2, and both were reported as one healthy map.
//
// indexNodes may be zero when the caller could not read the index, in which
// case the node comparison is skipped rather than assumed to hold.
func (m *Map) DisagreementsWith(indexGeneration, indexNodes int) []string {
	var out []string
	if !m.prov.Stamped {
		return []string{fmt.Sprintf(
			"the code graph carries no generation stamp, so whether it was derived from the "+
				"index now at generation %d is unverified; the scope rung is deciding from a "+
				"file of unknown age", indexGeneration)}
	}
	if indexGeneration > 0 && m.prov.GenerationID != indexGeneration {
		out = append(out, fmt.Sprintf(
			"the code graph was written from generation %d and the index now stands at %d, so "+
				"the scope rung is deciding from a snapshot the navigation tools have already "+
				"moved past — run `manvi map build` to rewrite it",
			m.prov.GenerationID, indexGeneration))
	}
	// A second, independent signal, and the reason the generation check is not
	// on its own: an artifact stamped with the right generation but holding a
	// different number of nodes was not written from it whatever it claims.
	// The producer's own dropped-duplicate count is subtracted rather than
	// tolerated, so the comparison stays exact.
	if indexNodes > 0 {
		expected := indexNodes - m.prov.DuplicateNodeIDsDropped
		if m.prov.Nodes != expected {
			out = append(out, fmt.Sprintf(
				"the code graph holds %d node(s) and the index reports %d (less %d dropped as "+
					"duplicates), so the two do not describe the same tree",
				m.prov.Nodes, indexNodes, m.prov.DuplicateNodeIDsDropped))
		}
	}
	return out
}

// ErrNotBuilt reports that no artifact exists yet.
var ErrNotBuilt = errors.New("repomap: no code graph artifact")

// LoadIfPresent returns a map, or nil with no error when the artifact is
// absent.
//
// Nil is meaningful downstream: the policy layer takes a nil SubsystemMap and
// records `repo_map.unavailable` on every decision that would have consulted
// it. That is the honest outcome for a repository whose index has not been
// built, and it is why this returns nil rather than an empty map that would
// answer "no" confidently.
func LoadIfPresent(graphPath string) (*Map, error) {
	if _, err := os.Stat(graphPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return Load(graphPath)
}

func normalize(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), `\`, "/")
	p = strings.TrimPrefix(p, "./")
	return strings.TrimSuffix(p, "/")
}
