package gemini

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"manvi/llm"
	"manvi/llm/adaptertest"
)

// overloadedThenFine is the shape the live endpoint actually produces under
// load: status 200, the stream opens, and only then does it say no.
const overloadedFrames = `event: interaction.created
data: {"interaction":{"id":"v1_x","status":"in_progress"},"event_type":"interaction.created"}

event: interaction.status_update
data: {"interaction_id":"v1_x","status":"in_progress","event_type":"interaction.status_update"}

event: error
data: {"error":{"message":"gemini-3.7-flash is currently experiencing high demand, spikes in demand are usually temporary. Please try again later.","code":"invalid_request"},"event_type":"error"}

`

// flakyServer fails the first n requests with an in-stream error frame and then
// serves the real body.
func flakyServer(t *testing.T, failures int32, body string) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	// failures counts how many opening requests get the overload frame; the
	// given body is served from then on. Zero means the body is always served,
	// which is what a test asserting on the body's own failure wants.
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		out := body
		if n <= failures {
			out = overloadedFrames
		}
		for i := 0; i < len(out); i += 9 {
			end := i + 9
			if end > len(out) {
				end = len(out)
			}
			w.Write([]byte(out[i:end]))
			flusher.Flush()
		}
	}))
	t.Cleanup(s.Close)
	return s, &calls
}

// TestAnOverloadReportedInTheStreamIsRetried.
//
// The transport classifies on HTTP status, and this failure arrives at 200 —
// so a deliberate four-attempt retry policy retried none of the failures this
// provider actually produces. Measured live: a five-scenario benchmark lost
// every scenario to errors of exactly this shape, two of them plainly
// transient ("currently experiencing high demand") and one proved transient by
// replaying the identical request afterwards and having it accepted.
func TestAnOverloadReportedInTheStreamIsRetried(t *testing.T) {
	server, calls := flakyServer(t, 2, happyStream)
	adapter := New(server.URL, adaptertest.Secret("k"))
	adapter.client.Retry.BaseDelay = 0

	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatalf("a transient in-stream failure was fatal: %v", err)
	}
	chunks, resp, err := adaptertest.Drain(stream)
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Errorf("server saw %d request(s), want 3 — the failure was not retried", got)
	}
	if got := adaptertest.TextOf(chunks); got != "Reading the file." {
		t.Errorf("text = %q; the retried stream did not decode whole", got)
	}
	if len(resp.Message.ToolCalls()) != 1 {
		t.Errorf("calls = %d, want 1", len(resp.Message.ToolCalls()))
	}
}

// TestAPermanentInStreamFailureIsNotRetried.
//
// The direction the message match fails in is deliberate — unrecognised means
// retry, because treating an unknown transient as permanent is what discarded
// whole turns. But a request the server has told us is malformed must not be
// sent four times: the answer will not change and the report should name the
// defect rather than an exhausted retry budget.
func TestAPermanentInStreamFailureIsNotRetried(t *testing.T) {
	const malformed = `event: interaction.created
data: {"event_type":"interaction.created"}

event: error
data: {"error":{"message":"Unknown parameter 'id' at 'input[2]'.","code":"invalid_request"},"event_type":"error"}

`
	server, calls := flakyServer(t, 0, malformed)
	adapter := New(server.URL, adaptertest.Secret("k"))
	adapter.client.Retry.BaseDelay = 0

	_, err := adapter.Stream(adaptertest.Ctx(), request())
	if err == nil {
		t.Fatal("a malformed request was accepted")
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("server saw %d request(s), want 1 — a permanent failure was retried", got)
	}
	if !strings.Contains(err.Error(), "Unknown parameter") {
		t.Errorf("the error does not carry what the server said: %v", err)
	}
}

// TestPreflightPutsBackEverythingItRead.
//
// Inspecting the opening of a stream must not consume it. A frame eaten by the
// inspection is a chunk the caller never sees, and the loss would be silent.
func TestPreflightPutsBackEverythingItRead(t *testing.T) {
	adapter, _ := adapterFor(t, happyStream)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	chunks, resp, err := adaptertest.Drain(stream)
	if err != nil {
		t.Fatal(err)
	}
	if got := adaptertest.TextOf(chunks); got != "Reading the file." {
		t.Errorf("text = %q — preflight swallowed part of the stream", got)
	}
	if got := adaptertest.ReasoningOf(chunks); got != "planning" {
		t.Errorf("reasoning = %q — the first content frame was consumed", got)
	}
	if resp.Usage.InputTokens != 19 {
		t.Errorf("usage = %+v; the completion frame was lost", resp.Usage)
	}
}

// TestAnErrorAfterContentIsNotRetried is the safety bound on all of this.
//
// Retrying is only sound while nothing has reached the caller. Once content has
// streamed, re-issuing the request would deliver the first half twice, so the
// scan has to stop at the first non-preamble frame whatever comes after it.
func TestAnErrorAfterContentIsNotRetried(t *testing.T) {
	const lateFailure = `event: interaction.created
data: {"event_type":"interaction.created"}

event: step.delta
data: {"event_type":"step.delta","step":{"type":"model_output","content":[{"type":"text","text":"partial"}]}}

event: error
data: {"error":{"message":"backend went away","code":"internal"},"event_type":"error"}

`
	server, calls := flakyServer(t, 0, lateFailure)
	adapter := New(server.URL, adaptertest.Secret("k"))
	adapter.client.Retry.BaseDelay = 0

	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatalf("a stream that had begun producing content was rejected up front: %v", err)
	}
	if _, _, err := adaptertest.Drain(stream); err == nil {
		t.Error("a failure after content was not reported at all")
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("server saw %d request(s), want 1 — content had already been read, "+
			"so a retry would deliver it twice", got)
	}
}

// TestTheStopReasonSurvivesAStatusUpdate guards the event the live stream sends
// on every interaction and this decoder had never heard of. Its status sits at
// the top level rather than inside `interaction`, and an in_progress frame
// arriving after a completion must not undo it.
func TestTheStopReasonSurvivesAStatusUpdate(t *testing.T) {
	const body = `event: interaction.completed
data: {"event_type":"interaction.completed","interaction":{"status":"completed","usage":{"total_input_tokens":5,"total_output_tokens":2}}}

event: interaction.status_update
data: {"interaction_id":"v1_x","status":"in_progress","event_type":"interaction.status_update"}

event: done
data: [DONE]

`
	adapter, _ := adapterFor(t, body)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	_, resp, err := adaptertest.Drain(stream)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != llm.StopEndTurn {
		t.Errorf("stop = %q, want end_turn — a progress frame overwrote a settled stop reason",
			resp.StopReason)
	}
}
