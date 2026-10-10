package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/transport"
)

// serveBodies answers each attempt with the next body (the last repeats) and
// counts attempts.
func serveBodies(t *testing.T, bodies ...string) (string, *atomic.Int32) {
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
	return server.URL, &attempts
}

func adapterAt(url string, tune func(*Options)) *Adapter {
	opts := Options{
		Name:     "test-provider",
		BaseURL:  url,
		Validate: func(llm.Request) error { return nil },
		Header:   func() (http.Header, error) { return http.Header{}, nil },
	}
	if tune != nil {
		tune(&opts)
	}
	a := New(opts)
	a.client.Retry.BaseDelay = 0
	return a
}

func offered(names ...string) []llm.ToolSchema {
	var out []llm.ToolSchema
	for _, n := range names {
		out = append(out, llm.ToolSchema{Name: n, InputSchema: json.RawMessage(`{"type":"object"}`)})
	}
	return out
}

// settle runs a request to its end and returns the response or the first error.
func settle(t *testing.T, a *Adapter, tools []llm.ToolSchema) (llm.Response, error) {
	t.Helper()
	stream, err := a.Stream(context.Background(), llm.Request{Model: "m", Tools: tools})
	if err != nil {
		return llm.Response{}, err
	}
	defer func() { _ = stream.Close() }()
	for {
		if _, err := stream.Next(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return llm.Response{}, err
		}
	}
	return stream.Response()
}

const callStart = `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"bash"}}]}}]}` + "\n\n"

// TestTruncatedStreamDoesNotDispatchAnArgumentlessCall: EOF with neither a
// finish_reason nor [DONE] was promoted to tool_use with bash({}).
func TestTruncatedStreamDoesNotDispatchAnArgumentlessCall(t *testing.T) {
	for name, body := range map[string]string{
		"no arguments": callStart,
		"cut arguments": callStart +
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":\"rm "}}]}}]}` + "\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			url, _ := serveBodies(t, body)
			resp, err := settle(t, adapterAt(url, nil), offered("bash"))
			if err == nil {
				t.Fatalf("truncated stream dispatched %d call(s), stop=%s", len(resp.Message.ToolCalls()), resp.StopReason)
			}
			var te *transport.Error
			if !errors.As(err, &te) || !te.Retryable() {
				t.Fatalf("err = %#v, want a retryable transport error", err)
			}
		})
	}
}

// TestTruncatedTextStreamIsNotASuccess: half an answer is not an answer.
func TestTruncatedTextStreamIsNotASuccess(t *testing.T) {
	url, _ := serveBodies(t, `data: {"choices":[{"delta":{"content":"The fix is to"}}]}`+"\n\n")
	if resp, err := settle(t, adapterAt(url, nil), nil); err == nil {
		t.Fatalf("settled as success: text=%q stop=%s", resp.Message.Text(), resp.StopReason)
	}
}

// TestAFinishReasonWithoutDoneIsAFinishedStream: several local servers send a
// finish_reason and close without [DONE]; that is the wire saying it stopped.
func TestAFinishReasonWithoutDoneIsAFinishedStream(t *testing.T) {
	url, _ := serveBodies(t, `data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`+"\n\n")
	resp, err := settle(t, adapterAt(url, nil), nil)
	if err != nil || resp.Message.Text() != "hi" {
		t.Fatalf("resp=%q err=%v", resp.Message.Text(), err)
	}
}

// TestDoneWithoutTrailingBlankLineIsACleanEnd: the [DONE] frame terminated by
// EOF rather than a blank line was handed to the JSON decoder.
func TestDoneWithoutTrailingBlankLineIsACleanEnd(t *testing.T) {
	for name, tail := range map[string]string{"newline": "data: [DONE]\n", "no newline": "data: [DONE]"} {
		t.Run(name, func(t *testing.T) {
			url, _ := serveBodies(t, `data: {"choices":[{"delta":{"content":"hi"}}]}`+"\n\n"+tail)
			resp, err := settle(t, adapterAt(url, nil), nil)
			if err != nil || resp.Message.Text() != "hi" {
				t.Fatalf("resp=%q err=%v", resp.Message.Text(), err)
			}
		})
	}
}

// TestATruncatedStreamReportsTheUsageItReceived: the budget settles a cut-short
// attempt at the prompt usage received, so it has to be reachable.
func TestATruncatedStreamReportsTheUsageItReceived(t *testing.T) {
	url, _ := serveBodies(t,
		`data: {"choices":[{"delta":{"content":"The"}}],"usage":{"prompt_tokens":77,"completion_tokens":1}}`+"\n\n")
	stream, err := adapterAt(url, nil).Stream(context.Background(), llm.Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	for {
		if _, err := stream.Next(); err != nil {
			break
		}
	}
	reporter, ok := stream.(interface{ UsageSoFar() (llm.Usage, bool) })
	if !ok {
		t.Fatal("stream cannot report usage so far")
	}
	if u, ok := reporter.UsageSoFar(); !ok || u.InputTokens != 77 {
		t.Fatalf("UsageSoFar = %+v, %v", u, ok)
	}
}

const wireError503 = `data: {"error":{"message":"The server had an error","type":"server_error"}}` + "\n\n"
const okBody = `data: {"choices":[{"delta":{"content":"hello"},"finish_reason":"stop"}]}` + "\n\n" + "data: [DONE]\n\n"

// TestErrorChunkBeforeContentIsRetried: a transient error chunk that is the
// first thing on the wire is the in-stream form of a 5xx.
func TestErrorChunkBeforeContentIsRetried(t *testing.T) {
	url, attempts := serveBodies(t, wireError503, okBody)
	resp, err := settle(t, adapterAt(url, nil), nil)
	if err != nil || resp.Message.Text() != "hello" {
		t.Fatalf("resp=%q err=%v", resp.Message.Text(), err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

// TestRoleOnlyOpeningFrameDoesNotHideAnError: most servers open with an empty
// role delta before anything else.
func TestRoleOnlyOpeningFrameDoesNotHideAnError(t *testing.T) {
	opening := `data: {"choices":[{"delta":{"role":"assistant","content":""}}]}` + "\n\n"
	url, attempts := serveBodies(t, opening+wireError503, okBody)
	if _, err := settle(t, adapterAt(url, nil), nil); err != nil {
		t.Fatal(err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

// TestPermanentErrorChunkIsNotRetried: a bare string, or a type that is not a
// service condition, fails closed after one attempt.
func TestPermanentErrorChunkIsNotRetried(t *testing.T) {
	for name, frame := range map[string]string{
		"bare string":     `data: {"error":"context length exceeded"}`,
		"invalid_request": `data: {"error":{"message":"bad","type":"invalid_request_error"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			url, attempts := serveBodies(t, frame+"\n\n", okBody)
			if _, err := settle(t, adapterAt(url, nil), nil); err == nil {
				t.Fatal("a permanent error was not reported")
			}
			if got := attempts.Load(); got != 1 {
				t.Fatalf("attempts = %d, want 1", got)
			}
		})
	}
}

// TestErrorChunkAfterContentIsNotRetried: nothing is re-issued once a chunk
// reached the consumer.
func TestErrorChunkAfterContentIsNotRetried(t *testing.T) {
	url, attempts := serveBodies(t, `data: {"choices":[{"delta":{"content":"partial"}}]}`+"\n\n"+wireError503, okBody)
	stream, err := adapterAt(url, nil).Stream(context.Background(), llm.Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	var got strings.Builder
	var failure error
	for {
		c, err := stream.Next()
		if err != nil {
			failure = err
			break
		}
		got.WriteString(c.Text)
	}
	if failure == nil || errors.Is(failure, io.EOF) || got.String() != "partial" {
		t.Fatalf("failure=%v text=%q", failure, got.String())
	}
	if n := attempts.Load(); n != 1 {
		t.Fatalf("attempts = %d, want 1", n)
	}
}

// TestAMuteServerIsAbandonedAtTheHeaderTimeout: a server that accepts the
// request and never answers must not hold the call for as long as the caller's
// context lives.
func TestAMuteServerIsAbandonedAtTheHeaderTimeout(t *testing.T) {
	release := make(chan struct{})
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the body first: the server notices the client hanging up only
		// once it has, and Close waits for this handler.
		_, _ = io.Copy(io.Discard, r.Body)

		attempts.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)

	a := adapterAt(server.URL, func(o *Options) { o.HeaderTimeout = 50 * time.Millisecond })
	a.client.Retry.MaxAttempts = 2
	start := time.Now()
	_, err := a.Stream(context.Background(), llm.Request{Model: "m"})
	if err == nil {
		t.Fatal("a mute server was waited on forever")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %s to give up", elapsed)
	}
	var timeout *transport.ErrHeaderTimeout
	if !errors.As(err, &timeout) {
		t.Fatalf("err = %v, want a typed header timeout", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want the timeout retried once", got)
	}
}

// --- repair -------------------------------------------------------------

func callArguments(t *testing.T, args string) (llm.Response, error) {
	t.Helper()
	frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "id": "c1", "function": map[string]any{"name": "grep", "arguments": args}}}},
		"finish_reason": "tool_calls"}}})
	url, _ := serveBodies(t, "data: "+string(frame)+"\n\ndata: [DONE]\n\n")
	return settle(t, adapterAt(url, nil), offered("grep"))
}

// TestRepairDoesNotRewriteStringContents: the repair that makes `True` valid
// also rewrote ",]" and ",}" inside a string value.
func TestRepairDoesNotRewriteStringContents(t *testing.T) {
	resp, err := callArguments(t, `{"pattern": "a,]", "sep": ",}", "flag": True}`)
	if err != nil {
		t.Fatal(err)
	}
	calls := resp.Message.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	var got map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &got); err != nil {
		t.Fatal(err)
	}
	if got["pattern"] != "a,]" || got["sep"] != ",}" || got["flag"] != true {
		t.Fatalf("arguments = %s", calls[0].Arguments)
	}
	if !resp.Decoding.ArgumentsRepaired || resp.Decoding.Clean() {
		t.Fatalf("a repaired call must be recorded: %+v", resp.Decoding)
	}
}

// TestRepairStillStripsStructuralTrailingCommas: the fix must not cost the repair.
func TestRepairStillStripsStructuralTrailingCommas(t *testing.T) {
	resp, err := callArguments(t, "{\"a\": [1, 2, ],\n \"b\": \"x\",\n}")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resp.Message.ToolCalls()[0].Arguments); !json.Valid([]byte(got)) || !strings.Contains(got, `"x"`) {
		t.Fatalf("arguments = %s", got)
	}
}

// TestAValidCallIsNotMarkedRepaired: no false positives for the operator flag.
func TestAValidCallIsNotMarkedRepaired(t *testing.T) {
	resp, err := callArguments(t, `{"pattern":"a,]"}`)
	if err != nil || !resp.Decoding.Clean() {
		t.Fatalf("err=%v decoding=%+v", err, resp.Decoding)
	}
}

func FuzzRepairNeverRewritesAStringValue(f *testing.F) {
	for _, seed := range []string{"a,]", ",}", ", ]", ",\n}", "plain", `q"uote,}`, "\\,]", "日本,]語", ""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		enc, err := json.Marshal(value)
		if err != nil {
			t.Skipf("json.Marshal refused %q (%v), so there is no payload for the repair path to run on", value, err)
		}
		// A payload that is invalid for an unrelated reason (a Python literal
		// and a trailing comma), so the repair path always runs.
		payload := `{"k": ` + string(enc) + `, "flag": True, "list": [1, 2,],}`
		out := repairJSONLiterals(payload)
		var got struct {
			K string `json:"k"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("repair of %q produced invalid JSON: %v\n%s", payload, err, out)
		}
		// Compared as decoded strings: json.Marshal replaces invalid UTF-8, so
		// the reference is the decoding of the input itself.
		var want string
		_ = json.Unmarshal(enc, &want)
		if got.K != want {
			t.Fatalf("string value changed: %q -> %q", want, got.K)
		}
	})
}

// --- text fallback -------------------------------------------------------

const proseWithFence = "Here is how a call looks:\n```json\n" +
	`{"name": "bash", "arguments": {"command": "rm -rf build"}}` +
	"\n```\nThat is only an example, do not run it."

func proseStream() string {
	frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"content": proseWithFence}, "finish_reason": "stop"}}})
	return "data: " + string(frame) + "\n\ndata: [DONE]\n\n"
}

// TestProseExampleOnAFinishedTurnIsNotExecuted pins the decision for the
// fenced form on a server that does opt in to text recovery: a fence followed
// by prose is an example, not a call.
func TestProseExampleOnAFinishedTurnIsNotExecuted(t *testing.T) {
	url, _ := serveBodies(t, proseStream())
	resp, err := settle(t, adapterAt(url, func(o *Options) { o.TextToolFallback = true }), offered("bash"))
	if err != nil {
		t.Fatal(err)
	}
	if calls := resp.Message.ToolCalls(); len(calls) != 0 {
		t.Fatalf("prose example became an executed call: %s", calls[0].Arguments)
	}
	if resp.StopReason == llm.StopToolUse || resp.Decoding.FallbackFormat != "" {
		t.Fatalf("stop=%s format=%q", resp.StopReason, resp.Decoding.FallbackFormat)
	}
}

// TestTextFallbackIsOffUnlessAskedFor: a server that parses tool calls itself
// never leaves one in the text, so nothing in the text is recovered by default.
func TestTextFallbackIsOffUnlessAskedFor(t *testing.T) {
	call := "Running it.\n```json\n" + `{"name": "bash", "arguments": {"command": "ls"}}` + "\n```"
	frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"content": call}, "finish_reason": "stop"}}})
	body := "data: " + string(frame) + "\n\ndata: [DONE]\n\n"

	url, _ := serveBodies(t, body)
	resp, err := settle(t, adapterAt(url, nil), offered("bash"))
	if err != nil || len(resp.Message.ToolCalls()) != 0 {
		t.Fatalf("default recovered a call from text: err=%v calls=%d", err, len(resp.Message.ToolCalls()))
	}
	url, _ = serveBodies(t, body)
	resp, err = settle(t, adapterAt(url, func(o *Options) { o.TextToolFallback = true }), offered("bash"))
	if err != nil || len(resp.Message.ToolCalls()) != 1 || resp.Decoding.FallbackFormat != string(FallbackFencedJSON) {
		t.Fatalf("opt-in lost the recovery: err=%v calls=%d format=%q",
			err, len(resp.Message.ToolCalls()), resp.Decoding.FallbackFormat)
	}
}
