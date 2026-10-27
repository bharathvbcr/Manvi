package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/session"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

const wireStart = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":42}}}

`

// wireToolCall is one tool_use block at index with a single input_json_delta
// (partial is already a JSON string literal) and optionally its closing event.
func wireToolCall(index int, id, name, partial string, sealed bool) string {
	out := fmt.Sprintf(`event: content_block_start
data: {"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":%q}}

`, index, id, name)
	if partial != "" {
		out += fmt.Sprintf(`event: content_block_delta
data: {"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}

`, index, partial)
	}
	if sealed {
		out += fmt.Sprintf(`event: content_block_stop
data: {"type":"content_block_stop","index":%d}

`, index)
	}
	return out
}

func wireFinish(stop string) string {
	return `event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"` + stop + `"},"usage":{"output_tokens":4096}}

event: message_stop
data: {"type":"message_stop"}

`
}

const wireAnswer = `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

`

// driveAnthropic runs the real loop and the real anthropic encoder against a
// fixture server that serves one scripted stream per request (the last one
// repeats). It returns the request bodies the server saw.
func driveAnthropic(t *testing.T, bodies []string, result tools.Result) ([]string, Outcome, error) {
	t.Helper()
	var mu sync.Mutex
	var reqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		mu.Lock()
		i := len(reqs)
		reqs = append(reqs, string(raw))
		mu.Unlock()
		if i >= len(bodies) {
			i = len(bodies) - 1
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, bodies[i])
	}))
	t.Cleanup(srv.Close)

	b := bus.New()
	log := session.NewLog()
	reg := tools.NewRegistry(b)
	if err := reg.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "echo", Description: "echo", InputSchema: []byte(`{"type":"object","properties":{"v":{"type":"integer"}}}`)},
		Handler: func(context.Context, tools.Call) tools.Result {
			return result
		},
	}); err != nil {
		t.Fatal(err)
	}
	loop, err := NewLoop(Config{
		Provider: anthropic.New(srv.URL, adaptertest.Secret("sk-FIXTURE-not-a-real-key")),
		Model:    "claude-opus-5-5", SystemPrompt: "p", MaxSteps: 8, MaxTokens: 1024, AssertInvariant: true,
	}, b, log, reg)
	if err != nil {
		t.Fatal(err)
	}
	out, runErr := loop.Run(context.Background(), llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}})
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), reqs...), out, runErr
}

// A tool that fails and says nothing (an MCP server's isError with empty
// content, a command that exits non-zero silently) goes back as an error
// tool_result with content, which the live API is reported to require.
func TestAnErrorResultWithNoOutputGoesBackWithContent(t *testing.T) {
	reqs, _, err := driveAnthropic(t, []string{
		wireStart + wireToolCall(0, "toolu_A", "echo", `"{\"v\":1}"`, true) + wireFinish("tool_use"),
		wireStart + wireAnswer + wireFinish("end_turn"),
	}, tools.Result{Text: "", IsError: true})
	if err != nil {
		t.Fatalf("turn died: %v", err)
	}
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if !strings.Contains(reqs[1], `"content":[{"type":"text","text":"(no output)"}],"is_error":true`) {
		t.Errorf("error tool_result went back without content:\n%s", reqs[1])
	}
}

// And a successful tool that printed nothing goes back as a bare tool_result.
func TestAnEmptySuccessfulResultGoesBackWithoutContent(t *testing.T) {
	reqs, _, err := driveAnthropic(t, []string{
		wireStart + wireToolCall(0, "toolu_A", "echo", `"{\"v\":1}"`, true) + wireFinish("tool_use"),
		wireStart + wireAnswer + wireFinish("end_turn"),
	}, tools.Result{Text: ""})
	if err != nil {
		t.Fatalf("turn died: %v", err)
	}
	if !strings.Contains(reqs[1], `{"type":"tool_result","tool_use_id":"toolu_A"}`) {
		t.Errorf("empty tool result was not a bare tool_result:\n%s", reqs[1])
	}
}
