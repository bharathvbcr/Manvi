package agent

import (
	"encoding/json"
	"fmt"
	"strings"
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

// A ChatOnly run offers no tools, so the server has no tool set for its
// clearing to act on and the adapter sends no context_management (that
// combination is not documented). Anthropic's own stand-down therefore does not
// apply to such a request: the loop compacts client-side, at a turn boundary,
// and the strip removes the signed reasoning the edit invalidated.
func chatOnlyOverToolHistory(t *testing.T, endWithUserText bool) (*Loop, *session.Log, []llm.Message) {
	t.Helper()
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
	if endWithUserText {
		if _, err := log.Append(session.UserMessage, session.MessageData{Message: llm.Message{
			Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "now just chat"}}}}); err != nil {
			t.Fatal(err)
		}
	}
	msgs, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	return loop, log, msgs
}

func TestAdvW11ChatOnlyAnthropicHistoryIsShrunkAtATurnBoundary(t *testing.T) {
	loop, log, msgs := chatOnlyOverToolHistory(t, true)
	if len(loop.schemas()) != 0 {
		t.Fatalf("a ChatOnly loop offers %d tools", len(loop.schemas()))
	}
	if _, err := loop.compact(msgs); err != nil {
		t.Fatal(err)
	}
	if compactionEvents(log) == 0 {
		t.Fatal("history is far over the window with no tools to hang a server-side edit on, and the loop compacted nothing")
	}
}

// The same history, ending in a tool result, is mid tool round: the edit waits
// for the turn boundary, because the assistant turn whose tool_use was just
// answered must go back with its reasoning intact.
func TestAdvW11ChatOnlyAnthropicEditWaitsForTheTurnBoundary(t *testing.T) {
	loop, log, msgs := chatOnlyOverToolHistory(t, false)
	if AtTurnBoundary(msgs) {
		t.Fatal("a history ending in a tool result was called a turn boundary")
	}
	if _, err := loop.compact(msgs); err != nil {
		t.Fatal(err)
	}
	if n := compactionEvents(log); n != 0 {
		t.Fatalf("%d tool results were edited between a tool_use and its tool_result", n)
	}
}

// And the edit is followed by the strip on the wire: a ChatOnly run over
// signed reasoning behind its own compaction sends none from the edit onward.
func TestAdvW11ChatOnlyAnthropicCompactionIsFollowedByTheStripOnTheWire(t *testing.T) {
	seed := func(log *session.Log) {
		t.Helper()
		appendMsg := func(typ session.Type, msg llm.Message) {
			if _, err := log.Append(typ, session.MessageData{Message: msg}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := log.Append(session.TurnStart, nil); err != nil {
			t.Fatal(err)
		}
		appendMsg(session.UserMessage, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "first"}}})
		for n := 0; n < 8; n++ {
			id := llm.CallID(fmt.Sprintf("toolu_%d", n))
			appendMsg(session.AssistantMessage, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				llm.ReasoningBlock{Text: "plan", Signature: fmt.Sprintf("sig-%d", n)},
				llm.ToolCallBlock{ID: id, Name: "echo", Arguments: json.RawMessage(`{"v":1}`)},
			}})
			if _, err := log.Append(session.ToolResult, session.ToolResultData{ToolCallID: id, Text: bigOutput(260, "r")}); err != nil {
				t.Fatal(err)
			}
		}
		appendMsg(session.AssistantMessage, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ReasoningBlock{Text: "done", Signature: "sig-last"},
			llm.TextBlock{Text: "that is everything"},
		}})
	}
	run := drivePreservedWith(t, true, 8192, "", []string{thinkingAnswer("sig-new")}, "ok", seed, "now just chat")
	if run.err != nil {
		t.Fatalf("turn failed: %v", run.err)
	}
	if compactionEvents(run.log) == 0 {
		t.Fatal("nothing was compacted; the fixture no longer overflows its window")
	}
	body := string(run.raw[0])
	if strings.Contains(body, "context_management") {
		t.Error("a request with no tools carried context_management")
	}
	// Every signed block that survives sits before the first edited result, and
	// nothing signed follows a removed block: the last turns' reasoning is gone
	// even though it was recorded after the edit was planned.
	for _, b := range signedBlocks(t, run.requests[0]) {
		if b.signature == "sig-last" || b.signature == "sig-7" {
			t.Errorf("%s follows an edited result and was replayed", b.signature)
		}
	}
}
