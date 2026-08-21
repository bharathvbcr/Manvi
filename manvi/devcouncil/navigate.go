package devcouncil

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"manvi/dc/devmap"
	"manvi/tools"
)

// The repo-navigation tools.
//
// They are the same three DevCouncil offers, answering from the same index its
// own Rust port builds. What is added here is the thing a navigation tool has
// to get right and usually does not: every answer says how much of the question
// it actually covered.
//
// A search that hit its budget, an index built before the last edit, a file
// still pending indexing — each of these produces a shorter answer that reads
// exactly like a complete one. `code_dead` is where that matters most: "no
// callers found" and "the index has no data" are the same empty list, and
// acting on the first is a cleanup while acting on the second deletes live
// code.

func (r *Registry) navigationTools() []tools.Tool {
	return []tools.Tool{
		{
			ReadOnly: true,
			Extended: true,
			Group:    tools.GroupNav,
			Schema:   schema("devcouncil_graph_query", "Find symbols by name or path in the code graph, with the file and the source span of each hit.", `{"type":"object","properties":{"query":{"type":"string","description":"symbol name, partial name, or path"}},"required":["query"]}`),
			Handler:  r.graphQuery,
		},
		{
			ReadOnly: true,
			Extended: true,
			Group:    tools.GroupNav,
			Schema:   schema("devcouncil_code_dead", "List symbols with no discovered callers. Each carries a confidence and, where one applies, the reason it is exempt — a symbol reached only through a build tag, a registry, or reflection looks callerless to any static analyser.", `{"type":"object","properties":{"include_exempt":{"type":"boolean","description":"include symbols already judged exempt (default false)"}}}`),
			Handler:  r.codeDead,
		},
		{
			ReadOnly: true,
			Extended: true,
			Group:    tools.GroupNav,
			Schema:   schema("devcouncil_graph_context", "For a set of files, what they depend on and what depends on them. Use before editing to see the blast radius.", `{"type":"object","properties":{"files":{"type":"array","items":{"type":"string"},"description":"repo-relative paths"}},"required":["files"]}`),
			Handler:  r.graphContext,
		},
	}
}

// navigator returns the client, or an error phrased as the unavailability it is.
func (r *Registry) navigator() (*devmap.Client, error) {
	if r.deps.Map == nil {
		return nil, fmt.Errorf("the code index is not configured; run `manvi map build` and set MANVI_MAP_BINARY")
	}
	return r.deps.Map, nil
}

func (r *Registry) graphQuery(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Query string `json:"query"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if strings.TrimSpace(args.Query) == "" {
		return tools.Errorf("query is required")
	}
	client, err := r.navigator()
	if err != nil {
		return unavailable("code graph", err)
	}
	if _, err := client.Available(ctx); err != nil {
		return unavailable("code graph", err)
	}

	result, err := client.Search(ctx, args.Query)
	if err != nil {
		return unavailable("code graph query", err)
	}

	type hit struct {
		Path string `json:"path"`
		Name string `json:"symbol,omitempty"`
		Kind string `json:"kind,omitempty"`
		// Span is truncated: a search result carrying whole function bodies
		// would spend the turn's context on material the agent did not ask for
		// and can fetch with read_file.
		Span string `json:"source_span,omitempty"`
	}
	hits := make([]hit, 0, len(result.Items))
	for _, item := range result.Items {
		hits = append(hits, hit{
			Path: item.FilePath, Name: item.Name, Kind: item.Kind,
			Span: firstLines(item.SourceSpan, 3),
		})
	}
	return payload(map[string]any{
		"query": args.Query, "matches": len(hits), "results": hits,
	}, result.Degraded, result.Stale)
}

func (r *Registry) codeDead(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		IncludeExempt bool `json:"include_exempt"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	client, err := r.navigator()
	if err != nil {
		return unavailable("dead-code analysis", err)
	}
	// Availability is checked before the query, not after an empty result. An
	// unbuilt index returns an empty list, and "no dead code" is a conclusion
	// somebody would act on.
	if _, err := client.Available(ctx); err != nil {
		return unavailable("dead-code analysis", err)
	}

	result, err := client.Dead(ctx)
	if err != nil {
		return unavailable("dead-code analysis", err)
	}

	type candidate struct {
		Path       string  `json:"path"`
		Symbol     string  `json:"symbol"`
		Confidence float64 `json:"confidence"`
		Exempt     bool    `json:"exempt,omitempty"`
		Reason     string  `json:"exemption_reason,omitempty"`
	}
	var out []candidate
	exempt := 0
	for _, item := range result.Items {
		reason := ""
		if item.ExemptionReason != nil {
			reason = *item.ExemptionReason
		}
		if item.IsExempt || reason != "" {
			exempt++
			if !args.IncludeExempt {
				continue
			}
		}
		out = append(out, candidate{
			Path: item.FilePath, Symbol: item.Name,
			Confidence: item.Confidence, Exempt: item.IsExempt, Reason: reason,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Confidence > out[j].Confidence })

	degraded := append([]string(nil), result.Degraded...)
	if !args.IncludeExempt && exempt > 0 {
		degraded = append(degraded, fmt.Sprintf(
			"%d candidate(s) with an exemption reason were omitted; pass include_exempt to see them", exempt))
	}
	return payload(map[string]any{
		"candidates": out,
		"count":      len(out),
		"guidance": "A candidate is not a deletion. Reflection, build tags, registries, " +
			"route tables, and generated code all look callerless to a static analyser — " +
			"confirm with a second independent signal before removing anything.",
	}, degraded, result.Stale)
}

func (r *Registry) graphContext(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Files []string `json:"files"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if len(args.Files) == 0 {
		return tools.Errorf("files is required")
	}
	// Bounded: a caller passing every path in the repository would issue one
	// subprocess per file and spend the turn on it.
	const maxFiles = 25
	truncatedFiles := 0
	if len(args.Files) > maxFiles {
		truncatedFiles = len(args.Files) - maxFiles
		args.Files = args.Files[:maxFiles]
	}
	client, err := r.navigator()
	if err != nil {
		return unavailable("graph context", err)
	}
	if _, err := client.Available(ctx); err != nil {
		return unavailable("graph context", err)
	}

	type fileContext struct {
		Path       string   `json:"path"`
		Area       string   `json:"area,omitempty"`
		Neighbours []string `json:"neighbouring_areas,omitempty"`
		DependsOn  []string `json:"depends_on,omitempty"`
		UsedBy     []string `json:"used_by,omitempty"`
		Error      string   `json:"error,omitempty"`
	}

	var degraded []string
	stale := false
	out := make([]fileContext, 0, len(args.Files))
	for _, file := range args.Files {
		fc := fileContext{Path: file}

		// The area and its neighbours come from the same map the write gate
		// consults, so what this reports and what the gate will permit are one
		// answer rather than two that can disagree.
		if r.deps.Subsystems != nil {
			if area, ok := r.deps.Subsystems.AreaForPath(file); ok {
				fc.Area = area
				fc.Neighbours = r.deps.Subsystems.Neighbours(area)
			}
		}

		edges, err := client.Deps(ctx, file)
		if err != nil {
			fc.Error = err.Error()
			out = append(out, fc)
			continue
		}
		// Direction comes from the data, not from the subcommand's name: an
		// edge whose source is this file is something it depends on, and one
		// whose target is this file is something that depends on it.
		fc.DependsOn, fc.UsedBy = split(file, edges.Items)
		stale = stale || edges.Stale
		degraded = append(degraded, edges.Degraded...)
		out = append(out, fc)
	}
	if truncatedFiles > 0 {
		degraded = append(degraded, fmt.Sprintf(
			"%d file(s) beyond the first %d were not examined", truncatedFiles, maxFiles))
	}
	return payload(map[string]any{"files": out}, dedupe(degraded), stale)
}

// payload renders a navigation answer, always carrying its qualifications.
func payload(body map[string]any, degraded []string, stale bool) tools.Result {
	degraded = dedupe(degraded)
	if stale {
		body["stale"] = true
	}
	if len(degraded) > 0 {
		body["degraded"] = degraded
	}
	// Stated in the body as well as in the fields, because the model reads the
	// text. A shorter answer that reads as a complete one is the failure mode
	// these tools exist to avoid.
	if len(degraded) > 0 {
		body["answer_is_partial"] = true
	}
	raw, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return tools.Errorf("rendering the answer: %v", err)
	}
	return tools.Result{Text: string(raw), Degraded: degraded}
}

// split partitions edges touching a file into what it depends on and what
// depends on it.
func split(file string, edges []devmap.Edge) (dependsOn, usedBy []string) {
	forward := map[string]bool{}
	backward := map[string]bool{}
	for _, e := range edges {
		if e.SourceFile == file && e.TargetFile != "" && e.TargetFile != file {
			forward[e.TargetFile] = true
		}
		if e.TargetFile == file && e.SourceFile != "" && e.SourceFile != file {
			backward[e.SourceFile] = true
		}
	}
	return sorted(forward), sorted(backward)
}

func sorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func firstLines(text string, n int) string {
	lines := strings.Split(text, "\n")
	if len(lines) <= n {
		return text
	}
	return strings.Join(lines[:n], "\n") + "\n…"
}

func dedupe(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
