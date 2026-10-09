package main

import (
	"context"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/ui"
)

// commandHost is a host with one idle session, S1, whose transcript is
// captured, so what a slash command reports there can be asserted.
func commandHost(t *testing.T) (*harnessHost, *capturingSink) {
	t.Helper()
	sink := &capturingSink{}
	h := &harnessHost{
		reg:       newTestRegistry(t),
		sessions:  map[string]*tuiSession{"S1": {id: "S1"}},
		busyTurns: func(string) []string { return nil },
		sinks:     func(string) ui.Sink { return sink },
	}
	return h, sink
}

// kinds is the sequence of event kinds a transcript received.
func kinds(events []ui.Event) []ui.Kind {
	out := make([]ui.Kind, 0, len(events))
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

func sameKinds(got, want []ui.Kind) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestASlashCommandReportsIntoTheTranscript pins what Command puts in front of
// the operator for each way a command can end. A command's failure is reported
// into the transcript and never returned: a failed command is not a reason to
// end the session.
func TestASlashCommandReportsIntoTheTranscript(t *testing.T) {
	cases := []struct {
		name, cmd, args string
		busy            []string
		want            []ui.Kind
		// text is a fragment the last event must carry.
		text string
	}{
		{
			name: "unreadable arguments", cmd: "flags", args: `set llm.local.stop "unterminated`,
			want: []ui.Kind{ui.KindToolStart, ui.KindError}, text: "unterminated",
		},
		{
			name: "unknown command", cmd: "nope",
			want: []ui.Kind{ui.KindToolStart, ui.KindError}, text: `unknown command "nope"`,
		},
		{
			name: "a command handled by the face", cmd: "quit",
			want: []ui.Kind{ui.KindToolStart, ui.KindToolResult}, text: "(no output)",
		},
		{
			name: "reading the flag table", cmd: "flags",
			want: []ui.Kind{ui.KindToolStart, ui.KindToolResult, ui.KindNotice}, text: "/settings opens",
		},
		{
			name: "pull while another session is busy", cmd: "pull", busy: []string{"S2"},
			want: []ui.Kind{ui.KindToolStart, ui.KindError}, text: "/pull needs every session idle; active sessions: S2",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, sink := commandHost(t)
			if c.busy != nil {
				h.busyTurns = func(string) []string { return c.busy }
			}
			if err := h.Command(context.Background(), "S1", c.cmd, c.args); err != nil {
				t.Fatalf("Command returned %v; a command's own failure belongs in the transcript", err)
			}
			if got := kinds(sink.events); !sameKinds(got, c.want) {
				t.Fatalf("transcript kinds = %v, want %v (%+v)", got, c.want, sink.events)
			}
			if start := sink.events[0]; start.Tool != "/"+c.cmd || start.Detail != c.args {
				t.Errorf("tool start = %q %q, want %q %q", start.Tool, start.Detail, "/"+c.cmd, c.args)
			}
			if last := sink.events[len(sink.events)-1]; !strings.Contains(last.Text, c.text) {
				t.Errorf("last event text = %q, want it to contain %q", last.Text, c.text)
			}
		})
	}
}

// TestASlashFlagsSetAppliesTheChange: a set that moved a flag hands the key to
// the host, which tells every session what the move reaches, and does not
// offer the /settings pointer that belongs to reading the table.
func TestASlashFlagsSetAppliesTheChange(t *testing.T) {
	h, sink := commandHost(t)
	if err := h.Command(context.Background(), "S1", "flags", "set "+flags.LLMLocalTemperature+" 0.2"); err != nil {
		t.Fatal(err)
	}
	if got, want := kinds(sink.events), []ui.Kind{ui.KindToolStart, ui.KindToolResult, ui.KindNotice}; !sameKinds(got, want) {
		t.Fatalf("transcript kinds = %v, want %v (%+v)", got, want, sink.events)
	}
	if notice := sink.events[2].Text; strings.Contains(notice, "/settings opens") {
		t.Errorf("a set was followed by the read-only pointer rather than the change notice: %q", notice)
	}
	if v, _, err := h.reg.String(flags.LLMLocalTemperature); err != nil || v != "0.2" {
		t.Errorf("%s = %q, %v after the set", flags.LLMLocalTemperature, v, err)
	}
}

// TestASlashCommandForAnUnknownSessionIsAnError: the one failure Command
// returns rather than reports, because there is no transcript to report it in.
func TestASlashCommandForAnUnknownSessionIsAnError(t *testing.T) {
	h, sink := commandHost(t)
	if err := h.Command(context.Background(), "S9", "quit", ""); err == nil {
		t.Fatal("a command for a session that does not exist was accepted")
	}
	if len(sink.events) != 0 {
		t.Errorf("events were emitted for a session that does not exist: %+v", sink.events)
	}
}

// TestATurnReportsItsSpendAndOutcomeIntoTheTranscript pins what Submit puts in
// front of the operator after a turn: one usage line summing every step, then
// one line per outcome notice — here, the bounce the end-of-turn check made
// and the output cap the first answer hit.
func TestATurnReportsItsSpendAndOutcomeIntoTheTranscript(t *testing.T) {
	h := tuiTurnHost(t, truncatedThenDone(), nil)
	sink := &capturingSink{}
	h.sinks = func(string) ui.Sink { return sink }
	if err := h.Submit(context.Background(), "S1", "answer me"); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if got, want := kinds(sink.events), []ui.Kind{ui.KindUsage, ui.KindReport, ui.KindReport}; !sameKinds(got, want) {
		t.Fatalf("transcript kinds = %v, want %v (%+v)", got, want, sink.events)
	}
	if usage := sink.events[0]; usage.InputTokens != 300 || usage.OutputTokens != 30 {
		t.Errorf("usage = %d in / %d out, want both steps summed: 300 / 30", usage.InputTokens, usage.OutputTokens)
	}
	for i, fragment := range []string{"sent this turn back 1 time", "output cap"} {
		if text := sink.events[i+1].Text; !strings.Contains(text, fragment) {
			t.Errorf("report %d = %q, want it to mention %q", i, text, fragment)
		}
	}
}
