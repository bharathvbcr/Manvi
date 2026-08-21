package openaicompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"manvi/llm"
)

// The exact syntax mandated by the chat template shipped in
// mlx-community/Qwen3.8-27B-4bit. The previous parser handled only the Hermes
// JSON body and extracted nothing from this, so on a server without its own
// tool parser the call surfaced as prose and the turn ended reporting success
// with no work done.
const qwenXMLCall = `I will read the file first.
<tool_call>
<function=devcouncil_read_file>
<parameter=path>
src/main.go
</parameter>
<parameter=limit>
200
</parameter>
</function>
</tool_call>`

var readFileSchema = []llm.ToolSchema{{
	Name:        "devcouncil_read_file",
	InputSchema: []byte(`{"type":"object","properties":{"path":{"type":"string"},"limit":{"type":"integer"}}}`),
}}

func TestQwenNestedXMLToolCallIsRecovered(t *testing.T) {
	text, calls, format := extractFallbackToolCalls(qwenXMLCall, readFileSchema)

	if len(calls) != 1 {
		t.Fatalf("expected 1 recovered call, got %d", len(calls))
	}
	if format != FallbackQwenXML {
		t.Fatalf("format = %q, want %q", format, FallbackQwenXML)
	}
	if calls[0].Name != "devcouncil_read_file" {
		t.Fatalf("name = %q", calls[0].Name)
	}
	if strings.Contains(text, "<tool_call>") {
		t.Fatalf("the call markup was left in the visible text: %q", text)
	}

	var args struct {
		Path  string `json:"path"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatalf("arguments did not decode: %v (%s)", err, calls[0].Arguments)
	}
	if args.Path != "src/main.go" {
		t.Errorf("path = %q", args.Path)
	}
	if args.Limit != 200 {
		t.Errorf("limit = %d, want 200 (a numeric parameter must not stay a string)", args.Limit)
	}
}

func TestXMLParameterValuesKeepTheirTypeWhenAmbiguous(t *testing.T) {
	payload := `<tool_call>
<function=devcouncil_write_file>
<parameter=mode>
0755
</parameter>
<parameter=version>
1.10
</parameter>
<parameter=name>
null
</parameter>
<parameter=enabled>
true
</parameter>
<parameter=opts>
{"deep":true}
</parameter>
</function>
</tool_call>`

	schema := []llm.ToolSchema{{
		Name: "devcouncil_write_file",
		InputSchema: []byte(`{"type":"object","properties":{
			"mode":{"type":"string"},"version":{"type":"string"},"name":{"type":"string"},
			"enabled":{"type":"boolean"},"opts":{"type":"object"}}}`),
	}}
	_, calls, _ := extractFallbackToolCalls(payload, schema)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	var args map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatal(err)
	}

	// A file mode and a version string must survive as text: re-typing them
	// turns 0755 into 755 and 1.10 into 1.1, both of which are wrong answers
	// that look like right ones.
	if got, ok := args["mode"].(string); !ok || got != "0755" {
		t.Errorf("mode = %#v, want the string \"0755\"", args["mode"])
	}
	if got, ok := args["version"].(string); !ok || got != "1.10" {
		t.Errorf("version = %#v, want the string \"1.10\"", args["version"])
	}
	// A filename that happens to read as a literal must not become one.
	if got, ok := args["name"].(string); !ok || got != "null" {
		t.Errorf("name = %#v, want the string \"null\"", args["name"])
	}
	// Unambiguous booleans and containers are re-typed, because sending them
	// as strings fails schema validation on the other side.
	if got, ok := args["enabled"].(bool); !ok || !got {
		t.Errorf("enabled = %#v, want true", args["enabled"])
	}
	if _, ok := args["opts"].(map[string]any); !ok {
		t.Errorf("opts = %#v, want an object", args["opts"])
	}
}

func TestTwoToolCallsInOneMessageAreBothRecovered(t *testing.T) {
	payload := `First:
<tool_call>
<function=devcouncil_grep>
<parameter=pattern>
TODO
</parameter>
</function>
</tool_call>
Then:
<tool_call>
<function=devcouncil_list_dir>
<parameter=path>
src
</parameter>
</function>
</tool_call>`

	offered := []llm.ToolSchema{{Name: "devcouncil_grep"}, {Name: "devcouncil_list_dir"}}
	_, calls, _ := extractFallbackToolCalls(payload, offered)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].Name != "devcouncil_grep" || calls[1].Name != "devcouncil_list_dir" {
		t.Fatalf("names = %q, %q", calls[0].Name, calls[1].Name)
	}
	if calls[0].ID == calls[1].ID {
		t.Fatal("both calls were given the same id; results cannot be paired to calls")
	}
}

func TestUnterminatedToolCallIsNotInvented(t *testing.T) {
	// A response truncated mid-call. Recovering a call from half a payload
	// would run a tool the model never finished asking for.
	payload := "Let me read it.\n<tool_call>\n<function=devcouncil_read_file>\n<parameter=path>\nsrc/ma"
	// The tool IS offered, so truncation is the only reason nothing may be
	// recovered. Passing no tools would make this pass for the wrong reason.
	text, calls, format := extractFallbackToolCalls(payload, []llm.ToolSchema{{Name: "devcouncil_read_file"}})
	if len(calls) != 0 {
		t.Fatalf("recovered %d call(s) from a truncated payload", len(calls))
	}
	if format != FallbackNone {
		t.Fatalf("format = %q", format)
	}
	if text != payload {
		t.Error("the text was altered even though nothing was recovered")
	}
}

func TestFencedFallbackAcceptsAnyOfferedToolName(t *testing.T) {
	// The old markdown branch required a devcouncil_ prefix while the tagged
	// branch accepted anything, so the same call was recovered or dropped
	// depending on how the model wrapped it. Both branches now ask the same
	// question — is this one of the tools the request offered — so the shape of
	// the name is irrelevant and an unoffered name is refused in both.
	offered := []llm.ToolSchema{{Name: "some_other_tool"}}
	payload := "Calling it:\n```json\n{\"name\": \"some_other_tool\", \"arguments\": {\"a\": 1}}\n```"
	_, calls, format := extractFallbackToolCalls(payload, offered)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "some_other_tool" {
		t.Fatalf("name = %q", calls[0].Name)
	}
	if format != FallbackFencedJSON {
		t.Fatalf("format = %q", format)
	}

	// The same payload against a request that did not offer it stays prose.
	text, calls, format := extractFallbackToolCalls(payload, []llm.ToolSchema{{Name: "something_else"}})
	if len(calls) != 0 {
		t.Fatalf("recovered %d call(s) to a tool that was not offered", len(calls))
	}
	if format != FallbackNone {
		t.Fatalf("format = %q, want none", format)
	}
	if !strings.Contains(text, "some_other_tool") {
		t.Fatal("the fence was deleted from the answer even though no call was recovered")
	}
}

func TestServerParsedCallsNeverEnterTheFallback(t *testing.T) {
	// Prose that merely mentions the markup must not become a call.
	text, calls, format := extractFallbackToolCalls(
		"The template requires a <tool_call> block, which I will not emit here.", nil)
	if len(calls) != 0 {
		t.Fatalf("recovered %d call(s) from prose", len(calls))
	}
	if format != FallbackNone {
		t.Fatalf("format = %q", format)
	}
	if !strings.Contains(text, "<tool_call>") {
		t.Error("prose was rewritten")
	}
}

// --- prefilled think block -------------------------------------------------

// An unmatched closing tag is reported, never acted on.
//
// It is what a server prefilling "<think>" looks like from here — and it is
// equally what a model writing *about* think tags looks like, which in this
// harness is the ordinary case. Guessing prefill and moving the preceding text
// into reasoning deleted the answer whenever the guess was wrong, and deleted
// it from the settled message rather than only the live view. Nothing in the
// stream separates the two, so the text is kept and the suspicion is surfaced.
func TestAnUnmatchedClosingTagIsReportedNotActedOn(t *testing.T) {
	var f tagFilter
	out := f.feed("Let me plan the edit.")
	if len(out) != 1 || out[0].text == "" {
		t.Fatalf("expected the leading content to stream as text, got %#v", out)
	}

	out = f.feed("</think>Here is the answer.")
	var reasoning, text string
	for _, c := range out {
		reasoning += c.reasoning
		text += c.text
	}
	if !f.prefillSuspected {
		t.Error("an unmatched closing tag was not reported as a possible prefill")
	}
	if text != "Here is the answer." {
		t.Errorf("text = %q", text)
	}
	if strings.Contains(text, "</think>") || strings.Contains(reasoning, "</think>") {
		t.Error("the closing tag leaked into the output")
	}
}

// The case that made the old behaviour unacceptable: prose that mentions the
// closing tag must survive intact.
func TestProseAboutThinkTagsKeepsItsAnswer(t *testing.T) {
	answer := "To strip reasoning you remove everything up to the closing tag </think> in the buffer."
	visible, reasoning, suspected := SplitReasoning(answer, false)

	if !strings.Contains(visible, "To strip reasoning") {
		t.Fatalf("the answer was destroyed; visible = %q", visible)
	}
	if strings.Contains(visible, "</think>") {
		t.Errorf("the tag leaked into the answer: %q", visible)
	}
	if reasoning != "" {
		t.Errorf("prose was misfiled as reasoning: %q", reasoning)
	}
	if !suspected {
		t.Error("the unmatched tag should still be reported to the caller")
	}
}

func TestMatchedThinkBlockStillWorks(t *testing.T) {
	var f tagFilter
	out := f.feed("<think>weighing options</think>the answer")
	var reasoning, text string
	for _, c := range out {
		reasoning += c.reasoning
		text += c.text
	}
	if f.prefillSuspected {
		t.Error("a matched pair must not be reported as a prefill")
	}
	if reasoning != "weighing options" {
		t.Errorf("reasoning = %q", reasoning)
	}
	if text != "the answer" {
		t.Errorf("text = %q", text)
	}
}

func TestClosingTagAfterAMatchedPairIsNotAPrefill(t *testing.T) {
	// Once an opening tag has been seen the question is settled; a stray later
	// closing tag is framing and must not be reported as a prefill.
	var f tagFilter
	_ = f.feed("<think>first</think>answer text")
	_ = f.feed("</think>more")
	if f.prefillSuspected {
		t.Fatal("reported a prefill after the block shape was already known")
	}
}

func TestThinkTagSplitAcrossDeltasIsNotLeaked(t *testing.T) {
	var f tagFilter
	var text, reasoning string
	for _, part := range []string{"before", "</thi", "nk>after"} {
		for _, c := range f.feed(part) {
			text += c.text
			reasoning += c.reasoning
		}
	}
	if strings.Contains(text, "<") || strings.Contains(reasoning, "<") {
		t.Fatalf("a tag split across deltas leaked: text=%q reasoning=%q", text, reasoning)
	}
	// A tag split across deltas is still recognised as unmatched, so the
	// possible prefill is reported even though no single delta contained the
	// whole tag.
	if !f.prefillSuspected {
		t.Fatal("a tag split across deltas was not reported as a possible prefill")
	}
}

func TestDeclaredPrefillClassifiesFromTheFirstByte(t *testing.T) {
	f := tagFilter{assumePrefill: true}
	out := f.feed("thinking hard</think>done")
	var text, reasoning string
	for _, c := range out {
		text += c.text
		reasoning += c.reasoning
	}
	if reasoning != "thinking hard" {
		t.Errorf("reasoning = %q", reasoning)
	}
	if text != "done" {
		t.Errorf("text = %q", text)
	}
}

// --- parallel tool calls ---------------------------------------------------

func TestParallelToolCallStartsAreAllSurfaced(t *testing.T) {
	s := newStream(nil, "m", nil, Options{Name: "local"}, 0)
	// One delta opening two calls, which vLLM and llama.cpp both emit.
	s.applyToolCalls([]wireToolCall{
		{Index: 0, ID: "a", Function: wireCallFunc{Name: "devcouncil_read_file", Arguments: `{"path":`}},
		{Index: 1, ID: "b", Function: wireCallFunc{Name: "devcouncil_grep", Arguments: `{"pattern":`}},
	})

	seen := map[string]bool{}
	for _, c := range s.pending {
		if c.Kind == llm.ChunkToolCallStart {
			seen[c.ToolName] = true
		}
	}
	if !seen["devcouncil_read_file"] || !seen["devcouncil_grep"] {
		t.Fatalf("a tool-call start was dropped from the stream: %v", seen)
	}
	if s.pending[0].BlockIndex == s.pending[1].BlockIndex {
		t.Fatal("two calls shared a block index; a renderer would merge them")
	}
}

func TestInterleavedArgumentFragmentsStayWithTheirOwnCall(t *testing.T) {
	s := newStream(nil, "m", nil, Options{Name: "local"}, 0)
	s.applyToolCalls([]wireToolCall{
		{Index: 0, ID: "a", Function: wireCallFunc{Name: "tool_a", Arguments: `{"x":`}},
		{Index: 1, ID: "b", Function: wireCallFunc{Name: "tool_b", Arguments: `{"y":`}},
	})
	// Fragments arrive out of order, keyed only by index.
	s.applyToolCalls([]wireToolCall{{Index: 1, Function: wireCallFunc{Arguments: `2}`}}})
	s.applyToolCalls([]wireToolCall{{Index: 0, Function: wireCallFunc{Arguments: `1}`}}})

	if got := s.calls[0].args.String(); got != `{"x":1}` {
		t.Errorf("call 0 args = %q", got)
	}
	if got := s.calls[1].args.String(); got != `{"y":2}` {
		t.Errorf("call 1 args = %q", got)
	}
}

// --- settled-message behaviour ---------------------------------------------

// sseStream serves a fixed set of SSE frames.
func sseStream(t *testing.T, frames ...string) *Adapter {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, f := range frames {
			_, _ = w.Write([]byte(f + "\n\n"))
		}
	}))
	t.Cleanup(server.Close)
	return New(Options{
		Name:     "local",
		BaseURL:  server.URL,
		Validate: func(llm.Request) error { return nil },
		Header:   func() (http.Header, error) { return http.Header{}, nil },
	})
}

func drain(t *testing.T, a *Adapter, req llm.Request) (llm.Response, []llm.Chunk) {
	t.Helper()
	s, err := a.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()
	var chunks []llm.Chunk
	for {
		c, err := s.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		chunks = append(chunks, c)
	}
	resp, err := s.Response()
	if err != nil {
		t.Fatalf("Response: %v", err)
	}
	return resp, chunks
}

// The settled message is what is logged and replayed on every later step. If a
// prefilled think block leaves the chain of thought in the text block, that
// reasoning is fed back to the model for the rest of the session.
// Without a declaration, a prefilled think block cannot be distinguished from
// prose about think tags, so the settled message keeps the text and reports the
// suspicion.
//
// This test used to assert the opposite — that the harness separated the chain
// of thought on its own. It did that by guessing, and the guess deleted the
// answer whenever a model wrote about think tags. Declaring the prefill is how
// a server that really does prefill gets the separation; see
// TestDeclaredPrefillClassifiesFromTheFirstByte.
func TestUndeclaredPrefillKeepsTheTextAndReportsIt(t *testing.T) {
	a := sseStream(t,
		`data: {"choices":[{"delta":{"content":"First I should check the imports."}}]}`,
		`data: {"choices":[{"delta":{"content":"</think>The unused import is fmt."}}]}`,
		`data: {"choices":[{"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	)
	resp, _ := drain(t, a, llm.Request{Model: "qwen"})

	got := resp.Message.Text()
	if !strings.Contains(got, "The unused import is fmt.") {
		t.Errorf("the answer was lost: %q", got)
	}
	if !strings.Contains(got, "check the imports") {
		t.Errorf("text preceding the unmatched tag was discarded: %q", got)
	}
	if strings.Contains(got, "</think>") {
		t.Error("the closing tag survived into the answer")
	}
	if !resp.Decoding.ReasoningReclassified {
		t.Error("the possible prefill was not reported to the caller")
	}
}

func TestUsageCarriesCacheReuseAndThroughput(t *testing.T) {
	a := sseStream(t,
		`data: {"choices":[{"delta":{"content":"ok"}}]}`,
		`data: {"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":14738,"completion_tokens":12,`+
			`"prompt_tokens_details":{"cached_tokens":14722}},"timings":{"predicted_per_second":25.87,"prompt_per_second":122.6}}`,
		`data: [DONE]`,
	)
	resp, _ := drain(t, a, llm.Request{Model: "qwen"})

	if resp.Usage.CacheReadTokens != 14722 {
		t.Errorf("CacheReadTokens = %d; the server reported its prefix-cache hit and it was dropped",
			resp.Usage.CacheReadTokens)
	}
	if resp.Usage.OutputTokensPerSecond == 0 {
		t.Error("throughput was reported by the server and dropped")
	}
	if reuse := resp.Usage.CacheReuse(); reuse < 0.99 {
		t.Errorf("CacheReuse = %.3f, want ~1.0", reuse)
	}
}

func TestCacheReuseIsUnknownRatherThanZeroWhenUnreported(t *testing.T) {
	a := sseStream(t,
		`data: {"choices":[{"delta":{"content":"ok"}}]}`,
		`data: {"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":2}}`,
		`data: [DONE]`,
	)
	resp, _ := drain(t, a, llm.Request{Model: "qwen"})
	if got := resp.Usage.CacheReuse(); got != -1 {
		t.Fatalf("CacheReuse = %v; a server that said nothing must not read as zero reuse", got)
	}
}

func TestBothOutputCapSpellingsAreSent(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	a := New(Options{
		Name: "local", BaseURL: server.URL,
		Validate: func(llm.Request) error { return nil },
		Header:   func() (http.Header, error) { return http.Header{}, nil },
	})
	topK := 20
	seed := 7
	pp := 1.5
	s, err := a.Stream(context.Background(), llm.Request{Model: "qwen", MaxTokens: 4096,
		TopK: &topK, Seed: &seed, PresencePenalty: &pp})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	// llama.cpp and several other local servers read max_tokens and ignore
	// max_completion_tokens, so sending only the newer name means no cap at all.
	if body["max_tokens"] != float64(4096) {
		t.Errorf("max_tokens = %v, want 4096", body["max_tokens"])
	}
	if body["max_completion_tokens"] != float64(4096) {
		t.Errorf("max_completion_tokens = %v, want 4096", body["max_completion_tokens"])
	}
	if body["top_k"] != float64(20) {
		t.Errorf("top_k = %v, want 20 (the model's own generation_config declares it)", body["top_k"])
	}
	if body["seed"] != float64(7) {
		t.Errorf("seed = %v, want 7", body["seed"])
	}
	if body["presence_penalty"] != 1.5 {
		t.Errorf("presence_penalty = %v", body["presence_penalty"])
	}
}

func TestUnsetSamplingFieldsAreOmittedEntirely(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	a := New(Options{
		Name: "local", BaseURL: server.URL,
		Validate: func(llm.Request) error { return nil },
		Header:   func() (http.Header, error) { return http.Header{}, nil },
	})
	s, err := a.Stream(context.Background(), llm.Request{Model: "qwen"})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	// A server that rejects unknown fields must not see a field the caller
	// never set. Omission is the difference between a default and a decision.
	for _, key := range []string{"top_k", "seed", "presence_penalty", "frequency_penalty", "min_p", "temperature"} {
		if _, present := body[key]; present {
			t.Errorf("%s was sent despite being unset", key)
		}
	}
}

// Ids the harness invents must be unique for the life of the session, not just
// within one response.
//
// A local server that streams tool calls without an id leaves the harness to
// mint one. The obvious construction — the call's position in this response —
// restarts on the next step, so step 1 and step 2 both produce the same id.
// session.Log.DeriveMessages pairs each tool result to its call by id, so two
// steps sharing an id make the projection replay one step's output as the
// answer to both: twelve steps measured as twelve copies of step three.
func TestSynthesizedCallIDsAreUniqueAcrossResponses(t *testing.T) {
	seen := map[string]int{}
	// Ten responses, each shaped like a fresh step of one turn: the server
	// sends a tool call with no id, at index 0, every time.
	for step := 0; step < 10; step++ {
		s := &stream{calls: map[int]*callAccumulator{}}
		s.applyToolCalls([]wireToolCall{{
			Index:    0,
			Function: wireCallFunc{Name: "read_file", Arguments: `{"path":"a.go"}`},
		}})
		for _, acc := range s.calls {
			seen[acc.id]++
		}
	}
	if len(seen) != 10 {
		t.Fatalf("10 responses produced %d distinct call ids: %v", len(seen), seen)
	}

	// The fallback path mints ids too, and had the same per-response counter.
	offered := []llm.ToolSchema{{Name: "devcouncil_grep"}}
	payload := "<tool_call><function=devcouncil_grep><parameter=pattern>x</parameter></function></tool_call>"
	fallback := map[llm.CallID]int{}
	for step := 0; step < 10; step++ {
		_, calls, _ := extractFallbackToolCalls(payload, offered)
		if len(calls) != 1 {
			t.Fatalf("step %d recovered %d calls", step, len(calls))
		}
		fallback[calls[0].ID]++
	}
	if len(fallback) != 10 {
		t.Fatalf("10 recovered calls produced %d distinct ids: %v", len(fallback), fallback)
	}
}
