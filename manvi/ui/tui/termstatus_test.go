package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/ui"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
	"github.com/bharathvbcr/Manvi/manvi/ui/term"
)

// tabMark is the ribbon a title leads with, written out here rather than read
// from the logo package: this is the string a terminal is sent, and a change
// to it should have to be made twice.
const tabMark = "▞▚▞▚"

// newTabApp is the test app on a terminal that draws block characters, which
// is where the title leads with the ribbon.
func newTabApp() *App {
	a, _ := newTestApp()
	a.Unicode = true
	return a
}

// wantTab checks the tab against the mark followed by rest.
func wantTab(t *testing.T, a *App, rest string, progress term.Progress) {
	t.Helper()
	title := tabMark + " " + rest
	got := a.TermStatus()
	if got.Title != title || got.Progress != progress {
		t.Fatalf("the tab says %q with progress %d, want %q with progress %d", got.Title, got.Progress, title, progress)
	}
}

// The mark leads every title and never changes, whatever the agent is doing:
// it stands where another tool would put a spinner, and a title that is the
// same string for the whole of a phase is what keeps it to one write. On a
// terminal that is not drawn with block characters it is the letter.
func TestTabLeadsWithTheMarkAndTheMarkNeverMoves(t *testing.T) {
	a := newTabApp()
	a.Dispatch(ActionTurnStarted{SessionID: "S1"})
	for tick := range 40 {
		a.Dispatch(ActionTick{})
		if got := a.TermStatus().Title; got != "▞▚▞▚ working · session one" {
			t.Fatalf("tick %d: the title is %q", tick, got)
		}
	}
	a.Dispatch(ActionTurnEnded{SessionID: "S1"})
	if got := a.TermStatus().Title; !strings.HasPrefix(got, "▞▚▞▚ ") || strings.Contains(got, "manvi") {
		t.Fatalf("the idle title is %q: it should lead with the mark and not spell the name out after it", got)
	}

	plain, _ := newTestApp()
	if plain.Unicode {
		t.Fatal("the plain test app is expected to be without block characters")
	}
	plain.Dispatch(ActionTurnStarted{SessionID: "S1"})
	if got := plain.TermStatus().Title; got != "M working · session one" {
		t.Fatalf("without block characters the title is %q", got)
	}
	for _, v := range plain.Views() {
		plain.RemoveSession(v.ID)
	}
	if got := plain.TermStatus().Title; got != "M manvi" {
		t.Fatalf("without block characters and with no session the title is %q", got)
	}
}

func event(a *App, session string, ev ui.Event) {
	a.Dispatch(ActionEvent{SessionID: session, Event: ev})
}

// One session, through everything a turn does. The tab says what the agent is
// doing in plain words with the state first, never a percentage, and goes back
// to the session's own name once the operator has seen how the turn ended.
func TestTabFollowsATurn(t *testing.T) {
	a := newTabApp()
	wantTab(t, a, "session one", term.ProgressNone)

	a.Dispatch(ActionTurnStarted{SessionID: "S1"})
	wantTab(t, a, "working · session one", term.ProgressBusy)

	event(a, "S1", ui.Event{Kind: ui.KindReasoning, Text: "considering"})
	wantTab(t, a, "thinking · session one", term.ProgressBusy)

	// The tool's name, and nothing of what it was called with: its arguments
	// are where a path or a command line would be, and this is a tab.
	event(a, "S1", ui.Event{
		Kind: ui.KindToolStart, Tool: "run_command",
		Text: "rm -rf /srv/secret", Path: "/srv/secret", Arguments: []byte(`{"cmd":"rm -rf /srv/secret"}`),
	})
	wantTab(t, a, "running run_command · session one", term.ProgressBusy)
	if strings.Contains(a.TermStatus().Title, "secret") {
		t.Fatal("a tool's arguments reached the tab")
	}

	event(a, "S1", ui.Event{Kind: ui.KindToolResult, Tool: "run_command", Text: "ok"})
	wantTab(t, a, "working · session one", term.ProgressBusy)

	event(a, "S1", ui.Event{Kind: ui.KindText, Text: "Here is what I found."})
	wantTab(t, a, "writing · session one", term.ProgressBusy)

	a.Dispatch(ActionTurnEnded{SessionID: "S1"})
	wantTab(t, a, "done · session one", term.ProgressNone)

	// The operator is back: how it ended is no longer news.
	a.Dispatch(ActionRune{Runes: []rune{'h'}})
	wantTab(t, a, "session one", term.ProgressNone)
}

// A failed turn holds the error state until someone has seen it, and no
// longer. Both ends are tested: it must not clear on its own, and it must not
// outlive the keypress or the next turn.
func TestFailureStaysUntilSeenAndThenGoes(t *testing.T) {
	a := newTabApp()
	a.Dispatch(ActionTurnStarted{SessionID: "S1"})
	a.Dispatch(ActionTurnEnded{SessionID: "S1", Err: errors.New("provider refused")})
	wantTab(t, a, "failed · session one", term.ProgressError)

	for range 50 {
		a.Dispatch(ActionTick{})
	}
	wantTab(t, a, "failed · session one", term.ProgressError)

	a.Dispatch(ActionKey{Binding: "down"})
	wantTab(t, a, "session one", term.ProgressNone)

	// And a new turn clears it without any input at all.
	a.Dispatch(ActionTurnStarted{SessionID: "S1"})
	a.Dispatch(ActionTurnEnded{SessionID: "S1", Err: errors.New("again")})
	wantTab(t, a, "failed · session one", term.ProgressError)
	a.Dispatch(ActionTurnStarted{SessionID: "S1"})
	wantTab(t, a, "working · session one", term.ProgressBusy)
}

// An approval is the turn waiting on the operator, which is the most useful
// thing a tab can say to someone looking elsewhere. It shows as paused, and
// stops showing when the card is answered.
func TestApprovalShowsAsPausedUntilAnswered(t *testing.T) {
	a := newTabApp()
	a.Dispatch(ActionTurnStarted{SessionID: "S1"})
	reply := make(chan ui.Decision, 1)
	a.Dispatch(ActionApprovalRequest{SessionID: "S1", Request: blockedRequest(true), Reply: reply})
	wantTab(t, a, "needs approval · session one", term.ProgressPaused)

	v := a.Current()
	if v.Approval() == nil {
		t.Fatal("the request raised no card")
	}
	v.approval = nil
	wantTab(t, a, "working · session one", term.ProgressBusy)
}

// A running turn that has produced nothing for thirty seconds is stalled. The
// tab says so without a count of seconds, which would be a new title every
// second, and goes back to the phase as soon as something arrives.
func TestStallShowsOnceAndClearsOnActivity(t *testing.T) {
	a := newTabApp()
	a.Dispatch(ActionTurnStarted{SessionID: "S1"})
	event(a, "S1", ui.Event{Kind: ui.KindReasoning, Text: "…"})
	started := time.Now()

	a.now = func() time.Time { return started.Add(29 * time.Second) }
	wantTab(t, a, "thinking · session one", term.ProgressBusy)

	a.now = func() time.Time { return started.Add(31 * time.Second) }
	wantTab(t, a, "no progress · session one", term.ProgressPaused)
	a.now = func() time.Time { return started.Add(95 * time.Second) }
	wantTab(t, a, "no progress · session one", term.ProgressPaused)

	a.now = time.Now
	event(a, "S1", ui.Event{Kind: ui.KindText, Text: "back"})
	wantTab(t, a, "writing · session one", term.ProgressBusy)
}

// A stall is no progress since the turn started, not since the session last
// said anything. A turn begun on a session that had been idle for a minute
// must open as working: before the turn's start counted as activity it opened
// as "no progress" with the indicator paused, and stayed that way until the
// first event arrived. The working row measures the same thing from the same
// field, so it is checked here too.
func TestATurnOnALongIdleSessionDoesNotOpenStalled(t *testing.T) {
	a := newTabApp()
	a.Dispatch(ActionTurnStarted{SessionID: "S1"})
	event(a, "S1", ui.Event{Kind: ui.KindText, Text: "first answer"})
	a.Dispatch(ActionTurnEnded{SessionID: "S1"})
	a.Dispatch(ActionKey{Binding: "down"})

	v := a.Current()
	v.lastActivity = time.Now().Add(-time.Minute)

	a.Dispatch(ActionTurnStarted{SessionID: "S1"})
	wantTab(t, a, "working · session one", term.ProgressBusy)

	b := render.NewBuffer(100, 30)
	a.Draw(b)
	if v.Status.StallSecs != 0 {
		t.Fatalf("the working row counts %d seconds of no progress on a turn that has just started", v.Status.StallSecs)
	}
}

// The title has no spinner in it, so it does not change while the state does
// not. Fifty ticks of one phase are one answer, which the terminal layer turns
// into one write.
func TestTabDoesNotChangeWhileTheStateDoesNot(t *testing.T) {
	a := newTabApp()
	a.Dispatch(ActionTurnStarted{SessionID: "S1"})
	event(a, "S1", ui.Event{Kind: ui.KindReasoning, Text: "…"})
	first := a.TermStatus()
	for tick := range 50 {
		a.Dispatch(ActionTick{})
		if got := a.TermStatus(); got != first {
			t.Fatalf("tick %d: the tab changed from %+v to %+v with nothing happening", tick, first, got)
		}
	}
}

// One tab speaks for every session. It speaks for the one with the most
// urgent thing to say, the session on screen winning a tie, and counts the
// others that are not idle.
func TestOneTabSpeaksForTheMostUrgentSession(t *testing.T) {
	a := newTabApp()
	a.AddSession("S2", "session two") // now on screen
	if a.Current().ID != "S2" {
		t.Fatalf("the session on screen is %s, want S2", a.Current().ID)
	}
	wantTab(t, a, "session two", term.ProgressNone)

	// Only the background session is running: the tab is about it.
	a.Dispatch(ActionTurnStarted{SessionID: "S1"})
	wantTab(t, a, "working · session one", term.ProgressBusy)

	// Both running: the one on screen keeps the tab, and the other is counted.
	a.Dispatch(ActionTurnStarted{SessionID: "S2"})
	wantTab(t, a, "working · session two (+1)", term.ProgressBusy)

	// The background one needs an answer: that outranks running.
	reply := make(chan ui.Decision, 1)
	a.Dispatch(ActionApprovalRequest{SessionID: "S1", Request: blockedRequest(true), Reply: reply})
	wantTab(t, a, "needs approval · session one (+1)", term.ProgressPaused)

	// A failure outranks a finished turn and a running one, but not that.
	a.Dispatch(ActionTurnEnded{SessionID: "S2", Err: errors.New("boom")})
	wantTab(t, a, "needs approval · session one (+1)", term.ProgressPaused)
	a.viewByID("S1").approval = nil
	wantTab(t, a, "failed · session two (+1)", term.ProgressError)

	// Input clears the outcome of the session in front of the operator only.
	a.Dispatch(ActionTurnEnded{SessionID: "S1"})
	wantTab(t, a, "failed · session two (+1)", term.ProgressError)
	a.Dispatch(ActionRune{Runes: []rune{'x'}})
	wantTab(t, a, "done · session one", term.ProgressNone)
}

// With no session the tab is the mark and the name, which is the one place the
// name is spelled out. A session with no title of its own is named by its id
// rather than left blank.
func TestTabWithNoSessionAndWithAnUntitledOne(t *testing.T) {
	a := newTabApp()
	for _, v := range a.Views() {
		a.RemoveSession(v.ID)
	}
	wantTab(t, a, "manvi", term.ProgressNone)

	a.AddSession("S9", "")
	wantTab(t, a, "S9", term.ProgressNone)
}
