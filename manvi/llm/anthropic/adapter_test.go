package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"manvi/llm"
	"manvi/llm/adaptertest"
)

// happyStream is a complete turn: thinking, text, and a tool call whose
// arguments arrive in fragments.
const happyStream = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":42,"cache_read_input_tokens":10}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"weighing it"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"abc"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Reading "}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"the file."}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"devcouncil_read_file"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"src/a.go\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":77,"output_tokens_details":{"thinking_tokens":30}}}

event: message_stop
data: {"type":"message_stop"}

`

func adapterFor(t *testing.T, body string) (*Adapter, *adaptertest.Server) {
	t.Helper()
	server := adaptertest.NewServer(t, body)
	return New(server.URL, adaptertest.Secret("sk-test-key-value")), server
}

func request() llm.Request {
	return llm.Request{
		Model:    "claude-opus-5",
		System:   "You are the builder.",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "read src/a.go"}}}},
		Tools: []llm.ToolSchema{{
			Name: "devcouncil_read_file", Description: "read a file",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}},
		MaxTokens: 4096,
	}
}

func TestAFullTurnDecodes(t *testing.T) {
	adapter, server := adapterFor(t, happyStream)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	chunks, resp, err := adaptertest.Drain(stream)
	if err != nil {
		t.Fatal(err)
	}

	if got := adaptertest.TextOf(chunks); got != "Reading the file." {
		t.Errorf("text = %q", got)
	}
	if got := adaptertest.ReasoningOf(chunks); got != "weighing it" {
		t.Errorf("reasoning = %q", got)
	}
	if resp.StopReason != llm.StopToolUse {
		t.Errorf("stop = %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 42 || resp.Usage.OutputTokens != 77 {
		t.Errorf("usage = %+v — message_start and message_delta must be merged, not replaced", resp.Usage)
	}
	if resp.Usage.CacheReadTokens != 10 || resp.Usage.ReasoningTokens != 30 {
		t.Errorf("usage detail = %+v", resp.Usage)
	}

	calls := resp.Message.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d", len(calls))
	}
	if calls[0].ID != "toolu_1" || calls[0].Name != "devcouncil_read_file" {
		t.Errorf("call = %+v", calls[0])
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatalf("fragmented arguments did not reassemble: %v (%s)", err, calls[0].Arguments)
	}
	if args.Path != "src/a.go" {
		t.Errorf("path = %q", args.Path)
	}

	// The signature is accumulated across deltas and never surfaced as content.
	var signature string
	for _, block := range resp.Message.Content {
		if r, ok := block.(llm.ReasoningBlock); ok {
			signature = r.Signature
		}
	}
	if signature != "sig-abc" {
		t.Errorf("signature = %q, want the fragments joined", signature)
	}
	if resp.Message.Provenance == nil || resp.Message.Provenance.Provider != Name {
		t.Errorf("provenance = %+v; without it the next turn cannot tell whether reasoning is replayable", resp.Message.Provenance)
	}

	// The request carried the documented auth and version headers.
	h := server.Headers[0]
	if h.Get(APIKeyHeader) != "sk-test-key-value" {
		t.Errorf("%s = %q", APIKeyHeader, h.Get(APIKeyHeader))
	}
	if h.Get(VersionHeader) != APIVersion {
		t.Errorf("%s = %q, want %q", VersionHeader, h.Get(VersionHeader), APIVersion)
	}
}

// TestTruncatedToolArgumentsAreRefused is the consequential one. A tool call
// whose arguments stopped halfway must not reach the tool layer: a partial
// path or a partial patch that happens to parse is a wrong action taken
// confidently.
func TestTruncatedToolArgumentsAreRefused(t *testing.T) {
	truncated := `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"devcouncil_write_file"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"src/a.go\""}}

event: message_stop
data: {"type":"message_stop"}

`
	adapter, _ := adapterFor(t, truncated)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = adaptertest.Drain(stream)
	if err == nil {
		t.Fatal("a tool call with incomplete arguments was accepted")
	}
	if !strings.Contains(err.Error(), "incomplete arguments") {
		t.Fatalf("err = %v, want it to name the incomplete arguments", err)
	}
}

// TestUnknownDeltaTypeFails: an unrecognised delta carries content this build
// cannot represent, and dropping it produces a truncated message that looks
// complete.
func TestUnknownDeltaTypeFails(t *testing.T) {
	body := `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"citation_delta","text":"x"}}

`
	adapter, _ := adapterFor(t, body)
	stream, _ := adapter.Stream(adaptertest.Ctx(), request())
	if _, _, err := adaptertest.Drain(stream); err == nil {
		t.Fatal("an unknown delta type was silently dropped")
	}
}

// TestUnknownEventTypeIsTolerated is the other side of that judgement: an event
// this decoder does not know carries nothing it needs, and failing a turn over
// it would break on every additive API change.
func TestUnknownEventTypeIsTolerated(t *testing.T) {
	body := `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":1}}}

event: some_future_event
data: {"type":"some_future_event","payload":{"anything":true}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hi"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`
	adapter, _ := adapterFor(t, body)
	stream, _ := adapter.Stream(adaptertest.Ctx(), request())
	chunks, resp, err := adaptertest.Drain(stream)
	if err != nil {
		t.Fatalf("an unknown event type failed the turn: %v", err)
	}
	if adaptertest.TextOf(chunks) != "hi" || resp.StopReason != llm.StopEndTurn {
		t.Fatalf("chunks = %v, stop = %q", chunks, resp.StopReason)
	}
}

// TestEventNameAndPayloadTypeMustAgree: when the two disagree the stream is not
// what this decoder was written against, and picking one is a guess.
func TestEventNameAndPayloadTypeMustAgree(t *testing.T) {
	body := `event: content_block_delta
data: {"type":"message_stop","index":0}

`
	adapter, _ := adapterFor(t, body)
	stream, _ := adapter.Stream(adaptertest.Ctx(), request())
	if _, _, err := adaptertest.Drain(stream); err == nil {
		t.Fatal("a disagreeing event name and payload type were accepted")
	}
}

// TestDeltaForAnUnstartedBlockFails: events were lost, and continuing would
// assemble a message with a hole in it.
func TestDeltaForAnUnstartedBlockFails(t *testing.T) {
	body := `event: content_block_delta
data: {"type":"content_block_delta","index":4,"delta":{"type":"text_delta","text":"orphan"}}

`
	adapter, _ := adapterFor(t, body)
	stream, _ := adapter.Stream(adaptertest.Ctx(), request())
	if _, _, err := adaptertest.Drain(stream); err == nil {
		t.Fatal("a delta for a block that never started was accepted")
	}
}

// TestAnErrorEventFailsTheTurn: an error frame mid-stream must not settle as a
// short but successful answer.
func TestAnErrorEventFailsTheTurn(t *testing.T) {
	body := `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"partial"}}

event: error
data: {"type":"error","error":{"type":"overloaded_error","message":"try again"}}

`
	adapter, _ := adapterFor(t, body)
	stream, _ := adapter.Stream(adaptertest.Ctx(), request())
	_, _, err := adaptertest.Drain(stream)
	if err == nil {
		t.Fatal("an error event settled as a successful response")
	}
	if !strings.Contains(err.Error(), "overloaded_error") {
		t.Fatalf("err = %v, want the provider's error type", err)
	}
}

// TestResponseBeforeExhaustionIsRefused: a caller that settles early would get
// a message missing everything still in flight.
func TestResponseBeforeExhaustionIsRefused(t *testing.T) {
	adapter, _ := adapterFor(t, happyStream)
	stream, _ := adapter.Stream(adaptertest.Ctx(), request())
	defer stream.Close()
	if _, err := stream.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Response(); err == nil {
		t.Fatal("Response was returned before the stream was exhausted")
	}
}

// TestRequestShapeMatchesTheDocumentedContract asserts what actually goes on
// the wire, since that is the part a capability catalogue cannot check.
func TestRequestShapeMatchesTheDocumentedContract(t *testing.T) {
	adapter, server := adapterFor(t, happyStream)
	adapter.Thinking = ThinkingAdaptive
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	adaptertest.Drain(stream)

	var body map[string]any
	if err := json.Unmarshal([]byte(server.Requests[0]), &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "claude-opus-5" || body["stream"] != true {
		t.Errorf("body = %v", body)
	}
	if body["system"] != "You are the builder." {
		t.Error("the system prompt must be a top-level field, not a message role")
	}
	if body["max_tokens"] != float64(4096) {
		t.Errorf("max_tokens = %v; the field is required and must never be omitted", body["max_tokens"])
	}
	thinking, _ := body["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" {
		t.Errorf("thinking = %v", body["thinking"])
	}
	if _, present := body["temperature"]; present {
		t.Error("temperature must never be sent; current models reject it")
	}
}

// TestASystemRoleMessageIsRefused: folding it into the user turn would change
// the prompt without saying so.
func TestASystemRoleMessageIsRefused(t *testing.T) {
	adapter, _ := adapterFor(t, happyStream)
	req := request()
	req.Messages = append([]llm.Message{{
		Role: llm.RoleSystem, Content: []llm.ContentBlock{llm.TextBlock{Text: "misplaced"}},
	}}, req.Messages...)
	if _, err := adapter.Stream(adaptertest.Ctx(), req); err == nil {
		t.Fatal("a system-role message was accepted into Messages")
	}
}

// TestAMissingCredentialFailsBeforeTheRequest: a request sent without auth
// wastes a round trip and returns a 401 that reads as a bad key.
func TestAMissingCredentialFailsBeforeTheRequest(t *testing.T) {
	server := adaptertest.NewServer(t, happyStream)
	adapter := New(server.URL, adaptertest.MissingSecret())
	if _, err := adapter.Stream(adaptertest.Ctx(), request()); err == nil {
		t.Fatal("a request was sent with no credential")
	}
	if len(server.Requests) != 0 {
		t.Fatal("the request reached the server despite having no credential")
	}
}

// TestAnHTTPErrorIsReportedNotStreamed: a 400 must not become an empty stream
// that settles as a successful empty answer.
func TestAnHTTPErrorIsReportedNotStreamed(t *testing.T) {
	server := adaptertest.NewStatusServer(t, http.StatusBadRequest,
		`{"type":"error","error":{"type":"invalid_request_error","message":"bad model"}}`)
	adapter := New(server.URL, adaptertest.Secret("sk-test-key-value"))
	if _, err := adapter.Stream(adaptertest.Ctx(), request()); err == nil {
		t.Fatal("a 400 produced a usable stream")
	}
}

// TestAnUnknownModelIsRefusedAtAssembly keeps a typo from becoming a 404.
func TestAnUnknownModelIsRefusedAtAssembly(t *testing.T) {
	adapter, server := adapterFor(t, happyStream)
	req := request()
	req.Model = "claude-opus-6"
	if _, err := adapter.Stream(adaptertest.Ctx(), req); err == nil {
		t.Fatal("an unknown model was sent")
	}
	if len(server.Requests) != 0 {
		t.Fatal("an unknown model reached the network")
	}
}

// TestRunawayResponseHitsTheDecodeLimit: a server generating past any output
// bound must fail the turn rather than allocate without limit. The stall
// watchdog bounds silence; this bounds volume.
func TestRunawayResponseHitsTheDecodeLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString("event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
	b.WriteString("event: content_block_start\n")
	b.WriteString("data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
	// Far past the 4 MiB decode ceiling in one delta.
	chunk := strings.Repeat("x", maxDecodedResponseBytes+1024)
	fmt.Fprintf(&b, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", chunk)

	adapter, _ := adapterFor(t, b.String())
	stream, err := adapter.Stream(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	var sawErr error
	for {
		_, err := stream.Next()
		if err != nil {
			sawErr = err
			break
		}
	}
	if sawErr == nil || !strings.Contains(sawErr.Error(), "decode limit") {
		t.Fatalf("a %d-byte response was accepted without complaint", len(chunk))
	}
	// The settled view reports the same failure, not a caller error.
	if _, err := stream.Response(); err == nil || !strings.Contains(err.Error(), "decode limit") {
		t.Fatalf("Response() = %v, want the decode-limit failure", err)
	}
}

// TestMidFlightFailureSettlesTheStream: when the connection dies partway, the
// settled view must report that failure. It used to answer "called before the
// stream was exhausted", blaming the caller for a server that went silent.
func TestMidFlightFailureSettlesTheStream(t *testing.T) {
	// A server that starts a valid SSE response and then rips the connection
	// away mid-event: the client sees an I/O failure, not clean EOF.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		flusher.Flush()
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(srv.Close)

	adapter := New(srv.URL, adaptertest.Secret("sk-test-key-value"))
	stream, err := adapter.Stream(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	firstErr := error(nil)
	for {
		_, err := stream.Next()
		if err != nil {
			firstErr = err
			break
		}
	}
	if firstErr == nil || errors.Is(firstErr, io.EOF) {
		t.Fatalf("expected a mid-flight I/O failure, got %v", firstErr)
	}
	_, respErr := stream.Response()
	if respErr == nil || !strings.Contains(respErr.Error(), firstErr.Error()) {
		t.Fatalf("Response() = %v, want the stream's own failure (%v)", respErr, firstErr)
	}
}
