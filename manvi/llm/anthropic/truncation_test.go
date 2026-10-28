package anthropic

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
	"github.com/bharathvbcr/Manvi/manvi/llm/transport"
)

const startFrame = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":42}}}

`

// scripted serves one body per attempt and counts attempts. The last body
// repeats if the client keeps asking.
func scripted(t *testing.T, bodies ...string) (*Adapter, *atomic.Int32) {
	t.Helper()
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		n := int(attempts.Add(1)) - 1
		if n >= len(bodies) {
			n = len(bodies) - 1
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, bodies[n])
	}))
	t.Cleanup(server.Close)
	adapter := New(server.URL, adaptertest.Secret("sk-test-key-value"))
	adapter.client.Retry.BaseDelay = 0
	return adapter, &attempts
}

// drainNext reads chunks until the stream ends and returns what ended it.
func drainNext(t *testing.T, s llm.Stream) error {
	t.Helper()
	for {
		if _, err := s.Next(); err != nil {
			return err
		}
	}
}

// TestEOFBeforeMessageStopIsAnErrorNotASuccess: a connection that closes
// mid-message ends the reader the way a finished message does. It must settle
// as a retryable failure, never as text.
func TestEOFBeforeMessageStopIsAnErrorNotASuccess(t *testing.T) {
	body := startFrame + `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"The fix is to"}}

`
	adapter, _ := scripted(t, body)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	err = drainNext(t, stream)
	if errors.Is(err, io.EOF) || err == nil {
		t.Fatalf("a stream with no message_stop ended as %v, want a failure", err)
	}
	var te *transport.Error
	if !errors.As(err, &te) || !te.Retryable() || !te.InStream() {
		t.Fatalf("truncation = %#v, want a retryable in-stream transport error", err)
	}
	if _, rerr := stream.Response(); rerr == nil {
		t.Fatal("Response settled a truncated stream")
	}
	if usage, ok := stream.(interface{ UsageSoFar() (llm.Usage, bool) }); !ok {
		t.Fatal("stream does not report usage so far")
	} else if u, ok := usage.UsageSoFar(); !ok || u.InputTokens != 42 {
		t.Fatalf("UsageSoFar = %+v, %v; the prompt usage received must survive the truncation", u, ok)
	}
}

// TestEOFMidToolCallIsNotDispatched is the half-received call: the block
// opened, its arguments stopped halfway, and the connection closed.
func TestEOFMidToolCallIsNotDispatched(t *testing.T) {
	for name, body := range map[string]string{
		"arguments cut off": startFrame + `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"devcouncil_read_file"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"src/"}}

`,
		"no arguments at all": startFrame + `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"devcouncil_read_file"}}

`,
		// message_stop arrived but the block was never closed: the framing event
		// for the call's arguments was lost, so an empty list is not believable.
		"message_stop without content_block_stop": startFrame + `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"devcouncil_read_file"}}

event: message_stop
data: {"type":"message_stop"}

`,
	} {
		t.Run(name, func(t *testing.T) {
			adapter, _ := scripted(t, body)
			stream, err := adapter.Stream(adaptertest.Ctx(), request())
			if err != nil {
				t.Fatal(err)
			}
			_, resp, err := adaptertest.Drain(stream)
			if err == nil {
				t.Fatalf("settled with %d tool call(s), stop=%s; a half-received call must never be dispatched",
					len(resp.Message.ToolCalls()), resp.StopReason)
			}
		})
	}
}

// TestASealedCallWithNoArgumentsIsStillLegitimate guards the other direction:
// a tool that takes no parameters streams an empty partial_json and a
// content_block_stop, and that is the one way {} may appear.
func TestASealedCallWithNoArgumentsIsStillLegitimate(t *testing.T) {
	body := startFrame + `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"devcouncil_read_file"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}

event: message_stop
data: {"type":"message_stop"}

`
	adapter, _ := scripted(t, body)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	_, resp, err := adaptertest.Drain(stream)
	if err != nil {
		t.Fatal(err)
	}
	calls := resp.Message.ToolCalls()
	if len(calls) != 1 || string(calls[0].Arguments) != "{}" {
		t.Fatalf("calls = %+v", calls)
	}
}

const overloadedFrame = `event: error
data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}

`

const okBody = startFrame + `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}

event: message_stop
data: {"type":"message_stop"}

`

// TestOverloadedErrorBeforeContentIsRetried: the documented in-stream
// overloaded_error is the 529 of a streaming response and is retried like one.
func TestOverloadedErrorBeforeContentIsRetried(t *testing.T) {
	for name, first := range map[string]string{
		"first event":     overloadedFrame,
		"after ping":      "event: ping\ndata: {\"type\": \"ping\"}\n\n" + overloadedFrame,
		"after msg_start": startFrame + overloadedFrame,
		"api_error":       strings.Replace(overloadedFrame, "overloaded_error", "api_error", 1),
		"CRLF framed":     strings.ReplaceAll(overloadedFrame, "\n", "\r\n"),
		"lone CR framed":  strings.ReplaceAll(overloadedFrame, "\n", "\r"),
		"after a BOM":     "\xEF\xBB\xBF" + overloadedFrame,
	} {
		t.Run(name, func(t *testing.T) {
			adapter, attempts := scripted(t, first, okBody)
			stream, err := adapter.Stream(adaptertest.Ctx(), request())
			if err != nil {
				t.Fatalf("Stream: %v (attempts=%d)", err, attempts.Load())
			}
			_, resp, err := adaptertest.Drain(stream)
			if err != nil {
				t.Fatal(err)
			}
			if got := attempts.Load(); got != 2 {
				t.Fatalf("attempts = %d, want 2", got)
			}
			if resp.Message.Text() != "hello" {
				t.Fatalf("text = %q; the retried attempt's content, once, was expected", resp.Message.Text())
			}
		})
	}
}

// TestAWholeResponseInOtherEventStreamFramingDecodes: the event-stream grammar
// allows a leading BOM and a bare CR as a line ending, and the ping events
// anthropic interleaves arrive in whichever the proxy in front of it chose.
func TestAWholeResponseInOtherEventStreamFramingDecodes(t *testing.T) {
	ping := "event: ping\ndata: {\"type\": \"ping\"}\n\n"
	for name, body := range map[string]string{
		"BOM":             "\xEF\xBB\xBF" + okBody,
		"lone CR":         strings.ReplaceAll(okBody, "\n", "\r"),
		"BOM and lone CR": "\xEF\xBB\xBF" + strings.ReplaceAll(ping+okBody, "\n", "\r"),
		"pings between":   ping + startFrame + ping + strings.TrimPrefix(okBody, startFrame),
	} {
		t.Run(name, func(t *testing.T) {
			adapter, attempts := scripted(t, body)
			stream, err := adapter.Stream(adaptertest.Ctx(), request())
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			_, resp, err := adaptertest.Drain(stream)
			if err != nil {
				t.Fatal(err)
			}
			if resp.Message.Text() != "hello" || attempts.Load() != 1 {
				t.Fatalf("text = %q after %d attempts, want \"hello\" after 1", resp.Message.Text(), attempts.Load())
			}
		})
	}
}

// TestPermanentErrorBeforeContentIsNotRetried: only overloaded_error and
// api_error are transient; everything else fails closed after one attempt.
func TestPermanentErrorBeforeContentIsNotRetried(t *testing.T) {
	for _, typ := range []string{"invalid_request_error", "authentication_error", "permission_error", "something_new"} {
		t.Run(typ, func(t *testing.T) {
			frame := strings.Replace(overloadedFrame, "overloaded_error", typ, 1)
			adapter, attempts := scripted(t, frame, okBody)
			_, err := adapter.Stream(adaptertest.Ctx(), request())
			if err == nil || !strings.Contains(err.Error(), typ) {
				t.Fatalf("err = %v, want the permanent %s error", err, typ)
			}
			if got := attempts.Load(); got != 1 {
				t.Fatalf("attempts = %d, want 1", got)
			}
		})
	}
}

// TestOverloadedErrorAfterContentIsNotRetried: once a content block has begun,
// a retry would re-issue a request whose first half the consumer already read.
func TestOverloadedErrorAfterContentIsNotRetried(t *testing.T) {
	body := startFrame + `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}

` + overloadedFrame
	adapter, attempts := scripted(t, body, okBody)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	var text strings.Builder
	var failure error
	for {
		chunk, err := stream.Next()
		if err != nil {
			failure = err
			break
		}
		text.WriteString(chunk.Text)
	}
	if failure == nil || errors.Is(failure, io.EOF) || !strings.Contains(failure.Error(), "overloaded_error") {
		t.Fatalf("failure = %v, want the in-stream overloaded_error", failure)
	}
	if text.String() != "partial" {
		t.Fatalf("delivered text = %q", text.String())
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1: nothing may be retried after a chunk reached the consumer", got)
	}
}
