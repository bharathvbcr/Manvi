package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/agent"
	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/credentials"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
	"github.com/bharathvbcr/Manvi/manvi/mcp"
	"github.com/bharathvbcr/Manvi/manvi/session"
)

const passthroughToken = "ghp_AUDIT2passthroughTOKEN0123456789"

// An MCP server that echoes a token it was given on stderr gets that text
// attached to the error of the failed call. The tool scrubber knew provider
// keys only, so the token reached the model and the session file.
func TestMCPPassthroughSecretIsScrubbedFromToolResultAndSessionLog(t *testing.T) {
	for name, cfg := range map[string]mcp.ServerConfig{
		"passthrough": {EnvPassthrough: []string{"MANVI_TEST_MCP_TOKEN"}},
		"declared":    {Env: map[string]string{"MANVI_TEST_MCP_TOKEN": passthroughToken}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("MANVI_TEST_MCP_TOKEN", passthroughToken)
			reg, err := flags.NewHarnessRegistry("")
			if err != nil {
				t.Fatal(err)
			}
			if err := reg.Set(flags.Human, flags.HarnessPosture, flags.PostureYolo); err != nil {
				t.Fatal(err)
			}
			_, pipeline, err := nativeTools(reg)
			if err != nil {
				t.Fatal(err)
			}
			core := harnessCoreFor(pipeline)
			if core == nil {
				t.Fatal("no harness core for the pipeline")
			}
			cfg.Name = "gh"
			cfg.Command = "sh"
			cfg.Args = []string{"-c", `echo "auth rejected for token $MANVI_TEST_MCP_TOKEN" >&2; exit 1`}
			if err := core.mcp.RegisterServer(cfg); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(core.mcp.CloseAll)

			call := llm.ToolCallBlock{
				ID: "c1", Name: "mcp_call_tool",
				Arguments: json.RawMessage(`{"server_name":"gh","tool_name":"x","arguments":{}}`),
			}
			b := bus.New()
			log := newSessionLog()
			loop, err := agent.NewLoop(agent.Config{
				Provider: replay.New(replay.Fixture{
					Provider: "replay",
					Capabilities: []llm.Capability{{
						Provider: "replay", Model: "m", ContextWindow: 200000, MaxOutputTokens: 8192,
						SupportsTools: true, SupportsStreaming: true,
					}},
					Turns: []replay.Turn{
						{
							Chunks:     []llm.Chunk{{Kind: llm.ChunkToolCallStart, ToolCallID: "c1", ToolName: call.Name}},
							Message:    llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{call}},
							StopReason: llm.StopToolUse,
						},
						{
							Chunks:     []llm.Chunk{{Kind: llm.ChunkText, Text: "done"}},
							Message:    llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: "done"}}},
							StopReason: llm.StopEndTurn,
						},
					},
				}),
				Model: "m", SystemPrompt: "s", MaxSteps: 6, MaxTokens: 1024,
			}, b, log, pipeline)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := loop.Run(context.Background(), llm.Message{
				Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}},
			}); err != nil {
				t.Fatalf("run: %v", err)
			}

			var sawResult bool
			for _, event := range log.Events() {
				if strings.Contains(string(event.Data), passthroughToken) {
					t.Fatalf("the token reached the session log (%s): %s", event.Type, event.Data)
				}
				if event.Type == session.ToolResult {
					sawResult = true
					if !strings.Contains(string(event.Data), credentials.Redacted) {
						t.Fatalf("the failed MCP call's result was not redacted: %s", event.Data)
					}
				}
			}
			if !sawResult {
				t.Fatal("the call never produced a tool result; this test proves nothing")
			}
		})
	}
}

// Every scrubber in this package is built and armed by scrubber.go, so a new
// secret source is added once. A direct credentials.NewScrubber() elsewhere is
// a scrubber that does not know about it.
func TestScrubbersAreArmedAtOneSite(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// probe.go watches the one key under test, deliberately.
		if name == "scrubber.go" || name == "probe.go" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "credentials.NewScrubber(") {
			t.Errorf("%s builds its own scrubber; use newArmedScrubber so MCP and GitHub "+
				"secrets are watched too", name)
		}
	}
}

// Short values are not scrubbed, matching Watch's own floor, and a value is
// not sealed twice.
func TestWatchValueSkipsShortAndDuplicateValues(t *testing.T) {
	s := credentials.NewScrubber()
	s.WatchValue("short", "x")
	s.WatchValue("", "x")
	if s.Count() != 0 {
		t.Fatalf("Count = %d for values under the floor", s.Count())
	}
	s.WatchValue("0123456789abcdef", "x")
	s.WatchValue("0123456789abcdef", "y")
	if s.Count() != 1 {
		t.Fatalf("Count = %d, want 1 for one repeated value", s.Count())
	}
	if got := s.Clean("a 0123456789abcdef b"); got != "a "+credentials.Redacted+" b" {
		t.Fatalf("Clean = %q", got)
	}
}
