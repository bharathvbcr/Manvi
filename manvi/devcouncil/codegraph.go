package devcouncil

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/devmap"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// devcouncil_code is the one graph tool every session with a code map holds.
//
// One tool with a kind rather than four: every schema is paid for on every
// request and selection accuracy falls as the offered set grows (see
// tools.Tool.Extended), so the graph's questions share a single entry point.
// It answers through DevCouncil's devmap.Client.Advanced — the same bounded,
// generation-qualified query manvi serve already uses — so the agent and the
// workbench ask one owner, not two.

const (
	// codeDefaultBudget is the token budget devmap spends on one answer when
	// the model names none. Large enough for an explore with its blast radius,
	// small enough that one call cannot fill a local model's window.
	codeDefaultBudget = 3000
	// codeMaxBudget is the most one call may ask for.
	codeMaxBudget = 20000
	// codeMaxDepth bounds the walk; Advanced accepts up to 64, which is a
	// whole-repository answer no turn should pay for unasked.
	codeMaxDepth = 8
	// maxCodeAnswerBytes bounds what reaches the model after redaction.
	maxCodeAnswerBytes = 256 << 10
)

func (r *Registry) codeTool() tools.Tool {
	return tools.Tool{
		Schema: schema("devcouncil_code",
			"Ask the code graph. kind=explore: definitions matching query, with source, callers, callees "+
				"and blast radius. kind=impact: everything that depends on a symbol or file (callers first). "+
				"kind=trace: how query reaches to. kind=affected: the test files reachable from targets. "+
				"Every answer says what it left out (truncated, walk_incomplete, unresolved namesakes): "+
				"an empty list on a truncated answer is not 'nothing exists'. The source in an answer can "+
				"be older than the file; when they disagree, the file wins.",
			`{"type":"object","properties":{`+
				`"kind":{"type":"string","enum":["explore","impact","trace","affected"]},`+
				`"query":{"type":"string","description":"symbol, file or name (explore, impact, trace from)"},`+
				`"to":{"type":"string","description":"trace destination"},`+
				`"targets":{"type":"array","items":{"type":"string"},"description":"files or symbols (affected)"},`+
				`"depth":{"type":"integer","minimum":1,"maximum":8,"description":"walk depth, default 2"},`+
				`"budget":{"type":"integer","minimum":1,"maximum":20000,"description":"token budget, default 3000"}`+
				`},"required":["kind"]}`),
		ReadOnly: true,
		// Core whenever a navigator is attached: the graph is the first thing
		// an agent should ask, so it is not something to discover and activate.
		// With no navigator it stays Extended and the profiles drop it, which is
		// the rule for a tool that cannot answer.
		Extended: r.deps.Map == nil,
		Group:    tools.GroupNav,
		Handler:  r.codeQuery,
		// Admitted inside devcouncil_batch: a lookup, safe to run concurrently.
		// Batchable is set where each tool is defined, never from a list of
		// names beside the registry, so a tool is in a batch because its own
		// definition says so.
		Batchable: true,
	}
}

func (r *Registry) codeQuery(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Kind    string   `json:"kind"`
		Query   string   `json:"query"`
		To      string   `json:"to"`
		Targets []string `json:"targets"`
		Depth   int      `json:"depth"`
		Budget  int      `json:"budget"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	q, err := codeQueryFor(args.Kind, args.Query, args.To, args.Targets, args.Depth, args.Budget)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	client, err := r.navigator()
	if err != nil {
		return unavailable("code graph", err)
	}
	res, err := client.Advanced(devmap.WithCall(ctx), q)
	if err != nil {
		return unavailable("code graph "+args.Kind, err)
	}
	data, withheld, err := withholdGraphSources(res.Data, func(path string) string { return r.readRefusal(path) })
	if err != nil {
		return unavailable("code graph "+args.Kind, err)
	}
	data, shared, err := hoistSharedEnvelopeFields(data)
	if err != nil {
		return unavailable("code graph "+args.Kind, err)
	}
	if len(data) > maxCodeAnswerBytes {
		return tools.Errorf("the %s answer is %d bytes after redaction, past the %d-byte bound; "+
			"ask again with a smaller budget or depth", args.Kind, len(data), maxCodeAnswerBytes)
	}
	body := map[string]any{"kind": args.Kind, "answer": jsontext.Value(data)}
	var notes []string
	stale := false
	if res.Index != nil {
		body["index_generation"] = res.Index.GenerationID
		stale = !res.Index.IsFresh
	}
	notes = append(notes, withheld.notes()...)
	if shared != nil {
		body["shared_envelope"] = shared
	}
	result := payload(body, notes, stale)
	if notice := r.staleIndexNotice(res.Index); notice != "" {
		return withLeadingField(result, "index_notice", notice)
	}
	return result
}

// withLeadingField puts a string field first in a payload's JSON object.
//
// payload renders its map in key order, which would leave a field named for its
// importance in the middle of the answer. A stale-index notice is read before
// the answer or not at all, so it is spliced ahead of the rest. A result that
// is an error, or whose text is not an indented object, is returned as it is:
// the notice then is not shown, but the answer is not corrupted.
func withLeadingField(res tools.Result, key, value string) tools.Result {
	const open = "{\n"
	if res.IsError || !strings.HasPrefix(res.Text, open) {
		return res
	}
	name, err := json.Marshal(key)
	if err != nil {
		return res
	}
	text, err := json.Marshal(value)
	if err != nil {
		return res
	}
	res.Text = open + "  " + string(name) + ": " + string(text) + ",\n" + res.Text[len(open):]
	return res
}

// sharedEnvelopeFields are the completeness fields devmap repeats on every
// sub-envelope of a composed answer (an explore carries one per definition,
// per direction, and one for the blast radius).
var sharedEnvelopeFields = []string{"source_freshness", "walk_incomplete"}

// envelopeMarker is the key every devmap completeness envelope carries.
const envelopeMarker = "truncated"

// hoistSharedEnvelopeFields moves a repeated completeness field to one place.
//
// A field is hoisted only when it can be restored without loss: it occurs at
// least twice, every occurrence has the identical value, and no envelope of the
// answer lacks it (an envelope without walk_incomplete is a walk that did
// complete, and folding the others into a shared value would erase that
// difference). Anything else stays where the producer put it. The returned map
// is nil when nothing was hoisted.
func hoistSharedEnvelopeFields(raw []byte) ([]byte, map[string]any, error) {
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, nil, fmt.Errorf("unparseable graph answer: %w", err)
	}
	var envelopes []map[string]any
	var walk func(v any)
	walk = func(v any) {
		switch node := v.(type) {
		case map[string]any:
			if _, ok := node[envelopeMarker]; ok {
				envelopes = append(envelopes, node)
			}
			for _, child := range node {
				walk(child)
			}
		case []any:
			for _, child := range node {
				walk(child)
			}
		}
	}
	walk(root)
	shared := map[string]any{}
	for _, field := range sharedEnvelopeFields {
		holders, value, ok := identicalEverywhere(envelopes, field)
		if !ok {
			continue
		}
		for _, holder := range holders {
			delete(holder, field)
		}
		shared[field] = value
	}
	if len(shared) == 0 {
		return raw, nil, nil
	}
	shared["note"] = "each of these was identical in every sub-envelope of the answer and is removed from each"
	out, err := json.Marshal(root, json.Deterministic(true))
	if err != nil {
		return nil, nil, err
	}
	return out, shared, nil
}

// identicalEverywhere reports the envelopes carrying field, and its value, when
// there are at least two, all carry it, and all carry the same value.
func identicalEverywhere(envelopes []map[string]any, field string) ([]map[string]any, any, bool) {
	var first []byte
	var value any
	for _, env := range envelopes {
		v, ok := env[field]
		if !ok {
			return nil, nil, false
		}
		enc, err := json.Marshal(v, json.Deterministic(true))
		if err != nil {
			return nil, nil, false
		}
		if first == nil {
			first, value = enc, v
		} else if string(enc) != string(first) {
			return nil, nil, false
		}
	}
	if len(envelopes) < 2 {
		return nil, nil, false
	}
	return envelopes, value, true
}

// codeQueryFor validates the model's arguments into the bounded query, applying
// this tool's defaults before devmap's own validation runs.
func codeQueryFor(kind, query, to string, targets []string, depth, budget int) (devmap.AdvancedQuery, error) {
	if depth == 0 {
		depth = 2
	}
	if depth < 1 || depth > codeMaxDepth {
		return devmap.AdvancedQuery{}, fmt.Errorf("depth must be in 1..%d, got %d", codeMaxDepth, depth)
	}
	if budget == 0 {
		budget = codeDefaultBudget
	}
	if budget < 1 || budget > codeMaxBudget {
		return devmap.AdvancedQuery{}, fmt.Errorf("budget must be in 1..%d, got %d", codeMaxBudget, budget)
	}
	q := devmap.AdvancedQuery{Kind: devmap.QueryKind(kind), Query: strings.TrimSpace(query), To: strings.TrimSpace(to),
		Targets: targets, Depth: depth, Budget: budget}
	switch q.Kind {
	case devmap.QueryExplore, devmap.QueryImpact, devmap.QueryTrace, devmap.QueryAffected:
	default:
		return devmap.AdvancedQuery{}, fmt.Errorf("kind must be explore, impact, trace or affected, got %q", kind)
	}
	if err := devmap.ValidateAdvancedQuery(q); err != nil {
		return devmap.AdvancedQuery{}, err
	}
	return q, nil
}

// sourceKeys are the envelope fields that carry file content. The list is
// maintained here, mirroring the content-bearing key list DevCouncil's
// feat/devmap-ask-evidence work publishes; the pinned devmap (d938f7f8) has no
// such symbol to import, and its own answers are not withheld for any of these.
// receiver is the raw text of an unbound call's receiver (up to 64 characters of
// source), so it is content: an unresolved_namesakes site names its file in
// source_file. evidence.source shares a name with source and is provenance;
// resolutionSources below lets it through.
var sourceKeys = []string{"source", "source_span", "source_indent", "signature", "value", "receiver"}

// resolutionSources are the values of devmap's ResolutionSource
// (devmap-resolve model.rs), which an edge's evidence object carries under
// "source". They say where a resolution came from and hold no file content, so
// they pass the gate; any other value under evidence is still an excerpt.
var resolutionSources = map[string]bool{"Resolver": true, "Stored": true, "Reconstructed": true}

// withholdGraphSources removes every source excerpt the read gate would not let
// the agent read, and every excerpt it cannot attribute to a file.
//
// The envelope is devmap's, passed through so its completeness fields
// (truncated, walk_incomplete, unresolved namesakes) reach the model intact.
// But an excerpt is file content: devcouncil_graph_query withholds a span by
// the same rule, and a second graph tool that passed raw source through would
// be a way round the read gate. An excerpt whose object names no file is
// withheld too — the check cannot run, and a check that cannot run must not
// pass.
func withholdGraphSources(raw []byte, refuse func(string) string) ([]byte, graphWithheld, error) {
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, graphWithheld{}, fmt.Errorf("unparseable graph answer: %w", err)
	}
	var withheld graphWithheld
	var walk func(v any, parent string)
	walk = func(v any, parent string) {
		switch node := v.(type) {
		case map[string]any:
			// Judged first, from the object as devmap wrote it: nothing below
			// may have touched a path key before the gate reads it.
			path := nodePath(node)
			for _, key := range sourceKeys {
				value, ok := node[key].(string)
				if !ok {
					continue
				}
				if parent == "evidence" && key == "source" && resolutionSources[value] {
					continue
				}
				rule := "source not attributable to a file"
				if path != "" {
					rule = refuse(path)
				}
				if rule != "" {
					delete(node, key)
					node["withheld"] = rule
					withheld.excerpts++
				}
			}
			// Only devmap's prose fields are rewritten. Identity keys (id,
			// symbol_name, qualified_name, name, nodes) and content keys
			// (sourceKeys) are never run through the sanitizer: a symbol whose
			// name or body mentions "call site" is data, and the path gate
			// already judges its content.
			for key, child := range node {
				if proseKeys[key] {
					var replaced int
					node[key], replaced = withholdProse(child)
					withheld.samples += replaced
				}
			}
			for name, child := range node {
				walk(child, name)
			}
		case []any:
			for _, child := range node {
				walk(child, parent)
			}
		}
	}
	walk(root, "")
	out, err := json.Marshal(root, json.Deterministic(true))
	if err != nil {
		return nil, graphWithheld{}, err
	}
	return out, withheld, nil
}

// graphWithheld counts what withholdGraphSources removed, by kind: source
// excerpts the read gate refused, and prose strings in which a call-site
// sample was replaced. They are different things to a reader, so they are never
// one number.
type graphWithheld struct {
	excerpts, samples int
}

func (w graphWithheld) total() int { return w.excerpts + w.samples }

// notes are the sentences that tell the model what was left out.
func (w graphWithheld) notes() []string {
	var notes []string
	if w.excerpts > 0 {
		notes = append(notes, fmt.Sprintf("%d source excerpt(s) withheld by the read gate", w.excerpts))
	}
	if w.samples > 0 {
		notes = append(notes, fmt.Sprintf("%d devmap note(s) had a call-site sample withheld", w.samples))
	}
	return notes
}

// proseKeys are the envelope fields in which devmap writes sentences about its
// own answer, taken from the pinned devmap source: walk_incomplete and
// coverage_gap carry the radius notes that quote call-site samples, and the
// rest carry notes the same walk can join into. Each is a string or an array or
// object of strings; every string under one is sanitized.
var proseKeys = map[string]bool{
	"walk_incomplete":          true,
	"coverage_gap":             true,
	"dead_clusters_incomplete": true,
	"note":                     true,
	"notes":                    true,
	"scope_note":               true,
	"degraded_reason":          true,
	"signature_note":           true,
}

// withholdProse sanitizes every string under a prose field and counts the
// strings in which a call-site sample was replaced. Non-string leaves (a
// boolean walk_incomplete) pass as they are.
func withholdProse(v any) (any, int) {
	switch node := v.(type) {
	case string:
		clean := WithholdCallSiteSamples(node)
		if clean == node {
			return clean, 0
		}
		return clean, 1
	case []any:
		replaced := 0
		for i := range node {
			var n int
			node[i], n = withholdProse(node[i])
			replaced += n
		}
		return node, replaced
	case map[string]any:
		replaced := 0
		for k := range node {
			var n int
			node[k], n = withholdProse(node[k])
			replaced += n
		}
		return node, replaced
	}
	return v, 0
}

// pathKeys are the keys under which an envelope object names its file:
// file_path, path, and source_file (an unresolved call site). The gate reads a
// path from these, so no redaction may rewrite their values.
var pathKeys = []string{"file_path", "path", "source_file"}

func isPathKey(key string) bool { return slices.Contains(pathKeys, key) }

// nodePath is the file an envelope object names, under any of the pathKeys.
func nodePath(node map[string]any) string {
	for _, key := range pathKeys {
		if path, _ := node[key].(string); path != "" {
			return path
		}
	}
	return ""
}
