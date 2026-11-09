package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/session"
)

// Adversarial tests for lane F (ft-9a5710b6 items 2 and 3). A test whose name
// starts TestAdvW17Break is a defect found on 112759d: it fails there.

// w17Script answers request i with a turn whose reasoning is signed "sig-i", so a
// test can tell which request produced a block it finds in a later one. Every
// answer is a plain reply: a request is one turn.
func w17Script(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = thinkingAnswer(fmt.Sprintf("sig-%d", i))
	}
	return out
}

// w17Producer is the request index a signature was produced by, -1 for a block
// that no request of this conversation produced.
func w17Producer(sig string) int {
	var n int
	if _, err := fmt.Sscanf(sig, "sig-%d", &n); err != nil {
		return -1
	}
	return n
}

// w17AssertPrefixesHold is the 400 the frame exists to prevent: every signed
// block in request j was produced by an earlier request whose top-level system
// and tools are byte-identical to request j's. A block no request produced (a
// pre-frame block, whose frame nobody recorded) must never be replayed.
func w17AssertPrefixesHold(t *testing.T, reqs []wireRequest) {
	t.Helper()
	for j, req := range reqs {
		for _, b := range signedBlocks(t, req) {
			p := w17Producer(b.signature)
			if p < 0 {
				t.Errorf("request %d replays %q, produced under a prefix nobody recorded", j, b.signature)
				continue
			}
			if p >= j {
				t.Errorf("request %d carries %q from request %d, which is not earlier", j, b.signature, p)
				continue
			}
			if !bytes.Equal(reqs[p].System, req.System) {
				t.Errorf("request %d replays %q under system %s, produced under %s", j, b.signature, req.System, reqs[p].System)
			}
			if !bytes.Equal(reqs[p].Tools, req.Tools) {
				t.Errorf("request %d replays %q under tools %s, produced under %s", j, b.signature, req.Tools, reqs[p].Tools)
			}
		}
	}
}

// w17PreFrameLog is what a build from before frames were recorded left: an
// answered turn with signed reasoning and no frame, optionally ending mid tool
// round.
func w17PreFrameLog(t *testing.T, midRound bool) func(*session.Log) {
	t.Helper()
	return func(log *session.Log) {
		t.Helper()
		app := func(typ session.Type, p any) {
			t.Helper()
			if _, err := log.Append(typ, p); err != nil {
				t.Fatal(err)
			}
		}
		app(session.TurnStart, nil)
		app(session.UserMessage, session.MessageData{Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "old task"}}}})
		if midRound {
			app(session.AssistantMessage, session.MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				llm.ReasoningBlock{Text: "plan", Signature: "pre-mid"},
				llm.ToolCallBlock{ID: "toolu_pre", Name: "echo", Arguments: json.RawMessage(`{"v":1}`)}}}})
			app(session.ToolResult, session.ToolResultData{ToolCallID: "toolu_pre", Text: "done"})
			return
		}
		app(session.AssistantMessage, session.MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ReasoningBlock{Text: "old", Signature: "pre-1"}, llm.TextBlock{Text: "old answer"}}}})
		app(session.TurnEnd, nil)
	}
}

// Attack 1a/1b/1c/1d. One conversation over a pre-frame log, driven by Loops
// whose prompts change and whose tool sets grow, shrink (ChatOnly) and grow
// again, with a dynamic registration between turns. At every request every
// replayed block shares its producer's system and tools.
func TestAdvW17NoSignedBlockIsReplayedUnderADifferentPrefix(t *testing.T) {
	for _, midRound := range []bool{false, true} {
		t.Run(fmt.Sprintf("midRound=%v", midRound), func(t *testing.T) {
			log := session.NewLog()
			w17PreFrameLog(t, midRound)(log)
			c := newConvo(t, w17Script(12), log)
			advRegister(t, c.reg, "echo")
			type step struct {
				system   string
				register string
				tune     func(*Config, *bus.Bus)
			}
			steps := []step{
				{system: "p1"},
				{system: "p1"},
				{system: "p2"},
				{system: "p2", register: "extra"},
				{system: "p2", tune: func(cfg *Config, _ *bus.Bus) { cfg.ChatOnly = true }},
				{system: "p3", tune: func(cfg *Config, _ *bus.Bus) { cfg.ChatOnly = true }},
				{system: "p3"},
				{system: "p3", register: "more"},
				{system: "p4", tune: func(cfg *Config, _ *bus.Bus) { cfg.ReadOnly = true }},
				{system: "p4"},
			}
			for i, s := range steps {
				if s.register != "" {
					advRegister(t, c.reg, s.register)
				}
				if err := c.turn(s.system, fmt.Sprintf("task %d", i), s.tune); err != nil {
					t.Fatalf("step %d: %v", i, err)
				}
			}
			reqs := c.requests()
			if len(reqs) != len(steps) {
				t.Fatalf("%d requests for %d turns", len(reqs), len(steps))
			}
			w17AssertPrefixesHold(t, reqs)
			// The conversation must still keep reasoning where it can: the last
			// request replays something, or the strip is deleting everything.
			if got := signedBlocks(t, reqs[len(reqs)-1]); len(got) == 0 {
				t.Errorf("the final request replays no reasoning at all; the frame is stripping what it need not")
			}
		})
	}
}

// Attack 1b. A frame that opens mid tool round: the log ends in a tool result
// when the turn starts. The block of that round is gone from the request that
// follows, because the frame opens as a boundary once the new user message ends
// the round, and it is never replayed behind a prefix nobody recorded.
func TestAdvW17MidRoundPreFrameLogNeverReplaysItsBlock(t *testing.T) {
	log := session.NewLog()
	w17PreFrameLog(t, true)(log)
	c := newConvo(t, w17Script(4), log)
	advRegister(t, c.reg, "echo")
	if err := c.turn("p", "go on", nil); err != nil {
		t.Fatal(err)
	}
	reqs := c.requests()
	if got := signedBlocks(t, reqs[0]); len(got) != 0 {
		t.Fatalf("first request replays %v over a log whose frame was never recorded", got)
	}
	var frames []session.FrameData
	for _, e := range log.Events() {
		if e.Type == session.WireFrame {
			var d session.FrameData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatal(err)
			}
			frames = append(frames, d)
		}
	}
	if len(frames) != 1 || frames[0].Why != session.WhyToolsAdded {
		t.Fatalf("frames = %+v, want one tools_added", frames)
	}
}

// Attack 1c. A resume with a changed prompt over a pre-frame log: the frame
// records the log's last recorded prompt (the one the old build sent), and the
// Loop's new prompt rides as a notice, so top-level system is what the old build
// sent and nothing signed under any other is replayed.
func TestAdvW17ResumeWithChangedPromptOverPreFrameLog(t *testing.T) {
	log := session.NewLog()
	if _, err := log.Append(session.SystemPrompt, session.SystemPromptData{Text: "old prompt"}); err != nil {
		t.Fatal(err)
	}
	w17PreFrameLog(t, false)(log)
	c := newConvo(t, w17Script(4), log)
	advRegister(t, c.reg, "echo")
	loop := c.loopOver("new prompt")
	user := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "resume"}}}
	draft := loop.Draft(user)
	if _, err := loop.Run(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("new prompt", "again", nil); err != nil {
		t.Fatal(err)
	}
	reqs := c.requests()
	w17AssertPrefixesHold(t, reqs)
	if !strings.Contains(string(reqs[0].System), "old prompt") {
		t.Errorf("top-level system = %s, want the prompt the old build sent", reqs[0].System)
	}
	if draft.System != "old prompt" {
		t.Errorf("draft system = %q, the request carries %s", draft.System, reqs[0].System)
	}
}

// Attack 4. Draft against the request actually sent: system and tools, for a
// fresh conversation, a framed one, a pre-frame one, and a ChatOnly Loop on a
// framed conversation.
func TestAdvW17DraftMatchesTheRequestSent(t *testing.T) {
	check := func(t *testing.T, draft llm.Request, sent wireRequest) {
		t.Helper()
		var sys string
		if len(sent.System) > 0 {
			// system is a string or a list of blocks; compare the text it carries.
			if err := json.Unmarshal(sent.System, &sys); err != nil {
				sys = string(sent.System)
			}
		}
		if draft.System != sys && !strings.Contains(string(sent.System), draft.System) {
			t.Errorf("draft system %q, sent %s", draft.System, sent.System)
		}
		var names []string
		for _, s := range draft.Tools {
			names = append(names, s.Name)
		}
		if want := wireToolNames(t, sent.Tools); !slices.Equal(names, want) && (len(names) != 0 || len(want) != 0) {
			t.Errorf("draft tools %v, sent %v", names, want)
		}
	}
	user := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "x"}}}

	t.Run("fresh", func(t *testing.T) {
		c := newConvo(t, w17Script(2), nil)
		advRegister(t, c.reg, "echo", "b")
		loop := c.loopOver("p")
		draft := loop.Draft(user)
		if _, err := loop.Run(context.Background(), user); err != nil {
			t.Fatal(err)
		}
		check(t, draft, c.requests()[0])
	})
	t.Run("chatonly over a framed conversation", func(t *testing.T) {
		c := newConvo(t, w17Script(3), nil)
		advRegister(t, c.reg, "echo")
		if err := c.turn("p", "one", nil); err != nil {
			t.Fatal(err)
		}
		b := bus.New()
		loop, err := NewLoop(Config{
			Provider: anthropic.New(c.url, adaptertest.Secret("sk-FIXTURE-not-a-real-key")),
			Model:    "claude-opus-5-5", SystemPrompt: "p", MaxSteps: 4, MaxTokens: 1024, ChatOnly: true,
		}, b, c.log, c.reg)
		if err != nil {
			t.Fatal(err)
		}
		draft := loop.Draft(user)
		if _, err := loop.Run(context.Background(), user); err != nil {
			t.Fatal(err)
		}
		check(t, draft, c.requests()[1])
	})
	t.Run("pre-frame", func(t *testing.T) {
		log := session.NewLog()
		w17PreFrameLog(t, false)(log)
		c := newConvo(t, w17Script(2), log)
		advRegister(t, c.reg, "echo", "b")
		loop := c.loopOver("p")
		draft := loop.Draft(user)
		if _, err := loop.Run(context.Background(), user); err != nil {
			t.Fatal(err)
		}
		check(t, draft, c.requests()[0])
	})
}

// Attack 4, report only. The fields of Draft's request that the sent request
// differs on, other than effort (fixed separately) and messages.
func TestAdvW17DraftFieldsAgainstTheWire(t *testing.T) {
	c := newConvo(t, w17Script(2), nil)
	advRegister(t, c.reg, "echo")
	loop := c.loopOver("p")
	user := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "x"}}}
	draft := loop.Draft(user)
	if _, err := loop.Run(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(c.raw[0], &body); err != nil {
		t.Fatal(err)
	}
	var model string
	var maxTokens int
	_ = json.Unmarshal(body["model"], &model)
	_ = json.Unmarshal(body["max_tokens"], &maxTokens)
	if draft.Model != model || draft.MaxTokens != maxTokens {
		t.Errorf("draft model/max_tokens = %q/%d, wire %q/%d", draft.Model, draft.MaxTokens, model, maxTokens)
	}
}

// BREAK candidate (size only). Draft sizes from the frame's System, which is the
// scrubbed text the log recorded (Append scrubs), but where no frame exists yet
// planFrame returns the frame to record with the unscrubbed prompt. The request
// sent carries the scrubbed one, so the draft sizes a request nobody sends,
// which is what item 2 set out to prevent.
func TestAdvW17BreakDraftSystemIsTheScrubbedOneTheRequestCarries(t *testing.T) {
	secret := "tok" + "-" + "ABCDEFGHIJKLMNOPQRSTUV"
	log := session.NewLog()
	log.SetScrubber(func(s string) string { return strings.ReplaceAll(s, secret, "[redacted]") })
	c := newConvo(t, w17Script(2), log)
	advRegister(t, c.reg, "echo")
	loop := c.loopOver("use key " + secret + " when asked")
	user := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "x"}}}
	draft := loop.Draft(user)
	if _, err := loop.Run(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	sent := string(c.requests()[0].System)
	if strings.Contains(sent, secret) {
		t.Fatalf("the secret reached the wire: %s", sent)
	}
	if strings.Contains(draft.System, secret) {
		t.Fatalf("draft system %q carries the unscrubbed prompt; the request carries %s", draft.System, sent)
	}
}

// Attack 5. Draft called while a turn runs, as the fan-out does for the next
// child while an earlier one is mid-turn. Run under -race.
func TestAdvW17DraftWhileATurnRuns(t *testing.T) {
	c := newConvo(t, w17Script(40), nil)
	advRegister(t, c.reg, "echo")
	loop := c.loopOver("p")
	user := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "x"}}}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = loop.Draft(user)
				}
			}
		}()
	}
	for i := 0; i < 8; i++ {
		if i == 3 {
			advRegister(t, c.reg, "grown")
		}
		if _, err := loop.Run(context.Background(), user); err != nil {
			close(stop)
			wg.Wait()
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	w17AssertPrefixesHold(t, c.requests())
}
