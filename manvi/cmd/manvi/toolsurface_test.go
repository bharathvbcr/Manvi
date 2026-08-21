package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"manvi/agents"
	"manvi/core/bus"
	"manvi/devcouncil"
	"manvi/flags"
	"manvi/llm"
	"manvi/tools"
)

// scriptedProvider answers with a fixed script of responses and records every
// request it was asked to stream.
//
// stubProvider in placement_test.go always ends the turn, which is all a
// placement test needs. Proving a tool is *absent* needs the child to actually
// try to call it: a schema that was never offered and a tool that cannot be
// dispatched are different claims, and Registry.Run dispatches by name whether
// or not the schema was offered.
type scriptedProvider struct {
	name   string
	serves []string
	script []llm.Response
	seen   []llm.Request
	turn   int
}

func (p *scriptedProvider) Name() string { return p.name }

func (p *scriptedProvider) Capability(model string) (llm.Capability, bool) {
	for _, m := range p.serves {
		if m == model {
			return llm.Capability{
				Provider:          p.name,
				Model:             model,
				ContextWindow:     32768,
				MaxOutputTokens:   4096,
				SupportsTools:     true,
				SupportsStreaming: true,
			}, true
		}
	}
	return llm.Capability{}, false
}

func (p *scriptedProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	p.seen = append(p.seen, req)
	resp := p.script[len(p.script)-1]
	if p.turn < len(p.script) {
		resp = p.script[p.turn]
	}
	p.turn++
	return &scriptedStream{resp: resp}, nil
}

type scriptedStream struct{ resp llm.Response }

func (s *scriptedStream) Next() (llm.Chunk, error) { return llm.Chunk{}, io.EOF }
func (s *scriptedStream) Close() error             { return nil }
func (s *scriptedStream) Response() (llm.Response, error) {
	return s.resp, nil
}

func endTurn(text string) llm.Response {
	return llm.Response{
		Message: llm.Message{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{llm.TextBlock{Text: text}},
		},
		StopReason: llm.StopEndTurn,
	}
}

func wantToolCall(name, args string) llm.Response {
	return llm.Response{
		Message: llm.Message{
			Role: llm.RoleAssistant,
			Content: []llm.ContentBlock{llm.ToolCallBlock{
				ID: "call-1", Name: name, Arguments: json.RawMessage(args),
			}},
		},
		StopReason: llm.StopToolUse,
	}
}

// offeredTools is the names the child was actually shown on its first request.
func offeredTools(t *testing.T, p *scriptedProvider) map[string]bool {
	t.Helper()
	if len(p.seen) == 0 {
		t.Fatal("the provider was never asked for anything; the child never ran")
	}
	out := map[string]bool{}
	for _, s := range p.seen[0].Tools {
		out[s.Name] = true
	}
	return out
}

// toolResultText digs the harness's answer to the child's tool call out of the
// follow-up request. This is what the child model actually read back.
func toolResultText(t *testing.T, p *scriptedProvider) string {
	t.Helper()
	if len(p.seen) < 2 {
		t.Fatalf("the child made %d request(s); it never got an answer to its tool call", len(p.seen))
	}
	var found []string
	for _, msg := range p.seen[len(p.seen)-1].Messages {
		for _, block := range msg.Content {
			result, isResult := block.(llm.ToolResultBlock)
			if !isResult {
				continue
			}
			for _, nested := range result.Content {
				if text, isText := nested.(llm.TextBlock); isText {
					found = append(found, text.Text)
				}
			}
		}
	}
	if len(found) == 0 {
		t.Fatal("no tool result reached the child's next request")
	}
	return strings.Join(found, "\n")
}

// runnerOver attaches a runner to one registry and one scripted provider.
func runnerOver(t *testing.T, registry *tools.Registry, p *scriptedProvider) *subAgentRunner {
	t.Helper()
	models := llm.NewRegistry()
	if err := models.Register(p); err != nil {
		t.Fatalf("registering provider: %v", err)
	}
	r := &subAgentRunner{}
	r.attach(subAgentConfig{
		provider:     p,
		models:       models,
		model:        p.serves[0],
		registry:     registry,
		systemPrompt: "PARENT PROMPT",
	})
	return r
}

// surfaceRegistry is a registry of stand-ins covering every group a narrowing
// rule can act on. Using fakes rather than the native surface keeps the
// assertions about the rule and not about which real tool happens to sit in
// which group today.
func surfaceRegistry(t *testing.T) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry(bus.New())
	for _, spec := range []struct {
		name     string
		group    string
		readOnly bool
	}{
		{"fake_read", tools.GroupCore, true},
		{"fake_write", tools.GroupCore, false},
		{"fake_mcp_read", tools.GroupMCP, true},
		{"fake_mcp_call", tools.GroupMCP, false},
		{"fake_nav", tools.GroupNav, true},
	} {
		spec := spec
		if err := reg.Register(tools.Tool{
			Schema:   llm.ToolSchema{Name: spec.name, Description: spec.name, InputSchema: json.RawMessage(`{"type":"object"}`)},
			Group:    spec.group,
			ReadOnly: spec.readOnly,
			Handler: func(ctx context.Context, call tools.Call) tools.Result {
				return tools.Result{Text: "ran " + spec.name}
			},
		}); err != nil {
			t.Fatalf("registering %s: %v", spec.name, err)
		}
	}
	return reg
}

// A child must not be able to dispatch a grandchild, and the depth bound is
// structural: the dispatching tools are absent from its registry.
//
// devcouncil_spawn_subagents was the only name the subset removed, but it is
// not the only tool that starts a child. devcouncil_invoke_subagent calls the
// very same runner, so a child holding it produced a grandchild — and the
// counter-free depth bound the subset's comment describes was one tool name
// wide. This asserts the property against the real native surface rather than
// against a list of names copied out of it.
func TestAChildCannotReachAnySubAgentDispatchTool(t *testing.T) {
	flagReg, err := flags.NewHarnessRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	_, pipeline, err := nativeTools(flagReg)
	if err != nil {
		t.Fatal(err)
	}
	const invokeTool = "devcouncil_invoke_subagent"
	if !pipeline.Has(invokeTool) || !pipeline.Has(spawnSubagentsTool) {
		t.Fatalf("the dispatch tools are not registered; this test no longer proves anything")
	}

	p := &scriptedProvider{
		name: "p", serves: []string{"m"},
		script: []llm.Response{
			wantToolCall(invokeTool, `{"subagents":[{"type_name":"self","role":"grandchild","prompt":"go"}]}`),
			endTurn("done"),
		},
	}
	r := runnerOver(t, pipeline, p)

	if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label: "child", Prompt: "work",
	}); err != nil {
		t.Fatalf("RunSubAgent: %v", err)
	}

	offered := offeredTools(t, p)
	for _, dispatch := range []string{invokeTool, spawnSubagentsTool,
		"devcouncil_define_subagent", "devcouncil_send_message", "devcouncil_manage_subagents"} {
		if offered[dispatch] {
			t.Errorf("a child was offered %s", dispatch)
		}
	}
	// Absence, not a hidden schema: the child named the tool anyway.
	if got := toolResultText(t, p); !strings.Contains(got, "unknown tool") {
		t.Errorf("a child that named %s reached something: %q", invokeTool, got)
	}
}

// A role that did not ask for the MCP group does not get it. The "critic" role
// ships with enable_mcp_tools:false and, before this, held mcp_read_resource
// and mcp_list_tools regardless — a written permission that did nothing.
func TestARoleThatDeniesMCPToolsDoesNotGetThem(t *testing.T) {
	p := &scriptedProvider{name: "p", serves: []string{"m"}, script: []llm.Response{endTurn("done")}}
	r := runnerOver(t, surfaceRegistry(t), p)

	if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label:  "critic",
		Prompt: "review it",
		Surface: agents.Definition{
			Name: "critic", EnableMCPTools: false, EnableWriteTools: true,
		}.Surface(),
	}); err != nil {
		t.Fatalf("RunSubAgent: %v", err)
	}

	offered := offeredTools(t, p)
	for _, mcpTool := range []string{"fake_mcp_read", "fake_mcp_call"} {
		if offered[mcpTool] {
			t.Errorf("a role with enable_mcp_tools:false was offered %s", mcpTool)
		}
	}
	if !offered["fake_read"] || !offered["fake_write"] || !offered["fake_nav"] {
		t.Errorf("denying the MCP group took away unrelated tools: %v", offered)
	}
}

// The denial is by absence from the registry, not by a narrowed schema list.
func TestAnMCPDeniedChildCannotDispatchAnMCPToolByName(t *testing.T) {
	p := &scriptedProvider{
		name: "p", serves: []string{"m"},
		script: []llm.Response{wantToolCall("fake_mcp_call", `{}`), endTurn("done")},
	}
	r := runnerOver(t, surfaceRegistry(t), p)

	if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label:  "critic",
		Prompt: "review it",
		Surface: agents.Definition{
			Name: "critic", EnableWriteTools: true,
		}.Surface(),
	}); err != nil {
		t.Fatalf("RunSubAgent: %v", err)
	}
	if got := toolResultText(t, p); !strings.Contains(got, "unknown tool") {
		t.Errorf("an MCP-denied child ran an MCP tool by naming it: %q", got)
	}
}

// A role naming allowed_tools gets those and nothing else.
func TestAllowedToolsIsAnAllowlist(t *testing.T) {
	p := &scriptedProvider{name: "p", serves: []string{"m"}, script: []llm.Response{endTurn("done")}}
	r := runnerOver(t, surfaceRegistry(t), p)

	if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label:  "narrow",
		Prompt: "just read",
		Surface: agents.Definition{
			Name: "narrow", EnableMCPTools: true, EnableWriteTools: true,
			AllowedTools: []string{"fake_read", "fake_nav"},
		}.Surface(),
	}); err != nil {
		t.Fatalf("RunSubAgent: %v", err)
	}

	offered := offeredTools(t, p)
	if !offered["fake_read"] || !offered["fake_nav"] {
		t.Errorf("an allowlisted tool was withheld: %v", offered)
	}
	for _, denied := range []string{"fake_write", "fake_mcp_read", "fake_mcp_call"} {
		if offered[denied] {
			t.Errorf("%s survived an allowlist that did not name it", denied)
		}
	}
}

// allowed_tools only ever removes. A caller that asked for a non-mutating child
// must not receive a writing one because the role's allowlist names a write
// tool — the read-only floor wins, in both orders of composition.
func TestAllowedToolsCannotWidenPastTheReadOnlyFloor(t *testing.T) {
	p := &scriptedProvider{
		name: "p", serves: []string{"m"},
		script: []llm.Response{wantToolCall("fake_write", `{}`), endTurn("done")},
	}
	r := runnerOver(t, surfaceRegistry(t), p)

	if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label:    "narrow",
		Prompt:   "just read",
		ReadOnly: true,
		Surface: agents.Definition{
			Name: "narrow", EnableMCPTools: true, EnableWriteTools: true,
			AllowedTools: []string{"fake_read", "fake_write", "fake_mcp_call"},
		}.Surface(),
	}); err != nil {
		t.Fatalf("RunSubAgent: %v", err)
	}

	offered := offeredTools(t, p)
	if offered["fake_write"] || offered["fake_mcp_call"] {
		t.Errorf("an allowlist widened a read-only child past its floor: %v", offered)
	}
	if !offered["fake_read"] {
		t.Errorf("the read-only child lost its allowlisted read tool: %v", offered)
	}
	if got := toolResultText(t, p); !strings.Contains(got, "unknown tool") {
		t.Errorf("a read-only child ran an allowlisted write tool: %q", got)
	}
}

// A child dispatched with no role keeps the surface it has always had. This is
// what makes the tri-state necessary: a zero ToolSurface must not read as a
// role that denied everything.
func TestAnUndeclaredSurfaceNarrowsNothing(t *testing.T) {
	p := &scriptedProvider{name: "p", serves: []string{"m"}, script: []llm.Response{endTurn("done")}}
	r := runnerOver(t, surfaceRegistry(t), p)

	if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label: "untyped", Prompt: "work",
	}); err != nil {
		t.Fatalf("RunSubAgent: %v", err)
	}
	offered := offeredTools(t, p)
	for _, name := range []string{"fake_read", "fake_write", "fake_mcp_read", "fake_mcp_call", "fake_nav"} {
		if !offered[name] {
			t.Errorf("an untyped child lost %s; a zero surface must narrow nothing", name)
		}
	}
}

// An allowlist that strands a tool whose prerequisite it left out is refused,
// not quietly shipped.
//
// This is the failure tools.Tool.Requires exists to catch: llm.local.
// core_tools_only once offered devcouncil_verify_task while dropping
// devcouncil_checkout_task, and the model was handed a tool whose only refusal
// named the tool that had just been taken away. An allowlist is a second way to
// build exactly that dead end, so it is checked the same way — and the refusal
// names the role, so an operator can fix the list rather than debug the child.
func TestAnAllowlistThatStrandsAPrerequisiteIsRefused(t *testing.T) {
	reg := tools.NewRegistry(bus.New())
	handler := func(ctx context.Context, call tools.Call) tools.Result { return tools.Result{Text: "ok"} }
	if err := reg.Register(tools.Tool{
		Schema:   llm.ToolSchema{Name: "needs_lease", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Group:    tools.GroupTask,
		ReadOnly: true,
		Handler:  handler,
		Requires: []string{"takes_lease"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(tools.Tool{
		Schema:  llm.ToolSchema{Name: "takes_lease", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Group:   tools.GroupTask,
		Handler: handler,
	}); err != nil {
		t.Fatal(err)
	}

	p := &scriptedProvider{name: "p", serves: []string{"m"}, script: []llm.Response{endTurn("done")}}
	r := runnerOver(t, reg, p)

	_, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label:  "stranded",
		Prompt: "verify it",
		Surface: agents.Definition{
			Name: "stranded", EnableWriteTools: true,
			AllowedTools: []string{"needs_lease"},
		}.Surface(),
	})
	if err == nil {
		t.Fatal("an allowlist that stranded a prerequisite was accepted")
	}
	for _, want := range []string{"stranded", "needs_lease", "takes_lease"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if len(p.seen) != 0 {
		t.Errorf("the child was started anyway: %d request(s)", len(p.seen))
	}
}

// A dead end the allowlist did not create is not blamed on it. A read-only
// child already loses devcouncil_checkout_task while keeping the read-only
// devcouncil_verify_task that needs it; that gap predates any role and must not
// turn every read-only child into a refusal.
func TestAPreExistingDeadEndIsNotBlamedOnTheAllowlist(t *testing.T) {
	reg := tools.NewRegistry(bus.New())
	handler := func(ctx context.Context, call tools.Call) tools.Result { return tools.Result{Text: "ok"} }
	if err := reg.Register(tools.Tool{
		Schema:   llm.ToolSchema{Name: "needs_lease", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Group:    tools.GroupTask,
		ReadOnly: true,
		Handler:  handler,
		Requires: []string{"takes_lease"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(tools.Tool{
		Schema:  llm.ToolSchema{Name: "takes_lease", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Group:   tools.GroupTask,
		Handler: handler,
	}); err != nil {
		t.Fatal(err)
	}

	p := &scriptedProvider{name: "p", serves: []string{"m"}, script: []llm.Response{endTurn("done")}}
	r := runnerOver(t, reg, p)

	if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label:    "reader",
		Prompt:   "verify it",
		ReadOnly: true,
		Surface: agents.Definition{
			Name: "reader", AllowedTools: []string{"needs_lease"},
		}.Surface(),
	}); err != nil {
		t.Fatalf("a pre-existing read-only dead end was reported as the role's fault: %v", err)
	}
}
