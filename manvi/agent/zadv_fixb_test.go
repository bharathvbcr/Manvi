package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/session"
)

// Adversarial review of fix-b at the wire. Every test drives the real loop and
// the real Anthropic adapter against a fixture server (AssertInvariant is on in
// convo.turn, so model-visible-means-logged is checked before every request) and
// reads the bytes that went out. Wire rules are cited from the claude-api
// skill's shared/ directory: prompt-caching.md "Mid-conversation system
// messages" and preserved-thinking-migration.md.

func advWireText(raw json.RawMessage) string { return string(raw) }

// advInvariantBroken reports a turn error that is the model-visible-means-logged
// check firing.
func advInvariantBroken(err error) bool {
	return err != nil && strings.Contains(err.Error(), "model-visible-means-logged")
}

func advAssistantCount(t *testing.T, req wireRequest) int {
	t.Helper()
	n := 0
	for _, raw := range req.Messages {
		var m struct {
			Role    string            `json:"role"`
			Content []json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		if len(m.Content) == 0 {
			t.Errorf("a %s message went out with no content: %s", m.Role, raw)
		}
		if m.Role == "assistant" {
			n++
		}
	}
	return n
}

func advSendableAssistants(t *testing.T, log *session.Log) int {
	t.Helper()
	msgs, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 {
		t.Fatal("the log projects no messages")
	}
	n := 0
	// The last request went out before the final reply was logged.
	for _, m := range msgs[:len(msgs)-1] {
		if m.Role == llm.RoleAssistant && m.HasSendableContent() {
			n++
		}
	}
	return n
}

// hollowStreams are replies as the real decoder settles them, each ending a turn.
func hollowStreams() map[string]string {
	return map[string]string{
		"whitespace text": wireStart + wireTextBlock(0, "  \n") + wireFinish("end_turn"),
		"empty text, never filled": wireStart + `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

` + wireFinish("end_turn"),
		"unsigned thinking, max_tokens": unsignedThinkingCutOff(),
		"redacted thinking only": wireStart + `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"blob-1"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

` + wireFinish("end_turn"),
		"signed thinking only": wireStart + wireThinkingBlock(0, "t", "sig-only") + wireFinish("end_turn"),
		"server tool block only": wireStart + `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

` + wireFinish("end_turn"),
		"hollow text after unsigned thinking": wireStart + `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"x"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

` + wireTextBlock(1, " ") + wireFinish("end_turn"),
	}
}

// A notice rides turn 2, whose reply has one of the shapes above. Whatever the
// shape, the turn after must succeed, no request of the conversation may place a
// system message wrongly, the notice that rode a hollow reply is said again, and
// the number of assistant messages on the wire is the number the predicate
// calls sendable: the predicate is the encoder.
func TestAdvFixBHollowShapesFromARealStream(t *testing.T) {
	for name, reply := range hollowStreams() {
		t.Run(name, func(t *testing.T) {
			c := newConvo(t, []string{thinkingAnswer("sig-a"), reply, thinkingAnswer("sig-c"), thinkingAnswer("sig-d")}, nil)
			if err := c.reg.Register(echoTool("echo", "echo", nil)); err != nil {
				t.Fatal(err)
			}
			if err := c.turn("prompt one", "first", nil); err != nil {
				t.Fatal(err)
			}
			err := c.turn("prompt two", "second", nil)
			if advInvariantBroken(err) {
				t.Fatalf("model-visible-means-logged fired on the hollow turn: %v", err)
			}
			for turn, prompt := range []string{"prompt two", "prompt three"} {
				if err := c.turn(prompt, fmt.Sprintf("later %d", turn), nil); err != nil {
					t.Fatalf("turn after a %s reply: %v", name, err)
				}
			}
			reqs := c.requests()
			for i, req := range reqs {
				if p := advPlacementProblems(t, req); len(p) != 0 {
					t.Errorf("request %d: %v", i, p)
				}
			}
			last := reqs[len(reqs)-1]
			if got, want := advAssistantCount(t, last), advSendableAssistants(t, c.log); got != want {
				t.Errorf("the wire carries %d assistant messages, the log has %d sendable ones", got, want)
			}
			notices := systemMessages(t, last)
			if len(notices) != 2 || !strings.Contains(notices[0], "prompt two") || !strings.Contains(notices[1], "prompt three") {
				t.Errorf("notices on the last request = %q, want prompt two then prompt three, once each", notices)
			}
			msgs, derr := c.log.DeriveMessages()
			if derr != nil {
				t.Fatal(derr)
			}
			if err := session.AssertModelVisible(c.log, msgs); err != nil {
				t.Error(err)
			}
		})
	}
}

// A tool call with an empty input is sendable (the encoder writes {}), so the
// notice it answered stays, and the wire shows it.
func TestAdvFixBEmptyToolInputAnswersTheNotice(t *testing.T) {
	emptyCall := wireStart + wireToolCall(0, "toolu_e", "echo", "", true) + wireFinish("tool_use")
	c := newConvo(t, []string{thinkingAnswer("sig-a"), emptyCall, thinkingAnswer("sig-c")}, nil)
	if err := c.reg.Register(echoTool("echo", "echo", nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("prompt one", "first", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("prompt two", "second", nil); err != nil {
		t.Fatal(err)
	}
	reqs := c.requests()
	for i, req := range reqs {
		if p := advPlacementProblems(t, req); len(p) != 0 {
			t.Errorf("request %d: %v", i, p)
		}
	}
	last := reqs[len(reqs)-1]
	if got := systemMessages(t, last); len(got) != 1 || !strings.Contains(got[0], "prompt two") {
		t.Errorf("notices = %q, want the one that the tool call answered", got)
	}
	if !strings.Contains(advWireText(last.Messages[len(last.Messages)-2]), `"input":{}`) {
		t.Errorf("the empty call was not sent with input {}: %s", last.Messages[len(last.Messages)-2])
	}
}

// Two notice-bearing requests fail in a row, then one succeeds. A failed
// request's notice is dropped by the next user message; the wire never holds two
// notices, a notice before a user message, or one at the start.
func TestAdvFixBTwoNoticeBearingRequestsFailThenSucceed(t *testing.T) {
	c := newConvo(t, nil, nil)
	advServe(t, c, []string{thinkingAnswer("sig-a"), thinkingAnswer("sig-d")}, map[int]int{1: 400, 2: 400})
	if err := c.reg.Register(echoTool("echo", "echo", nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("prompt one", "first", nil); err != nil {
		t.Fatal(err)
	}
	for i, prompt := range []string{"prompt two", "prompt three"} {
		err := c.turn(prompt, fmt.Sprintf("failing %d", i), nil)
		if err == nil {
			t.Fatalf("turn %d should have failed", i)
		}
		if advInvariantBroken(err) {
			t.Fatalf("invariant fired instead of the fixture refusal: %v", err)
		}
	}
	if err := c.turn("prompt four", "succeeding", nil); err != nil {
		t.Fatal(err)
	}
	reqs := c.requests()
	if len(reqs) != 4 {
		t.Fatalf("%d requests, want 4", len(reqs))
	}
	for i, req := range reqs {
		if p := advPlacementProblems(t, req); len(p) != 0 {
			t.Errorf("request %d: %v", i, p)
		}
		if n := len(systemMessages(t, req)); n != 1 && i > 0 {
			t.Errorf("request %d carries %d notices, want exactly the one it minted", i, n)
		}
	}
	last := reqs[3]
	if got := systemMessages(t, last); len(got) != 1 || !strings.Contains(got[0], "prompt four") {
		t.Errorf("last request's notices = %q, want only prompt four: the two failed ones rode requests the model never answered", got)
	}
	if got := c.log.Wire().Delivered; !strings.Contains(got, "prompt four") {
		t.Errorf("Delivered = %q", got)
	}
}

// A notice recorded at the very end of the history (the process ended after the
// log took it and before the reply) and resumed from a saved file. The next
// Loop adds a user message after it, so the projection drops it and the Loop
// says it again, once; later turns with the same prompt say nothing.
func TestAdvFixBNoticeAtTheEndOfHistoryAtResume(t *testing.T) {
	c := newConvo(t, []string{thinkingAnswer("sig-a"), thinkingAnswer("sig-b"), thinkingAnswer("sig-c"), thinkingAnswer("sig-d")}, nil)
	if err := c.reg.Register(echoTool("echo", "echo", nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("prompt one", "first", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.log.Append(session.UserMessage, session.MessageData{Message: llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "dangling"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.log.Append(session.WireNotice, session.NoticeData{Text: "dangling notice", Prompt: "prompt two"}); err != nil {
		t.Fatal(err)
	}
	store := session.NewStore(t.TempDir())
	id, err := session.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(id, c.log); err != nil {
		t.Fatal(err)
	}
	for turn := 0; turn < 3; turn++ {
		restored, _, err := store.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		c.log = restored
		if err := c.turn("prompt two", fmt.Sprintf("resumed %d", turn), nil); err != nil {
			t.Fatalf("resumed turn %d: %v", turn, err)
		}
		if _, err := store.Save(id, c.log); err != nil {
			t.Fatal(err)
		}
	}
	reqs := c.requests()
	for i, req := range reqs {
		if p := advPlacementProblems(t, req); len(p) != 0 {
			t.Errorf("request %d: %v", i, p)
		}
	}
	first := systemMessages(t, reqs[1])
	if len(first) != 1 || !strings.Contains(first[0], "prompt two") {
		t.Errorf("first resumed request notices = %q, want prompt two said again", first)
	}
	minted := 0
	for _, e := range c.log.Events() {
		if e.Type == session.WireNotice {
			minted++
		}
	}
	if minted != 2 {
		t.Errorf("%d notices in the log, want the dangling one and one re-delivery", minted)
	}
}

// What the model holds, in turns: the latest system message if the conversation
// has one, otherwise the top-level system.
func advVisiblePrompt(t *testing.T, req wireRequest) string {
	t.Helper()
	if n := systemMessages(t, req); len(n) > 0 {
		return n[len(n)-1]
	}
	return string(req.System)
}

// A notice answered only by a signed-thinking reply is delivered by the log's
// reckoning. A later tool-set boundary strips the signed block, the reply is
// left empty, and StripStale drops the notice so the wire is valid. The log still
// says the prompt was delivered, so no Loop says it again. This asks what the
// model sees afterwards: it must hold prompt two on every request after the
// boundary, not prompt one for the rest of the conversation.
func TestAdvFixBStrippedNoticeIsNotLostForGood(t *testing.T) {
	signedOnly := wireStart + wireThinkingBlock(0, "t", "sig-b") + wireFinish("end_turn")
	c := newConvo(t, []string{thinkingAnswer("sig-a"), signedOnly, thinkingAnswer("sig-c"), thinkingAnswer("sig-d"), thinkingAnswer("sig-e")}, nil)
	if err := c.reg.Register(echoTool("echo", "echo", nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("prompt one", "first", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("prompt two", "second", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.reg.Register(echoTool("late", "added after the notice", nil)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := c.turn("prompt two", fmt.Sprintf("after the boundary %d", i), nil); err != nil {
			t.Fatalf("turn after the boundary: %v", err)
		}
	}
	reqs := c.requests()
	var seen []string
	for i, req := range reqs {
		if p := advPlacementProblems(t, req); len(p) != 0 {
			t.Errorf("request %d: %v", i, p)
		}
		v := advVisiblePrompt(t, req)
		switch {
		case strings.Contains(v, "prompt two"):
			seen = append(seen, "two")
		case strings.Contains(v, "prompt one"):
			seen = append(seen, "one")
		default:
			seen = append(seen, "?")
		}
	}
	t.Logf("prompt the model holds per request: %v; delivered per log: %q", seen, c.log.Wire().Delivered)
	if len(seen) != len(reqs) {
		t.Fatalf("classified %d of %d requests", len(seen), len(reqs))
	}
	for i := 3; i < len(reqs); i++ {
		if seen[i] != "two" {
			t.Errorf("request %d: the model holds prompt %s while every Loop asked for prompt two (log says delivered %q); per request: %v",
				i, seen[i], c.log.Wire().Delivered, seen)
		}
	}
}

// ---------------------------------------------------------------------------
// A file written by 6b0c915.

// A conversation resumed from a file whose carried frame still holds delivered
// and told. Saved and reloaded every turn, the prompt the frame did not say is
// delivered once, and a prompt the frame already said is never delivered.
func TestAdvFixBOldCarriedFrameCausesNoNoticeStorm(t *testing.T) {
	for _, tc := range []struct {
		name, prompt string
		want         int
	}{
		{"prompt the old notice had delivered", "prompt B", 1},
		{"prompt the frame holds", "prompt A", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			echo := echoTool("echo", "echo", nil)
			log := session.NewLog()
			for _, step := range []struct {
				typ  session.Type
				data any
			}{
				{session.TurnStart, nil},
				{session.WireFrame, map[string]any{
					"system": "prompt A", "why": "carried", "delivered": "prompt B", "told": []string{"echo"}, "through": 1,
					"tools": []llm.ToolSchema{echo.Schema},
				}},
				{session.UserMessage, session.MessageData{Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "kept user"}}}}},
				{session.AssistantMessage, session.MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: "kept answer"}}}}},
				{session.TurnEnd, nil},
			} {
				if _, err := log.Append(step.typ, step.data); err != nil {
					t.Fatal(err)
				}
			}
			c := newConvo(t, []string{thinkingAnswer("sig-a")}, log)
			if err := c.reg.Register(echo); err != nil {
				t.Fatal(err)
			}
			store := session.NewStore(t.TempDir())
			id, err := session.NewID()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Save(id, c.log); err != nil {
				t.Fatal(err)
			}
			for turn := 0; turn < 5; turn++ {
				restored, _, err := store.Load(id)
				if err != nil {
					t.Fatal(err)
				}
				c.log = restored
				if err := c.turn(tc.prompt, fmt.Sprintf("turn %d", turn), nil); err != nil {
					t.Fatalf("turn %d: %v", turn, err)
				}
				if _, err := store.Save(id, c.log); err != nil {
					t.Fatal(err)
				}
			}
			minted := 0
			for _, e := range c.log.Events() {
				if e.Type == session.WireNotice {
					minted++
				}
			}
			if minted != tc.want {
				t.Errorf("%d notices over 5 resumed turns, want %d", minted, tc.want)
			}
			for i, req := range c.requests() {
				if p := advPlacementProblems(t, req); len(p) != 0 {
					t.Errorf("request %d: %v", i, p)
				}
			}
		})
	}
}
