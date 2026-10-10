package local

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// TestLocalRecoversATextToolCall: a local server is only as good as the
// tool-call parser it was launched with, so text recovery stays on here even
// though the hosted providers sharing the wire have it off.
func TestLocalRecoversATextToolCall(t *testing.T) {
	text := "<tool_call>{\"name\": \"read_file\", \"arguments\": {\"path\": \"a.go\"}}</tool_call>"
	frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"content": text}, "finish_reason": "stop"}}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
	}))
	t.Cleanup(srv.Close)

	a := New(Config{BaseURL: srv.URL + "/v1", SupportsTools: true, AssumeModelServed: true}, noCredential)
	stream, err := a.Stream(t.Context(), llm.Request{
		Model: "qwen-local",
		Tools: []llm.ToolSchema{{Name: "read_file", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	for {
		if _, err := stream.Next(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	resp, err := stream.Response()
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Message.ToolCalls()) != 1 || resp.Decoding.FallbackFormat == "" {
		t.Fatalf("calls=%d format=%q; local lost its text recovery", len(resp.Message.ToolCalls()), resp.Decoding.FallbackFormat)
	}
}
