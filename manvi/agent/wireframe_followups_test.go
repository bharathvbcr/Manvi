package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/session"
)

// loopOver builds the Loop turn() would, so a test can ask it for a Draft before
// it runs.
func (c *convo) loopOver(system string) *Loop {
	c.t.Helper()
	b := bus.New()
	loop, err := NewLoop(Config{
		Provider: anthropic.New(c.url, adaptertest.Secret("sk-FIXTURE-not-a-real-key")),
		Model:    "claude-opus-5-5", SystemPrompt: system,
		MaxSteps: 16, MaxTokens: 1024, AssertInvariant: true,
	}, b, c.log, c.reg)
	if err != nil {
		c.t.Fatal(err)
	}
	return loop
}

func wireToolNames(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var tools []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatalf("tools are not a list: %v\n%s", err, raw)
	}
	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Name
	}
	return names
}

// A fan-out is admitted on the size of the Loop's draft, so the draft must carry
// the system prompt and tools the request will, which are the conversation's
// frame, not this Loop's own prompt and tool set (ft-9a5710b6 item 2). The second
// Loop here has a different prompt and one more tool than the frame declares: the
// first request after a growth at the turn boundary carries the first Loop's
// prompt and the grown tool set, and the draft says the same, without recording
// anything.
func TestDraftSizesFromTheFrameTheTurnWillSend(t *testing.T) {
	c := newConvo(t, twoTurnScript(), nil)
	advRegister(t, c.reg, "echo")
	if err := c.turn("first prompt", "one", nil); err != nil {
		t.Fatal(err)
	}
	advRegister(t, c.reg, "extra")

	user := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "two"}}}
	loop := c.loopOver("second prompt")
	before := c.log.Len()
	draft := loop.Draft(user)
	if got := c.log.Len(); got != before {
		t.Fatalf("Draft recorded %d event(s); it must record nothing", got-before)
	}

	sentFrom := len(c.requests())
	if _, err := loop.Run(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	sent := c.requests()[sentFrom]

	if !strings.Contains(string(sent.System), "first prompt") || strings.Contains(string(sent.System), "second prompt") {
		t.Fatalf("the request carries system %s, want the frame's first prompt", sent.System)
	}
	if draft.System != "first prompt" {
		t.Errorf("draft system = %q, the request carries the frame's %s", draft.System, sent.System)
	}
	var draftTools []string
	for _, s := range draft.Tools {
		draftTools = append(draftTools, s.Name)
	}
	if want := wireToolNames(t, sent.Tools); !slices.Equal(draftTools, want) {
		t.Errorf("draft tools = %v, the request carries %v", draftTools, want)
	}
	if !slices.Contains(draftTools, "extra") {
		t.Errorf("draft tools = %v, want the tool the turn will grow the frame by", draftTools)
	}
}

// The draft of a Loop that sends no frame is still the Loop's own prompt and
// tools.
func TestDraftWithoutAFrameKeepsTheLoopsOwnPromptAndTools(t *testing.T) {
	h := build(t, costTurns(), nil)
	registerRead(t, h)
	req := h.loop.Draft(userMsg("go"))
	if req.System != "you are a builder" || len(req.Tools) != 1 || req.Tools[0].Name != "read" {
		t.Fatalf("draft = system %q tools %+v", req.System, req.Tools)
	}
}

// A log from before frames were recorded: the conversation answered once, and
// nothing says which prompt and tools that answer's reasoning was signed under.
// The frame that opens over it is a growth from nothing, so the reasoning before
// it is stripped rather than replayed (ft-9a5710b6 item 3). The same log with a
// frame recorded at its start replays everything
// (TestAnUneditedAnthropicHistoryKeepsAllItsReasoning).
func TestAFrameOpenedOnAConversationThatPredatesFramesStripsEarlierReasoning(t *testing.T) {
	run := drivePreserved(t, 1_000_000, "", []string{thinkingAnswer("sig-5")}, "ok",
		legacyConversationFrom(t, "", false), "second task")
	if run.err != nil {
		t.Fatalf("turn failed: %v", run.err)
	}
	if got := signedBlocks(t, run.requests[0]); len(got) != 0 {
		t.Errorf("reasoning signed under an unrecorded frame was replayed: %v", got)
	}
	body := string(run.raw[0])
	for _, kept := range []string{`"text":"step 1"`, `"id":"toolu_1"`, `"text":"first task"`} {
		if !strings.Contains(body, kept) {
			t.Errorf("the strip removed more than reasoning: %q is gone", kept)
		}
	}
	var frames []session.FrameData
	for _, e := range run.log.Events() {
		if e.Type != session.WireFrame {
			continue
		}
		var d session.FrameData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatal(err)
		}
		frames = append(frames, d)
	}
	if len(frames) != 1 || frames[0].Why != session.WhyToolsAdded || !slices.Equal(frames[0].Added, []string{"echo"}) {
		t.Fatalf("frames = %+v, want one tools_added frame declaring echo", frames)
	}
	// The strip is the log's own, so a Loop built later and a resume read the same:
	// the reasoning produced after the boundary is kept, the older is not.
	msgs, err := run.log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := run.log.StripStaleReasoning(msgs)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, m := range out {
		for _, b := range m.Content {
			if r, ok := b.(llm.ReasoningBlock); ok && r.Signature != "" {
				kept = append(kept, r.Signature)
			}
		}
	}
	if !slices.Equal(kept, []string{"sig-5"}) {
		t.Errorf("signed blocks kept after the boundary = %v, want only the one produced after it", kept)
	}
}

// Mid tool round the assistant turn awaiting its result keeps its reasoning, so a
// frame that would be a boundary waits for the turn to end, as a growth does.
func TestAFrameOverAConversationMidToolRoundWaits(t *testing.T) {
	c := newConvo(t, twoTurnScript(), nil)
	advRegister(t, c.reg, "echo")
	call := llm.ToolCallBlock{ID: "toolu_x", Name: "echo", Arguments: json.RawMessage(`{"v":1}`)}
	for _, e := range []struct {
		typ session.Type
		p   any
	}{
		{session.UserMessage, session.MessageData{Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}}}},
		{session.AssistantMessage, session.MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ReasoningBlock{Text: "plan", Signature: "sig-mid"}, call}}}},
		{session.ToolResult, session.ToolResultData{ToolCallID: "toolu_x", Text: "done"}},
	} {
		if _, err := c.log.Append(e.typ, e.p); err != nil {
			t.Fatal(err)
		}
	}
	next, err := c.loopOver("p").planFrame()
	if err != nil || next != nil {
		t.Fatalf("planFrame mid tool round = %+v, %v; want nothing recorded yet", next, err)
	}
	if got := c.log.Wire().Frame; got != nil {
		t.Fatalf("a frame was recorded mid tool round: %+v", got)
	}
}
