package session

import (
	"reflect"
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
		if got := msgs[5].Content[0].(llm.TextBlock).Text; got != "now in go" {
			t.Errorf("%s: notice text = %q", name, got)
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
