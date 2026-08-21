package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"manvi/ui"
	"manvi/ui/render"
)

// stubHost records what the UI asked for without doing any of it.
type stubHost struct {
	submitted []string
	commands  []string
	closed    []string
	newErr    error
	seq       int
	settings  []SettingSpec
}

func (h *stubHost) Submit(ctx context.Context, sessionID, text string) error {
	h.submitted = append(h.submitted, text)
	return nil
}
func (h *stubHost) Cancel(string) {}
func (h *stubHost) Command(ctx context.Context, sessionID, name, args string) error {
	h.commands = append(h.commands, name+" "+args)
	return nil
}
func (h *stubHost) Commands() []CommandSpec {
	return []CommandSpec{
		{Name: "doctor", Summary: "configuration"},
		{Name: "check", Args: "PATH", Summary: "evaluate a write"},
		{Name: "allow", Args: "PATH", Summary: "record an override", Mutating: true},
		{Name: "flags", Args: "[--all] | set KEY VALUE", Summary: "settings and how to move one"},
	}
}
func (h *stubHost) Settings() []SettingSpec {
	if h.settings != nil {
		return h.settings
	}
	return []SettingSpec{
		{Key: "policy.file.mode", Value: "advisory", Origin: "override", Mutable: "human",
			Safety: true, AtSafest: false, Choices: []string{"enforce", "advisory", "off"},
			Summary: "how the file gate treats a soft denial"},
		{Key: "grants.enabled", Value: "true", Origin: "default", Mutable: "human",
			Summary: "whether the override seam is available at all"},
		{Key: "policy.hard_rules.enabled", Value: "true", Origin: "default", Mutable: "startup",
			Safety: true, AtSafest: true, Summary: "hard rules, fixed after boot"},
	}
}
func (h *stubHost) NewSession(ctx context.Context) (string, string, error) {
	if h.newErr != nil {
		return "", "", h.newErr
	}
	h.seq++
	return "S" + itoa(h.seq), "session", nil
}
func (h *stubHost) CloseSession(ctx context.Context, id string) error {
	h.closed = append(h.closed, id)
	return nil
}

func newTestApp() (*App, *stubHost) {
	host := &stubHost{}
	a := NewApp(Dark(), host)
	a.Dispatch(ActionResize{W: 100, H: 30})
	a.AddSession("S1", "session one")
	return a, host
}

// effectsOf runs a key and returns what it asked for.
func key(a *App, binding string) []Effect { return a.Dispatch(ActionKey{Binding: binding}) }

func typeText(a *App, text string) {
	for _, r := range text {
		a.Dispatch(ActionRune{Runes: []rune{r}})
	}
}

func TestTypingAndSubmitting(t *testing.T) {
	a, host := newTestApp()
	typeText(a, "fix the parser")
	effects := key(a, "enter")

	if len(effects) != 1 {
		t.Fatalf("submit produced %d effects: %#v", len(effects), effects)
	}
	sub, ok := effects[0].(EffectSubmit)
	if !ok || sub.Text != "fix the parser" {
		t.Fatalf("got %#v, want a submit of the typed text", effects[0])
	}
	if !a.Current().Prompt.Empty() {
		t.Fatal("the composer was not cleared on submit")
	}
	_ = host
}

func TestSubmitWhileBusyQueuesInsteadOfStartingASecondTurn(t *testing.T) {
	// Two turns on one session would interleave their writes into a single
	// session log, and that log is the only thing the model's history is
	// projected from.
	a, _ := newTestApp()
	a.Dispatch(ActionTurnStarted{SessionID: "S1"})

	typeText(a, "follow up")
	if effects := key(a, "enter"); len(effects) != 0 {
		t.Fatalf("submitting during a turn produced %#v; it must queue", effects)
	}
	if got := a.Current().Queued(); len(got) != 1 || got[0] != "follow up" {
		t.Fatalf("queue = %v", got)
	}

	// It goes when the turn ends.
	effects := a.Dispatch(ActionTurnEnded{SessionID: "S1"})
	if len(effects) != 1 {
		t.Fatalf("turn end produced %#v, want the queued follow-up", effects)
	}
	if sub := effects[0].(EffectSubmit); sub.Text != "follow up" {
		t.Fatalf("sent %q", sub.Text)
	}
}

func TestQueuedFollowUpsGoOneAtATime(t *testing.T) {
	a, _ := newTestApp()
	a.Dispatch(ActionTurnStarted{SessionID: "S1"})
	typeText(a, "one")
	key(a, "enter")
	typeText(a, "two")
	key(a, "enter")

	effects := a.Dispatch(ActionTurnEnded{SessionID: "S1"})
	if len(effects) != 1 {
		t.Fatalf("two queued follow-ups produced %d submits at once", len(effects))
	}
	if left := a.Current().Queued(); len(left) != 1 {
		t.Fatalf("%d follow-ups still queued, want 1", len(left))
	}
}

func TestSlashCommandRuns(t *testing.T) {
	a, _ := newTestApp()
	typeText(a, "/check src/main.go")
	effects := key(a, "enter")
	if len(effects) != 1 {
		t.Fatalf("got %#v", effects)
	}
	cmd, ok := effects[0].(EffectCommand)
	if !ok || cmd.Name != "check" || cmd.Args != "src/main.go" {
		t.Fatalf("got %#v, want a check command with its argument", effects[0])
	}
}

func TestUnknownSlashCommandIsRefusedNotSent(t *testing.T) {
	a, _ := newTestApp()
	typeText(a, "/nonsense")
	if effects := key(a, "enter"); len(effects) != 0 {
		t.Fatalf("an unknown command produced %#v", effects)
	}
	notice, kind := a.Notice()
	if !strings.Contains(notice, "nonsense") || kind != StatusWarn {
		t.Fatalf("notice = %q (%v)", notice, kind)
	}
}

func TestSlashTriggerOpensCompletionAndTypingAPathDoesNot(t *testing.T) {
	a, _ := newTestApp()
	typeText(a, "/")
	if a.overlay == nil || a.overlay.Kind != OverlayComplete {
		t.Fatal("typing / at the start of the prompt did not open completion")
	}
	a.Dispatch(ActionKey{Binding: "esc"})

	// A slash inside a path is a path separator, not a command.
	typeText(a, "edit src/main.go")
	if a.overlay != nil {
		t.Fatalf("a slash inside a path opened %v", a.overlay.Kind)
	}
}

func TestTabCompletesAndEnterSends(t *testing.T) {
	// A dropdown that swallows Enter is a dropdown that swallows submissions.
	a, host := newTestApp()
	typeText(a, "/ch")
	if a.overlay == nil {
		t.Fatal("no completion overlay")
	}
	key(a, "tab")
	if got := a.Current().Prompt.Value(); got != "/check " {
		t.Fatalf("tab left the prompt as %q", got)
	}
	if a.overlay != nil {
		t.Fatal("the overlay stayed open after completing")
	}

	// A command typed in full and sent with Enter must run, not be replaced by
	// whichever row the dropdown had highlighted.
	a.Current().Prompt.Clear()
	typeText(a, "/doctor")
	if a.overlay == nil {
		t.Fatal("the completer did not reopen")
	}
	effects := key(a, "enter")
	if len(effects) != 1 {
		t.Fatalf("Enter with the completer open produced %#v", effects)
	}
	cmd, ok := effects[0].(EffectCommand)
	if !ok || cmd.Name != "doctor" {
		t.Fatalf("got %#v, want the doctor command", effects[0])
	}
	if a.overlay != nil {
		t.Fatal("the completer stayed open after sending")
	}
	_ = host
}

func TestQuitNeedsConfirmation(t *testing.T) {
	// A single keystroke that discards a running turn and its transcript is too
	// cheap.
	a, _ := newTestApp()
	if effects := key(a, "ctrl+q"); len(effects) != 0 {
		t.Fatalf("the first quit press produced %#v", effects)
	}
	if a.Quitting() {
		t.Fatal("one press quit")
	}
	effects := key(a, "ctrl+q")
	if !a.Quitting() || len(effects) != 1 {
		t.Fatalf("the second press did not quit: %#v", effects)
	}
}

func TestQuitConfirmationNamesRunningTurns(t *testing.T) {
	a, _ := newTestApp()
	a.Dispatch(ActionTurnStarted{SessionID: "S1"})
	key(a, "ctrl+q")
	notice, kind := a.Notice()
	if !strings.Contains(notice, "still running") || kind != StatusWarn {
		t.Fatalf("notice = %q (%v)", notice, kind)
	}
}

func TestCtrlCCancelsRatherThanQuits(t *testing.T) {
	// In raw mode Ctrl+C is the only way to interrupt a turn. A harness that
	// exits on it loses the transcript of whatever was being stopped.
	a, _ := newTestApp()
	a.Dispatch(ActionTurnStarted{SessionID: "S1"})
	effects := key(a, "ctrl+c")
	if a.Quitting() {
		t.Fatal("ctrl+c quit the application")
	}
	if len(effects) != 1 {
		t.Fatalf("got %#v, want a cancel", effects)
	}
	if _, ok := effects[0].(EffectCancel); !ok {
		t.Fatalf("got %#v, want EffectCancel", effects[0])
	}
}

func TestEscapeNeedsTwoPressesToDiscardADraft(t *testing.T) {
	a, _ := newTestApp()
	typeText(a, "half a thought")
	key(a, "esc")
	if a.Current().Prompt.Empty() {
		t.Fatal("one Escape discarded the draft")
	}
	key(a, "esc")
	if !a.Current().Prompt.Empty() {
		t.Fatal("the second Escape did not clear the draft")
	}
}

// --- approvals: the load-bearing half ---

func blockedRequest(grantable bool) ui.Request {
	return ui.Request{
		ID: "APPROVAL-1", Rule: "scope.unplanned", Severity: "soft",
		Path: "src/helper.go", Reason: "not in the task's planned files",
		TaskID: "TASK-001", Grantable: grantable,
	}
}

func raiseApproval(a *App, grantable bool) chan ui.Decision {
	reply := make(chan ui.Decision, 1)
	a.Dispatch(ActionApprovalRequest{SessionID: "S1", Request: blockedRequest(grantable), Reply: reply})
	return reply
}

func TestApprovalDefaultsToDeny(t *testing.T) {
	// The landing position must be the safe one.
	a, _ := newTestApp()
	reply := raiseApproval(a, true)
	effects := key(a, "enter")
	if len(effects) != 1 {
		t.Fatalf("got %#v", effects)
	}
	d := effects[0].(EffectDecide)
	if d.Decision.Allow {
		t.Fatal("Enter on a fresh approval card allowed the operation")
	}
	if d.Reply != reply {
		t.Fatal("the decision went to the wrong reply channel")
	}
}

func TestApprovalAllowRequiresAReason(t *testing.T) {
	// A grant carrying a manufactured reason reads, in a later review, exactly
	// like one carrying a real reason.
	a, _ := newTestApp()
	raiseApproval(a, true)

	key(a, "down") // move to "allow once"
	if effects := key(a, "enter"); len(effects) != 0 {
		t.Fatalf("choosing allow decided immediately: %#v", effects)
	}
	// Arriving at the field is not an error, and must not be reported as one.
	if notice, _ := a.Notice(); strings.Contains(notice, "reason is required") {
		t.Fatalf("entering the reason field warned before anything was submitted: %q", notice)
	}
	// Empty reason: still no decision.
	if effects := key(a, "enter"); len(effects) != 0 {
		t.Fatalf("an empty reason produced a decision: %#v", effects)
	}
	notice, _ := a.Notice()
	if !strings.Contains(notice, "reason is required") {
		t.Fatalf("notice = %q", notice)
	}

	typeText(a, "the plan omitted this helper")
	effects := key(a, "enter")
	if len(effects) != 1 {
		t.Fatalf("got %#v", effects)
	}
	d := effects[0].(EffectDecide).Decision
	if !d.Allow || d.Reason != "the plan omitted this helper" || d.By != "human" {
		t.Fatalf("decision = %#v", d)
	}
}

func TestHardRuleOffersNoAllow(t *testing.T) {
	// Offering an allow that will be refused downstream teaches an operator
	// that the control is advisory.
	a, _ := newTestApp()
	raiseApproval(a, false)
	card := a.Current().Approval()
	if card == nil {
		t.Fatal("no card")
	}
	if len(card.options()) != 1 {
		t.Fatalf("a non-grantable rule offered %d options: %v", len(card.options()), card.options())
	}
	effects := key(a, "enter")
	d := effects[0].(EffectDecide).Decision
	if d.Allow {
		t.Fatal("a non-grantable rule was allowed")
	}
}

func TestApprovalDismissDenies(t *testing.T) {
	a, _ := newTestApp()
	raiseApproval(a, true)
	effects := key(a, "esc")
	if len(effects) != 1 {
		t.Fatalf("got %#v", effects)
	}
	if effects[0].(EffectDecide).Decision.Allow {
		t.Fatal("dismissing an approval allowed it")
	}
}

func TestApprovalWithNoSessionIsDenied(t *testing.T) {
	// An approval nobody can see must not be granted by absence.
	a, _ := newTestApp()
	reply := make(chan ui.Decision, 1)
	effects := a.Dispatch(ActionApprovalRequest{
		SessionID: "nonexistent", Request: blockedRequest(true), Reply: reply,
	})
	if len(effects) != 1 {
		t.Fatalf("got %#v", effects)
	}
	if effects[0].(EffectDecide).Decision.Allow {
		t.Fatal("an approval with no view was allowed")
	}
}

func TestApprovalsQueueRatherThanStack(t *testing.T) {
	a, _ := newTestApp()
	raiseApproval(a, true)
	raiseApproval(a, true)
	v := a.Current()
	if v.PendingApprovals() != 1 {
		t.Fatalf("%d pending, want 1 behind the active card", v.PendingApprovals())
	}
	key(a, "enter") // deny the first
	if v.Approval() == nil {
		t.Fatal("the queued approval was not promoted")
	}
	if v.PendingApprovals() != 0 {
		t.Fatal("the queue did not drain")
	}
}

func TestApprovalIsModal(t *testing.T) {
	// Keys behind a human-in-the-loop control must not stay live: an approval
	// answered by accident is indistinguishable in the ledger from one answered
	// on purpose.
	a, _ := newTestApp()
	typeText(a, "draft text")
	raiseApproval(a, true)
	typeText(a, "XYZ")
	if strings.Contains(a.Current().Prompt.Value(), "XYZ") {
		t.Fatalf("typing reached the composer behind a modal card: %q", a.Current().Prompt.Value())
	}
}

func TestDigitPicksAnApprovalOptionDirectly(t *testing.T) {
	a, _ := newTestApp()
	raiseApproval(a, true)
	// "1" is deny, and it must decide immediately.
	effects := a.Dispatch(ActionRune{Runes: []rune{'1'}})
	if len(effects) != 1 {
		t.Fatalf("got %#v", effects)
	}
	if effects[0].(EffectDecide).Decision.Allow {
		t.Fatal("option 1 allowed; deny must be first")
	}
}

func TestApproverRefusesWhenTheContextIsCancelled(t *testing.T) {
	// A question that could not be answered is a refusal, never an allow.
	actions := make(chan Action, 4)
	ap := &Approver{SessionID: "S1", Actions: actions}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan ui.Decision, 1)
	go func() {
		d, _ := ap.Approve(ctx, blockedRequest(true))
		done <- d
	}()
	<-actions // the request was raised
	cancel()

	select {
	case d := <-done:
		if d.Allow {
			t.Fatal("a cancelled approval was treated as consent")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Approve never returned after cancellation")
	}
}

// --- sessions and the dashboard ---

func TestEventsArrivingBeforeTheirSessionAreNotLost(t *testing.T) {
	// A host builds a session's state and announces it in the same call, so its
	// opening events are in flight before the view exists. The weakened-settings
	// notice is among them, and it is the one that decides whether the run may
	// be described as strict.
	a := NewApp(Dark(), &stubHost{})
	a.Dispatch(ActionResize{W: 100, H: 30})

	a.Dispatch(ActionEvent{SessionID: "S9", Event: ui.Event{
		Kind: ui.KindSessionStart, Posture: "dev", Model: "some-model",
	}})
	a.Dispatch(ActionEvent{SessionID: "S9", Event: ui.Event{
		Kind: ui.KindNotice, Text: "not strict", Degraded: []string{"harness.posture=dev"},
	}})
	a.Dispatch(ActionSessionAdded{ID: "S9", Title: "late"})

	v := a.Current()
	if v == nil || v.ID != "S9" {
		t.Fatal("the session was not registered")
	}
	if v.Status.Posture != "dev" {
		t.Fatalf("posture = %q; the session-start event was dropped", v.Status.Posture)
	}
	if v.Scroll.Len() != 2 {
		t.Fatalf("%d entries replayed, want 2", v.Scroll.Len())
	}
	if strict, why := v.Status.StrictSummary(); strict {
		t.Fatalf("the run reported as strict (%q) after a dev-posture session start", why)
	}
}

func TestOverflowingTheHoldBufferIsReportedNotSilent(t *testing.T) {
	a := NewApp(Dark(), &stubHost{})
	for i := 0; i < maxHeld+25; i++ {
		a.Dispatch(ActionEvent{SessionID: "S9", Event: ui.Event{Kind: ui.KindNotice, Text: "n"}})
	}
	a.Dispatch(ActionSessionAdded{ID: "S9", Title: "late"})

	v := a.Current()
	if v.Scroll.Len() != maxHeld+1 {
		t.Fatalf("%d entries, want %d held plus one report", v.Scroll.Len(), maxHeld)
	}
	last := v.Scroll.Entries()[v.Scroll.Len()-1]
	if len(last.Event.Degraded) == 0 {
		t.Fatal("the discarded events were not reported as a gap in the transcript")
	}
	if !strings.Contains(last.Event.Text, "25") {
		t.Fatalf("the report did not name the count: %q", last.Event.Text)
	}
}

func TestDashboardOpensAndSelects(t *testing.T) {
	a, _ := newTestApp()
	a.AddSession("S2", "session two")
	key(a, "ctrl+g")
	if a.mode != ModeDashboard {
		t.Fatal("ctrl+g did not open the dashboard")
	}
	a.dashboard.Select(0)
	key(a, "enter")
	if a.mode != ModeSession || a.Current().ID != "S1" {
		t.Fatalf("opening from the dashboard landed on %v/%s", a.mode, a.Current().ID)
	}
}

func TestClosingASessionAsksTheHostToReleaseIt(t *testing.T) {
	// A session holds a lease. Dropping it from the UI without releasing leaves
	// a task no other builder can take until the TTL lapses.
	a, _ := newTestApp()
	a.AddSession("S2", "session two")
	key(a, "ctrl+g")
	a.dashboard.Select(1)
	effects := key(a, "ctrl+x")
	if len(effects) != 1 {
		t.Fatalf("got %#v", effects)
	}
	if got := effects[0].(EffectCloseSession); got.SessionID != "S2" {
		t.Fatalf("closed %q", got.SessionID)
	}
	// The view goes only once the host has confirmed.
	if len(a.Views()) != 2 {
		t.Fatal("the session was removed before the host released it")
	}
	a.Dispatch(actionRemoveSession{ID: "S2"})
	if len(a.Views()) != 1 {
		t.Fatal("the session was not removed after release")
	}
}

// --- status and the strict claim ---

func TestStrictSummaryNamesEveryReasonARunIsNotStrict(t *testing.T) {
	cases := []struct {
		name string
		st   Status
		want string
	}{
		{"clean", Status{Posture: "strict"}, ""},
		{"dev posture", Status{Posture: "dev"}, "dev posture"},
		{"yolo posture", Status{Posture: "yolo"}, "yolo posture"},
		{"weakened flag", Status{Posture: "strict", Weakened: []string{"policy.hard_rules"}}, "safety flag"},
		{"unrun check", Status{Posture: "strict", Degraded: []string{"secret_scan"}}, "could not run"},
		{"grant applied", Status{Posture: "strict", Grants: 2}, "override"},
	}
	for _, c := range cases {
		strict, why := c.st.StrictSummary()
		if c.want == "" {
			if !strict {
				t.Errorf("%s: reported not strict (%s)", c.name, why)
			}
			continue
		}
		if strict {
			t.Errorf("%s: reported strict", c.name)
			continue
		}
		if !strings.Contains(why, c.want) {
			t.Errorf("%s: reason %q does not mention %q", c.name, why, c.want)
		}
	}
}

func TestGrantsAndDegradedChecksAccumulateOnTheStatus(t *testing.T) {
	a, _ := newTestApp()
	a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{
		Kind: ui.KindToolResult, Text: "wrote it", GrantID: "GRANT-1", GrantedBy: "human", Rule: "scope.unplanned",
	}})
	a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{
		Kind: ui.KindToolResult, Text: "verified", Degraded: []string{"secret_scan"},
	}})
	st := a.Current().Status
	if st.Grants != 1 {
		t.Fatalf("grants = %d", st.Grants)
	}
	if len(st.Degraded) != 1 || st.Degraded[0] != "secret_scan" {
		t.Fatalf("degraded = %v", st.Degraded)
	}
	if strict, _ := st.StrictSummary(); strict {
		t.Fatal("a run with a grant and an unrun check reported as strict")
	}
}

// --- drawing ---

// screen renders the app and returns the frame as plain text rows.
func screen(a *App, w, h int) []string {
	b := render.NewBuffer(w, h)
	a.Draw(b)
	rows := make([]string, h)
	for y := 0; y < h; y++ {
		var sb strings.Builder
		for x := 0; x < w; x++ {
			c := b.Cell(x, y)
			if c.R == 0 {
				continue
			}
			sb.WriteRune(c.R)
		}
		rows[y] = strings.TrimRight(sb.String(), " ")
	}
	return rows
}

func contains2(rows []string, want string) bool {
	for _, r := range rows {
		if strings.Contains(r, want) {
			return true
		}
	}
	return false
}

func TestFrameShowsTheStrictWarningPersistently(t *testing.T) {
	// A banner in the transcript scrolls away; the fact does not stop being
	// true when it does.
	a, _ := newTestApp()
	a.Current().Status.Posture = "dev"
	rows := screen(a, 90, 24)
	if !contains2(rows, "not a strict run") {
		t.Fatalf("the strict warning is missing from the frame:\n%s", strings.Join(rows, "\n"))
	}
	// It survives a screenful of transcript.
	for i := 0; i < 200; i++ {
		a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{Kind: ui.KindText, Text: "line\n"}})
	}
	rows = screen(a, 90, 24)
	if !contains2(rows, "not a strict run") {
		t.Fatal("the strict warning scrolled away")
	}
}

func TestFrameNeverOverflowsItsWidth(t *testing.T) {
	// Overflow wraps in the terminal, which shifts every row below it and
	// desynchronises the painter's model of the screen.
	a, _ := newTestApp()
	a.Current().Status = Status{
		Posture: "dev", Model: "a-very-long-model-identifier-indeed",
		TaskID: "TASK-0001", Grants: 3, Degraded: []string{"secret_scan", "diff_coverage"},
	}
	a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{
		Kind: ui.KindToolResult, Text: strings.Repeat("漢字 wide content ", 50),
	}})
	raiseApproval(a, true)

	for _, size := range [][2]int{{40, 12}, {80, 24}, {200, 60}, {20, 8}} {
		b := render.NewBuffer(size[0], size[1])
		a.Draw(b)
		for y := 0; y < size[1]; y++ {
			// A continuation cell contributes nothing: its columns were
			// already counted by the wide rune that owns it.
			w := 0
			for x := 0; x < size[0]; x++ {
				c := b.Cell(x, y)
				if c.IsContinuation() {
					continue
				}
				w += render.RuneWidth(c.R)
			}
			if w > size[0] {
				t.Fatalf("at %dx%d row %d occupies %d columns", size[0], size[1], y, w)
			}
		}
	}
}

func TestDrawingATinyTerminalDoesNotPanic(t *testing.T) {
	// A window being dragged reports sizes the layout was never designed for.
	a, _ := newTestApp()
	raiseApproval(a, true)
	for _, size := range [][2]int{{1, 1}, {2, 3}, {0, 0}, {5, 2}, {80, 1}} {
		b := render.NewBuffer(size[0], size[1])
		a.Draw(b)
	}
}

func TestClickSelectsTheEntryUnderIt(t *testing.T) {
	// The click handler and the frame must agree about where the transcript
	// is. When they do not, a click selects a neighbouring row and the symptom
	// reads as flaky selection rather than as layout arithmetic in two places.
	a, _ := newTestApp()
	for i := 0; i < 6; i++ {
		a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{
			Kind: ui.KindNotice, Text: "entry " + itoa(i),
		}})
	}
	b := render.NewBuffer(80, 20)
	a.Draw(b)

	sb := a.Current().Scroll
	if sb.Viewport() == 0 {
		t.Fatal("the transcript reported no geometry after a draw")
	}

	// Click the last transcript row — content is bottom-aligned, so the rows
	// above a short transcript are padding and correctly select nothing — then
	// find that same entry's text on that row of the frame.
	row := sb.view.Bottom() - 1
	a.Dispatch(ActionClick{X: 4, Y: row})
	if a.Current().Focus != CtxScrollback {
		t.Fatal("clicking the transcript did not move focus to it")
	}
	selected := sb.Selected()
	if selected == nil {
		t.Fatal("clicking the transcript selected nothing")
	}

	var onScreen strings.Builder
	for x := 0; x < 80; x++ {
		onScreen.WriteRune(b.Cell(x, row).R)
	}
	if !strings.Contains(onScreen.String(), strings.TrimSpace(selected.text())) {
		t.Fatalf("clicked row %d shows %q but selected %q",
			row, strings.TrimSpace(onScreen.String()), selected.text())
	}
}

func TestClickIsNotFooledByPanesBetweenTheTranscriptAndTheComposer(t *testing.T) {
	// The transcript does not run down to the composer: a strict banner takes a
	// row off the top, and a queue pane takes rows off the bottom. A hit test
	// that only knows about the composer treats those rows as transcript.
	a, _ := newTestApp()
	a.Current().Status.Posture = "dev"
	a.Dispatch(ActionTurnStarted{SessionID: "S1"})
	for i := 0; i < 8; i++ {
		a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{
			Kind: ui.KindNotice, Text: "entry " + itoa(i),
		}})
	}
	typeText(a, "queued one")
	key(a, "enter")
	typeText(a, "queued two")
	key(a, "enter")

	b := render.NewBuffer(80, 20)
	a.Draw(b)
	sb := a.Current().Scroll
	if len(a.Current().Queued()) != 2 {
		t.Fatalf("fixture did not queue: %v", a.Current().Queued())
	}

	// A row inside the queue pane is below the transcript and must not select.
	queueRow := sb.view.Bottom()
	a.Current().Focus = CtxPrompt
	sb.SelectNone()
	a.Dispatch(ActionClick{X: 4, Y: queueRow})
	if sb.Selected() != nil {
		t.Fatalf("a click on row %d (below the transcript, which ends at %d) selected an entry",
			queueRow, sb.view.Bottom()-1)
	}
	if a.Current().Focus != CtxPrompt {
		t.Fatal("a click outside the transcript moved focus into it")
	}
}

func TestClickBelowTheTranscriptFocusesTheComposer(t *testing.T) {
	a, _ := newTestApp()
	a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{Kind: ui.KindNotice, Text: "x"}})
	b := render.NewBuffer(80, 20)
	a.Draw(b)

	a.Dispatch(ActionClick{X: 4, Y: 19})
	if a.Current().Focus != CtxPrompt {
		t.Fatal("clicking the composer area did not focus it")
	}
	if a.Current().Scroll.Selected() != nil {
		t.Fatal("focusing the composer left a transcript selection behind")
	}
}

func TestDashboardFrameCountsBlockedSessions(t *testing.T) {
	a, _ := newTestApp()
	a.AddSession("S2", "second")
	raiseApproval(a, true)
	a.mode = ModeDashboard
	rows := screen(a, 100, 24)
	if !contains2(rows, "blocked on you") {
		t.Fatalf("the dashboard did not surface the blocked session:\n%s", strings.Join(rows, "\n"))
	}
}

func TestATransientNoticeNeverCoversTheStrictBanner(t *testing.T) {
	// A notice may cover transcript — that scrolls back. It may not cover the
	// statement that this run's results are not strict, which has nowhere else
	// to be read.
	a, _ := newTestApp()
	a.Current().Status.Posture = "dev"
	a.setNotice("press again to quit", StatusInfo)

	rows := screen(a, 110, 24)
	found := false
	for _, row := range rows {
		if strings.Contains(row, "not a strict run") {
			found = true
			if strings.Contains(row, "press again to quit") {
				t.Fatalf("the notice was drawn over the strict banner: %q", row)
			}
		}
	}
	if !found {
		t.Fatal("the strict banner is missing entirely")
	}
	if !contains2(rows, "press again to quit") {
		t.Fatal("the notice was not drawn at all")
	}
}

// cardFrame draws just the approval card and returns its rows as text.
func cardFrame(t *testing.T, a *App, w, h int) []string {
	t.Helper()
	card := a.Current().Approval()
	if card == nil {
		t.Fatal("no approval card")
	}
	th := a.Theme
	want := card.Height(th, w)
	if want > h {
		t.Fatalf("the card asked for %d rows in a %d-row test frame", want, h)
	}
	b := render.NewBuffer(w, h)
	card.Draw(b, render.Rect{X: 0, Y: 0, W: w, H: want}, th, 0, false)
	rows := make([]string, h)
	for y := 0; y < h; y++ {
		var sb strings.Builder
		for x := 0; x < w; x++ {
			if c := b.Cell(x, y); !c.IsContinuation() {
				sb.WriteRune(c.R)
			}
		}
		rows[y] = sb.String()
	}
	return rows
}

func TestEveryApprovalOptionIsActuallyDrawn(t *testing.T) {
	// The card asked for one row fewer than it drew, so the last option — the
	// allow — fell off the bottom. A control that presents a decision with one
	// of its answers invisible is worse than no control.
	a, _ := newTestApp()
	raiseApproval(a, true)
	card := a.Current().Approval()

	rows := cardFrame(t, a, 90, 40)
	for i, opt := range card.options() {
		if !contains2(rows, opt) {
			t.Fatalf("option %d (%q) is not in the card at the height it asked for:\n%s",
				i+1, opt, strings.Join(rows, "\n"))
		}
	}
}

func TestNonGrantableCardShowsItsRefusalAndSingleOption(t *testing.T) {
	a, _ := newTestApp()
	raiseApproval(a, false)
	rows := cardFrame(t, a, 90, 40)
	if !contains2(rows, "never grantable") {
		t.Fatalf("the card did not say the rule is not negotiable:\n%s", strings.Join(rows, "\n"))
	}
	if !contains2(rows, "no grant clears this rule") {
		t.Fatalf("the trailing refusal line is missing:\n%s", strings.Join(rows, "\n"))
	}
}

func TestReasonEditorIsDrawnWhenTheAllowStageIsReached(t *testing.T) {
	a, _ := newTestApp()
	raiseApproval(a, true)
	key(a, "down")
	key(a, "enter") // move to the reason stage
	typeText(a, "the plan omitted this helper")

	rows := cardFrame(t, a, 90, 40)
	if !contains2(rows, "reason (required") {
		t.Fatalf("the reason prompt is missing:\n%s", strings.Join(rows, "\n"))
	}
	if !contains2(rows, "the plan omitted this helper") {
		t.Fatalf("the typed reason is not shown:\n%s", strings.Join(rows, "\n"))
	}
}

func TestATruncatedCardSaysSoRatherThanHidingAnOption(t *testing.T) {
	// A terminal too short for the card must not answer by quietly dropping
	// rows off the bottom.
	a, _ := newTestApp()
	raiseApproval(a, true)
	card := a.Current().Approval()

	b := render.NewBuffer(90, 8)
	card.Draw(b, render.Rect{X: 0, Y: 0, W: 90, H: 8}, a.Theme, 0, false)
	var frame strings.Builder
	for y := 0; y < 8; y++ {
		for x := 0; x < 90; x++ {
			frame.WriteRune(b.Cell(x, y).R)
		}
		frame.WriteString("\n")
	}
	if !strings.Contains(frame.String(), "truncated") {
		t.Fatalf("a clipped card did not report itself:\n%s", frame.String())
	}
}

func TestAStrayNoticeLandsInTheTranscriptNotJustOnTheBar(t *testing.T) {
	// Captured stderr arrives with no session attached. Routed nowhere, it
	// would survive only as a message that fades off the shortcut bar — which
	// for a write that failed is the same as losing it.
	a, _ := newTestApp()
	a.Dispatch(ActionNotice{Text: "the grant ledger could not be written: read-only file system",
		Status: StatusBlock})

	v := a.Current()
	if v.Scroll.Len() != 1 {
		t.Fatalf("%d transcript entries, want the notice", v.Scroll.Len())
	}
	e := v.Scroll.Entries()[0]
	if e.Event.Kind != ui.KindError {
		t.Fatalf("kind = %v, want an error", e.Event.Kind)
	}
	if !strings.Contains(e.text(), "read-only file system") {
		t.Fatalf("text = %q", e.text())
	}
	if got, _ := a.Notice(); !strings.Contains(got, "read-only") {
		t.Fatalf("the shortcut bar did not show it either: %q", got)
	}
}

// bufferText flattens a drawn buffer into lines, for assertions about what
// actually reached the cells rather than about what a helper returned.
func bufferText(b *render.Buffer) string {
	var out strings.Builder
	for y := range b.H {
		for x := range b.W {
			c := b.Cell(x, y)
			if c.IsContinuation() {
				continue
			}
			out.WriteRune(c.R)
		}
		out.WriteByte('\n')
	}
	return out.String()
}

// With no session open the pane carries the mark. It is the one screen with
// nothing else on it, so a splash that failed to draw would leave a blank
// terminal that reads as a hang.
func TestEmptyPaneDrawsTheMark(t *testing.T) {
	a, _ := newTestApp()
	for _, v := range a.Views() {
		a.RemoveSession(v.ID)
	}
	if a.Current() != nil {
		t.Fatal("the test app still has a session")
	}
	th := a.Theme
	th.Unicode = true
	a.Theme = th

	// The hint types itself out over the first ticks; run past the reveal so
	// the assertion is about the settled frame, not a frame mid-animation.
	for i := 0; i < 30; i++ {
		a.Dispatch(ActionTick{})
	}
	b := render.NewBuffer(80, 24)
	a.Draw(b)
	got := bufferText(b)
	if !strings.Contains(got, "█") {
		t.Fatalf("the empty pane drew no tile:\n%s", got)
	}
	for _, want := range []string{"manvi", "the DevCouncil execution harness", "ctrl+n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the empty pane is missing %q:\n%s", want, got)
		}
	}

	// Narrow enough that the tile cannot fit: the name survives, and the frame
	// is not overrun.
	narrow := render.NewBuffer(12, 6)
	a.Draw(narrow)
	if !strings.Contains(bufferText(narrow), "manvi") {
		t.Fatal("a narrow pane dropped the name as well as the tile")
	}
}

// TestAutocompleteNavigationAndEnterExecution.
//
// This asserted that Enter on any highlighted row ran that command
// immediately, with no arguments. For /doctor that is right. For /check, whose
// declared usage is "PATH", it meant the menu ran a command that cannot work
// without an argument the menu gave no way to supply — and the same rule made
// "/flags --all" unreachable from the dropdown that prints "[--all]" in its own
// row. A command with arguments now completes into the composer and waits.
func TestAutocompleteNavigationAndEnterExecution(t *testing.T) {
	a, _ := newTestApp()
	typeText(a, "/")
	if a.overlay == nil || a.overlay.Kind != OverlayComplete {
		t.Fatal("no completion overlay")
	}
	key(a, "down") // navigate down to check, which takes a PATH
	if effects := key(a, "enter"); len(effects) != 0 {
		t.Fatalf("Enter ran a command that needs an argument: %#v", effects)
	}
	if a.overlay != nil {
		t.Fatal("overlay should be closed after Enter")
	}
	if got := a.Current().Prompt.Value(); got != "/check " {
		t.Fatalf("prompt = %q, want the command waiting for its argument", got)
	}
	if notice, kind := a.Notice(); !strings.Contains(notice, "PATH") || kind != StatusInfo {
		t.Fatalf("notice = %q (%v), want the usage", notice, kind)
	}

	// And the argument the operator then types is what runs.
	typeText(a, "src/main.go")
	effects := key(a, "enter")
	if len(effects) != 1 {
		t.Fatalf("Enter with the argument typed produced %#v", effects)
	}
	cmd, ok := effects[0].(EffectCommand)
	if !ok || cmd.Name != "check" || cmd.Args != "src/main.go" {
		t.Fatalf("got %#v, want check with its argument", effects[0])
	}
}

// TestAutocompleteRunsACommandThatTakesNoArguments is the other half: waiting
// for arguments a command does not have would be a keystroke charged for
// nothing.
func TestAutocompleteRunsACommandThatTakesNoArguments(t *testing.T) {
	a, _ := newTestApp()
	typeText(a, "/doc")
	if a.overlay == nil || a.overlay.Kind != OverlayComplete {
		t.Fatal("no completion overlay")
	}
	effects := key(a, "enter")
	if len(effects) != 1 {
		t.Fatalf("Enter on an argument-free command produced %#v", effects)
	}
	if cmd, ok := effects[0].(EffectCommand); !ok || cmd.Name != "doctor" {
		t.Fatalf("got %#v, want the doctor command", effects[0])
	}
}

// TestCompletionDropdownShowsTheUsage. CommandSpec.Args documents itself as "a
// usage hint shown in the dropdown" and was rendered only in the palette.
func TestCompletionDropdownShowsTheUsage(t *testing.T) {
	a, _ := newTestApp()
	typeText(a, "/che")
	if a.overlay == nil {
		t.Fatal("no completion overlay")
	}
	item, ok := a.overlay.Selected()
	if !ok {
		t.Fatal("nothing selected")
	}
	if !strings.Contains(item.Detail, "PATH") {
		t.Fatalf("dropdown row = %q, want it to name the argument", item.Detail)
	}
}

func TestAutocompleteBackspacing(t *testing.T) {
	a, _ := newTestApp()
	typeText(a, "/doct")
	if a.overlay == nil {
		t.Fatal("no completion overlay")
	}
	key(a, "backspace")
	key(a, "backspace")
	if got := a.Current().Prompt.Value(); got != "/do" {
		t.Fatalf("prompt = %q, want /do", got)
	}
	if a.overlay == nil || a.overlay.Empty() {
		t.Fatal("overlay should still match /do")
	}
}

func TestSearchableOverlayFilteringAndEditing(t *testing.T) {
	a, _ := newTestApp()
	key(a, "ctrl+p")
	if a.overlay == nil || a.overlay.Kind != OverlayPalette {
		t.Fatal("palette did not open")
	}
	typeText(a, "doc")
	if len(a.overlay.filtered) != 1 || a.overlay.filtered[0].Value != "doctor" {
		t.Fatalf("filtered = %#v, want only doctor", a.overlay.filtered)
	}
	key(a, "backspace")
	key(a, "backspace")
	key(a, "backspace")
	// The palette lists the host's commands and the ones the UI answers itself.
	if len(a.overlay.filtered) != len(a.menuCommands()) {
		t.Fatalf("filtered after clearing = %d items, want %d", len(a.overlay.filtered), len(a.menuCommands()))
	}
	typeText(a, "che")
	// /check takes a PATH, so accepting it arms the composer rather than
	// running it bare — see TestPaletteArmsACommandThatTakesArguments.
	if effects := key(a, "enter"); len(effects) != 0 {
		t.Fatalf("Enter on filtered palette ran a command needing an argument: %#v", effects)
	}
	if got := a.Current().Prompt.Value(); got != "/check " {
		t.Fatalf("prompt = %q, want the command waiting for its argument", got)
	}
}

// TestPaletteArmsACommandThatTakesArguments.
//
// The palette rendered the usage — "[--all]", "list|acquire|release",
// "PATH --reason TEXT" — in the same row whose Enter handler then called
// runCommand(name, ""). It displayed every command's options and supplied none
// of them, which is how a harness comes to have a settings command with no way
// to pass --all.
func TestPaletteArmsACommandThatTakesArguments(t *testing.T) {
	a, _ := newTestApp()
	key(a, "ctrl+p")
	typeText(a, "allow")
	effects := key(a, "enter")
	if len(effects) != 0 {
		t.Fatalf("the palette ran /allow with no reason: %#v", effects)
	}
	if got := a.Current().Prompt.Value(); got != "/allow " {
		t.Fatalf("prompt = %q, want /allow waiting for its arguments", got)
	}
	if a.Current().Focus != CtxPrompt {
		t.Fatal("the composer was armed but focus was left elsewhere")
	}
	if notice, _ := a.Notice(); !strings.Contains(notice, "PATH") {
		t.Fatalf("notice = %q, want the usage", notice)
	}
}

// TestPaletteRunsACommandThatTakesNoArguments.
func TestPaletteRunsACommandThatTakesNoArguments(t *testing.T) {
	a, _ := newTestApp()
	key(a, "ctrl+p")
	typeText(a, "doc")
	effects := key(a, "enter")
	if len(effects) != 1 {
		t.Fatalf("the palette did not run /doctor: %#v", effects)
	}
	if cmd, ok := effects[0].(EffectCommand); !ok || cmd.Name != "doctor" {
		t.Fatalf("got %#v, want the doctor command", effects[0])
	}
}

// TestPaletteDoesNotClobberADraft. The palette is reachable with half a prompt
// already typed, and replacing that text to save a keystroke destroys work.
func TestPaletteDoesNotClobberADraft(t *testing.T) {
	a, _ := newTestApp()
	typeText(a, "half a thought")
	key(a, "ctrl+p")
	typeText(a, "allow")
	if effects := key(a, "enter"); len(effects) != 0 {
		t.Fatalf("accepting produced %#v", effects)
	}
	if got := a.Current().Prompt.Value(); got != "half a thought" {
		t.Fatalf("prompt = %q, want the draft untouched", got)
	}
	notice, kind := a.Notice()
	if !strings.Contains(notice, "draft") || kind != StatusWarn {
		t.Fatalf("notice = %q (%v), want an explanation of why nothing happened", notice, kind)
	}
}

func TestApprovalReasonEditorBackspaceAndEditing(t *testing.T) {
	a, _ := newTestApp()
	raiseApproval(a, true)
	key(a, "down")
	key(a, "enter") // move to reason stage
	typeText(a, "safe change typo")
	key(a, "backspace")
	key(a, "backspace")
	key(a, "backspace")
	key(a, "backspace")
	if got := a.Current().Approval().Prompt().Value(); got != "safe change " {
		t.Fatalf("reason prompt = %q, want 'safe change '", got)
	}
}

func TestBuiltinSlashCommands(t *testing.T) {
	a, _ := newTestApp()
	// /help opens help overlay
	typeText(a, "/help")
	key(a, "enter")
	if a.overlay == nil || a.overlay.Kind != OverlayHelp {
		t.Fatal("/help did not open help overlay")
	}
	key(a, "esc")

	// /clear resets scrollback
	addEntries(a, 3)
	if a.Current().Scroll.Len() == 0 {
		t.Fatal("scrollback should have entries")
	}
	typeText(a, "/clear")
	key(a, "enter")
	if a.Current().Scroll.Len() != 0 {
		t.Fatal("/clear did not reset scrollback")
	}

	// /quit arms quit confirmation
	typeText(a, "/quit")
	key(a, "enter")
	notice, _ := a.Notice()
	if !strings.Contains(notice, "quit") {
		t.Fatalf("notice = %q, want quit confirmation", notice)
	}
}

// TestASlashCommandDuringATurnIsRefusedNotDropped.
//
// submit() queues a prompt typed during a turn and says so on the shortcut bar.
// A slash command returned before reaching that check, became an EffectCommand,
// and met the runner's one-turn-per-session guard — which cancelled it and
// returned without a word. The operator saw the command echo into the composer,
// press Enter, and nothing at all happen, which reads as a hung harness rather
// than a refused command.
func TestASlashCommandDuringATurnIsRefusedNotDropped(t *testing.T) {
	a, _ := newTestApp()
	a.Dispatch(ActionTurnStarted{SessionID: "S1"})

	typeText(a, "/doctor")
	if effects := key(a, "enter"); len(effects) != 0 {
		t.Fatalf("a command was started during a turn: %#v", effects)
	}
	notice, kind := a.Notice()
	if !strings.Contains(notice, "doctor") || kind != StatusWarn {
		t.Fatalf("notice = %q (%v), want a refusal naming the command", notice, kind)
	}
	if !strings.Contains(notice, "ctrl+c") {
		t.Fatalf("notice = %q, want it to say how to make the session idle", notice)
	}
}

// TestUIAnsweredCommandsStillWorkDuringATurn. The refusal above is about
// commands the harness runs. /help, /clear and the pickers are answered by the
// UI itself and have no reason to wait for a turn to end — an operator watching
// a long turn is exactly who needs to open help.
func TestUIAnsweredCommandsStillWorkDuringATurn(t *testing.T) {
	a, _ := newTestApp()
	addEntries(a, 3)
	a.Dispatch(ActionTurnStarted{SessionID: "S1"})

	typeText(a, "/help")
	key(a, "enter")
	if a.overlay == nil || a.overlay.Kind != OverlayHelp {
		t.Fatal("/help was refused during a turn")
	}
	key(a, "esc")

	typeText(a, "/clear")
	key(a, "enter")
	if a.Current().Scroll.Len() != 0 {
		t.Fatal("/clear was refused during a turn")
	}
}

// TestTheCommandRefusalNamesEveryHostCommand walks the host's own catalogue
// rather than a sample of it. A command added later inherits the guard, and a
// command that somehow bypassed it would show up here as a started turn.
func TestTheCommandRefusalNamesEveryHostCommand(t *testing.T) {
	a, host := newTestApp()
	a.Dispatch(ActionTurnStarted{SessionID: "S1"})
	for _, spec := range host.Commands() {
		a.Current().Prompt.Clear()
		typeText(a, "/"+spec.Name)
		// Arm the composer first for the ones that take arguments, so what is
		// being tested is the busy guard and not the missing argument.
		if spec.Args != "" {
			a.Current().Prompt.SetValue("/" + spec.Name + " x")
		}
		if effects := key(a, "enter"); len(effects) != 0 {
			t.Errorf("/%s started a turn while one was running: %#v", spec.Name, effects)
		}
	}
}
