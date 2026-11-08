package session

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

func appendNotice(t *testing.T, log *Log, data NoticeData) {
	t.Helper()
	if _, err := log.Append(WireNotice, data); err != nil {
		t.Fatal(err)
	}
}

// A notice replays as a role:system message at the place it was recorded, on a
// warm projection and on one rebuilt from the events, and never moves.
func TestWireNoticeProjectsInPlace(t *testing.T) {
	log := NewLog()
	staleFixture(t, log, 2)
	appendNotice(t, log, NoticeData{Text: "now in go", Prompt: "p2"})
	staleRound(t, log, 2)

	check := func(name string, l *Log) {
		msgs, err := l.DeriveMessages()
		if err != nil {
			t.Fatal(err)
		}
		var roles []llm.Role
		for _, m := range msgs {
			roles = append(roles, m.Role)
		}
		want := []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleUser, llm.RoleAssistant, llm.RoleUser, llm.RoleSystem, llm.RoleAssistant, llm.RoleUser}
		if !reflect.DeepEqual(roles, want) {
			t.Fatalf("%s: roles = %v, want %v", name, roles, want)
		}
		if got, ok := msgs[5].Content[0].(llm.TextBlock); !ok || got.Text != "now in go" {
			t.Errorf("%s: notice content = %v", name, msgs[5].Content[0])
		}
	}
	check("warm", log)
	restored, err := RestoreLog(log.Events())
	if err != nil {
		t.Fatal(err)
	}
	check("restored", restored)
	if got := restored.Wire().Delivered; got != "p2" {
		t.Errorf("restored Delivered = %q, want p2", got)
	}
}

func TestWireNoticeWithoutTextIsRefused(t *testing.T) {
	if _, err := NewLog().Append(WireNotice, NoticeData{Prompt: "p"}); err == nil {
		t.Fatal("a notice with no text would project an empty message")
	}
}

// A frame that grew the tool set strips every signed block before it and none
// after; a frame that only opened strips nothing.
func TestWireFrameGrowthIsALeadingStrip(t *testing.T) {
	log := NewLog()
	if _, err := log.Append(WireFrame, FrameData{System: "s", Why: WhyOpened}); err != nil {
		t.Fatal(err)
	}
	staleFixture(t, log, 2)
	msgs, _ := log.DeriveMessages()
	if _, n, err := log.StripStaleReasoning(msgs); err != nil || n != 0 {
		t.Fatalf("an opened frame stripped %d blocks (err %v)", n, err)
	}
	if _, err := log.Append(WireFrame, FrameData{System: "s", Why: WhyToolsAdded, Added: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	staleRound(t, log, 2)
	msgs, _ = log.DeriveMessages()
	out, n, err := log.StripStaleReasoning(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("stripped %d blocks, want the 2 before the boundary", n)
	}
	got := signaturesOf(out)
	if len(got) != 1 || got[len(out)-2] != "sig-2" {
		t.Errorf("signatures kept = %v, want only sig-2", got)
	}
}

// A notice no assistant message answered before the next user message rode a
// request that failed. The provider refuses user, system, user
// (shared/prompt-caching.md, "Mid-conversation system messages"), so the
// projection drops it, on a warm projection and a rebuilt one, and the log
// counts what it said as never delivered so it is said again.
func TestUnansweredNoticeIsDroppedAndNotDelivered(t *testing.T) {
	build := func() *Log {
		log := NewLog()
		mustAppend(t, log, WireFrame, FrameData{System: "first", Why: WhyOpened})
		mustAppend(t, log, UserMessage, MessageData{Message: user("one")})
		mustAppend(t, log, AssistantMessage, MessageData{Message: assistant("a")})
		mustAppend(t, log, UserMessage, MessageData{Message: user("two")})
		appendNotice(t, log, NoticeData{Text: "now second", Prompt: "second", Offered: []string{"x"}})
		return log
	}
	log := build()
	if w := log.Wire(); w.Delivered != "second" || !reflect.DeepEqual(w.Told, []string{"x"}) {
		t.Fatalf("while the request is in flight the notice counts as delivered, wire = %+v", w)
	}
	mustAppend(t, log, UserMessage, MessageData{Message: user("three")})
	for name, l := range map[string]*Log{"warm": log, "restored": func() *Log {
		r, err := RestoreLog(log.Events())
		if err != nil {
			t.Fatal(err)
		}
		return r
	}()} {
		msgs, err := l.DeriveMessages()
		if err != nil {
			t.Fatal(err)
		}
		for i, m := range msgs {
			if m.Role == llm.RoleSystem {
				t.Errorf("%s: the unanswered notice is still projected at %d", name, i)
			}
		}
		if w := l.Wire(); w.Delivered != "first" || w.Told != nil {
			t.Errorf("%s: an unanswered notice counts as delivered, wire = %+v", name, w)
		}
		stale, err := staleMessages(l.snapshot())
		if err != nil || len(stale) != len(msgs) {
			t.Errorf("%s: staleMessages covers %d messages (err %v), the projection holds %d", name, len(stale), err, len(msgs))
		}
	}
}

// A notice an assistant message answered stays, and so does its state; an empty
// assistant message answers nothing.
func TestAnsweredNoticeStaysDelivered(t *testing.T) {
	log := NewLog()
	mustAppend(t, log, WireFrame, FrameData{System: "first", Why: WhyOpened})
	mustAppend(t, log, UserMessage, MessageData{Message: user("one")})
	appendNotice(t, log, NoticeData{Text: "now second", Prompt: "second"})
	mustAppend(t, log, AssistantMessage, MessageData{Message: llm.Message{Role: llm.RoleAssistant}})
	if got := log.Wire().Delivered; got != "second" {
		t.Fatalf("Delivered = %q before an answer", got)
	}
	mustAppend(t, log, AssistantMessage, MessageData{Message: assistant("a")})
	mustAppend(t, log, UserMessage, MessageData{Message: user("two")})
	if got := log.Wire().Delivered; got != "second" {
		t.Errorf("Delivered = %q after the notice was answered", got)
	}
	msgs, _ := log.DeriveMessages()
	if len(msgs) != 4 || msgs[1].Role != llm.RoleSystem {
		t.Errorf("projection = %d messages, want the answered notice kept at 1", len(msgs))
	}
}

// Retention drops the first turn, which holds the frame and the notices. The
// file restates the frame with what the notices had told the model, and a
// second trim of the reloaded file restates it again.
func TestRetentionTrimCarriesFrameAndNoticeState(t *testing.T) {
	store := NewStore(t.TempDir())
	store.setLimits(6000, 0)
	id := newID(t)
	log := NewLog()
	for i, text := range []string{"one", "two", "three", "four", "five"} {
		mustAppend(t, log, TurnStart, nil)
		switch i {
		case 0:
			mustAppend(t, log, WireFrame, FrameData{System: "the first prompt", Why: WhyOpened,
				Tools: []llm.ToolSchema{{Name: "echo", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}}})
		}
		mustAppend(t, log, UserMessage, MessageData{Message: user(text + strings.Repeat("y", 1500))})
		if i == 1 {
			appendNotice(t, log, NoticeData{Text: "now b", Prompt: "prompt b", Offered: []string{"echo"}})
		}
		mustAppend(t, log, AssistantMessage, MessageData{Message: assistant("done")})
		mustAppend(t, log, TurnEnd, nil)
	}
	report, err := store.Save(id, log)
	if err != nil {
		t.Fatal(err)
	}
	if report.TrimmedTurns < 2 {
		t.Fatalf("trimmed %d turns; the notice's turn must go too for this to say anything", report.TrimmedTurns)
	}
	check := func(l *Log) {
		t.Helper()
		w := l.Wire()
		if w.Frame == nil || w.Frame.System != "the first prompt" || len(w.Frame.Tools) != 1 {
			t.Fatalf("frame = %+v", w.Frame)
		}
		if w.Delivered != "prompt b" || !reflect.DeepEqual(w.Told, []string{"echo"}) {
			t.Errorf("delivered/told = %q %v, want the dropped notice's state", w.Delivered, w.Told)
		}
		if msgs, err := l.DeriveMessages(); err != nil || msgs[0].Role != llm.RoleUser {
			t.Errorf("history begins with %v (err %v), want a user message", msgs[0].Role, err)
		}
	}
	reloaded, _, err := NewStore(store.Dir()).Load(id)
	if err != nil {
		t.Fatal(err)
	}
	check(reloaded)
	again := NewStore(store.Dir())
	again.setLimits(4000, 0)
	reloaded, _, err = again.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	mustAppend(t, reloaded, TurnStart, nil)
	mustAppend(t, reloaded, UserMessage, MessageData{Message: user("six" + strings.Repeat("y", 1500))})
	mustAppend(t, reloaded, AssistantMessage, MessageData{Message: assistant("done")})
	mustAppend(t, reloaded, TurnEnd, nil)
	if _, err := again.Save(id, reloaded); err != nil {
		t.Fatal(err)
	}
	second, _, err := NewStore(store.Dir()).Load(id)
	if err != nil {
		t.Fatal(err)
	}
	check(second)
}
