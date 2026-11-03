package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

// Serve tests do not use t.Parallel, so a test may set these package knobs for
// its own duration.
func shortContinueRate(t *testing.T) {
	t.Helper()
	was := continueMinInterval
	continueMinInterval = 0
	t.Cleanup(func() { continueMinInterval = was })
}

// continuable builds a lifecycle whose provider completes a turn and then waits.
func continuable(t *testing.T, tune func(*managedTestSession)) *managedLifecycle {
	t.Helper()
	shortContinueRate(t)
	var m *managedLifecycle
	m = newManagedLifecycle(t, func(ctx context.Context, o codingagent.Options) *managedTestSession {
		s := m.fixtureSession(ctx, o)
		s.withRequest = false
		if tune != nil {
			tune(s)
		}
		return s
	})
	return m
}

// startContinuable prepares the run for max follow-ups and activates it.
func (m *managedLifecycle) startContinuable(max int) {
	m.t.Helper()
	prepared, err := m.runner.Prepare(m.t.Context(), json.RawMessage(fmt.Sprintf(`{"id":"run","request_id":"launch","expected_revision":1,"max_continuations":%d}`, max)))
	if err != nil {
		m.t.Fatal(err)
	}
	if _, err := m.runner.Activate(m.t.Context(), json.RawMessage(fmt.Sprintf(`{"id":"run","owner_id":%q,"session_id":%q,"process_id":123,"process_start":"fixture-native-observation"}`, prepared.Owner, prepared.Session))); err != nil {
		m.t.Fatal(err)
	}
}

func (m *managedLifecycle) send(requestID, message string) (ManagedContinueResult, error) {
	m.t.Helper()
	raw, err := json.Marshal(map[string]any{"id": "run", "request_id": requestID, "message": message})
	if err != nil {
		m.t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(m.t.Context(), 10*time.Second)
	defer stop()
	return m.runner.Continue(ctx, raw)
}

func (m *managedLifecycle) state() ManagedContinueResult {
	m.t.Helper()
	res, err := m.runner.Continue(m.t.Context(), json.RawMessage(`{"id":"run"}`))
	if err != nil {
		m.t.Fatalf("status read: %v", err)
	}
	return res
}

func (m *managedLifecycle) awaitState(want string) ManagedContinueResult {
	m.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last ManagedContinueResult
	for time.Now().Before(deadline) {
		last = m.state()
		if last.State == want {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	m.t.Fatalf("the run never reached %q; last %+v", want, last)
	return last
}

func (m *managedLifecycle) stop() {
	m.t.Helper()
	if err := m.runner.Stop(json.RawMessage(`{"id":"run"}`)); err != nil {
		m.t.Fatal(err)
	}
}

// wantContinueCode fails the test unless err is a typed error with that code.
func wantContinueCode(t *testing.T, err error, code string) {
	t.Helper()
	continueErrorOf(t, err, code)
}

// continueErrorOf is wantContinueCode for a test that reads the error's other
// members. It returns a copy, so no caller holds a pointer that might be nil.
func continueErrorOf(t *testing.T, err error, code string) Error {
	t.Helper()
	var typed *Error
	if !errors.As(err, &typed) || typed == nil || typed.Code != code {
		t.Fatalf("got %v, want a %s error", err, code)
		return Error{}
	}
	return *typed
}

// readSavedRun is the store's record of the run, with the saved output. It
// returns its error instead of failing the test, so a provider fake running on
// the drive loop's goroutine may call it.
func (m *managedLifecycle) readSavedRun() (state, providerState, output string, err error) {
	_, raw, err := m.runner.run(context.Background(), "run")
	if err != nil {
		return "", "", "", err
	}
	var row struct {
		Item struct {
			State         string `json:"state"`
			ProviderState string `json:"provider_state"`
			Output        string `json:"output"`
		} `json:"item"`
	}
	if err := json.Unmarshal(raw, &row); err != nil {
		return "", "", "", err
	}
	return row.Item.State, row.Item.ProviderState, row.Item.Output, nil
}

// savedRun is readSavedRun for the test's own goroutine.
func (m *managedLifecycle) savedRun() (state, providerState, output string) {
	m.t.Helper()
	state, providerState, output, err := m.readSavedRun()
	if err != nil {
		m.t.Fatal(err)
	}
	return state, providerState, output
}

// claimFaultStore fails the checkpoint that carries a follow-up's claim, as a
// store that is down at that moment would, and passes everything else on.
type claimFaultStore struct {
	managedStore
	failClaims atomic.Bool
	failed     atomic.Int32
}

func (s *claimFaultStore) Workbench(ctx context.Context, method string, input json.RawMessage) (json.RawMessage, error) {
	if method == "runs.protocol" && s.failClaims.Load() && strings.Contains(string(input), "claimed: request") {
		s.failed.Add(1)
		return nil, errors.New("store offline")
	}
	return s.managedStore.Workbench(ctx, method, input)
}

func TestManagedContinueDeliversFollowUpsOnTheSameRunUntilItsTurnBound(t *testing.T) {
	// Each follow-up turn stays open until the test completes it, so what the op
	// answers while a turn runs is observable.
	m := continuable(t, func(s *managedTestSession) { s.continueOpen = true })
	m.startContinuable(2)
	parked := m.awaitState("parked")
	if parked.Turn != 1 || parked.MaxTurns != 3 || parked.IdleDeadline == 0 {
		t.Fatalf("parked state: %+v", parked)
	}
	// The store has to keep saying running while the run is parked: it never
	// lets a run leave completed, so completing here would end the run for good.
	if state, provider, _ := m.savedRun(); state != "running" || provider != "running" {
		t.Fatalf("a parked run is recorded as %s/%s, not running/running", state, provider)
	}
	first, err := m.send("c1", "Also add a regression test")
	if err != nil {
		t.Fatal(err)
	}
	if first.State != "delivered" || first.Turn != 2 || first.RequestID != "c1" {
		t.Fatalf("first follow-up: %+v", first)
	}
	m.session.events <- codingagent.Event{Text: "Follow-up 1 answered", Status: "completed"}
	m.awaitState("parked")
	second, err := m.send("c2", "Now run the whole suite")
	if err != nil || second.Turn != 3 {
		t.Fatalf("second follow-up: %+v %v", second, err)
	}
	// The third and last turn is running: another follow-up can never be taken.
	_, err = m.send("c3", "One more")
	wantContinueCode(t, err, ErrContinueLimit)
	m.session.events <- codingagent.Event{Text: "Follow-up 2 answered", Status: "completed"}
	// A run on its last turn ends when that turn completes, without a stop.
	run, raw := m.finished()
	if run.State != "exited" || run.ProviderState != "completed" || m.session.closes.Load() != 1 {
		t.Fatalf("the run ended as %s/%s: %s", run.State, run.ProviderState, raw)
	}
	if got := m.session.offered(); len(got) != 2 || got[0] != "Also add a regression test" || got[1] != "Now run the whole suite" {
		t.Fatalf("the provider was offered %q", got)
	}
	// One StartTurn, two ContinueTurn, one process for all of it.
	if m.session.turns.Load() != 1 {
		t.Fatalf("turns %d: the follow-ups did not stay on the one session", m.session.turns.Load())
	}
	// The claim for each follow-up is in the saved record, before its reply.
	_, _, output := m.savedRun()
	for _, want := range []string{`[follow-up 1 claimed: request "c1"`, `[follow-up 2 claimed: request "c2"`, "Follow-up 1 answered", "Follow-up 2 answered"} {
		if !strings.Contains(output, want) {
			t.Fatalf("saved output lacks %q:\n%s", want, output)
		}
	}
	if strings.Index(output, `follow-up 1 claimed`) > strings.Index(output, "Follow-up 1 answered") {
		t.Fatalf("a claim was recorded after its turn's reply:\n%s", output)
	}
	if end := m.state(); end.State != "ended" || end.Turn != 3 {
		t.Fatalf("after the run ended: %+v", end)
	}
	_, err = m.send("c4", "After the end")
	wantContinueCode(t, err, ErrRunEnded)
}

func TestManagedContinueRefusesWhileATurnIsInFlight(t *testing.T) {
	m := continuable(t, func(s *managedTestSession) { s.leaveOpen = true })
	m.startContinuable(3)
	// Turn 0 is the moment before the first turn has begun.
	for deadline := time.Now().Add(10 * time.Second); m.state().Turn != 1; {
		if time.Now().After(deadline) {
			t.Fatalf("the first turn never began: %+v", m.state())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := m.state(); got.State != "running" {
		t.Fatalf("%+v", got)
	}
	_, err := m.send("early", "Too soon")
	if e := continueErrorOf(t, err, ErrTurnInFlight); !e.Retryable {
		t.Fatal("an in-flight refusal is retryable once the turn ends")
	}
	if len(m.session.offered()) != 0 {
		t.Fatalf("a follow-up reached the provider during a turn: %q", m.session.offered())
	}
	// The turn ends; the same follow-up is now accepted, and a second one while
	// that new turn is still running is refused again.
	m.session.continueOpen = true
	m.session.events <- codingagent.Event{Text: "first turn done", Status: "completed"}
	m.awaitState("parked")
	if _, err := m.send("early", "Too soon"); err != nil {
		t.Fatal(err)
	}
	if got := m.awaitState("running"); got.Turn != 2 {
		t.Fatalf("%+v", got)
	}
	_, err = m.send("again", "While it runs")
	wantContinueCode(t, err, ErrTurnInFlight)
	if got := m.session.offered(); len(got) != 1 {
		t.Fatalf("offered %q", got)
	}
}

func TestManagedContinueIsRefusedOnceTheRunEndedOrTheRunnerClosed(t *testing.T) {
	m := continuable(t, nil)
	m.startContinuable(3)
	m.awaitState("parked")
	m.stop()
	m.finished()
	_, err := m.send("late", "After the end")
	wantContinueCode(t, err, ErrRunEnded)
	if len(m.session.offered()) != 0 || m.session.closes.Load() != 1 {
		t.Fatalf("offered %q closes %d", m.session.offered(), m.session.closes.Load())
	}

	// A closed runner owns nothing.
	if err := m.runner.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = m.send("later", "After close")
	wantContinueCode(t, err, ErrRunEnded)
	_, err = m.runner.Continue(t.Context(), json.RawMessage(`{"id":"run"}`))
	wantContinueCode(t, err, ErrRunEnded)
}

func TestManagedContinueRunnerCloseEndsAParkedRun(t *testing.T) {
	m := continuable(t, nil)
	m.startContinuable(3)
	m.awaitState("parked")
	if err := m.runner.Close(); err != nil {
		t.Fatal(err)
	}
	run, _ := m.finished()
	if run.State != "exited" || run.ProviderState != "completed" || m.session.closes.Load() != 1 {
		t.Fatalf("Close left the parked run %s/%s", run.State, run.ProviderState)
	}
	_, err := m.send("late", "After Close")
	wantContinueCode(t, err, ErrRunEnded)
}

func TestManagedRunWithoutMaxContinuationsStillEndsAfterItsOneTurn(t *testing.T) {
	m := continuable(t, nil)
	m.start("launch")
	run, _ := m.finished()
	if run.State != "exited" || run.ProviderState != "completed" || m.session.closes.Load() != 1 {
		t.Fatalf("a run prepared as before ended as %s/%s", run.State, run.ProviderState)
	}
	_, err := m.send("c1", "No one is listening")
	wantContinueCode(t, err, ErrContinueUnsupported)
	_, err = m.runner.Continue(t.Context(), json.RawMessage(`{"id":"run"}`))
	wantContinueCode(t, err, ErrContinueUnsupported)
}

func TestManagedPrepareRefusesFollowUpsForAProviderThatCannotTakeThem(t *testing.T) {
	was := codingagent.ContinuableProviders
	codingagent.ContinuableProviders = []string{"codex"}
	t.Cleanup(func() { codingagent.ContinuableProviders = was })
	m := continuable(t, nil) // the lifecycle's provider is claude
	_, err := m.runner.Prepare(t.Context(), json.RawMessage(`{"id":"run","request_id":"launch","expected_revision":1,"max_continuations":2}`))
	e := continueErrorOf(t, err, ErrContinueUnsupported)
	if !strings.Contains(e.Message, "claude") {
		t.Fatalf("the refusal does not name the provider: %s", e.Message)
	}
	// Refused before the claim: the saved attempt is still prepared, so the same
	// run goes ahead without follow-ups.
	if run, _, err := m.runner.run(t.Context(), "run"); err != nil || run.State != "prepared" {
		t.Fatalf("the refusal consumed the attempt: %+v %v", run, err)
	}
	m.start("launch-2")
	if run, _ := m.finished(); run.State != "exited" || run.ProviderState != "completed" {
		t.Fatalf("%+v", run)
	}
}

func TestManagedContinueSessionThatRefusesIsReportedAndTheRunStaysParked(t *testing.T) {
	// Only a cause the host knows would pass is retryable: an unrecognised or
	// rejected refusal must not tell the host that a retry will help.
	cases := map[string]struct {
		refuse    error
		code      string
		retryable bool
		ends      bool
	}{
		"unsupported":       {codingagent.RefuseContinue(codingagent.ErrContinueUnsupported), ErrContinueUnsupported, false, false},
		"in flight":         {codingagent.RefuseContinue(codingagent.ErrTurnInFlight), ErrTurnInFlight, true, false},
		"session limit":     {codingagent.RefuseContinue(codingagent.ErrContinueLimit), ErrContinueLimit, false, false},
		"refused halted":    {codingagent.RefuseContinue(codingagent.ErrContinuationHalted), ErrContinueUnresolved, false, true},
		"no completed turn": {codingagent.RefuseContinue(codingagent.ErrNoCompletedTurn), ErrContinueUnsupported, false, false},
		"provider rejected": {codingagent.RefuseContinue(codingagent.ErrProviderRejected), ErrDependency, false, false},
		"stopping":          {codingagent.RefuseContinue(context.Canceled), ErrRunEnded, false, true},
		"unrecognised":      {codingagent.RefuseContinue(errors.New("something new")), ErrDependency, false, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m := continuable(t, func(s *managedTestSession) {
				s.continueErr = func(n int) error {
					if n == 1 {
						return c.refuse
					}
					return nil
				}
			})
			m.startContinuable(2)
			m.awaitState("parked")
			_, err := m.send("c1", "Try it")
			if e := continueErrorOf(t, err, c.code); e.Retryable != c.retryable {
				t.Fatalf("retryable %v, want %v: %+v", e.Retryable, c.retryable, e)
			}
			if c.ends {
				// A halted or stopping session cannot take another; the run is over.
				m.awaitState("ended")
				return
			}
			// Nothing was sent, so the slot is back and the same request id is
			// accepted again.
			if got := m.awaitState("parked"); got.Turn != 1 {
				t.Fatalf("a refusal consumed a turn: %+v", got)
			}
			res, err := m.send("c1", "Try it")
			if err != nil || res.State != "delivered" || res.Turn != 2 {
				t.Fatalf("the retry after a refusal: %+v %v", res, err)
			}
			if got := m.session.offered(); len(got) != 2 {
				t.Fatalf("offered %q", got)
			}
		})
	}
}

// A follow-up that was refused having written nothing leaves the rate window as
// it was: the host may offer it again at once, and is not told to wait out an
// interval that no delivery started.
func TestManagedContinueRefusedFollowUpDoesNotUseUpTheRateWindow(t *testing.T) {
	m := continuable(t, func(s *managedTestSession) {
		s.continueErr = func(n int) error {
			if n == 1 {
				return codingagent.RefuseContinue(codingagent.ErrTurnInFlight)
			}
			return nil
		}
	})
	m.startContinuable(2)
	m.awaitState("parked")
	continueMinInterval = time.Hour
	_, err := m.send("c1", "Refused first")
	wantContinueCode(t, err, ErrTurnInFlight)
	m.awaitState("parked")
	if res, err := m.send("c1", "Refused first"); err != nil || res.State != "delivered" {
		t.Fatalf("the retry right after a refusal: %+v %v", res, err)
	}
	// A delivery does use it up.
	m.awaitState("parked")
	_, err = m.send("c2", "Too soon after a delivery")
	wantContinueCode(t, err, ErrContinueRate)
}

// A host that retries a refusal that cannot succeed is held to the rate window
// like any other caller: only a refusal the host may usefully offer again gives
// the window back. Otherwise a host looping on E_CONTINUE_UNSUPPORTED or
// E_DEPENDENCY is never rate-limited and writes a note into the run's saved
// output every time.
func TestManagedContinueNonRetryableRefusalKeepsTheRateWindowAndWritesOneNote(t *testing.T) {
	for name, c := range map[string]struct {
		refusal error
		code    string
	}{
		"unsupported":       {codingagent.RefuseContinue(codingagent.ErrContinueUnsupported), ErrContinueUnsupported},
		"provider rejected": {codingagent.RefuseContinue(codingagent.ErrProviderRejected), ErrDependency},
		"unrecognised":      {codingagent.RefuseContinue(errors.New("something new")), ErrDependency},
	} {
		t.Run(name, func(t *testing.T) {
			m := continuable(t, func(s *managedTestSession) {
				s.continueErr = func(int) error { return c.refusal }
			})
			m.startContinuable(3)
			m.awaitState("parked")
			continueMinInterval = time.Hour
			_, err := m.send("c1", "Cannot be taken")
			if e := continueErrorOf(t, err, c.code); e.Retryable {
				t.Fatalf("the refusal must not be retryable: %+v", e)
			}
			m.awaitState("parked")
			// The host retries, with the same id and with new ones: each is turned
			// away by the rate window before it reaches the provider or the record.
			for _, id := range []string{"c1", "c2", "c3", "c4", "c5"} {
				_, err := m.send(id, "Cannot be taken")
				if e := continueErrorOf(t, err, ErrContinueRate); !e.Retryable {
					t.Fatalf("a rate refusal is retryable: %+v", e)
				}
			}
			if got := m.session.offered(); len(got) != 1 {
				t.Fatalf("a rate-limited retry reached the provider: %q", got)
			}
			// A refusal's note is in the saved record from the next checkpoint on; the
			// run's end writes the last one.
			m.stop()
			m.finished()
			_, _, output := m.savedRun()
			if n := strings.Count(output, "[follow-up 1 not sent:"); n != 1 {
				t.Fatalf("the refusal left %d notes in the saved output, want 1:\n%s", n, output)
			}
		})
	}
}

// A provider that dies while the run is parked between turns is noticed at once.
// Nothing reads the provider while the run waits for a message, so before the
// session reported its own exit the status kept saying parked until the idle
// budget (ten minutes here) ran out, and the run then ended as completed.
func TestManagedParkedRunEndsPromptlyWhenItsProviderProcessExits(t *testing.T) {
	m := continuable(t, nil)
	m.startContinuable(3)
	m.awaitState("parked")
	m.session.die()
	// awaitState gives up after ten seconds; the idle budget is ten minutes.
	if got := m.awaitState("ended"); got.Turn != 1 {
		t.Fatalf("%+v", got)
	}
	run, raw := m.finished()
	if run.ProviderState != "failed" {
		t.Fatalf("a provider that died while parked ended the run as %s/%s, not failed: %s", run.State, run.ProviderState, raw)
	}
	var row struct {
		Item struct {
			Reason string `json:"reason"`
		} `json:"item"`
	}
	if err := json.Unmarshal(raw, &row); err != nil || !strings.Contains(row.Item.Reason, "parked between turns") {
		t.Fatalf("the reason does not say the process exited while the run was parked: %s", raw)
	}
	_, _, output := m.savedRun()
	if !strings.Contains(output, "provider process exited while the run was parked") {
		t.Fatalf("the saved output does not record the exit:\n%s", output)
	}
	_, err := m.send("late", "After the exit")
	wantContinueCode(t, err, ErrRunEnded)
	if got := m.session.offered(); len(got) != 0 || m.session.closes.Load() != 1 {
		t.Fatalf("offered %q closes %d", got, m.session.closes.Load())
	}
}

func TestManagedContinueAmbiguousDeliveryIsReportedUnresolvedAndNeverResent(t *testing.T) {
	m := continuable(t, func(s *managedTestSession) {
		s.continueErr = func(int) error { return errors.New("write |1: broken pipe") }
	})
	m.startContinuable(3)
	m.awaitState("parked")
	_, err := m.send("c1", "Maybe delivered")
	e := continueErrorOf(t, err, ErrContinueUnresolved)
	if e.Retryable || !strings.Contains(e.Message, `"c1"`) {
		t.Fatalf("%+v", e)
	}
	// The same request id answers the same way and sends nothing; so does a new
	// one, and so does the same text under another id.
	_, err = m.send("c1", "Maybe delivered")
	wantContinueCode(t, err, ErrContinueUnresolved)
	for _, id := range []string{"c2", "c3"} {
		_, err = m.send(id, "Maybe delivered")
		if err == nil {
			t.Fatal("a follow-up was accepted after an unresolved delivery")
		}
	}
	if got := m.session.offered(); len(got) != 1 {
		t.Fatalf("an ambiguous delivery was offered %d times: %q", len(got), got)
	}
	run, raw := m.finished()
	if run.State != "exited" || run.ProviderState != "unresolved" {
		t.Fatalf("an unresolved delivery ended the run as %s/%s: %s", run.State, run.ProviderState, raw)
	}
	_, _, output := m.savedRun()
	if !strings.Contains(output, `follow-up 1 claimed: request "c1"`) || !strings.Contains(output, "follow-up 1 delivery unresolved") {
		t.Fatalf("the record does not show the claim and its unresolved end:\n%s", output)
	}
	if end := m.state(); end.State != "unresolved" {
		t.Fatalf("%+v", end)
	}
}

func TestManagedContinueReplayReturnsTheFirstAnswerAndSendsNothing(t *testing.T) {
	m := continuable(t, nil)
	m.startContinuable(3)
	m.awaitState("parked")
	first, err := m.send("c1", "Once only")
	if err != nil {
		t.Fatal(err)
	}
	again, err := m.send("c1", "Once only")
	if err != nil || again != first {
		t.Fatalf("replay: %+v %v, first %+v", again, err, first)
	}
	_, err = m.send("c1", "A different message")
	wantContinueCode(t, err, ErrBadRequest)
	if got := m.session.offered(); len(got) != 1 {
		t.Fatalf("a replay sent again: %q", got)
	}
}

func TestManagedContinueBoundsTheMessageTheRateAndThePreparation(t *testing.T) {
	m := continuable(t, nil)
	m.startContinuable(4)
	m.awaitState("parked")
	for name, message := range map[string]string{
		"blank":    "  \n",
		"oversize": strings.Repeat("x", codingagent.MaxContinueMessageBytes+1),
		"nul":      "a\x00b",
	} {
		_, err := m.send("bad-"+name, message)
		wantContinueCode(t, err, ErrBadRequest)
	}
	for name, params := range map[string]string{
		"no request id": `{"id":"run","message":"x"}`,
		"unknown field": `{"id":"run","request_id":"r","message":"x","resume":true}`,
		"no run":        `{"request_id":"r","message":"x"}`,
		"not json":      `[1]`,
	} {
		if _, err := m.runner.Continue(t.Context(), json.RawMessage(params)); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	if len(m.session.offered()) != 0 {
		t.Fatalf("a refused follow-up reached the provider: %q", m.session.offered())
	}
	// Rate: a second follow-up inside the interval is refused, retryably, and
	// accepted once the interval has passed.
	continueMinInterval = time.Hour
	if _, err := m.send("c1", "Fast"); err != nil {
		t.Fatal(err)
	}
	m.awaitState("parked")
	_, err := m.send("c2", "Faster")
	if e := continueErrorOf(t, err, ErrContinueRate); !e.Retryable {
		t.Fatal("a rate refusal is retryable")
	}
	continueMinInterval = 0
	if _, err := m.send("c2", "Faster"); err != nil {
		t.Fatal(err)
	}
	if got := m.session.offered(); len(got) != 2 {
		t.Fatalf("offered %q", got)
	}
	for _, bad := range []string{"-1", "9", `"x"`, "1.5"} {
		_, err := m.runner.Prepare(t.Context(), json.RawMessage(`{"id":"other","request_id":"p","expected_revision":1,"max_continuations":`+bad+`}`))
		if err == nil {
			t.Fatalf("max_continuations %s was accepted", bad)
		}
	}
}

func TestManagedContinueRetryOfPrepareMustRepeatTheMaxContinuations(t *testing.T) {
	m := continuable(t, nil)
	if _, err := m.runner.Prepare(t.Context(), json.RawMessage(`{"id":"run","request_id":"launch","expected_revision":1,"max_continuations":2}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.runner.Prepare(t.Context(), json.RawMessage(`{"id":"run","request_id":"launch","expected_revision":1,"max_continuations":3}`)); err == nil {
		t.Fatal("a retry changed what the run may do")
	}
	if _, err := m.runner.Prepare(t.Context(), json.RawMessage(`{"id":"run","request_id":"launch","expected_revision":1}`)); err == nil {
		t.Fatal("a retry dropped the follow-ups the run was prepared for")
	}
}

func TestManagedParkedRunEndsWhenItsIdleBudgetRunsOut(t *testing.T) {
	was := managedIdleBudget
	managedIdleBudget = 150 * time.Millisecond
	t.Cleanup(func() { managedIdleBudget = was })
	m := continuable(t, nil)
	m.startContinuable(3)
	run, _ := m.finished()
	if run.State != "exited" || run.ProviderState != "completed" || m.session.closes.Load() != 1 {
		t.Fatalf("an idle run ended as %s/%s", run.State, run.ProviderState)
	}
	_, err := m.send("late", "Too late")
	wantContinueCode(t, err, ErrRunEnded)
}

func TestManagedFollowUpTurnKeepsEveryApprovalBinding(t *testing.T) {
	m := continuable(t, func(s *managedTestSession) { s.continueApproval = "n:43" })
	m.startContinuable(2)
	m.awaitState("parked")
	if _, err := m.send("c1", "Run the diff"); err != nil {
		t.Fatal(err)
	}
	rows := m.awaitDecisions(1)
	if len(rows) != 1 || rows[0].Request != "n:43" {
		t.Fatalf("the follow-up turn's approval is not durable: %+v", rows)
	}
	var page struct {
		Items []struct {
			Turn    string `json:"provider_turn_id"`
			Thread  string `json:"provider_thread_id"`
			Run     string `json:"run_id"`
			Owner   string `json:"owner_id"`
			Session string `json:"session_id"`
			Digest  string `json:"payload_digest"`
		} `json:"items"`
	}
	if err := json.Unmarshal(enhancementCall(t, m.client, "decisions.list", `{"run_id":"run"}`), &page); err != nil {
		t.Fatal(err)
	}
	row := page.Items[0]
	// The approval is bound to the follow-up turn the provider raised it in, to
	// this attempt and its owner and session, and to the payload digest the
	// human is asked to approve.
	if row.Turn != "turn-2" || row.Thread != "thread" || row.Run != "run" || row.Owner == "" || row.Session == "" || row.Digest != rows[0].Digest {
		t.Fatalf("approval binding: %+v", row)
	}
	m.allow(rows[0])
	m.awaitState("parked")
	if m.session.replies.Load() != 1 {
		t.Fatalf("the answer reached the provider %d times", m.session.replies.Load())
	}
	if got := m.decisions(); len(got) != 1 || got[0].State != "resolved" {
		t.Fatalf("%+v", got)
	}
	m.stop()
	if run, _ := m.finished(); run.State != "exited" || run.ProviderState != "completed" {
		t.Fatalf("%+v", run)
	}
}

func TestManagedParkSettlesTheFinishedTurnsCallbacks(t *testing.T) {
	// A callback left open when the turn completes is cancelled in the store at
	// the park, so a parked run holds none and no later answer can reach it.
	m := continuable(t, func(s *managedTestSession) {
		s.withRequest = true
		s.requestIDs = []string{"n:1"}
		s.completeAfter = 50 * time.Millisecond
	})
	m.startContinuable(1)
	m.awaitDecisions(1)
	m.awaitState("parked")
	got := m.decisions()
	if len(got) != 1 || got[0].State != "cancelled" {
		t.Fatalf("an open callback survived the park: %+v", got)
	}
	m.runner.mu.Lock()
	job := m.runner.jobs["run"]
	m.runner.mu.Unlock()
	if job == nil {
		t.Fatal("the run's job is gone")
	}
	if len(job.callbacks) != 0 {
		t.Fatalf("a parked run still holds %d callbacks", len(job.callbacks))
	}
}

// Stopping a run while a follow-up turn is running is an interruption, as it is
// for the first turn; only a stop between turns is a clean end.
func TestManagedStopDuringAFollowUpTurnIsAnInterruption(t *testing.T) {
	m := continuable(t, func(s *managedTestSession) { s.continueOpen = true })
	m.startContinuable(2)
	m.awaitState("parked")
	if _, err := m.send("c1", "Keep working"); err != nil {
		t.Fatal(err)
	}
	m.awaitState("running")
	m.stop()
	run, _ := m.finished()
	if run.State != "exited" || run.ProviderState != "interrupted" {
		t.Fatalf("a stop mid-turn ended the run as %s/%s", run.State, run.ProviderState)
	}
}

func TestManagedContinueSecondFollowUpTurnFailingEndsTheRunAsFailed(t *testing.T) {
	m := continuable(t, func(s *managedTestSession) { s.continueOpen = true })
	m.startContinuable(2)
	m.awaitState("parked")
	if _, err := m.send("c1", "Try"); err != nil {
		t.Fatal(err)
	}
	m.session.events <- codingagent.Event{Text: "could not", Status: "failed"}
	run, _ := m.finished()
	if run.ProviderState != "failed" {
		t.Fatalf("a failed follow-up turn ended the run as %s/%s", run.State, run.ProviderState)
	}
}

// The durable claim is written before the provider is touched, not merely
// somewhere in the final record: at the moment ContinueTurn is entered, the
// store already holds the claim line. Moving the checkpoint below the write
// leaves the store without it at that moment, and this fails.
func TestManagedContinueClaimIsDurableBeforeTheProviderWrite(t *testing.T) {
	var seen struct {
		sync.Mutex
		output string
		err    error
		calls  int
	}
	var m *managedLifecycle
	m = continuable(t, func(s *managedTestSession) {
		s.onContinue = func() {
			_, _, output, err := m.readSavedRun()
			seen.Lock()
			defer seen.Unlock()
			seen.output, seen.err = output, err
			seen.calls++
		}
	})
	m.startContinuable(2)
	m.awaitState("parked")
	if _, err := m.send("c1", "Record me first"); err != nil {
		t.Fatal(err)
	}
	seen.Lock()
	defer seen.Unlock()
	if seen.calls != 1 || seen.err != nil {
		t.Fatalf("ContinueTurn ran %d times and the store read gave %v", seen.calls, seen.err)
	}
	if !strings.Contains(seen.output, `[follow-up 1 claimed: request "c1"`) {
		t.Fatalf("the store did not hold the claim when the message was written:\n%s", seen.output)
	}
}

// A claim the store will not acknowledge sends nothing: the provider is not
// touched, the op says so, the run stays parked on its first turn, and the same
// request id is accepted once the store is back.
func TestManagedContinueClaimThatCannotBeRecordedSendsNothing(t *testing.T) {
	m := continuable(t, nil)
	fault := &claimFaultStore{managedStore: m.runner.client}
	m.runner.client = fault
	m.startContinuable(2)
	m.awaitState("parked")

	fault.failClaims.Store(true)
	_, err := m.send("c1", "Cannot be recorded")
	e := continueErrorOf(t, err, ErrDependency)
	if !strings.Contains(e.Message, "nothing was sent") {
		t.Fatalf("the refusal does not say nothing was sent: %q", e.Message)
	}
	if fault.failed.Load() == 0 {
		t.Fatal("the claim checkpoint never reached the store")
	}
	if got := m.session.offered(); len(got) != 0 {
		t.Fatalf("a follow-up reached the provider without a durable claim: %q", got)
	}
	if got := m.awaitState("parked"); got.Turn != 1 {
		t.Fatalf("a claim that failed consumed a turn: %+v", got)
	}

	fault.failClaims.Store(false)
	res, err := m.send("c1", "Cannot be recorded")
	if err != nil || res.State != "delivered" || res.Turn != 2 {
		t.Fatalf("the same request id after the store recovered: %+v %v", res, err)
	}
	if got := m.session.offered(); len(got) != 1 {
		t.Fatalf("offered %q", got)
	}
}
