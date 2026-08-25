package devcouncil

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"manvi/mcp"
	"manvi/policy"
	"manvi/tools"
)

func (r *Registry) mcpTools() []tools.Tool {
	return []tools.Tool{
		{
			Schema: schema("mcp_list_tools",
				"List available tools from configured Model Context Protocol (MCP 2.0) servers and Open Plugins.",
				`{"type":"object","properties":{"server_name":{"type":"string","description":"optional server name to filter by"}},"required":[]}`),
			Group:    tools.GroupMCP,
			ReadOnly: true,
			Handler:  r.mcpListTools,
			Extended: true,
		},
		{
			Schema: schema("mcp_call_tool",
				"Call a tool on an external MCP 2.0 server or Open Plugin over stateless JSON-RPC.",
				`{"type":"object","properties":{"server_name":{"type":"string","description":"target MCP server name"},"tool_name":{"type":"string","description":"tool name to invoke"},"arguments":{"type":"object","description":"tool arguments object"}},"required":["server_name","tool_name","arguments"]}`),
			Group:    tools.GroupMCP,
			Handler:  r.mcpCallTool,
			Extended: true,
		},
		{
			Schema: schema("mcp_list_resources",
				"List resources exposed by a target MCP server.",
				`{"type":"object","properties":{"server_name":{"type":"string","description":"target MCP server name"}},"required":["server_name"]}`),
			Group:    tools.GroupMCP,
			ReadOnly: true,
			Handler:  r.mcpListResources,
			Extended: true,
		},
		{
			Schema: schema("mcp_read_resource",
				"Read the contents of a resource from a target MCP server by URI.",
				`{"type":"object","properties":{"server_name":{"type":"string","description":"target MCP server name"},"uri":{"type":"string","description":"resource URI"}},"required":["server_name","uri"]}`),
			Group:    tools.GroupMCP,
			ReadOnly: true,
			Handler:  r.mcpReadResource,
			Extended: true,
		},
	}
}

// getMCPManager returns the manager this surface was built with, and refuses
// rather than improvising one when it was built without.
//
// It used to construct a manager and auto-discover servers whenever Deps.MCP
// was nil, which made mcp.enabled unenforceable through this path: the setting
// is applied where the manager is built, so a surface that builds its own has
// already decided the question. cmd/manvi now always passes one — a live
// manager, or a disabled manager carrying the reason — so the nil case is no
// longer the harness. It is some other embedder: DevPrism constructing Deps
// over the sidecar, or a caller yet to be written.
//
// Silently enabling MCP for exactly those callers is the wrong default. An
// embedder that wants MCP passes a manager; one that did not pass anything did
// not ask for servers, and discovering some on its behalf is a decision this
// package is not entitled to make. So the nil case becomes a disabled manager,
// whose refusals name the cause — distinct from the refusal an operator gets
// from mcp.enabled=false, because the remedies differ: one is a setting to
// change, the other a Deps field to populate.
func (r *Registry) getMCPManager() *mcp.Manager {
	if r.deps.MCP != nil {
		return r.deps.MCP
	}
	return mcp.NewDisabledManager(r.deps.Root,
		"this harness was built without an MCP manager (Deps.MCP is nil), so no servers were ever configured")
}

func (r *Registry) mcpListTools(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		ServerName string `json:"server_name,omitempty"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}

	mgr := r.getMCPManager()
	if args.ServerName != "" {
		client, err := mgr.Client(ctx, args.ServerName)
		if err != nil {
			return tools.Errorf("accessing MCP server %q: %v", args.ServerName, err)
		}
		tList, err := client.ListTools(ctx)
		if err != nil {
			return tools.Errorf("listing tools from %q: %v", args.ServerName, err)
		}
		return ok(map[string]any{
			"server": args.ServerName,
			"tools":  tList,
		})
	}

	allTools, err := mgr.ListAllTools(ctx)
	if err != nil {
		return tools.Errorf("listing MCP tools: %v", err)
	}

	return ok(map[string]any{
		"servers": allTools,
	})
}

// Bounds on what one MCP tool call may return into a model's context. The
// server is a separate program and its reply is entirely its own choice.
const (
	maxMCPResultBytes  = 256 << 10
	maxMCPContentParts = 512
)

// mcpToolTarget is what the command gate is asked about for an MCP tool call.
//
// The call is gated as a command because that is what it is: an instruction to
// a separate program to do something outside this harness, with effects the
// harness cannot see. An MCP server advertising run_shell or write_file was,
// before this, a complete route around the command gate, the write gate and the
// approval prompt at once — mgr.CallTool was reached with no Gate call and no
// Approver call, and Result.Blocked could not be set on this path at all, so
// the run report had no way to show that anything had happened.
//
// The rendered form is stable and allowlistable: an operator who wants a
// particular server's tools to run without a prompt every time adds
// "mcp_call_tool weather/*" to the task or global allowed commands, exactly as
// they would for any other command.
func mcpToolTarget(server, tool string) string {
	return fmt.Sprintf("mcp_call_tool %s/%s", server, tool)
}

// checkMCPName rejects a server or tool name that could not be rendered into
// the gate's target unambiguously.
//
// Both names are attacker-influenced: the server name comes from a declaration
// file in the checked-out tree, and the tool name from the server's own
// listing. A name carrying shell metacharacters would be split by the gate's
// command-chain splitter into parts that were never one command, so the thing
// judged would not be the thing invoked. Refusing the name is the fix; sanitising
// it would leave two spellings of the same call.
func checkMCPName(kind, name string) error {
	if len(name) > 256 {
		return fmt.Errorf("%s is %d bytes, past the 256-byte limit", kind, len(name))
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.' || r == ':':
		default:
			return fmt.Errorf("%s %q contains %q; MCP names are limited to letters, digits, "+
				"and the characters _-.:", kind, name, r)
		}
	}
	return nil
}

// mcpListResourcesTarget and mcpReadResourceTarget are what the command gate is
// asked about for the two resource tools.
//
// Gated for the same reason mcp_call_tool is. The trust fix means these can
// only reach a server an operator authorized, but an authorized server is
// still a separate program the harness does not control, and a resource read
// returns bytes of that program's choosing straight into the model's context.
// "Which third-party programs may feed this model, and which of their
// resources" is a policy question, and leaving it unasked made the answer
// "all of them, always" — with nothing in the run report to show it happened.
//
// Both forms are stable and allowlistable: an operator who wants a particular
// server's resources read without a prompt adds "mcp_list_resources weather"
// and "mcp_read_resource weather *" to the task or global allowed commands.
// The URI is a separate token rather than joined to the server with "/",
// because a URI carries "/" of its own and gluing the two together would make
// "which part is the server" a question the pattern could not answer.
func mcpListResourcesTarget(server string) string {
	return fmt.Sprintf("mcp_list_resources %s", server)
}

func mcpReadResourceTarget(server, uri string) string {
	return fmt.Sprintf("mcp_read_resource %s %s", server, uri)
}

// maxMCPResourceURI bounds a URI before it is rendered into a gate target.
const maxMCPResourceURI = 2048

// checkMCPResourceURI rejects a resource URI that could not be rendered into
// the gate's target unambiguously.
//
// The URI reaches here either from the model or from a server's own resource
// listing, so it is attacker-influenced on both routes. The gate splits its
// subject into a command chain before judging it, so a URI containing ";",
// "&&", "|", a newline, "$(" or a backtick would be split into parts that were
// never one operation: the thing judged would not be the thing read. Refusing
// is the fix; sanitising would leave two spellings of the same read.
//
// The accepted set is deliberately narrower than RFC 3986 — no "?", "&", "#",
// "*", "!", "$", quotes or parentheses — which costs query-string URIs and
// buys a target that no server-supplied string can restructure. A server that
// needs one of those characters is a refusal an operator can see and answer,
// which is the direction this has to fail.
func checkMCPResourceURI(uri string) error {
	if len(uri) > maxMCPResourceURI {
		return fmt.Errorf("uri is %d bytes, past the %d-byte limit", len(uri), maxMCPResourceURI)
	}
	for _, r := range uri {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("_-.:/~%+@,=", r):
		default:
			return fmt.Errorf("uri %q contains %q; a resource URI is limited to letters, digits, "+
				"and the characters _-.:/~%%+@,= so it cannot restructure the policy target", uri, r)
		}
	}
	return nil
}

// gateMCPAccess runs one MCP operation through the command gate and returns the
// settled decision, or the refusal to hand back unchanged.
//
// Shared by the two resource tools so they cannot drift into two different
// answers for the same question. mcp_call_tool keeps its own copy of this
// sequence only because its refusal text names the tool as well as the server.
func (r *Registry) gateMCPAccess(ctx context.Context, verb, target string) (policy.Decision, *tools.Result) {
	task, refusal := r.authorisingTask(ctx, verb)
	if refusal != nil {
		return policy.Decision{}, refusal
	}
	// A gate that is not there does not mean an operation that is permitted.
	// This is an error result, not a pass — see unavailable.
	if r.deps.Gate == nil {
		res := unavailable("MCP resource policy decision",
			errors.New("this harness was built without a policy gate, so nothing was read"))
		return policy.Decision{}, &res
	}
	decision, err := r.deps.Gate.EvaluateCommand(target, task)
	if err != nil {
		res := unavailable("MCP resource policy decision", err)
		return policy.Decision{}, &res
	}
	if decision.Blocked() {
		// The subject is the operation the human is being asked to clear, not
		// the decision's own target: an approval card naming something other
		// than what will run is not consent.
		escalated, ok := r.escalate(ctx, decision, target)
		if !ok {
			res := r.refusal(decision)
			return policy.Decision{}, &res
		}
		decision = escalated
	}
	return decision, nil
}

func (r *Registry) mcpCallTool(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		ServerName string         `json:"server_name"`
		ToolName   string         `json:"tool_name"`
		Arguments  map[string]any `json:"arguments"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if strings.TrimSpace(args.ServerName) == "" {
		return tools.Errorf("server_name is required")
	}
	if strings.TrimSpace(args.ToolName) == "" {
		return tools.Errorf("tool_name is required")
	}
	if err := checkMCPName("server_name", args.ServerName); err != nil {
		return tools.Errorf("%v", err)
	}
	if err := checkMCPName("tool_name", args.ToolName); err != nil {
		return tools.Errorf("%v", err)
	}

	mgr := r.getMCPManager()
	// Asked before the gate so that a surface with no manager keeps reporting
	// the configuration fault it has, rather than a policy refusal for a call
	// that was never going to reach a server.
	if err := mgr.Unavailable(); err != nil {
		return tools.Errorf("calling MCP tool %s/%s: %v", args.ServerName, args.ToolName, err)
	}

	task, refusal := r.authorisingTask(ctx, "calling an MCP tool")
	if refusal != nil {
		return *refusal
	}

	// A gate that is not there does not mean a call that is permitted. This is
	// an error result, not a pass — see unavailable.
	if r.deps.Gate == nil {
		return unavailable("MCP tool policy decision",
			errors.New("this harness was built without a policy gate, so the call was not made"))
	}
	decision, err := r.deps.Gate.EvaluateCommand(mcpToolTarget(args.ServerName, args.ToolName), task)
	if err != nil {
		return unavailable("MCP tool policy decision", err)
	}
	if decision.Blocked() {
		// The subject is the operation the human is being asked to clear, not
		// the decision's own target: an approval card that names something
		// other than what will run is not consent.
		escalated, ok := r.escalate(ctx, decision,
			mcpToolTarget(args.ServerName, args.ToolName))
		if !ok {
			return r.refusal(decision)
		}
		decision = escalated
	}

	res, err := mgr.CallTool(ctx, args.ServerName, args.ToolName, args.Arguments)
	if err != nil {
		return annotate(tools.Result{
			Text:    fmt.Sprintf("calling MCP tool %s/%s: %v", args.ServerName, args.ToolName, err),
			IsError: true,
		}, decision)
	}

	outText, truncated := renderMCPContent(res.Content)
	if outText == "" && !res.IsError {
		outText = fmt.Sprintf("tool %s/%s completed successfully", args.ServerName, args.ToolName)
	}
	if truncated != "" {
		// Said out loud, and inside the text the model reads. A capped sample
		// presented as the whole reply is how a partial answer comes to be
		// treated as a complete one.
		outText += "\n[" + truncated + "]"
	}

	return annotate(tools.Result{
		Text:    outText,
		IsError: res.IsError,
	}, decision)
}

// renderMCPContent flattens a tool reply into text, bounded, saying so when it
// had to cut.
func renderMCPContent(blocks []mcp.ContentBlock) (text string, truncated string) {
	notes := make([]string, 0, 2)
	if len(blocks) > maxMCPContentParts {
		notes = append(notes, fmt.Sprintf("the server returned %d content blocks; only the first %d are shown",
			len(blocks), maxMCPContentParts))
		blocks = blocks[:maxMCPContentParts]
	}

	var b strings.Builder
	cut := false
	for _, block := range blocks {
		var part string
		switch {
		case block.Text != "":
			part = block.Text
		case block.Data != "":
			part = fmt.Sprintf("[%s data: %d bytes]", block.MIMEType, len(block.Data))
		default:
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		if remaining := maxMCPResultBytes - b.Len(); len(part) > remaining {
			if remaining > 0 {
				b.WriteString(part[:remaining])
			}
			cut = true
			break
		}
		b.WriteString(part)
	}
	if cut {
		notes = append(notes, fmt.Sprintf("the reply was cut at %d bytes; this is not the whole of what "+
			"the server returned", maxMCPResultBytes))
	}
	return b.String(), strings.Join(notes, "; ")
}

func (r *Registry) mcpListResources(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		ServerName string `json:"server_name"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if strings.TrimSpace(args.ServerName) == "" {
		return tools.Errorf("server_name is required")
	}

	if err := checkMCPName("server_name", args.ServerName); err != nil {
		return tools.Errorf("%v", err)
	}

	mgr := r.getMCPManager()
	// Asked before the gate so that a surface with no manager keeps reporting
	// the configuration fault it has, rather than a policy refusal for a
	// listing that was never going to reach a server.
	if err := mgr.Unavailable(); err != nil {
		return tools.Errorf("listing resources on %s: %v", args.ServerName, err)
	}

	decision, refused := r.gateMCPAccess(ctx, "listing MCP resources",
		mcpListResourcesTarget(args.ServerName))
	if refused != nil {
		return *refused
	}

	// Only now: Client spawns the server process, which is itself the effect
	// the gate is there to decide about.
	client, err := mgr.Client(ctx, args.ServerName)
	if err != nil {
		return annotate(tools.Errorf("accessing server %s: %v", args.ServerName, err), decision)
	}

	res, err := client.ListResources(ctx)
	if err != nil {
		return annotate(tools.Errorf("listing resources on %s: %v", args.ServerName, err), decision)
	}

	return annotate(ok(map[string]any{
		"server":    args.ServerName,
		"resources": res,
	}), decision)
}

func (r *Registry) mcpReadResource(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		ServerName string `json:"server_name"`
		URI        string `json:"uri"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if strings.TrimSpace(args.ServerName) == "" {
		return tools.Errorf("server_name is required")
	}
	if strings.TrimSpace(args.URI) == "" {
		return tools.Errorf("uri is required")
	}

	if err := checkMCPName("server_name", args.ServerName); err != nil {
		return tools.Errorf("%v", err)
	}
	if err := checkMCPResourceURI(args.URI); err != nil {
		return tools.Errorf("%v", err)
	}

	mgr := r.getMCPManager()
	// Asked before the gate, for the reason mcp_call_tool asks first: a
	// missing manager is a configuration fault, not a policy refusal.
	if err := mgr.Unavailable(); err != nil {
		return tools.Errorf("reading resource %s on %s: %v", args.URI, args.ServerName, err)
	}

	decision, refused := r.gateMCPAccess(ctx, "reading an MCP resource",
		mcpReadResourceTarget(args.ServerName, args.URI))
	if refused != nil {
		return *refused
	}

	contents, err := mgr.ReadResource(ctx, args.ServerName, args.URI)
	if err != nil {
		return annotate(tools.Errorf("reading resource %s on %s: %v", args.URI, args.ServerName, err),
			decision)
	}

	return annotate(ok(map[string]any{
		"server":   args.ServerName,
		"uri":      args.URI,
		"contents": contents,
	}), decision)
}
