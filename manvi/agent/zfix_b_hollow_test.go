package agent

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/session"
)

// unsignedThinkingCutOff is a response that hit max_tokens inside its thinking
// block: the stream never delivered a signature, so the adapter keeps an
// unsigned block, which the encoder cannot replay and omits.
func unsignedThinkingCutOff() string {
	return wireStart + `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"still thinking"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

` + wireFinish("max_tokens")
}

// A notice rides a request whose reply is hollow (cut off in thinking, nothing
// the encoder can send). The reply does not answer the notice: the wire would
// carry user, system, user, which shared/prompt-caching.md ("Mid-conversation
// system messages") refuses, and every later request of the session would fail
// with it. No request of the conversation may place a system message anywhere
// but last or before an assistant turn, and the lost notice is said again.
func TestAdvHollowReplyAfterANoticeDoesNotBreakTheSession(t *testing.T) {
	c := newConvo(t, []string{thinkingAnswer("sig-a"), unsignedThinkingCutOff(), thinkingAnswer("sig-c")}, nil)
	if err := c.reg.Register(echoTool("echo", "echo", nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("prompt one", "first", nil); err != nil {
		t.Fatal(err)
	}
	_ = c.turn("prompt two", "second", nil) // the hollow reply; whether the turn errors is not the point
	if err := c.turn("prompt two", "third", nil); err != nil {
		t.Fatalf("the turn after a hollow reply must succeed: %v", err)
	}
	reqs := c.requests()
	for i, req := range reqs {
		if p := advPlacementProblems(t, req); len(p) != 0 {
			t.Errorf("request %d: %v", i, p)
		}
	}
	last := reqs[len(reqs)-1]
	if got := systemMessages(t, last); len(got) != 1 || !strings.Contains(got[0], "prompt two") {
		t.Errorf("system messages = %q, want the one notice that says prompt two", got)
	}
}

// A trim dropped the turn holding a notice that delivered prompt B over frame
// prompt A. The carried frame must not claim B as delivered: the model of the
// resumed conversation was never sent it, so the next request says it again.
// The carried frame is written the way the store writes one, as raw fields, so
// this holds for a file written by the build that carried Delivered and Told.
func TestAdvResumedAfterATrimStillDeliversTheSupersededPrompt(t *testing.T) {
	log := session.NewLog()
	for _, step := range []struct {
		typ  session.Type
		data any
	}{
		{session.TurnStart, nil},
		{session.WireFrame, map[string]any{
			"system": "prompt A", "why": "carried", "delivered": "prompt B", "told": []string{"echo"}, "through": 1,
		}},
	} {
		if _, err := log.Append(step.typ, step.data); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []struct {
		typ  session.Type
		role llm.Role
		text string
	}{{session.UserMessage, llm.RoleUser, "kept user"}, {session.AssistantMessage, llm.RoleAssistant, "kept answer"}} {
		msg := llm.Message{Role: m.role, Content: []llm.ContentBlock{llm.TextBlock{Text: m.text}}}
		if _, err := log.Append(m.typ, session.MessageData{Message: msg}); err != nil {
			t.Fatal(err)
		}
	}
	c := newConvo(t, []string{thinkingAnswer("sig-a")}, log)
	if err := c.reg.Register(echoTool("echo", "echo", nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("prompt B", "next", nil); err != nil {
		t.Fatal(err)
	}
	req := c.requests()[0]
	if !strings.Contains(string(req.System), "prompt A") {
		t.Errorf("system = %s, want the frame's prompt A", req.System)
	}
	if got := systemMessages(t, req); len(got) != 1 || !strings.Contains(got[0], "prompt B") {
		t.Errorf("system messages = %q, want a notice carrying prompt B: the model never held it", got)
	}
}
