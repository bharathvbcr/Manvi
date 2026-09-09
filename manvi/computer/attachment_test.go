package computer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

type attachmentPipe struct {
	mu       sync.Mutex
	client   *Client
	replies  []pendingResult
	requests []request
}

func (p *attachmentPipe) Write(raw []byte) (int, error) {
	var r request
	if err := json.Unmarshal(raw, &r); err != nil {
		return 0, err
	}
	p.mu.Lock()
	p.requests = append(p.requests, r)
	index := len(p.requests) - 1
	if index >= len(p.replies) {
		p.mu.Unlock()
		return 0, io.ErrUnexpectedEOF
	}
	reply := p.replies[index]
	p.mu.Unlock()
	p.client.mu.Lock()
	ch := p.client.pending[r.ID]
	p.client.mu.Unlock()
	ch <- reply
	return len(raw), nil
}
func (p *attachmentPipe) Close() error { return nil }

func attachmentClient(replies ...pendingResult) (*Client, *attachmentPipe) {
	p := &attachmentPipe{replies: replies}
	done := make(chan struct{})
	close(done)
	c := &Client{in: p, write: make(chan struct{}, 1), pending: map[string]chan pendingResult{}, done: done}
	p.client = c
	return c, p
}

func nativeAttachmentFailure(code, delivery string) pendingResult {
	return pendingResult{response: response{Error: &BrokerError{Code: code, Delivery: delivery}}}
}

func TestAttachmentReadinessRetriesOnlyMissingWindowUntilOneValidSession(t *testing.T) {
	c, p := attachmentClient(nativeAttachmentFailure("window_missing", "not_sent"),
		pendingResult{response: response{OK: true, Result: json.RawMessage(`{"session_id":"attached","epoch":1,"window":{"pid":42}}`)}})
	s, admission, err := c.AttachFocusedReady(context.Background(), "run", 42, []Selector{{Role: "button", Name: "Search"}})
	if err != nil || s.ID != "attached" || admission.Attempts != 2 || admission.LastTransientCode != "window_missing" {
		t.Fatalf("readiness failed: %+v %+v %v", s, admission, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.requests) != 2 || p.requests[0].ID == p.requests[1].ID {
		t.Fatal("attempt IDs not unique")
	}
	for _, r := range p.requests {
		var payload struct {
			PID      uint32
			Focus    bool
			ReadOnly []Selector `json:"read_only_targets"`
		}
		if err := json.Unmarshal(r.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if r.Op != "attach" || r.RunID != "run" || r.SessionID != "" || payload.PID != 42 || !payload.Focus || len(payload.ReadOnly) != 1 || payload.ReadOnly[0].Name != "Search" {
			t.Fatalf("readiness changed scope or trusted input policy: %+v", r)
		}
	}
}
func TestAttachWithoutReadOnlyTargetsSendsEmptyArray(t *testing.T) {
	c, p := attachmentClient(pendingResult{response: response{OK: true, Result: json.RawMessage(`{"session_id":"attached","epoch":1,"window":{"pid":42}}`)}})
	if _, _, err := c.AttachFocusedReady(context.Background(), "run", 42, nil); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var payload struct {
		ReadOnly json.RawMessage `json:"read_only_targets"`
	}
	if err := json.Unmarshal(p.requests[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if string(payload.ReadOnly) != "[]" {
		t.Fatalf("native Vec requires [] for no allowance, got %s", payload.ReadOnly)
	}
}

func TestAttachmentReadinessBoundsPersistentAmbiguity(t *testing.T) {
	calls := 0
	_, admission, err := waitForAttachment(context.Background(), func(context.Context) (Session, error) {
		calls++
		return Session{RunID: "run"}, &BrokerError{Code: "window_ambiguous", Delivery: "not_sent"}
	}, attachmentAttempts, time.Millisecond)
	var native *BrokerError
	if calls != attachmentAttempts || admission.Attempts != attachmentAttempts || !errors.As(err, &native) || native.Code != "window_ambiguous" {
		t.Fatalf("unbounded or incorrectly classified exhaustion: %d %+v %v", calls, admission, err)
	}
}

func TestAttachmentReadinessWaitsForRegistrationButNotDisabledAccessibility(t *testing.T) {
	for _, code := range []string{"accessibility_pending", "accessibility_inactive"} {
		t.Run(code, func(t *testing.T) {
			calls := 0
			s, admission, err := waitForAttachment(context.Background(), func(context.Context) (Session, error) {
				calls++
				if calls == 1 {
					return Session{}, &BrokerError{Code: code, Delivery: "not_sent"}
				}
				return Session{ID: "ready", Epoch: 1}, nil
			}, 3, time.Millisecond)
			if code == "accessibility_pending" {
				if err != nil || s.ID != "ready" || calls != 2 || admission.LastTransientCode != code {
					t.Fatalf("registration readiness: %+v %+v %v", s, admission, err)
				}
			} else if err == nil || calls != 1 {
				t.Fatalf("inactive accessibility retried: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestAttachmentReadinessCancellationInterruptsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempted := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, _, err := waitForAttachment(ctx, func(context.Context) (Session, error) {
			close(attempted)
			return Session{}, &BrokerError{Code: "window_missing", Delivery: "not_sent"}
		}, attachmentAttempts, time.Hour)
		finished <- err
	}()
	<-attempted
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled attachment waited for its backoff")
	}
}

func TestAttachmentReadinessNeverRetriesUncertainOrInconsistentResults(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply pendingResult
	}{
		{"unknown", nativeAttachmentFailure("window_missing", "unknown")},
		{"sent", nativeAttachmentFailure("window_missing", "sent")},
		{"missing_delivery", nativeAttachmentFailure("window_ambiguous", "")},
		{"permission", nativeAttachmentFailure("accessibility_permission", "not_sent")},
		{"transport", pendingResult{err: io.ErrUnexpectedEOF}},
		{"cancelled", pendingResult{err: context.Canceled}},
		{"invalid_success", pendingResult{response: response{OK: true, Result: json.RawMessage(`{"session_id":"possibly-attached","epoch":1,"window":{"pid":99}}`)}}},
		{"contradictory_failure", pendingResult{response: response{Error: &BrokerError{Code: "window_missing", Delivery: "not_sent"}, Result: json.RawMessage(`{"session_id":"possibly-attached"}`)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, p := attachmentClient(tc.reply)
			_, admission, err := c.AttachFocusedReady(context.Background(), "run", 42, nil)
			p.mu.Lock()
			defer p.mu.Unlock()
			if err == nil || admission.Attempts != 1 || len(p.requests) != 1 {
				t.Fatalf("retried unsafe outcome: %+v %v", admission, err)
			}
		})
	}
}

func TestAttachmentReadinessHonorsAlreadyCancelledAndShorterDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, p := attachmentClient()
	_, admission, err := c.AttachFocusedReady(ctx, "run", 42, nil)
	if !errors.Is(err, context.Canceled) || admission.Attempts != 0 || len(p.requests) != 0 {
		t.Fatal("cancelled admission dispatched")
	}
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	c, p = attachmentClient(nativeAttachmentFailure("window_missing", "not_sent"))
	_, admission, err = c.AttachFocusedReady(ctx, "run", 42, nil)
	if !errors.Is(err, context.DeadlineExceeded) || admission.Attempts != 1 || len(p.requests) != 1 {
		t.Fatalf("ignored earlier caller deadline: %+v %v", admission, err)
	}
}
