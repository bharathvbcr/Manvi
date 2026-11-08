package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/session"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// Adversarial review of w13-thinking (card ft-12ea2ecd). Every test drives the
// real loop and the real Anthropic encoder against a fixture server and reads
// the bytes that went out. Doc citations are to the claude-api skill's shared/
// directory: preserved-thinking-migration/causes.md (what the history-editing
// check compares), prompt-caching.md "Mid-conversation system messages" (the
// placement rules), tool-use-concepts.md (tool_choice).

// advTail is the part of a request body the review reads besides wireRequest.
type advTail struct {
	Model      string          `json:"model"`
	ToolChoice json.RawMessage `json:"tool_choice"`
}

func (c *convo) advBodies() []advTail {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]advTail, 0, len(c.raw))
	for _, raw := range c.raw {
		var b advTail
		if err := json.Unmarshal(raw, &b); err != nil {
			c.t.Fatalf("request is not JSON: %v", err)
		}
		out = append(out, b)
	}
	return out
}

// advServe replaces the conversation's server with one that answers the request
// numbers in failAt with that HTTP status (a non-retryable 4xx, so the turn
// fails) and otherwise serves the script in order, a failed request not
// consuming a script entry.
func advServe(t *testing.T, c *convo, script []string, failAt map[int]int) {
	t.Helper()
	served := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		c.mu.Lock()
		i := len(c.raw)
		c.raw = append(c.raw, body)
		j := served
		code, fail := failAt[i]
		if !fail {
			served++
		}
		c.mu.Unlock()
		if fail {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"fixture refusal"}}`)
			return
		}
		if j >= len(script) {
			j = len(script) - 1
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, script[j])
	}))
	t.Cleanup(srv.Close)
	c.url = srv.URL
}

// advPlacementProblems reports every way a request breaks the documented
// placement of a mid-conversation system message: it follows a user message,
// is not messages[0], and is either last or followed by an assistant turn
// (prompt-caching.md, "Mid-conversation system messages").
func advPlacementProblems(t *testing.T, req wireRequest) []string {
	t.Helper()
	var out []string
	roles := make([]string, len(req.Messages))
	for i, raw := range req.Messages {
		roles[i] = roleOf(t, raw)
	}
	for i, r := range roles {
		if r != "system" {
			continue
		}
		switch {
		case i == 0:
			out = append(out, fmt.Sprintf("system message is messages[0]: %v", roles))
		case roles[i-1] != "user":
			out = append(out, fmt.Sprintf("system message %d follows %s, not user: %v", i, roles[i-1], roles))
		}
		if i+1 < len(roles) && roles[i+1] != "assistant" {
			out = append(out, fmt.Sprintf("system message %d is followed by %s, not an assistant turn or the end: %v", i, roles[i+1], roles))
		}
	}
	return out
}

// advAppendOnly fails unless each request's messages begin with the previous
// request's messages, byte for byte: the notice, wherever it was appended, is
// replayed by the projection at exactly the position it was first sent.
func advAppendOnly(t *testing.T, reqs []wireRequest) {
	t.Helper()
	for i := 1; i < len(reqs); i++ {
		prev, cur := reqs[i-1].Messages, reqs[i].Messages
		if len(cur) < len(prev) {
			t.Errorf("request %d has %d messages, fewer than request %d's %d", i, len(cur), i-1, len(prev))
			continue
		}
		for k := range prev {
			if !bytes.Equal(prev[k], cur[k]) {
				t.Errorf("request %d message %d differs from request %d:\nbefore: %.200s\nafter:  %.200s", i, k, i-1, prev[k], cur[k])
				break
			}
		}
	}
}

func advNotices(log *session.Log) []session.NoticeData {
	var out []session.NoticeData
	for _, e := range log.Events() {
		if e.Type != session.WireNotice {
			continue
		}
		var d session.NoticeData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			panic(err)
		}
		out = append(out, d)
	}
	return out
}

func advFrames(t *testing.T, log *session.Log) []session.FrameData {
	t.Helper()
	var out []session.FrameData
	for _, e := range log.Events() {
		if e.Type != session.WireFrame {
			continue
		}
		var d session.FrameData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func advRegister(t *testing.T, reg *tools.Registry, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := reg.Register(echoTool(n, n, nil)); err != nil {
			t.Fatal(err)
		}
	}
}

// Item 1. tool_choice is outside the compared prefix (causes.md, "Changing
// model, max_tokens, ... tool_choice ... none is part of the compared prefix"),
// and none is unaffected on the 5.x models (tool-use-concepts.md: "auto and none
// are unaffected"; only any and tool 400). So across one conversation that
// toggles it, every replayed block must still see an identical system and tools,
// and the only value ever sent must be none.
func TestAdvToolChoiceNoneIsOutsideThePrefixAndOnlyEverNone(t *testing.T) {
	c := newConvo(t, twoTurnScript(), nil)
	advRegister(t, c.reg, "echo")
	for _, turn := range []struct {
		prompt string
		tune   func(*Config, *bus.Bus)
	}{
		{"one", nil},
		{"two", func(cfg *Config, _ *bus.Bus) { cfg.ChatOnly = true }},
		{"three", nil},
	} {
		if err := c.turn("p", turn.prompt, turn.tune); err != nil {
			t.Fatalf("%s: %v", turn.prompt, err)
		}
	}
	reqs := c.requests()
	assertFrameStable(t, reqs)
	if n := assertPrefixesPreserved(t, reqs); n < 4 {
		t.Fatalf("only %d replays were checked", n)
	}
	sawNone, sawAbsent := false, false
	for i, b := range c.advBodies() {
		switch string(b.ToolChoice) {
		case "":
			sawAbsent = true
		case `{"type":"none"}`:
			sawNone = true
		default:
			t.Errorf("request %d sent tool_choice %s; only none is ever valid to send (any and tool 400 on 5.x)", i, b.ToolChoice)
		}
	}
	if !sawNone || !sawAbsent {
		t.Errorf("the fixture did not toggle tool_choice (none seen=%v, absent seen=%v)", sawNone, sawAbsent)
	}
}

// Item 2. Tools are compared as a name-keyed set; reordering is fine
// (causes.md: "Reordering tools in the tools array without changing them -
// compared as a name-keyed set"; preserved-thinking-migration.md table row 0).
// Registering in a different order in each Loop must therefore still be a
// byte-identical frame, and the frame is name-sorted.
func TestAdvToolRegistrationOrderIsNotAFrameChange(t *testing.T) {
	c := newConvo(t, twoTurnScript(), nil)
	advRegister(t, c.reg, "zeta", "alpha", "echo", "mid")
	if err := c.turn("p", "first", nil); err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry(c.bus)
	c.reg = reg
	advRegister(t, reg, "mid", "echo", "alpha", "zeta")
	if err := c.turn("p", "second", nil); err != nil {
		t.Fatal(err)
	}
	reqs := c.requests()
	assertFrameStable(t, reqs)
	var declared []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(reqs[0].Tools, &declared); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range declared {
		names = append(names, d.Name)
	}
	if !slices.IsSorted(names) || len(names) != 4 {
		t.Errorf("declared tools = %v, want four, name-sorted", names)
	}
	if frames := advFrames(t, c.log); len(frames) != 1 {
		t.Errorf("%d frames recorded, want 1: a reorder is not a growth", len(frames))
	}
}

// Item 3. Notice growth. With identical inputs on every turn the loop must send
// nothing after the first request: no new notice, and the same bytes.
func TestAdvIdenticalPromptsAcrossLoopsMintNoNotice(t *testing.T) {
	c := newConvo(t, twoTurnScript(), nil)
	advRegister(t, c.reg, "echo")
	for _, p := range []string{"one", "two", "three"} {
		if err := c.turn("the same prompt every turn", p, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := advNotices(c.log); len(got) != 0 {
		t.Fatalf("%d notices for a conversation whose prompt never changed", len(got))
	}
	assertFrameStable(t, c.requests())
}

// Item 3, the oscillation: prompt A, then B, then A again. The model holds B by
// then, so A has to be said again in full; that is correct, and it costs one
// whole prompt per change, never one per turn. A repeated B costs nothing.
func TestAdvEachPromptChangeCostsOneNoticeAndRepeatsCostNone(t *testing.T) {
	script := []string{
		thinkingToolStep(0), thinkingAnswer("sig-a"),
		thinkingToolStep(2), thinkingAnswer("sig-b"),
		thinkingToolStep(4), thinkingAnswer("sig-c"),
		thinkingToolStep(6), thinkingAnswer("sig-d"),
	}
	c := newConvo(t, script, nil)
	advRegister(t, c.reg, "echo")
	for i, p := range []string{"prompt A", "prompt B", "prompt B", "prompt A"} {
		if err := c.turn(p, fmt.Sprintf("turn %d", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	got := advNotices(c.log)
	if len(got) != 2 || got[0].Prompt != "prompt B" || got[1].Prompt != "prompt A" {
		t.Fatalf("notices = %+v, want exactly B then A", got)
	}
	reqs := c.requests()
	assertFrameStable(t, reqs)
	advAppendOnly(t, reqs)
	if n := assertPrefixesPreserved(t, reqs); n < 6 {
		t.Fatalf("only %d replays were checked", n)
	}
}

// Item 3, scrubber. The log's scrubber rewrites a secret in the prompt as it is
// appended, so the log's record of the prompt is never equal to the Loop's
// unscrubbed Config.SystemPrompt. The notice test is "prompt differs from what
// the model holds"; if the held value is the scrubbed one and the live one is
// not, the whole prompt is re-sent as a notice on every turn of a conversation
// whose prompt never changed.
func TestAdvScrubbedPromptDoesNotMintANoticeEveryTurn(t *testing.T) {
	log := session.NewLog()
	log.SetScrubber(func(s string) string { return strings.ReplaceAll(s, advNeedle, "[redacted]") })
	c := newConvo(t, twoTurnScript(), log)
	advRegister(t, c.reg, "echo")
	// The invariant check is off here so the count is measured on its own; the
	// next test holds the invariant on.
	noAssert := func(cfg *Config, _ *bus.Bus) { cfg.AssertInvariant = false }
	for _, p := range []string{"one", "two", "three"} {
		if err := c.turn(advPromptWithSecret, p, noAssert); err != nil {
			t.Fatal(err)
		}
	}
	if got := advNotices(c.log); len(got) != 0 {
		t.Errorf("%d notices for a conversation whose prompt never changed; the scrubbed record is compared to the live prompt, "+
			"so the whole prompt is appended to the context on every turn", len(got))
	}
	for i, req := range c.requests() {
		if bytes.Contains(req.System, []byte(advNeedle)) {
			t.Errorf("request %d carries the secret in system", i)
		}
	}
}

const (
	advNeedle           = "needle-LIVE-123"
	advPromptWithSecret = "project instructions mention " + advNeedle + " verbatim"
)

// Item 3/9, scrubber with the model-visible-means-logged invariant on (the
// loop's AssertInvariant, which the CLI turns on from a flag): what the
// notice sent must equal what the log recorded for it, and a notice is sent
// when a prompt really changes, so the notice text is scrubbed in the log and
// whatever went out must be that same text.
func TestAdvScrubbedNoticeSentEqualsNoticeLogged(t *testing.T) {
	log := session.NewLog()
	log.SetScrubber(func(s string) string { return strings.ReplaceAll(s, advNeedle, "[redacted]") })
	c := newConvo(t, twoTurnScript(), log)
	advRegister(t, c.reg, "echo")
	if err := c.turn("prompt one", "first", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.turn(advPromptWithSecret, "second", nil); err != nil {
		t.Fatalf("a changed prompt that holds a scrubbed secret fails the turn: %v", err)
	}
	for i, req := range c.requests() {
		for _, m := range systemMessages(t, req) {
			if strings.Contains(m, advNeedle) {
				t.Errorf("request %d sends the secret in a system message the log holds scrubbed", i)
			}
		}
	}
}

// Item 3, per step. A listener that alternates the offered set every step of one
// turn makes one short notice per change (names only, never the prompt), and the
// requests stay append-only.
func TestAdvAlternatingStepNarrowingKeepsRequestsAppendOnly(t *testing.T) {
	script := []string{
		thinkingToolStep(0), thinkingToolStep(1), thinkingToolStep(2), thinkingAnswer("sig-end"),
	}
	c := newConvo(t, script, nil)
	advRegister(t, c.reg, "echo", "other")
	steps := 0
	tune := func(_ *Config, b *bus.Bus) {
		if _, err := bus.OnWaterfall(b, func(e LLMRequest, next bus.Next[LLMRequest]) LLMRequest {
			steps++
			if steps%2 == 0 {
				var kept []llm.ToolSchema
				for _, s := range e.Request.Tools {
					if s.Name == "echo" {
						kept = append(kept, s)
					}
				}
				e.Request.Tools = kept
			}
			return next(e)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.turn("p", "go", tune); err != nil {
		t.Fatal(err)
	}
	reqs := c.requests()
	assertFrameStable(t, reqs)
	advAppendOnly(t, reqs)
	if n := assertPrefixesPreserved(t, reqs); n < 3 {
		t.Fatalf("only %d replays were checked", n)
	}
	for i, req := range reqs {
		if p := advPlacementProblems(t, req); len(p) != 0 {
			t.Errorf("request %d: %v", i, p)
		}
	}
	for _, n := range advNotices(c.log) {
		if n.Prompt != "" {
			t.Errorf("a tools-only change re-sent the prompt: %q", n.Prompt)
		}
	}
}

// Item 4. A notice is appended to the request after the log append; the next
// request's projection must put it at exactly the same place. Across a tool
// round (assistant tool_use, user tool_result, then the notice) and across a
// resume from the events.
func TestAdvNoticeStaysWhereItWasSentAcrossToolRoundAndResume(t *testing.T) {
	c := newConvo(t, twoTurnScript(), nil)
	advRegister(t, c.reg, "echo")
	if err := c.turn("prompt one", "first", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("prompt two", "second", nil); err != nil {
		t.Fatal(err)
	}
	restored, err := session.RestoreLog(c.log.Events())
	if err != nil {
		t.Fatal(err)
	}
	c.log = restored
	if err := c.turn("prompt three", "third", nil); err != nil {
		t.Fatal(err)
	}
	reqs := c.requests()
	advAppendOnly(t, reqs)
	assertFrameStable(t, reqs)
	if n := assertPrefixesPreserved(t, reqs); n < 5 {
		t.Fatalf("only %d replays were checked", n)
	}
	for i, req := range reqs {
		if p := advPlacementProblems(t, req); len(p) != 0 {
			t.Errorf("request %d: %v", i, p)
		}
	}
	if got := systemMessages(t, reqs[len(reqs)-1]); len(got) != 2 {
		t.Errorf("last request holds %d system messages, want the two notices", len(got))
	}
}

// Item 4, the crash case. The request that carried a notice fails (here a 400),
// so the log holds user, notice and no assistant turn, and the next turn adds a
// user message: the projection is user, system, user. The documented rule is
// that a system message must be the last entry or be followed by an assistant
// turn (prompt-caching.md), so this request would be refused by the API, and
// ValidateRequest only checks the message before it.
func TestAdvNoticeIsNeverFollowedByAUserMessageOnTheWire(t *testing.T) {
	c := newConvo(t, twoTurnScript(), nil)
	advRegister(t, c.reg, "echo")
	advServe(t, c, twoTurnScript(), map[int]int{2: 400})
	if err := c.turn("prompt one", "first", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("prompt two", "second", nil); err == nil {
		t.Fatal("the fixture refusal did not fail the turn")
	}
	if len(advNotices(c.log)) != 1 {
		t.Fatalf("notices = %d after the failed turn, want the one that rode the failed request", len(advNotices(c.log)))
	}
	_ = c.turn("prompt two", "third", nil)
	reqs := c.requests()
	last := reqs[len(reqs)-1]
	if p := advPlacementProblems(t, last); len(p) != 0 {
		t.Errorf("the request after a failed turn breaks the system message placement rule: %v", p)
	}
}

// Item 5. A tool added while a tool round is running is refused with a proper
// tool_result and never executed; the declared tools do not change in that turn.
// At the next turn boundary the frame grows and only a leading run of signed
// reasoning goes.
func TestAdvLateToolInAToolRoundIsRefusedNotRunThenDeclaredAtTheBoundary(t *testing.T) {
	script := []string{
		thinkingToolStep(0),
		wireStart + wireThinkingBlock(0, "plan", "sig-1") + wireToolCall(1, "toolu_late", "late", `"{}"`, true) + wireFinish("tool_use"),
		thinkingAnswer("sig-a"),
		thinkingAnswer("sig-b"), thinkingAnswer("sig-c"),
	}
	c := newConvo(t, script, nil)
	var lateRan int
	registered := false
	first := tools.Tool{
		Schema: llm.ToolSchema{Name: "echo", Description: "echo", InputSchema: []byte(`{"type":"object"}`)},
		Handler: func(context.Context, tools.Call) tools.Result {
			if !registered {
				registered = true
				if err := c.reg.Register(echoTool("late", "late", &lateRan)); err != nil {
					t.Error(err)
				}
			}
			return tools.Result{Text: "done"}
		},
	}
	if err := c.reg.Register(first); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("p", "go", nil); err != nil {
		t.Fatalf("a call to a tool the conversation never declared must not fail the turn: %v", err)
	}
	if lateRan != 0 {
		t.Fatalf("the undeclared tool ran %d times", lateRan)
	}
	reqs := c.requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want 3", len(reqs))
	}
	assertFrameStable(t, reqs)
	c.mu.Lock()
	third := string(c.raw[2])
	c.mu.Unlock()
	if !strings.Contains(third, "was not offered in this step") || !strings.Contains(third, `"is_error":true`) {
		t.Errorf("the model was not given a refused tool_result for the undeclared tool:\n%.400s", third)
	}
	if err := c.turn("p", "next", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("p", "after", nil); err != nil {
		t.Fatal(err)
	}
	reqs = c.requests()
	if !strings.Contains(string(reqs[3].Tools), `"late"`) {
		t.Fatalf("the tool was not declared at the boundary: %s", reqs[3].Tools)
	}
	advSuffixOnly(t, reqs[3:], []string{"sig-0", "sig-1", "sig-a", "sig-b", "sig-c"}, 3)
}

// advSuffixOnly checks the documented removal rule on the requests at/after a
// boundary: the signed blocks still replayed are a contiguous suffix of the
// blocks produced so far, so no removed block follows a kept one
// (preserved-thinking-migration.md: "Remove thinking only as a contiguous run").
// produced is the signature each response carried, in order; produced[i] is the
// response to request i, and request 3+j has seen produced[:3+j].
func advSuffixOnly(t *testing.T, reqs []wireRequest, produced []string, firstIndex int) {
	t.Helper()
	for j, req := range reqs {
		seen := produced[:min(firstIndex+j, len(produced))]
		have := map[string]bool{}
		for _, b := range signedBlocks(t, req) {
			have[b.signature] = true
		}
		kept := false
		for _, sig := range seen {
			switch {
			case have[sig]:
				kept = true
			case kept:
				t.Errorf("request %d dropped %s after an earlier block was kept: a gap, not a leading run (%v)", firstIndex+j, sig, seen)
			}
		}
	}
}

// Item 5, deferred growth. A turn that starts with history ending in a tool
// result (the previous turn stopped at MaxSteps) must not grow the frame: the
// assistant turn awaiting that result keeps its signed reasoning.
func TestAdvGrowthWaitsWhenAPriorTurnStoppedMidToolRound(t *testing.T) {
	script := []string{
		thinkingToolStep(0),
		thinkingAnswer("sig-a"),
		thinkingAnswer("sig-b"),
	}
	c := newConvo(t, script, nil)
	advRegister(t, c.reg, "echo")
	_ = c.turn("p", "first", func(cfg *Config, _ *bus.Bus) { cfg.MaxSteps = 1 })
	history, err := c.log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	if AtTurnBoundary(history) {
		t.Fatal("the fixture turn did not stop mid tool round, so the deferral path was not reached")
	}
	advRegister(t, c.reg, "late")
	if err := c.turn("p", "second", nil); err != nil {
		t.Fatal(err)
	}
	reqs := c.requests()
	if strings.Contains(string(reqs[len(reqs)-1].Tools), `"late"`) {
		t.Fatalf("the frame grew while the history ended in a tool result: %s", reqs[len(reqs)-1].Tools)
	}
	if len(advFrames(t, c.log)) != 1 {
		t.Fatalf("frames = %d, want only the opening one", len(advFrames(t, c.log)))
	}
	if got := signedBlocks(t, reqs[len(reqs)-1]); len(got) == 0 {
		t.Error("signed reasoning was stripped although no boundary was crossed")
	}
}

// Item 6. The conversation declared an old schema; the handler registered by
// the next Loop is new. The loop does not validate arguments against either
// schema, so a call valid for the declared one reaches the new handler as sent.
// This pins the observed behaviour: the handler runs, with the old-shaped
// arguments, and nothing tells the model the definition changed.
func TestAdvArgumentsValidForTheDeclaredSchemaReachTheNewHandler(t *testing.T) {
	c := newConvo(t, twoTurnScript(), nil)
	advRegister(t, c.reg, "echo")
	if err := c.turn("p", "first", nil); err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry(c.bus)
	var got []string
	if err := reg.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "echo", Description: "echo",
			InputSchema: []byte(`{"type":"object","properties":{"w":{"type":"string"}},"required":["w"]}`)},
		Handler: func(_ context.Context, call tools.Call) tools.Result {
			got = append(got, string(call.Arguments))
			return tools.Result{Text: "done"}
		},
	}); err != nil {
		t.Fatal(err)
	}
	c.reg = reg
	if err := c.turn("p", "second", nil); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != `{"v":2}` {
		t.Fatalf("new handler saw %q, want exactly one call with the declared-schema arguments", got)
	}
	if !strings.Contains(string(c.requests()[3].Tools), `"v":{"type":"integer"}`) {
		t.Errorf("the declared (old) schema is no longer what goes out: %s", c.requests()[3].Tools)
	}
}

// Item 7. Switch to a model that takes no system messages and back. The target
// without support gets its own prompt and no notices; switching back, every
// request of the supporting model must still extend the previous one of that
// model byte for byte, because the notices are only hidden in the request.
func TestAdvModelSwitchAndBackKeepsThePrefixOfTheBoundModel(t *testing.T) {
	script := []string{
		thinkingToolStep(0), thinkingAnswer("sig-a"),
		thinkingToolStep(2), thinkingAnswer("sig-b"),
		thinkingToolStep(4), thinkingAnswer("sig-c"),
		thinkingToolStep(6), thinkingAnswer("sig-d"),
	}
	c := newConvo(t, script, nil)
	advRegister(t, c.reg, "echo")
	sonnet5 := func(cfg *Config, _ *bus.Bus) { cfg.Model = "claude-sonnet-5" }
	for _, turn := range []struct {
		system, prompt string
		tune           func(*Config, *bus.Bus)
	}{
		{"prompt A", "one", nil}, {"prompt B", "two", nil}, {"prompt C", "three", sonnet5}, {"prompt C", "four", nil},
	} {
		if err := c.turn(turn.system, turn.prompt, turn.tune); err != nil {
			t.Fatalf("%s: %v", turn.prompt, err)
		}
	}
	reqs, bodies := c.requests(), c.advBodies()
	var opus []wireRequest
	for i, b := range bodies {
		switch b.Model {
		case "claude-sonnet-5":
			if got := systemMessages(t, reqs[i]); len(got) != 0 {
				t.Errorf("request %d sent %q to a model that takes no system messages", i, got)
			}
			if !strings.Contains(string(reqs[i].System), "prompt C") {
				t.Errorf("request %d system = %s, want the Loop's own prompt on a target that is not frozen", i, reqs[i].System)
			}
		default:
			opus = append(opus, reqs[i])
		}
	}
	if len(opus) != 6 {
		t.Fatalf("opus requests = %d, want 6", len(opus))
	}
	assertFrameStable(t, opus)
	advAppendOnly(t, opus)
	for i, req := range opus {
		if p := advPlacementProblems(t, req); len(p) != 0 {
			t.Errorf("opus request %d: %v", i, p)
		}
	}
}

// Item 7, gating. A model the adapter does not know has no Capability, so the
// frame is not sent for it. The adapter must then refuse the request (catalogue
// Validate) rather than send it with a per-Loop system: nothing may reach the
// server, and the log must not gain a frame.
func TestAdvUnknownModelFailsClosedBeforeTheWire(t *testing.T) {
	c := newConvo(t, twoTurnScript(), nil)
	advRegister(t, c.reg, "echo")
	err := c.turn("p", "first", func(cfg *Config, _ *bus.Bus) { cfg.Model = "claude-opus-5-5-20261001-custom" })
	if err == nil {
		t.Fatal("a model outside the catalogue was served")
	}
	if n := len(c.requests()); n != 0 {
		t.Errorf("%d requests reached the server for an unknown model", n)
	}
	if got := advFrames(t, c.log); len(got) != 0 {
		t.Errorf("a frame was recorded for a model that never sent a request: %+v", got)
	}
}

// Item 3/AC3, first-turn narrowing. A first turn that offers no tools (ChatOnly)
// declares an empty frame; the next turn that wants tools is then a growth of the
// set. That is the documented price (strip a leading run), never a 400. The
// strip must be a clean leading run and the wire must stay consistent after it.
func TestAdvChatOnlyFirstTurnThenToolsStripsCleanlyInsteadOfBreaking(t *testing.T) {
	script := []string{
		thinkingAnswer("sig-a"),
		thinkingToolStep(1), thinkingAnswer("sig-b"),
		thinkingToolStep(3), thinkingAnswer("sig-c"),
	}
	c := newConvo(t, script, nil)
	advRegister(t, c.reg, "echo")
	if err := c.turn("p", "chat", func(cfg *Config, _ *bus.Bus) { cfg.ChatOnly = true }); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"work", "more"} {
		if err := c.turn("p", p, nil); err != nil {
			t.Fatal(err)
		}
	}
	reqs := c.requests()
	frames := advFrames(t, c.log)
	if len(frames) != 2 || frames[1].Why != session.WhyToolsAdded {
		t.Fatalf("frames = %+v, want an opening frame and one tools_added", frames)
	}
	if n := assertPrefixesPreserved(t, reqs[1:]); n < 2 {
		t.Fatalf("only %d replays after the boundary were checked", n)
	}
	advSuffixOnly(t, reqs[1:], []string{"sig-a", "sig-1", "sig-b", "sig-3", "sig-c"}, 1)
	t.Logf("signed blocks replayed on the first request after the boundary: %d (sig-a, produced before it, was stripped)",
		len(signedBlocks(t, reqs[1])))
}
