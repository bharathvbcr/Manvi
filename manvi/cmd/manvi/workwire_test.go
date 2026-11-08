package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// Wiring of the agent-facing work_* board tools (card ft-ad5dfc70, owner
// decision 2026-11-07: "Wire it now"). The tools exist in devcouncil.Registry
// only when Deps.Work is set; these tests hold where that happens and where it
// must never happen.

var workToolNames = []string{
	"work_create_item", "work_get_brief", "work_get_history",
	"work_get_item", "work_list_items", "work_update_own_item",
}

func workToolsIn(names map[string]bool) []string {
	var held []string
	for name := range names {
		if devcouncil.IsWorkTool(name) {
			held = append(held, name)
		}
	}
	sort.Strings(held)
	return held
}

func surfaceNames(p *tools.Registry) map[string]bool {
	out := map[string]bool{}
	for _, s := range p.Schemas() {
		out[s.Name] = true
	}
	return out
}

func requireWorkSurface(t *testing.T, what string, p *tools.Registry) {
	t.Helper()
	held := workToolsIn(surfaceNames(p))
	if strings.Join(held, ",") != strings.Join(workToolNames, ",") {
		t.Errorf("%s holds work tools %v, want exactly %v", what, held, workToolNames)
	}
}

// The agent surfaces list the board tools: `manvi run`'s and the TUI's
// registries are built by agentTools, the constructor that sets Deps.Work. A
// surface built without it holds none, and a session id that does not fit the
// binding (a resumed id longer than the store's identifier rule) must not cost
// the session its tools.
func TestAgentSurfacesHoldTheWorkTools(t *testing.T) {
	flagReg := newTestRegistry(t)

	_, plain, err := nativeToolsWith(flagReg, nil)
	if err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}
	if held := workToolsIn(surfaceNames(plain)); len(held) != 0 {
		t.Errorf("nativeToolsWith offers %v: only agentTools may bind the board", held)
	}

	for _, id := range []string{"", "0123456789abcdef", strings.Repeat("a", 64), "not valid!"} {
		_, p, err := agentTools(flagReg, nil, id)
		if err != nil {
			t.Fatalf("agentTools(%q): %v", id, err)
		}
		requireWorkSurface(t, "agentTools("+id+")", p)
	}

	// The TUI builds each session through newSessionState.
	s, err := newSessionState(flagReg, "S1", "0123456789abcdef", nil)
	if err != nil {
		t.Fatalf("newSessionState: %v", err)
	}
	requireWorkSurface(t, "a TUI session", s.pipeline)
}

func e2eOffered(req e2eRequest) map[string]bool {
	offered := map[string]bool{}
	for _, tool := range req.Tools {
		if fn, ok := tool["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				offered[name] = true
			}
		}
	}
	return offered
}

// manvi run, as the shipped binary: a real process against a scripted model
// server. The tool schemas on a request are what the model was offered.
//
// Three runs, because the local provider's default profile is dynamic: it opens
// with the core tools and the board tools are Extended, so they are discoverable
// (devcouncil_search_tools) but not on the first request. With dynamic loading
// off the whole surface is offered, and that is the run that proves the binary's
// Registry was built with the board bound. --chat-only offers nothing.
func TestHeadlessRunOffersTheWorkToolsAndChatOnlyOffersNone(t *testing.T) {
	stop := func(int) (map[string]any, string) { return map[string]any{"content": "ok"}, "stop" }

	full := &e2eModel{reply: stop}
	if out, status := e2eRun(t, e2eRepo(t), full.start(t), "say ok", "MANVI_LLM_LOCAL_DYNAMIC_TOOLS=false"); status != 0 {
		t.Fatalf("a turn that completed exited %d\n%s", status, out)
	}
	if len(full.seen()) == 0 {
		t.Fatal("the model server saw no request")
	}
	offered := e2eOffered(full.seen()[0])
	if len(offered) == 0 {
		t.Fatal("the first request offered no tools; the test proves nothing")
	}
	for _, name := range workToolNames {
		if !offered[name] {
			t.Errorf("manvi run did not offer %s; offered work tools: %v", name, workToolsIn(offered))
		}
	}

	// The default dynamic profile: not on the first request, found by search.
	dynamic := &e2eModel{reply: func(turn int) (map[string]any, string) {
		if turn == 1 {
			return toolCall("devcouncil_search_tools", map[string]any{"query": "work_create_item"})
		}
		return map[string]any{"content": "ok"}, "stop"
	}}
	if out, status := e2eRun(t, e2eRepo(t), dynamic.start(t), "find the board tool"); status != 0 {
		t.Fatalf("a dynamic turn exited %d\n%s", status, out)
	}
	seen := dynamic.seen()
	if len(seen) < 2 {
		t.Fatalf("the dynamic run made %d request(s), want the search and its follow-up", len(seen))
	}
	first := e2eOffered(seen[0])
	if !first["devcouncil_search_tools"] {
		t.Fatal("the dynamic profile did not offer devcouncil_search_tools; the board tools would be undiscoverable")
	}
	if held := workToolsIn(first); len(held) != 0 {
		t.Errorf("the dynamic profile offered %v up front; the board tools are Extended", held)
	}
	if !strings.Contains(marshal(t, seen[1].Messages), "work_create_item") {
		t.Errorf("searching for work_create_item found nothing, so a dynamic session cannot reach the board:\n%s", marshal(t, seen[1].Messages))
	}

	// --chat-only offers the model no tools at all, board tools included.
	chat := &e2eModel{reply: stop}
	if out, status := e2eRunWith(t, false, e2eRepo(t), chat.start(t), []string{"run", "--chat-only", "-p", "say ok"}); status != 0 {
		t.Fatalf("a chat-only turn exited %d\n%s", status, out)
	}
	if len(chat.seen()) == 0 {
		t.Fatal("the chat-only run made no request")
	}
	for _, req := range chat.seen() {
		if len(req.Tools) != 0 {
			t.Errorf("a chat-only run offered %d tool(s): %v", len(req.Tools), req.Tools)
		}
	}
}

// An inherited child never holds the board, whether it is untyped, read-only,
// or a role whose allowed_tools names the write tools. Absence, not refusal:
// the child names the tool anyway and is told it was not offered.
func TestChildrenHoldNoWorkTool(t *testing.T) {
	_, pipeline, err := agentTools(newTestRegistry(t), nil, "0123456789abcdef")
	if err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}
	requireWorkSurface(t, "the parent", pipeline)

	cases := []struct {
		name string
		req  devcouncil.SubAgentRequest
	}{
		{"untyped child", devcouncil.SubAgentRequest{Label: "child", Prompt: "work"}},
		{"read-only child", devcouncil.SubAgentRequest{Label: "child", Prompt: "look", ReadOnly: true}},
		{"role naming the write tools", devcouncil.SubAgentRequest{
			Label: "child", Prompt: "work",
			Surface: agents.Definition{
				Name: "namer", EnableWriteTools: true,
				AllowedTools: []string{"work_create_item", "work_update_own_item", "work_list_items", "devcouncil_read_file"},
			}.Surface(),
		}},
		{"read-only role naming the write tools", devcouncil.SubAgentRequest{
			Label: "child", Prompt: "look", ReadOnly: true,
			Surface: agents.Definition{
				Name: "namer", EnableWriteTools: true,
				AllowedTools: []string{"work_create_item", "work_update_own_item", "devcouncil_read_file"},
			}.Surface(),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &scriptedProvider{
				name: "p", serves: []string{"m"},
				script: []llm.Response{wantToolCall("work_create_item", `{"title":"x"}`), endTurn("done")},
			}
			r := runnerOver(t, pipeline, p)
			if _, err := r.RunSubAgent(context.Background(), tc.req); err != nil {
				t.Fatalf("RunSubAgent: %v", err)
			}
			if held := workToolsIn(offeredTools(t, p)); len(held) != 0 {
				t.Errorf("a child was offered %v", held)
			}
			if got := toolResultText(t, p); !strings.Contains(got, "was not offered") {
				t.Errorf("a child that named work_create_item reached something: %q", got)
			}
		})
	}
}

// The sub-agent floor is what takes the board out of a child, so it has to
// recognise every tool the board registers. If a tool were added with another
// prefix this fails, rather than the child quietly holding it.
func TestEveryBoardToolIsRecognisedAsOne(t *testing.T) {
	flagReg := newTestRegistry(t)
	_, board, err := agentTools(flagReg, nil, "")
	if err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}
	_, plain, err := nativeToolsWith(flagReg, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := surfaceNames(plain)
	for name := range surfaceNames(board) {
		if !base[name] && !devcouncil.IsWorkTool(name) {
			t.Errorf("agentTools adds %s, which IsWorkTool does not recognise, so a child would keep it", name)
		}
	}
	for name := range base {
		if devcouncil.IsWorkTool(name) {
			t.Errorf("%s is a non-board tool that IsWorkTool claims", name)
		}
	}
}

// Where the board is bound is a short, closed list: agentTools is called only
// by `manvi run` (run.go), the TUI (tui.go) and the listing of what those two
// offer (harness.go), and nothing in the whole module but harness.go builds a
// WorkBinding or sets Deps.Work. A call from serve.go would put the board behind
// the host connection's enhancement path, one from reflect.go or verifycritic.go
// would give a read-only analysis agent the write tools, and a Work key in
// serve/ (which also imports devcouncil, for the managed-run harness) would
// reach a managed run's tool surface.
func TestWorkBindingIsReachedOnlyFromRunAndTUI(t *testing.T) {
	callers := map[string][]string{}
	err := filepath.WalkDir(filepath.Join("..", ".."), func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(filepath.Join("..", ".."), path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		ast.Inspect(parsed, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				switch fn := x.Fun.(type) {
				case *ast.Ident:
					if fn.Name == "agentTools" || fn.Name == "buildNativeTools" {
						callers[fn.Name] = append(callers[fn.Name], rel)
					}
				case *ast.SelectorExpr:
					if fn.Sel.Name == "WorkBindingFor" {
						callers["WorkBindingFor"] = append(callers["WorkBindingFor"], rel)
					}
				}
			case *ast.KeyValueExpr:
				if key, ok := x.Key.(*ast.Ident); ok && key.Name == "Work" {
					callers["Deps.Work"] = append(callers["Deps.Work"], rel)
				}
			case *ast.UnaryExpr:
				if lit, ok := x.X.(*ast.CompositeLit); ok && x.Op == token.AND {
					if sel, ok := lit.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "WorkBinding" {
						callers["WorkBinding literal"] = append(callers["WorkBinding literal"], rel)
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"agentTools":          {"cmd/manvi/harness.go", "cmd/manvi/run.go", "cmd/manvi/tui.go"},
		"buildNativeTools":    {"cmd/manvi/harness.go", "cmd/manvi/harness.go"},
		"WorkBindingFor":      {"cmd/manvi/harness.go"},
		"Deps.Work":           {"cmd/manvi/harness.go"},
		"WorkBinding literal": nil,
	}
	for name, files := range want {
		got := callers[name]
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(files, ",") {
			t.Errorf("%s is reached from %v, want exactly %v", name, got, files)
		}
	}
}

// Managed Codex and Claude runs are separate processes with no channel back to
// Manvi, and nothing here builds one: the package that starts them imports
// neither the devcouncil tool surface nor the tool registry, so there is no
// path from a board tool into a managed run's arguments, environment or
// prompt. Exposing the board to them needs its own security-impact statement.
func TestManagedRunsHaveNoPathToTheBoardTools(t *testing.T) {
	dir := filepath.Join("..", "..", "codingagent")
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no codingagent sources found under %s: %v", dir, err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.HasSuffix(path, "/manvi/devcouncil") || strings.HasSuffix(path, "/manvi/tools") {
				t.Errorf("%s imports %s: a managed run could be handed the agent tool surface", filepath.Base(file), path)
			}
		}
		body, err := os.ReadFile(file) // #nosec G304 -- a source file this test just globbed.
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "work_") {
			t.Errorf("%s names a work_ tool", filepath.Base(file))
		}
	}
}
