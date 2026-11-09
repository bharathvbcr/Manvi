package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/session"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// Preserved thinking, on the wire.
//
// A thinking block's signature binds the system prompt, the tools and every
// message before the block (shared/model-migration.md, "Breaking change 3").
// These tests run the real loop and the real Anthropic encoder against a
// fixture server and read what actually went out, because the property is about
// bytes on the wire: a projection that looks append-only in memory can still
// send a prefix that differs from the one a block was signed against.

// wireThinkingBlock is a signed thinking block at index, as the API streams it.
func wireThinkingBlock(index int, text, signature string) string {
	return fmt.Sprintf(`event: content_block_start
data: {"type":"content_block_start","index":%[1]d,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":%[1]d,"delta":{"type":"thinking_delta","thinking":%[2]q}}

event: content_block_delta
data: {"type":"content_block_delta","index":%[1]d,"delta":{"type":"signature_delta","signature":%[3]q}}

event: content_block_stop
data: {"type":"content_block_stop","index":%[1]d}

`, index, text, signature)
}

// wireTextBlock is a text block at index.
func wireTextBlock(index int, text string) string {
	return fmt.Sprintf(`event: content_block_start
data: {"type":"content_block_start","index":%[1]d,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":%[1]d,"delta":{"type":"text_delta","text":%[2]q}}

event: content_block_stop
data: {"type":"content_block_stop","index":%[1]d}

`, index, text)
}

// thinkingToolStep is one scripted response: reasoning signed as sig, then a
// call to the echo tool.
func thinkingToolStep(n int) string {
	return wireStart +
		wireThinkingBlock(0, fmt.Sprintf("plan %d", n), fmt.Sprintf("sig-%d", n)) +
		wireToolCall(1, fmt.Sprintf("toolu_%d", n), "echo", fmt.Sprintf(`"{\"v\":%d}"`, n), true) +
		wireFinish("tool_use")
}

// thinkingAnswer is the closing response of a turn: signed reasoning, then text.
func thinkingAnswer(signature string) string {
	return wireStart + wireThinkingBlock(0, "wrap up", signature) + wireTextBlock(1, "done") + wireFinish("end_turn")
}

// windowed gives a provider a small context window, so a modest history is over
// its budget, and an optional name. The embedded provider keeps the wire real.
type windowed struct {
	llm.Provider
	window int
	name   string
}

func (w windowed) Name() string {
	if w.name != "" {
		return w.name
	}
	return w.Provider.Name()
}

func (w windowed) Capability(model string) (llm.Capability, bool) {
	c, ok := w.Provider.Capability(model)
	if ok {
		c.ContextWindow = w.window
	}
	return c, ok
}

func (w windowed) ReplayableOn(from, to string) bool {
	r, ok := w.Provider.(llm.ReasoningReplayer)
	return ok && r.ReplayableOn(from, to)
}

// preservedRun is what a driven turn left behind.
type preservedRun struct {
	requests []wireRequest
	raw      [][]byte
	headers  []http.Header
	log      *session.Log
	err      error
}

// wireRequest is the part of a request the history-editing check compares.
type wireRequest struct {
	System   json.RawMessage   `json:"system"`
	Tools    json.RawMessage   `json:"tools"`
	Messages []json.RawMessage `json:"messages"`
}

// drivePreserved runs one turn of the real loop against a fixture Anthropic
// server serving script[i] for request i (the last repeats). seed, when set,
// prepares the log before the turn, which is how a session written by an older
// build is stood up.
func drivePreserved(t *testing.T, window int, name string, script []string, result string,
	seed func(*session.Log), prompt string) preservedRun {
	t.Helper()
	return drivePreservedWith(t, false, window, name, script, result, seed, prompt)
}

// drivePreservedWith is drivePreserved with the loop's ChatOnly switch, which
// offers the model no tools.
func drivePreservedWith(t *testing.T, chatOnly bool, window int, name string, script []string, result string,
	seed func(*session.Log), prompt string) preservedRun {
	t.Helper()
	var mu sync.Mutex
	var run preservedRun
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		mu.Lock()
		i := len(run.raw)
		run.raw = append(run.raw, body)
		run.headers = append(run.headers, r.Header.Clone())
		mu.Unlock()
		if i >= len(script) {
			i = len(script) - 1
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, script[i])
	}))
	t.Cleanup(srv.Close)

	b := bus.New()
	log := session.NewLog()
	if seed != nil {
		seed(log)
	}
	reg := tools.NewRegistry(b)
	if err := reg.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "echo", Description: "echo", InputSchema: []byte(`{"type":"object","properties":{"v":{"type":"integer"}}}`)},
		Handler: func(context.Context, tools.Call) tools.Result {
			return tools.Result{Text: result}
		},
	}); err != nil {
		t.Fatal(err)
	}
	provider := windowed{Provider: anthropic.New(srv.URL, adaptertest.Secret("sk-FIXTURE-not-a-real-key")), window: window, name: name}
	loop, err := NewLoop(Config{
		Provider: provider, Model: "claude-opus-5-5", SystemPrompt: "p",
		MaxSteps: 16, MaxTokens: 1024, AssertInvariant: true, ChatOnly: chatOnly,
	}, b, log, reg)
	if err != nil {
		t.Fatal(err)
	}
	_, run.err = loop.Run(context.Background(), llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: prompt}}})
	mu.Lock()
	defer mu.Unlock()
	run.log = log
	for _, raw := range run.raw {
		var req wireRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatalf("request is not JSON: %v\n%s", err, raw)
		}
		run.requests = append(run.requests, req)
	}
	return run
}

// signedBlock is one thinking or redacted_thinking block found in a request.
type signedBlock struct {
	message   int
	signature string
}

func signedBlocks(t *testing.T, req wireRequest) []signedBlock {
	t.Helper()
	var out []signedBlock
	for i, raw := range req.Messages {
		var msg struct {
			Content []struct {
				Type      string `json:"type"`
				Signature string `json:"signature"`
				Data      string `json:"data"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("message %d is not a message: %v", i, err)
		}
		for _, c := range msg.Content {
			switch c.Type {
			case "thinking":
				out = append(out, signedBlock{message: i, signature: c.Signature})
			case "redacted_thinking":
				out = append(out, signedBlock{message: i, signature: c.Data})
			}
		}
	}
	return out
}

// assertPrefixesPreserved checks, for every thinking block any request replays,
// that the request prefix up to the block is byte-identical to the request in
// which the block was produced, system prompt and tools included. It returns
// how many replays it checked, so a caller can refuse a vacuous pass.
func assertPrefixesPreserved(t *testing.T, requests []wireRequest) int {
	t.Helper()
	first := map[string]int{}
	checked, reported := 0, 0
	for n, req := range requests {
		for _, blk := range signedBlocks(t, req) {
			f, seen := first[blk.signature]
			if !seen {
				first[blk.signature] = n
				continue
			}
			// Request f is the first to carry the block, so the request that
			// produced it is f-1, and what that request sent is exactly the
			// messages before the block.
			if f == 0 {
				t.Fatalf("block %q appears in the first request, which cannot have produced it", blk.signature)
			}
			producer := requests[f-1]
			checked++
			if !bytes.Equal(req.System, producer.System) {
				t.Errorf("request %d replays %s with a different system prompt than request %d produced it under", n, blk.signature, f-1)
			}
			if !bytes.Equal(req.Tools, producer.Tools) {
				t.Errorf("request %d replays %s with a different tool set than request %d produced it under", n, blk.signature, f-1)
			}
			if len(producer.Messages) != blk.message {
				t.Errorf("request %d carries %s at message %d, but request %d produced it after %d messages",
					n, blk.signature, blk.message, f-1, len(producer.Messages))
				continue
			}
			for i := 0; i < blk.message; i++ {
				if !bytes.Equal(req.Messages[i], producer.Messages[i]) {
					// Three examples say it; the rest are the same defect repeated.
					if reported++; reported <= 3 {
						t.Errorf("request %d replays %s, but message %d differs from the one request %d signed it against:\nproduced: %.160s\nreplayed: %.160s",
							n, blk.signature, i, f-1, producer.Messages[i], req.Messages[i])
					}
					break
				}
			}
		}
	}
	return checked
}

func compactionEvents(log *session.Log) int {
	n := 0
	for _, e := range log.Events() {
		if e.Type == session.ToolResultCompacted {
			n++
		}
	}
	return n
}

// longThinkingScript is a turn of eight tool rounds, each opening with signed
// reasoning, and a closing answer.
func longThinkingScript() []string {
	var script []string
	for n := 0; n < 8; n++ {
		script = append(script, thinkingToolStep(n))
	}
	return append(script, thinkingAnswer("sig-final"))
}

// A long conversation that, on a build that compacted client-side, edited
// earlier tool results after thinking blocks existed. On a provider that binds
// thinking to the history that edit is a 400 for an enforced account, or the
// silent loss of every later block. The window is small enough that the
// fixture's tool output overflows it by the fourth round, which is the
// condition under which the old planner rewrote history.
func TestLongAnthropicConversationReplaysEveryThinkingBlockOnItsOwnPrefix(t *testing.T) {
	run := drivePreserved(t, 8192, "", longThinkingScript(), bigOutput(260, "r"), nil, "go")
	if run.err != nil {
		t.Fatalf("turn failed: %v", run.err)
	}
	if len(run.requests) != 9 {
		t.Fatalf("requests = %d, want 9 (eight tool rounds and the answer)", len(run.requests))
	}
	if n := assertPrefixesPreserved(t, run.requests); n < 20 {
		t.Fatalf("only %d replays were checked; the fixture no longer replays enough thinking to mean anything", n)
	}
}

// The edit itself must not happen on such a provider, mid tool round or at a
// boundary: the server clears old tool results instead (context-management-2025-06-27),
// and the history-editing check does not count that. The same fixture on a
// provider that does not bind thinking still compacts, which both shows the
// window was small enough to need it and keeps the other providers unchanged.
func TestAnthropicHistoryIsNotEditedClientSide(t *testing.T) {
	run := drivePreserved(t, 8192, "", longThinkingScript(), bigOutput(260, "r"), nil, "go")
	if run.err != nil {
		t.Fatalf("turn failed: %v", run.err)
	}
	if n := compactionEvents(run.log); n != 0 {
		t.Errorf("%d tool results were compacted client-side on an Anthropic conversation, want 0", n)
	}

	control := drivePreserved(t, 8192, "openai-compatible", longThinkingScript(), bigOutput(260, "r"), nil, "go")
	if control.err != nil {
		t.Fatalf("control turn failed: %v", control.err)
	}
	if compactionEvents(control.log) == 0 {
		t.Error("the same fixture compacted nothing on a provider that does not bind thinking; " +
			"either the window is too large to exercise compaction or other providers lost it")
	}
}

// legacyConversation stands up a session as a build that compacted client-side
// left it: three tool rounds, each opening with signed reasoning, then the
// first result shortened to note (no shortening when note is empty), then one
// more round whose reasoning was produced after the shortening, then an answer.
//
// The conversation opens with the frame its requests carried (the loop's own
// prompt and tool), so what these tests pin is how an edit strips reasoning. A
// conversation with no recorded frame at all is a boundary of its own
// (TestAFrameOpenedOnAConversationThatPredatesFramesStripsEarlierReasoning).
func legacyConversation(t *testing.T, note string) func(*session.Log) {
	t.Helper()
	return legacyConversationFrom(t, note, true)
}

// legacyConversationFrom is legacyConversation with the opening frame optional:
// unframed is the log a build from before frames were recorded wrote.
func legacyConversationFrom(t *testing.T, note string, framed bool) func(*session.Log) {
	t.Helper()
	return func(log *session.Log) {
		t.Helper()
		if framed {
			if _, err := log.Append(session.WireFrame, session.FrameData{
				System: "p", Why: session.WhyOpened,
				Tools: []llm.ToolSchema{{Name: "echo", Description: "echo", InputSchema: []byte(`{"type":"object","properties":{"v":{"type":"integer"}}}`)}},
			}); err != nil {
				t.Fatal(err)
			}
		}
		appendMsg := func(typ session.Type, msg llm.Message) {
			t.Helper()
			if _, err := log.Append(typ, session.MessageData{Message: msg}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := log.Append(session.TurnStart, nil); err != nil {
			t.Fatal(err)
		}
		appendMsg(session.UserMessage, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "first task"}}})
		round := func(n int) {
			t.Helper()
			id := llm.CallID(fmt.Sprintf("toolu_%d", n))
			appendMsg(session.AssistantMessage, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				llm.ReasoningBlock{Text: fmt.Sprintf("plan %d", n), Signature: fmt.Sprintf("sig-%d", n)},
				llm.TextBlock{Text: fmt.Sprintf("step %d", n)},
				llm.ToolCallBlock{ID: id, Name: "echo", Arguments: json.RawMessage(`{"v":1}`)},
			}})
			if _, err := log.Append(session.ToolResult, session.ToolResultData{ToolCallID: id, Text: bigOutput(40, fmt.Sprintf("r%d", n))}); err != nil {
				t.Fatal(err)
			}
		}
		round(0)
		round(1)
		round(2)
		if note != "" {
			seqs, err := log.ResultSeqs([]llm.ResultKey{{ID: "toolu_0", Occurrence: 1}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := log.Append(session.ToolResultCompacted, session.CompactionData{
				ToolCallID: "toolu_0", ResultSeq: seqs[0], Text: note, FromBytes: 2500, ToBytes: len(note),
			}); err != nil {
				t.Fatal(err)
			}
		}
		round(3)
		appendMsg(session.AssistantMessage, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ReasoningBlock{Text: "wrap up", Signature: "sig-4"},
			llm.TextBlock{Text: "first task done"},
		}})
	}
}

// A history that already carries a client-side edit, which is what a session
// resumed from an older build is, replayed to a provider that binds thinking.
// Reasoning before the edited result stays valid. Every signed block from the
// edited position on goes, however late it was recorded, and text and tool calls
// stay.
func TestEditedAnthropicHistoryStripsReasoningFromTheEditOnward(t *testing.T) {
	note := "[40 line(s) omitted to fit the context window]"
	run := drivePreserved(t, 1_000_000, "", []string{thinkingAnswer("sig-5")}, "ok", legacyConversation(t, note), "second task")
	if run.err != nil {
		t.Fatalf("turn failed: %v", run.err)
	}
	if len(run.requests) == 0 {
		t.Fatal("no request reached the server")
	}
	req := run.requests[0]
	got := map[string]int{}
	for _, blk := range signedBlocks(t, req) {
		got[blk.signature] = blk.message
	}
	// Assistant messages sit at 1 (round 0), 3, 5 and 7 (round 3), and 9 (the answer).
	if _, ok := got["sig-0"]; !ok {
		t.Errorf("reasoning produced before the edited result was stripped; it was signed against an intact prefix: %v", got)
	}
	for _, stale := range []string{"sig-1", "sig-2"} {
		if _, ok := got[stale]; ok {
			t.Errorf("%s was produced against a prefix that was edited afterwards and was replayed", stale)
		}
	}
	// Reasoning recorded after the edit goes too: each block records the one
	// before it, and only a leading run may be removed, so keeping sig-3 and
	// sig-4 while removing sig-1 and sig-2 would be a removal from the middle.
	for _, later := range []string{"sig-3", "sig-4"} {
		if _, ok := got[later]; ok {
			t.Errorf("%s follows a removed block and was replayed: %v", later, got)
		}
	}
	body := string(run.raw[0])
	for _, kept := range []string{`"text":"step 1"`, `"text":"step 2"`, `"id":"toolu_1"`, `"id":"toolu_2"`, note} {
		if !strings.Contains(body, kept) {
			t.Errorf("stripping removed more than reasoning: %q is gone from the request", kept)
		}
	}
	if strings.Contains(body, "plan 1") || strings.Contains(body, "plan 2") {
		t.Error("the stripped turns still carry their reasoning text")
	}
}

// The image notice from compaction replaces a result whose images were removed,
// so it is the same kind of edit and must take the same path.
func TestAnImageNoticeEditStripsLaterReasoningToo(t *testing.T) {
	note := withImageNotice("caption", 2)
	run := drivePreserved(t, 1_000_000, "", []string{thinkingAnswer("sig-5")}, "ok", legacyConversation(t, note), "second task")
	if run.err != nil {
		t.Fatalf("turn failed: %v", run.err)
	}
	body := string(run.raw[0])
	if !strings.Contains(body, fmt.Sprintf(ImageRemovalMarker, 2)) {
		t.Fatalf("the image notice never reached the request:\n%s", body)
	}
	for _, blk := range signedBlocks(t, run.requests[0]) {
		if blk.signature == "sig-1" || blk.signature == "sig-2" {
			t.Errorf("%s was replayed after the notice edited the history before it", blk.signature)
		}
	}
}

// A history with no edit in it loses nothing: stripping is for edits, not a
// standing policy.
func TestAnUneditedAnthropicHistoryKeepsAllItsReasoning(t *testing.T) {
	run := drivePreserved(t, 1_000_000, "", []string{thinkingAnswer("sig-5")}, "ok", legacyConversation(t, ""), "second task")
	if run.err != nil {
		t.Fatalf("turn failed: %v", run.err)
	}
	got := map[string]bool{}
	for _, blk := range signedBlocks(t, run.requests[0]) {
		got[blk.signature] = true
	}
	for _, sig := range []string{"sig-0", "sig-1", "sig-2", "sig-3", "sig-4"} {
		if !got[sig] {
			t.Errorf("%s was stripped from a history nobody edited", sig)
		}
	}
}
