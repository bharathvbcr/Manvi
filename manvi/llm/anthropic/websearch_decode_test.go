package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
)

// searchStream is a documented web_search turn (platform.claude.com/docs/en/
// agents-and-tools/tool-use/web-search-tool, "Streaming"; citations_delta from
// .../build-with-claude/citations, "Streaming support"): a lead-in text block,
// a server_tool_use whose query arrives as input_json_delta, the
// web_search_tool_result in one content_block_start, and cited text.
const searchStream = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":100}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"I will search."}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"server_tool_use","id":"srvtoolu_01","name":"web_search","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"query\": \"go "}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"release\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_01","content":[{"type":"web_search_result","url":"https://go.dev/dl","title":"Downloads","encrypted_content":"ENC-CONTENT==","page_age":"April 30, 2025"}]}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: content_block_start
data: {"type":"content_block_start","index":3,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":3,"delta":{"type":"text_delta","text":"Go 1.27 is current."}}

event: content_block_delta
data: {"type":"content_block_delta","index":3,"delta":{"type":"citations_delta","citation":{"type":"web_search_result_location","url":"https://go.dev/dl","title":"Downloads","encrypted_index":"ENC-INDEX==","cited_text":"go1.27"}}}

event: content_block_stop
data: {"type":"content_block_stop","index":3}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":50,"server_tool_use":{"web_search_requests":1}}}

event: message_stop
data: {"type":"message_stop"}

`

func drainSearch(t *testing.T, body string) (llm.Response, *adaptertest.Server) {
	t.Helper()
	adapter, server := adapterFor(t, body)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	_, resp, err := adaptertest.Drain(stream)
	if err != nil {
		t.Fatalf("a documented web_search stream must decode: %v", err)
	}
	return resp, server
}

func TestSearchStreamDecodesWithoutInventingAClientToolCall(t *testing.T) {
	resp, _ := drainSearch(t, searchStream)
	if calls := resp.Message.ToolCalls(); len(calls) != 0 {
		t.Fatalf("a server tool call is not a client tool call; got %+v", calls)
	}
	if got := resp.Message.Text(); got != "I will search.Go 1.27 is current." {
		t.Errorf("text = %q", got)
	}
	usage, err := json.Marshal(resp.Usage)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(usage), `"web_search_requests":1`) {
		t.Errorf("usage %s does not carry the 1 billed search", usage)
	}
}

func TestSearchErrorResultShapeDecodes(t *testing.T) {
	// On error content is one object, not a list (docs, "Errors").
	body := strings.Replace(searchStream,
		`"content":[{"type":"web_search_result","url":"https://go.dev/dl","title":"Downloads","encrypted_content":"ENC-CONTENT==","page_age":"April 30, 2025"}]`,
		`"content":{"type":"web_search_tool_result_error","error_code":"max_uses_exceeded"}`, 1)
	if body == searchStream {
		t.Fatal("fixture edit did not apply")
	}
	drainSearch(t, body)
}

func TestPauseTurnIsNotReportedAsAnUnknownStop(t *testing.T) {
	body := strings.Replace(searchStream, `"stop_reason":"end_turn"`, `"stop_reason":"pause_turn"`, 1)
	resp, _ := drainSearch(t, body)
	if string(resp.StopReason) != "pause_turn" {
		t.Errorf("stop = %q; pause_turn asks the caller to send the turn back to continue it", resp.StopReason)
	}
}
