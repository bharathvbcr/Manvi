package session

import (
	"encoding/json"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

func roleList(msgs []llm.Message) []llm.Role {
	out := make([]llm.Role, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role
	}
	return out
}

func hollowAssistants() map[string]llm.Message {
	return map[string]llm.Message{
		"whitespace text":   {Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: " \n"}}},
		"unsigned thinking": {Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ReasoningBlock{Text: "cut off"}}},
	}
}

// A reply with nothing the encoder can send does not answer a notice: the wire
// drops it, so the notice would sit before the next user message
// (shared/prompt-caching.md, "Mid-conversation system messages"). The next
// user-role message drops the notice, on a warm projection and on a rebuilt
// one, and the log counts what it said as never delivered.
func TestAdvHollowReplyDoesNotAnswerANotice(t *testing.T) {
	for name, hollow := range hollowAssistants() {
		t.Run(name, func(t *testing.T) {
			log := NewLog()
			mustAppend(t, log, WireFrame, FrameData{System: "first", Why: WhyOpened})
			mustAppend(t, log, UserMessage, MessageData{Message: user("one")})
			appendNotice(t, log, NoticeData{Text: "now second", Prompt: "second"})
			mustAppend(t, log, AssistantMessage, MessageData{Message: hollow})
			if got := log.Wire().Delivered; got != "second" {
				t.Fatalf("Delivered = %q while the notice still rides the latest request", got)
			}
			mustAppend(t, log, UserMessage, MessageData{Message: user("two")})
			restored, err := RestoreLog(log.Events())
			if err != nil {
				t.Fatal(err)
			}
			for label, l := range map[string]*Log{"warm": log, "restored": restored} {
				msgs, err := l.DeriveMessages()
				if err != nil {
					t.Fatal(err)
				}
				for _, m := range msgs {
					if m.Role == llm.RoleSystem {
						t.Errorf("%s: roles %v still hold the unanswered notice", label, roleList(msgs))
					}
				}
				if got := l.Wire().Delivered; got != "first" {
					t.Errorf("%s: Delivered = %q, want first: nothing answered the notice", label, got)
				}
			}
		})
	}
}

// The projection and staleSpans must agree on which messages exist, hollow
// replies included, or StripStale mistakes a log it understands for a misaligned
// one and strips everything.
func TestAdvStaleSpansAlignWithAProjectionHoldingAHollowReply(t *testing.T) {
	log := NewLog()
	staleFixture(t, log, 2)
	appendNotice(t, log, NoticeData{Text: "n", Prompt: "p2"})
	mustAppend(t, log, AssistantMessage, MessageData{Message: hollowAssistants()["unsigned thinking"]})
	mustAppend(t, log, UserMessage, MessageData{Message: user("again")})
	msgs, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	stale, err := staleMessages(log.Events())
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != len(msgs) {
		t.Fatalf("staleMessages sees %d messages, the projection %d: %v", len(stale), len(msgs), roleList(msgs))
	}
}

// Second route: the reply that answered the notice held only signed thinking, and
// a tool-set boundary later strips it, leaving nothing to send. The request
// would carry user, system, user. StripStale is where that is known, so the
// notice goes with it: the answering turn is gone, as in the unanswered case.
func TestAdvStrippedAnswerLeavesNoUnansweredNotice(t *testing.T) {
	log := NewLog()
	mustAppend(t, log, WireFrame, FrameData{System: "first", Why: WhyOpened})
	mustAppend(t, log, UserMessage, MessageData{Message: user("one")})
	appendNotice(t, log, NoticeData{Text: "now second", Prompt: "second"})
	mustAppend(t, log, AssistantMessage, MessageData{Message: llm.Message{Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{llm.ReasoningBlock{Text: "t", Signature: "sig"}}}})
	mustAppend(t, log, UserMessage, MessageData{Message: user("two")})
	mustAppend(t, log, AssistantMessage, MessageData{Message: assistant("ok")})
	mustAppend(t, log, WireFrame, FrameData{System: "first", Why: WhyToolsAdded, Added: []string{"x"}})
	mustAppend(t, log, UserMessage, MessageData{Message: user("three")})
	msgs, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	out, drops, err := log.StripStale(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(drops) == 0 {
		t.Fatal("the boundary stripped nothing; the fixture says nothing")
	}
	for i, m := range out {
		if m.Role == llm.RoleSystem && (i+1 >= len(out) || out[i+1].Role != llm.RoleAssistant || len(out[i+1].Content) == 0) {
			t.Errorf("roles %v: the notice at %d is not followed by a sendable assistant turn", roleList(out), i)
		}
	}
}

// Growth of the stale set by appended turns is one fact, "everything from the
// edit on", and is recorded once; recording the whole index list on every step
// made the log quadratic in the turns after a compaction.
func TestAdvHistoryEditDropRecordsAreBoundedAfterACompaction(t *testing.T) {
	log := NewLog()
	staleFixture(t, log, 3)
	compact(t, log, "c0", false)
	const steps = 40
	for i := 0; i < steps; i++ {
		staleRound(t, log, 10+i)
		msgs, err := log.DeriveMessages()
		if err != nil {
			t.Fatal(err)
		}
		_, drops, err := log.StripStale(msgs)
		if err != nil {
			t.Fatal(err)
		}
		if err := log.RecordStale(drops, "anthropic", "claude-opus-5-5"); err != nil {
			t.Fatal(err)
		}
	}
	var records, listed int
	for _, e := range log.Events() {
		if e.Type != ProvenanceDropped {
			continue
		}
		records++
		var d DropData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatal(err)
		}
		listed += len(d.Drops)
	}
	if records > 3 {
		t.Errorf("%d drop records after %d steps (%d indexes listed in all); want a bounded number", records, steps, listed)
	}
}

// Only growth past the recorded end is skipped. A later compaction that moves
// the first stale position earlier is a different fact and is recorded.
func TestAdvAMovedHistoryEditBoundaryIsStillRecorded(t *testing.T) {
	tail := map[int]bool{5: true, 7: true}
	cases := []struct {
		reason            string
		recorded, current []int
		want              bool
	}{
		{DropReasonHistoryEdit, []int{3, 5}, []int{3, 5, 7}, true},
		{DropReasonHistoryEdit, []int{3, 5}, []int{3, 5}, true},
		{DropReasonHistoryEdit, []int{3, 5}, []int{3, 5, 9}, false}, // 9 is in the front run: a second boundary
		{DropReasonHistoryEdit, []int{3, 5}, []int{1, 3, 5}, false},
		{DropReasonHistoryEdit, []int{3, 5}, []int{3, 6}, false},
		{DropReasonHistoryEdit, nil, []int{3}, false},
		{DropReasonRetentionTrim, []int{1}, []int{1, 3}, false},
		{DropReasonRetentionTrim, []int{1, 3}, []int{1, 3}, true},
	}
	for _, c := range cases {
		if got := alreadyRecorded(c.reason, c.recorded, c.current, tail); got != c.want {
			t.Errorf("%s recorded %v current %v: got %v, want %v", c.reason, c.recorded, c.current, got, c.want)
		}
	}
}
