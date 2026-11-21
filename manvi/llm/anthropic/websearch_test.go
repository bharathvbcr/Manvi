package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
	"github.com/bharathvbcr/Manvi/manvi/llm/budget"
)

func searchAdapter(t *testing.T, body string, uses int) (*Adapter, *adaptertest.Server) {
	t.Helper()
	adapter, server := adapterFor(t, body)
	if err := adapter.SetWebSearch(uses); err != nil {
		t.Fatal(err)
	}
	return adapter, server
}

func sentTools(t *testing.T, body string) []map[string]any {
	t.Helper()
	var req struct {
		Tools  []map[string]any `json:"tools"`
		System string           `json:"system"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	return req.Tools
}

func sendOne(t *testing.T, adapter *Adapter, server *adaptertest.Server, req llm.Request) string {
	t.Helper()
	stream, err := adapter.Stream(adaptertest.Ctx(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if _, _, err := adaptertest.Drain(stream); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	reqs := server.Requests()
	return reqs[len(reqs)-1]
}

func TestWebSearchIsOffByDefault(t *testing.T) {
	adapter, server := adapterFor(t, minimalStream)
	body := sendOne(t, adapter, server, request())
	if strings.Contains(body, "web_search") {
		t.Fatalf("the default request carries the hosted search tool: %s", body)
	}
}

func TestWebSearchToolIsSentWithItsBoundAndANote(t *testing.T) {
	adapter, server := searchAdapter(t, minimalStream, 3)
	body := sendOne(t, adapter, server, request())
	tools := sentTools(t, body)
	last := tools[len(tools)-1]
	if last["type"] != "web_search_20250305" || last["name"] != "web_search" || last["max_uses"] != float64(3) {
		t.Fatalf("hosted tool = %v", last)
	}
	if len(tools) != 2 || tools[0]["name"] != "devcouncil_read_file" {
		t.Errorf("client tool lost or reordered: %v", tools)
	}
	if !strings.Contains(body, "untrusted data written by strangers") {
		t.Errorf("the system prompt does not tell the model results are untrusted")
	}
	if got := adapter.MaxSearches(request()); got != 3 {
		t.Errorf("MaxSearches = %d, want 3", got)
	}
}

func TestWebSearchRidesOnlyOnRequestsThatCanCallTools(t *testing.T) {
	adapter, server := searchAdapter(t, minimalStream, 3)
	noTools := request()
	noTools.Tools = nil
	if body := sendOne(t, adapter, server, noTools); strings.Contains(body, "web_search") {
		t.Errorf("a request with no tools gained network reach: %s", body)
	}
	none := request()
	none.NoToolCalls = true
	if body := sendOne(t, adapter, server, none); strings.Contains(body, "web_search") {
		t.Errorf("a request that forbids tool calls gained network reach: %s", body)
	}
	if adapter.MaxSearches(noTools) != 0 || adapter.MaxSearches(none) != 0 {
		t.Errorf("the budget reserves searches the request cannot make")
	}
}

func TestWebSearchRefusesWhatItCannotDoLoudly(t *testing.T) {
	adapter, _ := searchAdapter(t, minimalStream, 3)
	unsupported := request()
	unsupported.Model = "claude-haiku-4-5"
	if _, err := adapter.Stream(adaptertest.Ctx(), unsupported); err == nil || !strings.Contains(err.Error(), "not listed as supporting") {
		t.Errorf("an unlisted model was silently sent without the tool: %v", err)
	}
	clash := request()
	clash.Tools[0].Name = "web_search"
	if _, err := adapter.Stream(adaptertest.Ctx(), clash); err == nil || !strings.Contains(err.Error(), "already offered") {
		t.Errorf("a client tool shadowing the hosted one was accepted: %v", err)
	}
	for _, bad := range []int{-1, MaxWebSearchUses + 1} {
		if err := adapter.SetWebSearch(bad); err == nil {
			t.Errorf("SetWebSearch(%d) accepted", bad)
		}
	}
}

// turnWith is a history of one search exchange: the user's question, what the
// model sent back, and a follow-up.
func searchHistory(t *testing.T) []llm.Message {
	t.Helper()
	resp, _ := drainSearch(t, searchStream)
	return []llm.Message{userMsg("what is current"), resp.Message, userMsg("and the one before?")}
}

func wireContent(t *testing.T, body string) [][]map[string]any {
	t.Helper()
	var req struct {
		Messages []struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	var out [][]map[string]any
	for _, m := range req.Messages {
		out = append(out, m.Content)
	}
	return out
}

func TestServerBlocksRoundTripByteForByte(t *testing.T) {
	history := searchHistory(t)
	adapter, server := searchAdapter(t, minimalStream, 3)
	req := request()
	req.Messages = history
	body := sendOne(t, adapter, server, req)

	assistant := wireContent(t, body)[1]
	var types []string
	for _, b := range assistant {
		types = append(types, b["type"].(string))
	}
	if strings.Join(types, ",") != "text,server_tool_use,web_search_tool_result,text" {
		t.Fatalf("assistant blocks = %v", types)
	}
	// The API refuses a replay whose encrypted fields were touched.
	for _, want := range []string{`"encrypted_content":"ENC-CONTENT=="`, `"encrypted_index":"ENC-INDEX=="`,
		`"page_age":"April 30, 2025"`, `"input":{"query":"go release"}`} {
		if !strings.Contains(body, want) {
			t.Errorf("replayed request lost %s", want)
		}
	}
	if assistant[3]["citations"] == nil {
		t.Errorf("cited text lost its citations: %v", assistant[3])
	}
}

func TestServerBlocksAreDroppedWhenSearchIsNotOnTheRequest(t *testing.T) {
	history := searchHistory(t)
	adapter, server := adapterFor(t, minimalStream) // search off
	req := request()
	req.Messages = history
	body := sendOne(t, adapter, server, req)
	if strings.Contains(body, "server_tool_use") || strings.Contains(body, "encrypted_") || strings.Contains(body, "citations") {
		t.Fatalf("hosted blocks reached a request without the tool: %s", body)
	}
	if got := wireContent(t, body)[1]; len(got) != 2 || got[1]["text"] != "Go 1.27 is current." {
		t.Errorf("the turn's visible text was lost: %v", got)
	}
}

func TestEditedHistoryFallsBackToNeutralBlocks(t *testing.T) {
	history := searchHistory(t)
	history[1].Content = history[1].Content[:1] // compaction or a trim removed the cited text
	adapter, server := searchAdapter(t, minimalStream, 3)
	req := request()
	req.Messages = history
	body := sendOne(t, adapter, server, req)
	if strings.Contains(body, "server_tool_use") || strings.Contains(body, "encrypted_") {
		t.Fatalf("server blocks replayed against content that no longer matches them: %s", body)
	}
}

func TestEditedCitedTextKeepsTheNewTextAndDropsItsCitations(t *testing.T) {
	history := searchHistory(t)
	history[1].Content[1] = llm.TextBlock{Text: "Go 1.27 is current (trimmed)."}
	adapter, server := searchAdapter(t, minimalStream, 3)
	req := request()
	req.Messages = history
	body := sendOne(t, adapter, server, req)
	if !strings.Contains(body, "(trimmed)") || strings.Contains(body, "ENC-INDEX") {
		t.Fatalf("an edited cited text must be sent as edited, without citations that no longer fit it: %s", body)
	}
}

func TestACancelledTurnDoesNotLeaveAnUnansweredServerToolUse(t *testing.T) {
	// A stream that ended after the query and before the result: the history
	// must never carry the call into a request whose next message is a plain
	// user message, which the API rejects on every later request.
	cut := strings.SplitN(searchStream, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2", 2)[0] +
		`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`
	resp, _ := drainSearch(t, cut)
	if !hasServerState(resp.Message) {
		t.Fatal("the response should carry the query")
	}
	adapter, server := searchAdapter(t, minimalStream, 3)
	req := request()
	req.Messages = []llm.Message{userMsg("q"), resp.Message, userMsg("never mind, do something else")}
	body := sendOne(t, adapter, server, req)
	if strings.Contains(body, "server_tool_use") {
		t.Fatalf("an unanswered server_tool_use was sent before a plain user message: %s", body)
	}
}

func TestPausedTurnIsContinuedByASendBackWithNothingAfterIt(t *testing.T) {
	paused := strings.Replace(searchStream, `"stop_reason":"end_turn"`, `"stop_reason":"pause_turn"`, 1)
	// pause_turn can land right after a query, before its result.
	paused = strings.SplitN(paused, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2", 2)[0] +
		`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"pause_turn"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`
	resp, _ := drainSearch(t, paused)
	if resp.StopReason != llm.StopPause {
		t.Fatalf("stop = %q", resp.StopReason)
	}
	adapter, server := searchAdapter(t, minimalStream, 3)
	req := request()
	req.Messages = []llm.Message{userMsg("q"), resp.Message}
	body := sendOne(t, adapter, server, req)
	content := wireContent(t, body)
	if len(content) != 2 || content[1][len(content[1])-1]["type"] != "server_tool_use" {
		t.Fatalf("the paused turn was not sent back as it was: %s", body)
	}

	// With search off the same history is a prefill, and is refused rather than sent.
	off, _ := adapterFor(t, minimalStream)
	if _, err := off.Stream(adaptertest.Ctx(), req); err == nil {
		t.Error("a paused turn was sent to an adapter that cannot continue it")
	}
}

func TestAServerOnlyMessageSurvivesTheNeutralPredicate(t *testing.T) {
	msg := llm.Message{Role: llm.RoleAssistant, Provenance: &llm.AssistantProvenance{Provider: Name, ReplayState: json.RawMessage(`{"version":1}`)}}
	if !msg.HasSendableContent() {
		t.Error("an assistant message carried by its replay state alone was treated as empty and would be dropped from the history")
	}
}

func TestSearchRequestsAreChargedAndReserved(t *testing.T) {
	p, ok := PriceOf("claude-haiku-5-5")
	if !ok || p.SearchRequest != 10_000_000 {
		t.Fatalf("price = %+v; the documented price is $10 per 1,000 searches (10,000,000 nano-USD)", p)
	}
	base := llm.Usage{InputTokens: 1000, OutputTokens: 100}
	plain, err := p.Charge(base)
	if err != nil {
		t.Fatal(err)
	}
	base.WebSearchRequests = 2
	with, err := p.Charge(base)
	if err != nil {
		t.Fatal(err)
	}
	if with-plain != 2*10_000_000 {
		t.Errorf("two searches added %d nano-USD, want 20000000", with-plain)
	}
	if extra, err := p.SearchSurcharge(3); err != nil || extra != 30_000_000 {
		t.Errorf("surcharge = %d, %v", extra, err)
	}
	var _ budget.SearchBounder = (*Adapter)(nil)
}
