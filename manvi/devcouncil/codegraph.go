package devcouncil

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
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
	if withheld > 0 {
		notes = append(notes, fmt.Sprintf("%d source excerpt(s) withheld by the read gate", withheld))
	}
	return payload(body, notes, stale)
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

// sourceKeys are the envelope fields that carry file content.
var sourceKeys = []string{"source", "source_span"}

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
func withholdGraphSources(raw []byte, refuse func(string) string) ([]byte, int, error) {
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, 0, fmt.Errorf("unparseable graph answer: %w", err)
	}
	withheld := 0
	var walk func(v any, parent string)
	walk = func(v any, parent string) {
		switch node := v.(type) {
		case map[string]any:
			for _, key := range sourceKeys {
				value, ok := node[key].(string)
				if !ok {
					continue
				}
				if parent == "evidence" && key == "source" && resolutionSources[value] {
					continue
				}
				path, _ := node["file_path"].(string)
				if path == "" {
					path, _ = node["path"].(string)
				}
				rule := "source not attributable to a file"
				if path != "" {
					rule = refuse(path)
				}
				if rule != "" {
					delete(node, key)
					node["withheld"] = rule
					withheld++
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
		return nil, 0, err
	}
	return out, withheld, nil
}
