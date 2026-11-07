package agent

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/llm/budget"
	"github.com/bharathvbcr/Manvi/manvi/llm/gemini"
	"github.com/bharathvbcr/Manvi/manvi/llm/local"
	"github.com/bharathvbcr/Manvi/manvi/llm/xai"
	"github.com/bharathvbcr/Manvi/manvi/session"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// Adversarial tests for lane w11-compaction (card ft-f7993787), agent half.
// Reuses drivePreserved, windowed and bigOutput from this package's tests.

// advBreak skips a test that demonstrates a confirmed break. Setting
// ADV_W11_BREAKS=1 runs it so the failure can be reproduced.
func advBreak(t *testing.T, msg string) {
	t.Helper()
	if os.Getenv("ADV_W11_BREAKS") != "" {
		return
	}
	t.Skip(msg)
}

// advNamed is a provider that only has a name, for the predicate.
type advNamed struct {
	llm.Provider
	name string
}

func (n advNamed) Name() string { return n.name }

// ServerSideEditing is true for Anthropic only, bare or metered, and false for
// every other adapter in the tree, bare or metered.
func TestAdvW11ServerSideEditingIsAnthropicOnly(t *testing.T) {
	anth := anthropic.New("http://unused", adaptertest.Secret("k"))
	for name, p := range map[string]llm.Provider{
		"anthropic":  anth,
		"anthropic+": &budget.Provider{Inner: anth},
	} {
		if !ServerSideEditing(p) {
			t.Errorf("%s: ServerSideEditing = false", name)
		}
	}
	others := map[string]llm.Provider{
		"gemini":            gemini.New("http://unused", adaptertest.Secret("k")),
		"xai":               advNamed{name: xai.Name},
		"local":             advNamed{name: local.Name},
		"openai-compatible": advNamed{name: "openai-compatible"},
		"openai":            advNamed{name: "openai"},
		"Anthropic":         advNamed{name: "Anthropic"},
		"anthropic-vertex":  advNamed{name: "anthropic-vertex"},
		"empty":             advNamed{name: ""},
	}
	for name, p := range others {
		if ServerSideEditing(p) {
			t.Errorf("%s: ServerSideEditing = true", name)
		}
		if ServerSideEditing(&budget.Provider{Inner: p}) {
			t.Errorf("%s through budget.Provider: ServerSideEditing = true", name)
		}
	}
	if llm.BindsThinkingToHistory(xai.Name) || llm.BindsThinkingToHistory(local.Name) || llm.BindsThinkingToHistory(gemini.Name) {
		t.Error("BindsThinkingToHistory names a non-Anthropic provider")
	}
	if got := anthropic.Name; got != "anthropic" {
		t.Errorf("anthropic.Name = %q; llm.providerAnthropic repeats it", got)
	}
}

// The other providers still compact client-side on the real loop, over the same
// fixture that Anthropic is spared.
func TestAdvW11OtherProvidersStillCompactClientSide(t *testing.T) {
	for _, name := range []string{gemini.Name, xai.Name, local.Name, "openai-compatible"} {
		run := drivePreserved(t, 8192, name, longThinkingScript(), bigOutput(260, "r"), nil, "go")
		if run.err != nil {
			t.Fatalf("%s: %v", name, run.err)
		}
		if compactionEvents(run.log) == 0 {
			t.Errorf("%s compacted nothing; client-side compaction changed for a non-Anthropic provider", name)
		}
	}
}

// A turn whose only content is signed reasoning (cut off at max_tokens inside
// thinking) sits behind a legacy client-side edit. The strip leaves it with no
// content; the request must not carry an assistant message with an empty
// content array. Result: the Anthropic encoder omits the emptied message, so
// the wire is valid (session.StripStaleReasoning itself does return an empty
// Content, which is harmless only because of that omission).
func TestAdvW11ThinkingOnlyTurnBehindEditReachesTheWireNonEmpty(t *testing.T) {
	seed := func(log *session.Log) {
		t.Helper()
		appendMsg := func(typ session.Type, msg llm.Message) {
			if _, err := log.Append(typ, session.MessageData{Message: msg}); err != nil {
				t.Fatal(err)
			}
		}
		appendMsg(session.UserMessage, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "first"}}})
		for n := 0; n < 2; n++ {
			id := llm.CallID("toolu_" + string(rune('0'+n)))
			appendMsg(session.AssistantMessage, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				llm.ReasoningBlock{Text: "plan", Signature: "sig-" + string(rune('0'+n))},
				llm.ToolCallBlock{ID: id, Name: "echo", Arguments: json.RawMessage(`{"v":1}`)},
			}})
			if _, err := log.Append(session.ToolResult, session.ToolResultData{ToolCallID: id, Text: "out"}); err != nil {
				t.Fatal(err)
			}
		}
		appendMsg(session.AssistantMessage, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ReasoningBlock{Text: "cut off mid-thought", Signature: "sig-cut"},
		}})
		seqs, err := log.ResultSeqs([]llm.ResultKey{{ID: "toolu_0", Occurrence: 1}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := log.Append(session.ToolResultCompacted, session.CompactionData{
			ToolCallID: "toolu_0", ResultSeq: seqs[0], Text: "short", FromBytes: 100, ToBytes: 5,
		}); err != nil {
			t.Fatal(err)
		}
	}
	run := drivePreserved(t, 1_000_000, "", []string{thinkingAnswer("sig-new")}, "ok", seed, "second")
	if len(run.raw) == 0 {
		t.Fatalf("no request reached the server; run.err=%v", run.err)
	}
	for _, raw := range run.requests[0].Messages {
		var m struct {
			Role    string            `json:"role"`
			Content []json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		if m.Role == "assistant" && len(m.Content) == 0 {
			t.Fatalf("request carries an assistant message with empty content: %s", raw)
		}
	}
}

// A ChatOnly run offers no tools, so the adapter attaches no context_management
// (it only does so when tools are offered), and Loop.compact has stood down for
// Anthropic. A resumed conversation with a long tool history is then shrunk by
// no one. Either the loop must still compact, or the request must still carry
// the edit.
func TestAdvW11ChatOnlyAnthropicHistoryIsShrunkBySomeone(t *testing.T) {
	advBreak(t, "BREAK: ChatOnly + Anthropic: Loop.compact returns early (ServerSideEditing) and anthropic buildRequest sends context_management only when len(req.Tools) > 0, so a ChatOnly session over a tool-heavy log never shrinks (before the lane, client compaction handled it). Severity medium-low; inferred, no live run.")
	b := bus.New()
	log := session.NewLog()
	reg := tools.NewRegistry(b)
	provider := windowed{Provider: anthropic.New("http://unused", adaptertest.Secret("k")), window: 8192}
	loop, err := NewLoop(Config{
		Provider: provider, Model: "claude-opus-5-5", SystemPrompt: "p",
		MaxSteps: 4, MaxTokens: 1024, ChatOnly: true,
	}, b, log, reg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.UserMessage, session.MessageData{Message: llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}}}); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 8; n++ {
		id := llm.CallID("toolu_" + string(rune('a'+n)))
		if _, err := log.Append(session.AssistantMessage, session.MessageData{Message: llm.Message{
			Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				llm.ToolCallBlock{ID: id, Name: "echo", Arguments: json.RawMessage(`{}`)}}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := log.Append(session.ToolResult, session.ToolResultData{ToolCallID: id, Text: bigOutput(260, "r")}); err != nil {
			t.Fatal(err)
		}
	}
	msgs, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.compact(msgs); err != nil {
		t.Fatal(err)
	}
	if compactionEvents(log) == 0 && len(loop.schemas()) == 0 {
		t.Fatal("history is far over the window; no client-side compaction and no tools to hang a server-side edit on")
	}
}
