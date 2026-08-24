package devcouncil

import (
	"context"
	"fmt"
	"strings"

	"manvi/mcp"
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

// mcpDispatchUnjudged is what every MCP tool call admits about itself.
//
// The handler validates two strings and hands the call to the server: no gate,
// no lease, no rule, and — until this — nothing in the result to say so. The
// only control was mcp.enabled, which is all-or-nothing and is not a safety
// flag, so a run that reached a filesystem write or a shell through an MCP
// server looked, in the report, exactly like a run that had not: no Degraded,
// no Rule, no Demoted, and nothing in Weakened().
//
// A lease is deliberately *not* required, and the reason is that requiring one
// would be the misleading choice rather than the strict one. This harness
// cannot tell a read from a write on the far side of a JSON-RPC boundary, so a
// lease requirement would refuse read-only MCP research that the native
// read-only tools do without one, and — worse — would dress the calls it did
// admit as authorised. A lease authorises a task's planned files; no MCP call
// is ever in a task's planned files, so holding one says nothing true about it.
// What can be said truthfully is that nothing judged this, and that is what
// travels on the result, on the failure path as much as the success path: a
// dispatch that errored still reached the server, and the harness has no way to
// know how far it got.
const mcpDispatchUnjudged = "mcp.dispatch.unjudged"

// unjudgedMCP marks a result as having crossed the MCP boundary.
func unjudgedMCP(res tools.Result, server, tool string) tools.Result {
	res.Degraded = append(res.Degraded, fmt.Sprintf(
		"%s: %s/%s ran outside the write and command gates; no rule was consulted and no lease was required",
		mcpDispatchUnjudged, server, tool))
	return res
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

	mgr := r.getMCPManager()
	res, err := mgr.CallTool(ctx, args.ServerName, args.ToolName, args.Arguments)
	if err != nil {
		return unjudgedMCP(
			tools.Errorf("calling MCP tool %s/%s: %v", args.ServerName, args.ToolName, err),
			args.ServerName, args.ToolName)
	}

	var texts []string
	for _, block := range res.Content {
		if block.Text != "" {
			texts = append(texts, block.Text)
		} else if block.Data != "" {
			texts = append(texts, fmt.Sprintf("[%s data: %d bytes]", block.MIMEType, len(block.Data)))
		}
	}

	outText := strings.Join(texts, "\n")
	if outText == "" && !res.IsError {
		outText = fmt.Sprintf("tool %s/%s completed successfully", args.ServerName, args.ToolName)
	}

	return unjudgedMCP(tools.Result{
		Text:    outText,
		IsError: res.IsError,
	}, args.ServerName, args.ToolName)
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

	mgr := r.getMCPManager()
	client, err := mgr.Client(ctx, args.ServerName)
	if err != nil {
		return tools.Errorf("accessing server %s: %v", args.ServerName, err)
	}

	res, err := client.ListResources(ctx)
	if err != nil {
		return tools.Errorf("listing resources on %s: %v", args.ServerName, err)
	}

	return ok(map[string]any{
		"server":    args.ServerName,
		"resources": res,
	})
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

	mgr := r.getMCPManager()
	contents, err := mgr.ReadResource(ctx, args.ServerName, args.URI)
	if err != nil {
		return tools.Errorf("reading resource %s on %s: %v", args.URI, args.ServerName, err)
	}

	return ok(map[string]any{
		"server":   args.ServerName,
		"uri":      args.URI,
		"contents": contents,
	})
}
