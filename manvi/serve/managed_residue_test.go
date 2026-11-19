package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

// The turn can end between ValidateResponse and the write: the adapter's own
// preflight in Respond then withdraws the callback and answers
// ErrCallbackWithdrawn. That is a callback gone, exactly as when ValidateResponse
// says it; the run carries on to the provider's status. It used to end the run
// failed, because only the first of the two was read as benign.
func TestManagedAnswerThatRacesTheTurnEndAfterValidationDoesNotFailTheRun(t *testing.T) {
	var m *managedLifecycle
	m = newManagedLifecycle(t, func(ctx context.Context, o codingagent.Options) *managedTestSession {
		s := m.fixtureSession(ctx, o)
		s.expires, s.completeAfter = time.Minute, 1500*time.Millisecond
		s.respondErr = codingagent.ErrCallbackWithdrawn
		return s
	})
	m.start("launch")
	m.allow(m.awaitDecisions(1)[0])
	run, raw := m.finished()
	if run.ProviderState != "completed" || m.session.replies.Load() != 0 {
		t.Fatalf("an answer withdrawn at delivery failed the run or was delivered: provider_state=%s replies=%d (%.300s)", run.ProviderState, m.session.replies.Load(), raw[strings.Index(string(raw), `"reason"`):])
	}
	if rows := m.decisions(); len(rows) != 1 || rows[0].State != "cancelled" || !strings.Contains(rows[0].Reason, "withdrew") {
		t.Fatalf("withdrawn callback recorded as %+v", rows)
	}
}

// failingResolve is a store whose receipt for a decision cannot be written.
type failingResolve struct {
	managedStore
	err error
}

func (f failingResolve) Workbench(ctx context.Context, method string, input json.RawMessage) (json.RawMessage, error) {
	if method == "decisions.resolve" {
		return nil, f.err
	}
	return f.managedStore.Workbench(ctx, method, input)
}

// A receipt that could not be written for a withdrawn callback ends the run with
// that error. It used to be overwritten by the next statement, the read of saved
// decisions, which returns nil when none is waiting: the run went on with a
// decision row still live.
func TestManagedWithdrawalReceiptFailureIsNotOverwrittenByTheDecisionRead(t *testing.T) {
	errReceipt := errors.New("decision store refused the receipt")
	var m *managedLifecycle
	m = newManagedLifecycle(t, func(ctx context.Context, o codingagent.Options) *managedTestSession {
		s := m.fixtureSession(ctx, o)
		s.expires = time.Minute
		// A wake is pending when the withdrawal is read, as when a person saved a
		// decision in the same moment.
		s.onNext = func(e codingagent.Event) {
			if e.CancelledID != "" {
				if err := m.runner.Wake(nil); err != nil {
					t.Error(err)
				}
			}
		}
		return s
	})
	m.runner.client = failingResolve{managedStore: m.runner.client, err: errReceipt}
	m.start("launch")
	rows := m.awaitDecisions(1)
	m.session.events <- codingagent.Event{CancelledID: rows[0].Request}
	// The provider would go on to finish; the run must not get that far.
	m.session.events <- codingagent.Event{Text: "Fixture result", Status: "completed"}
	run, raw := m.finished()
	if run.ProviderState != "failed" || !strings.Contains(string(raw), errReceipt.Error()) {
		t.Fatalf("a failed withdrawal receipt did not end the run: provider_state=%s (%.300s)", run.ProviderState, raw[strings.Index(string(raw), `"reason"`):])
	}
}

// Only the withdrawal itself is benign: an error joined to it, or wrapped
// beside another, is a different failure and must reach the caller.
func TestOnlyWithdrawnIsExactlyTheWithdrawal(t *testing.T) {
	other := errors.New("store down")
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"bare", codingagent.ErrCallbackWithdrawn, true},
		{"one wrap of context", fmt.Errorf("answering: %w", codingagent.ErrCallbackWithdrawn), true},
		{"joined with another error", errors.Join(codingagent.ErrCallbackWithdrawn, other), false},
		{"joined the other way", errors.Join(other, codingagent.ErrCallbackWithdrawn), false},
		{"two wraps in one", fmt.Errorf("%w; %w", codingagent.ErrCallbackWithdrawn, other), false},
		{"another error", other, false},
		{"nil", nil, false},
	} {
		if got := onlyWithdrawn(c.err); got != c.want {
			t.Errorf("%s: onlyWithdrawn = %v, want %v", c.name, got, c.want)
		}
	}
}
