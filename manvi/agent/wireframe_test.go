package agent

import (
	"bytes"
	"context"
	"encoding/json"
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

// A conversation driven by several Loops over one log, as the TUI and the serve
// chat path drive it: a Loop per turn, each assembling its own system prompt and
// tool set. The thinking block of each response is signed against the system
// prompt, the tools and every earlier message, so what matters is the bytes
// each request sends (shared/model-migration.md, "Breaking change 3").
type convo struct {
	t    *testing.T
	log  *session.Log
	reg  *tools.Registry
	url  string
	mu   sync.Mutex
	raw  [][]byte
	reqs []wireRequest
	bus  *bus.Bus
}

func echoTool(name, description string, ran *int) tools.Tool {
	return tools.Tool{
		Schema: llm.ToolSchema{Name: name, Description: description,
			InputSchema: []byte(`{"type":"object","properties":{"v":{"type":"integer"}}}`)},
		Handler: func(context.Context, tools.Call) tools.Result {
			if ran != nil {
				*ran++
			}
			return tools.Result{Text: "done"}
		},
	}
}

// newConvo starts a fixture Anthropic server that serves script[i] for the i-th
// request of the whole conversation (the last repeats).
func newConvo(t *testing.T, script []string, log *session.Log) *convo {
	t.Helper()
	c := &convo{t: t, log: log}
	if c.log == nil {
		c.log = session.NewLog()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		c.mu.Lock()
		i := len(c.raw)
		c.raw = append(c.raw, body)
		c.mu.Unlock()
		if i >= len(script) {
			i = len(script) - 1
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, script[i])
	}))
	t.Cleanup(srv.Close)
	c.url = srv.URL
	c.bus = bus.New()
	c.reg = tools.NewRegistry(c.bus)
	return c
}

// turn runs one Loop over the conversation's log. tune may adjust the Config
// and receives the Loop's bus, which is where a listener narrows the request.
func (c *convo) turn(system, prompt string, tune func(*Config, *bus.Bus)) error {
	c.t.Helper()
	b := bus.New()
	cfg := Config{
		Provider: anthropic.New(c.url, adaptertest.Secret("sk-FIXTURE-not-a-real-key")),
		Model:    "claude-opus-5-5", SystemPrompt: system,
		MaxSteps: 16, MaxTokens: 1024, AssertInvariant: true,
	}
	if tune != nil {
		tune(&cfg, b)
	}
	loop, err := NewLoop(cfg, b, c.log, c.reg)
	if err != nil {
		c.t.Fatal(err)
	}
	_, err = loop.Run(context.Background(), llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: prompt}}})
	return err
}

// requests is every request the server saw, decoded.
func (c *convo) requests() []wireRequest {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]wireRequest, 0, len(c.raw))
	for _, raw := range c.raw {
		var req wireRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			c.t.Fatalf("request is not JSON: %v\n%s", err, raw)
		}
		out = append(out, req)
	}
	return out
}

// twoTurnScript is a tool round and an answer per turn, each response opening
// with signed reasoning.
func twoTurnScript() []string {
	return []string{
		thinkingToolStep(0), thinkingAnswer("sig-a"),
		thinkingToolStep(2), thinkingAnswer("sig-b"),
		thinkingToolStep(4), thinkingAnswer("sig-c"),
	}
}

func roleOf(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var m struct {
		Role    string `json:"role"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m.Role
}

func systemMessages(t *testing.T, req wireRequest) []string {
	t.Helper()
	var out []string
	for _, raw := range req.Messages {
		if roleOf(t, raw) != "system" {
			continue
		}
		var m struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		var text string
		for _, c := range m.Content {
			text += c.Text
		}
		out = append(out, text)
	}
	return out
}

// assertFrameStable fails unless every request sends the same system and the
// same tool set, and returns how many requests it compared.
func assertFrameStable(t *testing.T, reqs []wireRequest) {
	t.Helper()
	for i, req := range reqs {
		if !bytes.Equal(req.System, reqs[0].System) {
			t.Errorf("request %d system differs from request 0:\n%s\n%s", i, req.System, reqs[0].System)
		}
		if !bytes.Equal(req.Tools, reqs[0].Tools) {
			t.Errorf("request %d tools differ from request 0:\n%s\n%s", i, req.Tools, reqs[0].Tools)
		}
	}
}

// AC1 and AC2: two Loops with different assembled prompts. The top-level system
// is the first prompt on every request; the change reaches the model as one
// role:system message after the second turn's user message, and it stays in the
// same place on every later request, so every replayed thinking block sees the
// prefix it was signed against.
func TestWireFrameFreezesSystemAcrossLoops(t *testing.T) {
	c := newConvo(t, twoTurnScript(), nil)
	if err := c.reg.Register(echoTool("echo", "echo", nil)); err != nil {
		t.Fatal(err)
	}
	for _, turn := range []struct{ system, prompt string }{
		{"prompt one", "first"}, {"prompt two, with a new lesson", "second"}, {"prompt two, with a new lesson", "third"},
	} {
		if err := c.turn(turn.system, turn.prompt, nil); err != nil {
			t.Fatalf("turn %q: %v", turn.prompt, err)
		}
	}
	reqs := c.requests()
	if len(reqs) != 6 {
		t.Fatalf("requests = %d, want 6", len(reqs))
	}
	if n := assertPrefixesPreserved(t, reqs); n < 5 {
		t.Fatalf("only %d replays were checked", n)
	}
	assertFrameStable(t, reqs)
	if !strings.Contains(string(reqs[0].System), "prompt one") {
		t.Errorf("system = %s, want the first prompt", reqs[0].System)
	}
	last := reqs[len(reqs)-1]
	notices := systemMessages(t, last)
	if len(notices) != 1 || !strings.Contains(notices[0], "prompt two, with a new lesson") {
		t.Fatalf("system messages on the last request = %q, want exactly the one that delivered the change", notices)
	}
	// It sits right after turn two's user message and follows a user message.
	for i, raw := range last.Messages {
		if roleOf(t, raw) == "system" && (i == 0 || roleOf(t, last.Messages[i-1]) != "user") {
			t.Errorf("system message at %d does not follow a user message", i)
		}
	}
	if got := systemMessages(t, reqs[1]); len(got) != 0 {
		t.Errorf("the first turn sent system messages %q before anything changed", got)
	}
}

// A conversation resumed from its log keeps the frame: a new Loop over a log
// rebuilt from its events freezes to what the original conversation sent.
func TestWireFrameSurvivesResume(t *testing.T) {
	c := newConvo(t, twoTurnScript(), nil)
	if err := c.reg.Register(echoTool("echo", "echo", nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("prompt one", "first", nil); err != nil {
		t.Fatal(err)
	}
	restored, err := session.RestoreLog(c.log.Events())
	if err != nil {
		t.Fatal(err)
	}
	c.log = restored
	if err := c.turn("prompt two", "second", nil); err != nil {
		t.Fatal(err)
	}
	reqs := c.requests()
	assertFrameStable(t, reqs)
	if n := assertPrefixesPreserved(t, reqs); n < 3 {
		t.Fatalf("only %d replays were checked", n)
	}
}

// A log written before the frame existed: what its conversation last sent
// top-level is the prompt its last system/prompt event recorded, and its blocks
// were signed under that. The frame adopts it, and the new prompt is a notice.
func TestWireFrameAdoptsTheLastPromptOfALegacyLog(t *testing.T) {
	log := session.NewLog()
	for _, e := range []struct {
		t session.Type
		p any
	}{
		{session.SystemPrompt, session.SystemPromptData{Text: "old one"}},
		{session.SystemPrompt, session.SystemPromptData{Text: "old two"}},
		{session.UserMessage, session.MessageData{Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "hi"}}}}},
		{session.AssistantMessage, session.MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ReasoningBlock{Text: "t", Signature: "sig-old"}, llm.TextBlock{Text: "hello"}}}}},
	} {
		if _, err := log.Append(e.t, e.p); err != nil {
			t.Fatal(err)
		}
	}
	c := newConvo(t, twoTurnScript(), log)
	if err := c.reg.Register(echoTool("echo", "echo", nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("brand new", "again", nil); err != nil {
		t.Fatal(err)
	}
	req := c.requests()[0]
	if !strings.Contains(string(req.System), "old two") {
		t.Errorf("system = %s, want the prompt the legacy conversation last sent", req.System)
	}
	if got := systemMessages(t, req); len(got) != 1 || !strings.Contains(got[0], "brand new") {
		t.Errorf("system messages = %q, want one delivering the new prompt", got)
	}
	if got := signedBlocks(t, req); len(got) != 1 {
		t.Errorf("the legacy reasoning block was not replayed: %v", got)
	}
}

// AC3: narrowing a step is not a change of the tool set on the wire. A listener
// that narrows the request leaves tools byte-identical; the model is told which
// tools are available; a call to a withheld tool is refused and nothing runs.
func TestWireFrameNarrowingDoesNotChangeTools(t *testing.T) {
	script := []string{
		thinkingToolStep(0), thinkingAnswer("sig-a"),
		wireStart + wireThinkingBlock(0, "plan", "sig-n") + wireToolCall(1, "toolu_n", "danger", `"{}"`, true) + wireFinish("tool_use"),
		thinkingAnswer("sig-b"),
	}
	c := newConvo(t, script, nil)
	var echoRan, dangerRan int
	if err := c.reg.Register(echoTool("echo", "echo", &echoRan)); err != nil {
		t.Fatal(err)
	}
	if err := c.reg.Register(echoTool("danger", "danger", &dangerRan)); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("p", "first", nil); err != nil {
		t.Fatal(err)
	}
	narrow := func(_ *Config, b *bus.Bus) {
		if _, err := bus.OnWaterfall(b, func(e LLMRequest, next bus.Next[LLMRequest]) LLMRequest {
			var kept []llm.ToolSchema
			for _, s := range e.Request.Tools {
				if s.Name == "echo" {
					kept = append(kept, s)
				}
			}
			e.Request.Tools = kept
			return next(e)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.turn("p", "second", narrow); err != nil {
		t.Fatal(err)
	}
	reqs := c.requests()
	assertFrameStable(t, reqs)
	if n := assertPrefixesPreserved(t, reqs); n < 3 {
		t.Fatalf("only %d replays were checked", n)
	}
	if dangerRan != 0 {
		t.Errorf("a withheld tool ran %d times", dangerRan)
	}
	notices := systemMessages(t, reqs[len(reqs)-1])
	if len(notices) != 1 || !strings.Contains(notices[0], "echo") || strings.Contains(notices[0], "danger") {
		t.Errorf("system messages = %q, want one naming only echo as available", notices)
	}
}

// Offering nothing is tool_choice none over the same tools, not an empty list.
func TestWireFrameChatOnlyTurnKeepsToolsAndForbidsCalls(t *testing.T) {
	c := newConvo(t, twoTurnScript(), nil)
	if err := c.reg.Register(echoTool("echo", "echo", nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("p", "first", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("p", "second", func(cfg *Config, _ *bus.Bus) { cfg.ChatOnly = true }); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	raw := c.raw[len(c.raw)-1]
	c.mu.Unlock()
	var body struct {
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if string(body.ToolChoice) != `{"type":"none"}` {
		t.Errorf("tool_choice = %s, want none", body.ToolChoice)
	}
	assertFrameStable(t, c.requests())
}

// A tool the conversation did not declare grows the frame at the turn boundary.
// The tools sit at the front of the prefix, so the signed reasoning before the
// boundary is stripped (a leading run) and the reason is in the log; reasoning
// produced afterwards is replayed against the new frame. A tool whose
// definition changed keeps the declared definition.
func TestWireFrameGrowthStripsOnlyTheLeadingRun(t *testing.T) {
	c := newConvo(t, twoTurnScript(), nil)
	if err := c.reg.Register(echoTool("echo", "echo", nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("p", "first", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.reg.Register(echoTool("late", "added after the first turn", nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("p", "second", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("p", "third", nil); err != nil {
		t.Fatal(err)
	}
	reqs := c.requests()
	if !strings.Contains(string(reqs[2].Tools), `"late"`) {
		t.Fatalf("the late tool was never declared: %s", reqs[2].Tools)
	}
	for i := 2; i < len(reqs); i++ {
		for _, blk := range signedBlocks(t, reqs[i]) {
			if blk.signature == "sig-0" || blk.signature == "sig-a" {
				t.Errorf("request %d replays %s, which was signed before the tool set grew", i, blk.signature)
			}
		}
	}
	if n := assertPrefixesPreserved(t, reqs[2:]); n < 2 {
		t.Fatalf("only %d replays after the boundary were checked", n)
	}
	var grew []string
	for _, e := range c.log.Events() {
		if e.Type != session.WireFrame {
			continue
		}
		var d session.FrameData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatal(err)
		}
		grew = append(grew, d.Why+":"+strings.Join(d.Added, ","))
	}
	if strings.Join(grew, " ") != "opened: tools_added:late" {
		t.Errorf("frames = %v, want opened then tools_added:late", grew)
	}
}

func TestWireFrameKeepsTheDeclaredDefinitionOfATool(t *testing.T) {
	c := newConvo(t, twoTurnScript(), nil)
	if err := c.reg.Register(echoTool("echo", "version one", nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("p", "first", nil); err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry(c.bus)
	if err := reg.Register(echoTool("echo", "version two, reworded", nil)); err != nil {
		t.Fatal(err)
	}
	c.reg = reg
	if err := c.turn("p", "second", nil); err != nil {
		t.Fatal(err)
	}
	reqs := c.requests()
	assertFrameStable(t, reqs)
	if n := assertPrefixesPreserved(t, reqs); n < 3 {
		t.Fatalf("only %d replays were checked", n)
	}
}

// Controls. A provider that does not bind thinking, and a model that does not
// take system messages, keep sending what each Loop assembled.
func TestWireFrameLeavesOtherTargetsAlone(t *testing.T) {
	for _, tc := range []struct {
		name, provider, model string
	}{
		{"unbound provider", "openai-compatible", "claude-opus-5-5"},
		{"model without system messages", "", "claude-sonnet-5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newConvo(t, twoTurnScript(), nil)
			if err := c.reg.Register(echoTool("echo", "echo", nil)); err != nil {
				t.Fatal(err)
			}
			tune := func(cfg *Config, _ *bus.Bus) {
				cfg.Model = tc.model
				if tc.provider != "" {
					cfg.Provider = windowed{Provider: cfg.Provider, window: 1_000_000, name: tc.provider}
				}
			}
			if err := c.turn("prompt one", "first", tune); err != nil {
				t.Fatal(err)
			}
			if err := c.turn("prompt two", "second", tune); err != nil {
				t.Fatal(err)
			}
			reqs := c.requests()
			if !strings.Contains(string(reqs[len(reqs)-1].System), "prompt two") {
				t.Errorf("system = %s, want the Loop's own prompt", reqs[len(reqs)-1].System)
			}
			for i, req := range reqs {
				if got := systemMessages(t, req); len(got) != 0 {
					t.Errorf("request %d sent system messages %q", i, got)
				}
			}
			for _, e := range c.log.Events() {
				if e.Type == session.WireFrame || e.Type == session.WireNotice {
					t.Errorf("a %s event was recorded", e.Type)
				}
			}
		})
	}
}

// A conversation that moved to a target without system messages is sent its
// history without the notices the earlier target took.
func TestWireNoticesAreNotSentToATargetThatCannotCarryThem(t *testing.T) {
	c := newConvo(t, twoTurnScript(), nil)
	if err := c.reg.Register(echoTool("echo", "echo", nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("one", "first", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("two", "second", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("two", "third", func(cfg *Config, _ *bus.Bus) { cfg.Model = "claude-sonnet-5" }); err != nil {
		t.Fatal(err)
	}
	reqs := c.requests()
	last := reqs[len(reqs)-1]
	if got := systemMessages(t, last); len(got) != 0 {
		t.Errorf("sent %q to a model that does not take system messages", got)
	}
}
