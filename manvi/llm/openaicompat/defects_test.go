package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"manvi/llm"
)

// --- 1. repairJSONLiterals must be linear ----------------------------------

// repairJSONLiterals ran in quadratic time *and* quadratic allocations, which
// made it a denial of service reachable from model output alone.
//
// replaceWordLiteral scanned the payload rune by rune and asked
// strings.HasPrefix(string(runes[i:]), oldWord) at every position, and
// string(runes[i:]) re-materialises the entire remaining payload — one
// allocation the size of the input, per input rune. Measured on the unfixed
// code: 5KB=40ms, 10KB=160ms, 20KB=649ms, 40KB=2.55s, 80KB=10.3s, a clean 4x
// per doubling, and 136MB allocated for a 16000-rune payload.
//
// The trigger is not exotic: it is exactly the case the function exists for. A
// tool call whose arguments fail json.Valid and contain a bare True/False/None
// — a Python-literal payload truncated at max_tokens — reaches it, and
// sanitizeJSONArguments may call it three times on one payload. It runs inside
// Response(), after the stream has ended and the stall watchdog has already
// been stopped, so nothing bounds it: the turn simply stops responding while a
// core spins.
//
// Allocation volume is asserted rather than wall time because it is
// deterministic on a loaded machine; the time bound is a loose backstop.
func TestRepairJSONLiteralsDoesNotScaleQuadratically(t *testing.T) {
	// A payload that forces the slow path: not valid JSON, containing the bare
	// literals the repair exists to fix, and — crucially — mostly *outside*
	// string values. The in-string branch was always linear; the scan that
	// re-materialised the tail ran only on unquoted content, which is what a
	// truncated array of numbers or a Python-repr payload is made of.
	const runes = 16384
	payload := `{"xs":[` + strings.Repeat("1,", runes/2) + `True,False,None`

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	got := repairJSONLiterals(payload)
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)

	if !strings.HasSuffix(got, `true,false,null`) {
		t.Fatalf("the repair stopped working: …%.40q", got[max(0, len(got)-40):])
	}

	allocated := after.TotalAlloc - before.TotalAlloc
	// A linear implementation allocates on the order of the payload size once
	// per pass; the quadratic one allocated 136MB here. 4MiB separates the two
	// by two orders of magnitude without being tight enough to flake.
	if allocated > 4<<20 {
		t.Errorf("repairJSONLiterals allocated %d bytes for a %d-byte payload; "+
			"that is the quadratic re-materialisation, not a linear scan",
			allocated, len(payload))
	}
	if elapsed > 250*time.Millisecond {
		t.Errorf("repairJSONLiterals took %v on a %d-byte payload", elapsed, len(payload))
	}
}

// BenchmarkRepairJSONLiterals exists so a regression shows up as a number
// rather than as a hung turn. Doubling the input must roughly double the cost;
// quadrupling it is the bug returning.
func BenchmarkRepairJSONLiterals(b *testing.B) {
	for _, size := range []int{4096, 8192, 16384} {
		b.Run(fmt.Sprintf("%d", size), func(b *testing.B) {
			payload := `{"xs":[` + strings.Repeat("1,", size/2) + `True,None`
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = repairJSONLiterals(payload)
			}
		})
	}
}

// --- 2. two <function=…> blocks in one <tool_call> -------------------------

// Two function blocks inside one <tool_call> merged into a single call.
//
// parseXMLCall found the first <function=name>, then scanned for <parameter=>
// to the end of the whole payload — straight past the </function> that closed
// its own block. The second tool vanished and its arguments were attached to
// the first. The shape is what makes it dangerous rather than merely lossy:
// delete_file's "target" arrived as an extra argument on read_file, so the
// arguments a model wrote for one tool were handed to a different one.
func TestTwoFunctionBlocksInOneToolCallDoNotMerge(t *testing.T) {
	payload := `<tool_call><function=read_file><parameter=path>a.txt</parameter></function>` +
		`<function=delete_file><parameter=target>b.txt</parameter></function></tool_call>`
	offered := []llm.ToolSchema{{Name: "read_file"}, {Name: "delete_file"}}

	_, calls, format := extractFallbackToolCalls(payload, offered)
	if len(calls) != 2 {
		t.Fatalf("expected both calls, got %d: %+v", len(calls), calls)
	}
	if calls[0].Name != "read_file" || calls[1].Name != "delete_file" {
		t.Fatalf("names = %q, %q", calls[0].Name, calls[1].Name)
	}
	if format != FallbackQwenXML {
		t.Errorf("format = %q", format)
	}

	var first, second map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(calls[1].Arguments, &second); err != nil {
		t.Fatal(err)
	}
	if _, leaked := first["target"]; leaked {
		t.Errorf("delete_file's argument was attached to read_file: %v", first)
	}
	if first["path"] != "a.txt" {
		t.Errorf("read_file path = %v", first["path"])
	}
	if second["target"] != "b.txt" {
		t.Errorf("delete_file target = %v", second["target"])
	}
	if calls[0].ID == calls[1].ID {
		t.Error("both calls share an id; their results cannot be paired back")
	}
}

// A function block whose own closing tag never arrived is a truncated
// response, and the whole <tool_call> is refused rather than half-recovered —
// the same rule the unterminated <tool_call> case already follows.
func TestATruncatedSecondFunctionBlockRefusesTheWholeToolCall(t *testing.T) {
	// The <tool_call> itself is terminated, so the unterminated-block rule
	// that already guards a truncated <tool_call> cannot be what saves this.
	payload := `<tool_call><function=read_file><parameter=path>a.txt</parameter></function>` +
		`<function=delete_file><parameter=target>b.tx</tool_call>`
	offered := []llm.ToolSchema{{Name: "read_file"}, {Name: "delete_file"}}

	text, calls, _ := extractFallbackToolCalls(payload, offered)
	if len(calls) != 0 {
		t.Fatalf("recovered %d call(s) from a truncated block: %+v", len(calls), calls)
	}
	if text != payload {
		t.Error("the text was altered even though nothing was recovered")
	}
}

// --- 3. a value containing </parameter> ------------------------------------

// A parameter value containing the literal "</parameter>" was silently
// truncated at the first occurrence, because the scan took strings.Index and
// stopped there.
//
// This is not a contrived input. Any write_file or edit whose content is
// XML-ish hits it — including this repository's own test fixtures, which are
// full of tool-call markup. The truncated content was then written to disk
// with no error raised anywhere: the model asked for one file and a shorter,
// different one appeared.
func TestAParameterValueMayContainItsOwnClosingTag(t *testing.T) {
	payload := `<tool_call><function=write_file>` +
		`<parameter=path>fixture.txt</parameter>` +
		`<parameter=content>prefix </parameter> suffix</parameter>` +
		`</function></tool_call>`
	offered := []llm.ToolSchema{{
		Name:        "write_file",
		InputSchema: []byte(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}}}`),
	}}

	_, calls, _ := extractFallbackToolCalls(payload, offered)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	var args map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatal(err)
	}
	if args["path"] != "fixture.txt" {
		t.Errorf("path = %v", args["path"])
	}
	if args["content"] != "prefix </parameter> suffix" {
		t.Errorf("content = %#v; the value was truncated at the embedded tag", args["content"])
	}
}

// --- 4. deltas that omit index ---------------------------------------------

// A server that omits "index" made two tool calls collapse into one.
//
// delta.Index is an int, so an absent field decodes as 0 for every fragment of
// every call, and both calls accumulated into the same slot. The accumulator
// kept the LAST id and the LAST name while holding BOTH calls' arguments, so
// the wreckage was reported under the wrong tool's name: a malformed-call
// report blamed "beta" for a payload that was mostly alpha's.
//
// Whether any real server omits index is unverified — but the field is
// optional in the shape this wire is copied from, and a harness must not
// corrupt when it is absent. The id is the discriminator when present.
func TestDeltasWithoutAnIndexAreKeptApartByTheirIDs(t *testing.T) {
	s := newStream(nil, "m", nil, Options{Name: "local"}, 0)
	// Neither fragment carries an index; the continuation fragments carry
	// neither an index nor an id, which is what a fragment-per-token server
	// looks like.
	s.applyToolCalls([]wireToolCall{{ID: "c1", Function: wireCallFunc{Name: "alpha", Arguments: `{"a":`}}})
	s.applyToolCalls([]wireToolCall{{Function: wireCallFunc{Arguments: `1}`}}})
	s.applyToolCalls([]wireToolCall{{ID: "c2", Function: wireCallFunc{Name: "beta", Arguments: `{"b":`}}})
	s.applyToolCalls([]wireToolCall{{Function: wireCallFunc{Arguments: `2}`}}})
	s.done = true

	resp, err := s.Response()
	if err != nil {
		t.Fatalf("Response: %v", err)
	}
	if len(resp.Malformed) != 0 {
		t.Fatalf("reported %d malformed call(s): %+v", len(resp.Malformed), resp.Malformed)
	}
	var got []llm.ToolCallBlock
	for _, block := range resp.Message.Content {
		if call, ok := block.(llm.ToolCallBlock); ok {
			got = append(got, call)
		}
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 calls, got %d: %+v", len(got), got)
	}
	if got[0].Name != "alpha" || string(got[0].Arguments) != `{"a":1}` {
		t.Errorf("call 0 = %s %s", got[0].Name, got[0].Arguments)
	}
	if got[1].Name != "beta" || string(got[1].Arguments) != `{"b":2}` {
		t.Errorf("call 1 = %s %s", got[1].Name, got[1].Arguments)
	}
	if got[0].ID != "c1" || got[1].ID != "c2" {
		t.Errorf("ids = %q, %q; the server's own ids must be preserved", got[0].ID, got[1].ID)
	}
}

// The wire's index stays the primary key: a server that does send indices and
// repeats the id on every fragment must not have its calls split apart.
func TestARepeatedIDOnEveryFragmentStillAccumulatesOneCall(t *testing.T) {
	s := newStream(nil, "m", nil, Options{Name: "local"}, 0)
	s.applyToolCalls([]wireToolCall{{Index: 0, ID: "x", Function: wireCallFunc{Name: "alpha", Arguments: `{"a":`}}})
	s.applyToolCalls([]wireToolCall{{Index: 0, ID: "x", Function: wireCallFunc{Arguments: `1}`}}})
	if len(s.calls) != 1 {
		t.Fatalf("one call was split into %d", len(s.calls))
	}
	if got := s.calls[0].args.String(); got != `{"a":1}` {
		t.Errorf("args = %q", got)
	}
}

// An id that arrives only on a later fragment identifies the call already open
// at that index rather than opening a second one. The accumulator mints a
// synthetic id as soon as it is created, so this cannot be decided by asking
// whether it already has one.
func TestALateArrivingIDAdoptsTheOpenCall(t *testing.T) {
	s := newStream(nil, "m", nil, Options{Name: "local"}, 0)
	s.applyToolCalls([]wireToolCall{{Index: 0, Function: wireCallFunc{Name: "alpha", Arguments: `{"a":`}}})
	s.applyToolCalls([]wireToolCall{{Index: 0, ID: "real", Function: wireCallFunc{Arguments: `1}`}}})
	if len(s.calls) != 1 {
		t.Fatalf("one call was split into %d", len(s.calls))
	}
	if got := s.calls[0].args.String(); got != `{"a":1}` {
		t.Errorf("args = %q", got)
	}
	if s.calls[0].id != "real" {
		t.Errorf("id = %q, want the server's own id", s.calls[0].id)
	}
}

// --- 5. typedParam and declared container types ----------------------------

// typedParam asked json.Valid for a declared object or array, and json.Valid
// is true for every scalar: "123" is valid JSON, so a parameter declared
// "object" was emitted as the number 123, and one declared "array" as the
// boolean true. Schema-driven coercion that produces the wrong kind is worse
// than no coercion — the documented conservative answer is the string.
func TestADeclaredContainerParameterRefusesAScalar(t *testing.T) {
	payload := `<tool_call><function=cfg>` +
		`<parameter=opts>123</parameter>` +
		`<parameter=items>true</parameter>` +
		`<parameter=good>{"k":1}</parameter>` +
		`<parameter=alsogood>[1,2]</parameter>` +
		`</function></tool_call>`
	offered := []llm.ToolSchema{{
		Name: "cfg",
		InputSchema: []byte(`{"type":"object","properties":{
			"opts":{"type":"object"},"items":{"type":"array"},
			"good":{"type":"object"},"alsogood":{"type":"array"}}}`),
	}}

	_, calls, _ := extractFallbackToolCalls(payload, offered)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	var args map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatal(err)
	}
	if got, ok := args["opts"].(string); !ok || got != "123" {
		t.Errorf("opts = %#v, want the conservative string \"123\"", args["opts"])
	}
	if got, ok := args["items"].(string); !ok || got != "true" {
		t.Errorf("items = %#v, want the conservative string \"true\"", args["items"])
	}
	// A real container still coerces; the fix must not disable the feature.
	if _, ok := args["good"].(map[string]any); !ok {
		t.Errorf("good = %#v, want an object", args["good"])
	}
	if _, ok := args["alsogood"].([]any); !ok {
		t.Errorf("alsogood = %#v, want an array", args["alsogood"])
	}
}

// --- 6. recovered arguments must be an object ------------------------------

// A recovered call carried whatever the "arguments" key held, verbatim. A
// model that wrote {"name":"read_file","arguments":"not an object"} produced a
// tool call whose Arguments was a JSON *string*, which every consumer of a
// ToolCallBlock unmarshals into a map and fails on — far from here, with
// nothing pointing back at the response that caused it.
//
// The adversarial suite already fed this input, but only asserted json.Valid,
// so it was covered as "does not panic" rather than as "is not a defect".
func TestRecoveredArgumentsAreAlwaysAJSONObject(t *testing.T) {
	offered := []llm.ToolSchema{{Name: "read_file"}}

	t.Run("a non-object string is not a call", func(t *testing.T) {
		payload := `<tool_call>{"name":"read_file","arguments":"not an object"}</tool_call>`
		text, calls, _ := extractFallbackToolCalls(payload, offered)
		if len(calls) != 0 {
			t.Fatalf("recovered %d call(s) with non-object arguments: %s", len(calls), calls[0].Arguments)
		}
		if text != payload {
			t.Error("the block was deleted from the text even though nothing was recovered")
		}
	})

	t.Run("a scalar is not a call", func(t *testing.T) {
		payload := `<tool_call>{"name":"read_file","arguments":42}</tool_call>`
		_, calls, _ := extractFallbackToolCalls(payload, offered)
		if len(calls) != 0 {
			t.Fatalf("recovered %d call(s) with scalar arguments: %s", len(calls), calls[0].Arguments)
		}
	})

	// The one non-object shape that *is* a call: OpenAI's own wire spells
	// arguments as a JSON-encoded string, and models copy that. Unwrapping it
	// recovers a real call that was previously handed on as a string.
	t.Run("a JSON-encoded object string is unwrapped", func(t *testing.T) {
		payload := `<tool_call>{"name":"read_file","arguments":"{\"path\":\"a.txt\"}"}</tool_call>`
		_, calls, _ := extractFallbackToolCalls(payload, offered)
		if len(calls) != 1 {
			t.Fatalf("expected 1 call, got %d", len(calls))
		}
		var args map[string]any
		if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
			t.Fatalf("arguments are not an object: %s (%v)", calls[0].Arguments, err)
		}
		if args["path"] != "a.txt" {
			t.Errorf("path = %v", args["path"])
		}
	})
}

// A rejected block must stay in the text. parseCallPayload's own comment
// promises this — "rejecting leaves the block in the text" — but it only held
// when *nothing* was recovered, because the walker wrote a rejected payload
// nowhere. With one good call alongside one bad one, the bad one vanished
// entirely: the model's request neither ran nor appeared in the answer.
func TestARejectedBlockSurvivesAlongsideARecoveredOne(t *testing.T) {
	payload := `<tool_call>{"name":"read_file","arguments":{"path":"a.txt"}}</tool_call>` +
		`<tool_call>{"name":"not_offered","arguments":{"x":1}}</tool_call>`
	offered := []llm.ToolSchema{{Name: "read_file"}}

	text, calls, _ := extractFallbackToolCalls(payload, offered)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if !strings.Contains(text, "not_offered") {
		t.Errorf("the rejected block was deleted rather than left as prose: %q", text)
	}
}

// --- 7. Response() idempotence ---------------------------------------------

// Response() appended to s.malformed on every call, so asking twice reported
// two malformed calls for one broken call — and three for a caller that
// logged, rendered and then persisted the same response. The settled response
// is a value, and reading it must not change it.
func TestResponseIsIdempotent(t *testing.T) {
	s := newStream(nil, "m", nil, Options{Name: "local"}, 0)
	s.applyToolCalls([]wireToolCall{
		{Index: 0, ID: "broken", Function: wireCallFunc{Name: "alpha", Arguments: `{"a":`}},
	})
	s.done = true

	first, err := s.Response()
	if err != nil {
		t.Fatalf("Response: %v", err)
	}
	second, err := s.Response()
	if err != nil {
		t.Fatalf("Response (second call): %v", err)
	}
	if len(first.Malformed) != 1 {
		t.Fatalf("first call reported %d malformed, want 1", len(first.Malformed))
	}
	if len(second.Malformed) != len(first.Malformed) {
		t.Errorf("second call reported %d malformed, first reported %d",
			len(second.Malformed), len(first.Malformed))
	}
	if len(second.Message.Content) != len(first.Message.Content) {
		t.Errorf("the settled message grew between calls: %d then %d",
			len(first.Message.Content), len(second.Message.Content))
	}
	if second.StopReason != first.StopReason {
		t.Errorf("stop reason changed between calls: %q then %q", first.StopReason, second.StopReason)
	}
}

// --- 8. nested think blocks -------------------------------------------------

// A nested <think> leaked the chain of thought into the visible answer.
//
// The inner </think> closed the outer block, so everything after it was
// classified as text: "<think>outer <think>inner</think> still-thinking
// </think>ANSWER" produced text " still-thinkingANSWER" and left a bare
// "</think>" to be dropped as stray framing. The settled message is what gets
// logged and replayed, so the model's private reasoning became part of the
// answer and was fed back on every later step.
//
// The byte-conservation property in adversarial_test.go covers this input
// already, but conservation cannot see a byte landing on the wrong side.
func TestNestedThinkTagsDoNotLeakReasoningIntoTheAnswer(t *testing.T) {
	visible, reasoning, _ := SplitReasoning("<think>outer <think>inner</think> still-thinking</think>ANSWER", false)
	if visible != "ANSWER" {
		t.Errorf("visible = %q, want %q", visible, "ANSWER")
	}
	if !strings.Contains(reasoning, "still-thinking") {
		t.Errorf("reasoning = %q; the tail of the block leaked into the answer", reasoning)
	}
}

// --- 9. unbounded accumulation ----------------------------------------------

// Nothing bounded how much a stream would buffer. s.text, s.reasoning and each
// accumulator's arguments grew without limit, and a server that keeps emitting
// also keeps resetting the stall watchdog — so a server ignoring its own
// max_tokens is not a slow turn, it is the harness consuming memory until the
// machine gives out. Measured: 8,192,000 bytes past any cap, all of it held.
//
// Exceeding the bound has to be loud. Truncating the text and settling it as
// the answer would be the silent corruption this whole package is written to
// avoid.
func TestAnUnboundedResponseIsRefusedRatherThanBuffered(t *testing.T) {
	const frameBytes = 64 << 10
	filler := strings.Repeat("x", frameBytes)
	frames := make([]string, 0, 256)
	for i := 0; i < 256; i++ { // 16 MiB, four times the bound
		frames = append(frames, `data: {"choices":[{"delta":{"content":"`+filler+`"}}]}`)
	}
	frames = append(frames, `data: {"choices":[{"finish_reason":"stop"}]}`, `data: [DONE]`)

	a := sseStream(t, frames...)
	s, err := a.Stream(t.Context(), llm.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()

	var streamed int
	var streamErr error
	for {
		c, err := s.Next()
		if err != nil {
			streamErr = err
			break
		}
		streamed += len(c.Text)
		if streamed > 64<<20 {
			t.Fatal("the stream buffered 64MiB without complaint")
		}
	}
	if streamErr == nil {
		t.Fatal("a 16MiB response settled without error")
	}
	if !strings.Contains(streamErr.Error(), "decode") && !strings.Contains(streamErr.Error(), "limit") {
		t.Errorf("the error does not name the bound: %v", streamErr)
	}
	if _, err := s.Response(); err == nil {
		t.Error("Response settled a truncated answer instead of reporting the refusal")
	}
}

// A response comfortably inside the bound must be unaffected. The bound exists
// to stop a runaway, not to cap real work: a local model writing a large file
// through a tool call is legitimate and must still land.
func TestALargeButLegitimateResponseIsNotRefused(t *testing.T) {
	body := strings.Repeat("y", 512<<10)
	a := sseStream(t,
		`data: {"choices":[{"delta":{"content":"`+body+`"}}]}`,
		`data: {"choices":[{"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	)
	resp, _ := drain(t, a, llm.Request{Model: "m"})
	if len(resp.Message.Text()) != len(body) {
		t.Errorf("a 512KiB answer came back as %d bytes", len(resp.Message.Text()))
	}
}

// --- 10. tag matching is deliberately exact ---------------------------------

// This test records a decision, not a defect: <THINK> and <think > are not
// recognised, and that is deliberate.
//
// The tags this filter reads are emitted by a chat template, not chosen by the
// model — Qwen3, DeepSeek-R1 and the Hermes templates all write exactly
// "<think>" in lowercase with no interior space, and a template does not vary
// its spelling between generations. What *does* vary is prose: a model
// discussing this harness writes about think tags in whatever case it likes,
// and in a codebase whose own source is full of them that is the common input,
// not the exotic one.
//
// So loosening the match trades a cosmetic, visible fault for a silent,
// invisible one. Failing to recognise <THINK> leaks reasoning into an answer,
// where an operator can see it and set AssumeReasoningPrefill or fix their
// server. Recognising it in prose deletes the surrounding answer instead —
// the exact failure the retroactive-reclassification removal was about, and
// unrecoverable once it has happened.
//
// If a template that spells the tag differently ever turns up, the fix is to
// add its literal spelling to openTags/closeTags, not to relax the match.
func TestTagMatchingIsDeliberatelyCaseAndSpaceExact(t *testing.T) {
	for _, in := range []string{
		"<THINK>reasoning</THINK>answer",
		"<think >reasoning</think >answer",
	} {
		visible, reasoning, _ := SplitReasoning(in, false)
		if reasoning != "" {
			t.Errorf("input %q: reasoning = %q; a spelling variant was recognised, "+
				"which means prose about think tags can now delete an answer", in, reasoning)
		}
		if visible != in {
			t.Errorf("input %q: visible = %q, want the input unchanged", in, visible)
		}
	}
}

// --- 11. a </think> inside a tool argument ----------------------------------

// A tool argument whose value contained "</think>" once destroyed the whole
// call. SplitReasoning runs before extractFallbackToolCalls, the unmatched
// closing tag was read as proof of a prefilled think block, and everything
// before it — the <tool_call> opening, the function name, the parameter key —
// was retroactively moved into reasoning. The result was zero calls, no
// malformed report, and a turn that reported success having done nothing.
//
// Removing that retroactive reclassification fixed the destruction: the text
// stays text and only the tag itself is dropped as framing, so the call is
// recovered. This pins that down.
//
// The residual this test used to record — the "</think>" being deleted from the
// argument *value* — is fixed in section 12: the filter now suspends tag
// interpretation inside tool-call markup. The assertions here stay as they
// were, deliberately loose about the value, because what they pin is the
// separate property that the *call survives at all*. Section 12 pins the value.
func TestAToolArgumentContainingAClosingThinkTagStillYieldsACall(t *testing.T) {
	raw := `<tool_call><function=write><parameter=s>please strip </think> markers</parameter>` +
		`<parameter=path>notes.md</parameter></function></tool_call>`
	offered := []llm.ToolSchema{{
		Name:        "write",
		InputSchema: []byte(`{"type":"object","properties":{"s":{"type":"string"},"path":{"type":"string"}}}`),
	}}

	rec := RecoverFromText(raw, offered, false)
	if len(rec.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d (text=%q reasoning=%q)", len(rec.Calls), rec.Text, rec.Reasoning)
	}
	if rec.Calls[0].Name != "write" {
		t.Fatalf("name = %q", rec.Calls[0].Name)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(rec.Calls[0].Arguments), &args); err != nil {
		t.Fatal(err)
	}
	if args["path"] != "notes.md" {
		t.Errorf("path = %v; a later parameter was lost", args["path"])
	}
	s, _ := args["s"].(string)
	if !strings.Contains(s, "please strip") || !strings.Contains(s, "markers") {
		t.Errorf("s = %q; the value either side of the tag must survive", s)
	}
	if rec.Reasoning != "" {
		t.Errorf("reasoning = %q; the call payload was moved into reasoning", rec.Reasoning)
	}
}

// --- 12. reasoning tags inside tool-call markup ------------------------------

// filterChunks runs the streaming filter over an already-split byte stream and
// returns what the stream would have accumulated. The streaming path and
// RecoverFromText share this filter, so a test that drives it directly is
// testing the same code the SSE loop runs — only with chunk boundaries the test
// chooses rather than the ones a server happens to send.
func filterChunks(chunks []string, assumePrefill bool) (text, reasoning string, suspected bool) {
	f := &tagFilter{assumePrefill: assumePrefill}
	var vis, think strings.Builder
	for _, chunk := range chunks {
		for _, c := range f.feed(chunk) {
			vis.WriteString(c.text)
			think.WriteString(c.reasoning)
		}
	}
	fl := f.flush()
	vis.WriteString(fl.text)
	think.WriteString(fl.reasoning)
	return vis.String(), think.String(), f.prefillSuspected
}

// A "</think>" inside a tool-call *argument value* was deleted from that value.
//
// The reasoning filter runs over the raw byte stream, before anything knows the
// bytes are a tool-call payload, so tag stripping was applied to markup rather
// than to prose. write(s="please strip </think> markers") reached the tool as
// "please strip  markers" — a file written through that call lands on disk with
// content the model never asked for, and nothing reports the edit.
//
// It also set Decoding.ReasoningReclassified, which tells the operator their
// server might be prefilling <think> when it plainly is not: the closing tag
// was payload the model was asked to write, not evidence about the template.
func TestAToolArgumentKeepsALiteralClosingThinkTag(t *testing.T) {
	const want = "please strip </think> markers"
	raw := `<tool_call><function=write><parameter=path>notes.md</parameter>` +
		`<parameter=s>` + want + `</parameter></function></tool_call>`
	offered := []llm.ToolSchema{{
		Name:        "write",
		InputSchema: []byte(`{"type":"object","properties":{"s":{"type":"string"},"path":{"type":"string"}}}`),
	}}

	rec := RecoverFromText(raw, offered, false)
	if len(rec.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d (text=%q reasoning=%q)", len(rec.Calls), rec.Text, rec.Reasoning)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(rec.Calls[0].Arguments), &args); err != nil {
		t.Fatal(err)
	}
	if got, _ := args["s"].(string); got != want {
		t.Errorf("s = %q, want %q; the argument value was rewritten in flight", got, want)
	}
	if args["path"] != "notes.md" {
		t.Errorf("path = %v; a sibling parameter was lost", args["path"])
	}
	if rec.Reclassified {
		t.Error("Reclassified is set; a closing tag inside tool-call payload is not " +
			"evidence that the server prefills <think>")
	}
	if rec.Reasoning != "" {
		t.Errorf("reasoning = %q; tool-call payload is not reasoning", rec.Reasoning)
	}
}

// The matched-pair spelling of the same defect. A model asked to write a file
// that documents this harness's own reasoning tags emits both halves inside one
// argument value, and the filter used to swallow the pair and everything
// between it — here the whole sentence would have become "reasoning".
func TestAToolArgumentKeepsAMatchedThinkPair(t *testing.T) {
	const want = "docs say <think>plan here</think> is stripped"
	raw := `<tool_call><function=write><parameter=path>guide.md</parameter>` +
		`<parameter=s>` + want + `</parameter></function></tool_call>`
	offered := []llm.ToolSchema{{
		Name:        "write",
		InputSchema: []byte(`{"type":"object","properties":{"s":{"type":"string"},"path":{"type":"string"}}}`),
	}}

	rec := RecoverFromText(raw, offered, false)
	if len(rec.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d (text=%q reasoning=%q)", len(rec.Calls), rec.Text, rec.Reasoning)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(rec.Calls[0].Arguments), &args); err != nil {
		t.Fatal(err)
	}
	if got, _ := args["s"].(string); got != want {
		t.Errorf("s = %q, want %q", got, want)
	}
	if rec.Reasoning != "" {
		t.Errorf("reasoning = %q; the pair was inside tool-call payload", rec.Reasoning)
	}
}

// The same, arriving as a stream. A "<tool_call>" opener splits across SSE
// frames exactly as a "<think>" tag can, and the protection has to survive
// every one of those splits or the protection is decorative: a server that
// happens to break the frame after "<tool" would strip the payload anyway.
//
// Every single-byte boundary is exercised, plus the all-at-once case.
func TestASplitToolCallOpenerStillProtectsItsPayload(t *testing.T) {
	const want = "please strip </think> markers"
	raw := `<tool_call><function=write><parameter=path>notes.md</parameter>` +
		`<parameter=s>` + want + `</parameter></function></tool_call>`
	offered := []llm.ToolSchema{{
		Name:        "write",
		InputSchema: []byte(`{"type":"object","properties":{"s":{"type":"string"},"path":{"type":"string"}}}`),
	}}

	check := func(t *testing.T, chunks []string) {
		t.Helper()
		text, reasoning, suspected := filterChunks(chunks, false)
		if reasoning != "" {
			t.Fatalf("reasoning = %q; tool-call payload was classified as reasoning", reasoning)
		}
		if suspected {
			t.Error("prefillSuspected set by a closing tag inside tool-call payload")
		}
		if text != raw {
			t.Fatalf("filtered text = %q,\n              want %q", text, raw)
		}
		_, calls, _ := extractFallbackToolCalls(text, offered)
		if len(calls) != 1 {
			t.Fatalf("expected 1 call from %q, got %d", text, len(calls))
		}
		var args map[string]any
		if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
			t.Fatal(err)
		}
		if got, _ := args["s"].(string); got != want {
			t.Errorf("s = %q, want %q", got, want)
		}
	}

	t.Run("whole", func(t *testing.T) { check(t, []string{raw}) })

	// One frame boundary at every byte offset.
	for split := 1; split < len(raw); split++ {
		t.Run(fmt.Sprintf("split-at-%d", split), func(t *testing.T) {
			check(t, []string{raw[:split], raw[split:]})
		})
	}

	// And the pathological server that sends one byte per frame.
	t.Run("byte-by-byte", func(t *testing.T) {
		chunks := make([]string, 0, len(raw))
		for i := 0; i < len(raw); i++ {
			chunks = append(chunks, raw[i:i+1])
		}
		check(t, chunks)
	})
}

// The fix must not be "stop filtering". Reasoning outside tool-call markup is
// still reasoning, including reasoning that sits immediately beside a call in
// the same response, and an unmatched closing tag out there still reports the
// prefill suspicion it always did.
func TestReasoningOutsideToolCallMarkupIsStillStripped(t *testing.T) {
	offered := []llm.ToolSchema{{
		Name:        "write",
		InputSchema: []byte(`{"type":"object","properties":{"s":{"type":"string"}}}`),
	}}
	call := `<tool_call><function=write><parameter=s>keep </think> this</parameter></function></tool_call>`

	t.Run("think block before a call", func(t *testing.T) {
		rec := RecoverFromText("<think>I should write the file</think>ok"+call, offered, false)
		if rec.Reasoning != "I should write the file" {
			t.Errorf("reasoning = %q; genuine reasoning stopped being separated", rec.Reasoning)
		}
		if rec.Text != "ok" {
			t.Errorf("text = %q, want %q", rec.Text, "ok")
		}
		if len(rec.Calls) != 1 {
			t.Fatalf("expected 1 call, got %d", len(rec.Calls))
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(rec.Calls[0].Arguments), &args); err != nil {
			t.Fatal(err)
		}
		if got, _ := args["s"].(string); got != "keep </think> this" {
			t.Errorf("s = %q", got)
		}
	})

	t.Run("think block after a call", func(t *testing.T) {
		rec := RecoverFromText(call+"<think>now I wait</think>done", offered, false)
		if rec.Reasoning != "now I wait" {
			t.Errorf("reasoning = %q; protection outlived the closing </tool_call>", rec.Reasoning)
		}
		if rec.Text != "done" {
			t.Errorf("text = %q, want %q", rec.Text, "done")
		}
	})

	t.Run("unmatched closer outside markup still reports", func(t *testing.T) {
		rec := RecoverFromText("the answer</think>continues"+call, offered, false)
		if !rec.Reclassified {
			t.Error("an unmatched closing tag in prose no longer reports the prefill suspicion")
		}
		if rec.Text != "the answercontinues" {
			t.Errorf("text = %q; the stray tag rule changed", rec.Text)
		}
	})

	t.Run("a considered call inside a think block is not dispatched", func(t *testing.T) {
		rec := RecoverFromText("<think>maybe "+call+"</think>no thanks", offered, false)
		if len(rec.Calls) != 0 {
			t.Fatalf("dispatched %d call(s) the model only thought about", len(rec.Calls))
		}
		if rec.Text != "no thanks" {
			t.Errorf("text = %q, want %q", rec.Text, "no thanks")
		}
	})
}

// A response cut off at max_tokens can open <tool_call> and never close it.
// The protected text must not vanish — no byte of it is recoverable from
// anywhere else — and the existing rule that an unterminated block is not
// recovered as a call has to keep holding, because half a payload is not a
// request.
func TestATruncatedToolCallKeepsItsTextAndYieldsNoCall(t *testing.T) {
	offered := []llm.ToolSchema{{
		Name:        "write",
		InputSchema: []byte(`{"type":"object","properties":{"s":{"type":"string"}}}`),
	}}
	raw := `writing it now <tool_call><function=write><parameter=s>please strip </think> mar`

	rec := RecoverFromText(raw, offered, false)
	if len(rec.Calls) != 0 {
		t.Fatalf("recovered %d call(s) from a truncated block", len(rec.Calls))
	}
	if rec.Text != raw {
		t.Errorf("text = %q,\n   want %q; truncated tool-call text was altered or lost", rec.Text, raw)
	}
	if rec.Reasoning != "" {
		t.Errorf("reasoning = %q; nothing here is reasoning", rec.Reasoning)
	}
	if rec.Reclassified {
		t.Error("Reclassified set from a closing tag inside a truncated tool-call payload")
	}

	// Byte-split too: the carry must be released, not dropped, when the stream
	// ends inside protected text.
	chunks := make([]string, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		chunks = append(chunks, raw[i:i+1])
	}
	text, reasoning, suspected := filterChunks(chunks, false)
	if text != raw || reasoning != "" || suspected {
		t.Errorf("streamed: text = %q, reasoning = %q, suspected = %v", text, reasoning, suspected)
	}
}

// Protection must not become a buffer. A <tool_call> the model opens and never
// closes — a response cut off at max_tokens — would, if the filter held its
// payload back waiting for a closer, accumulate the whole remainder of the
// generation in the filter and hand MaxDecodedResponseBytes a limit it could no
// longer see, since decodedBytes counts the carry but the stream never gets the
// text. Only a partial closing marker is ever held.
func TestProtectedToolCallPayloadIsNeverBuffered(t *testing.T) {
	f := &tagFilter{}
	var emitted int
	feed := func(chunk string) {
		for _, c := range f.feed(chunk) {
			emitted += len(c.text) + len(c.reasoning)
		}
	}
	feed("<tool_call><function=write><parameter=s>")
	const chunk = "some file content with </think> in it, over and over. "
	const rounds = 5000
	for i := 0; i < rounds; i++ {
		feed(chunk)
		if len(f.carry) >= len(toolCallClose) {
			t.Fatalf("round %d: carry is %d bytes (%q); at most %d may ever be held",
				i, len(f.carry), f.carry, len(toolCallClose)-1)
		}
	}
	if !f.inToolCall {
		t.Fatal("the block closed itself; the payload was not protected throughout")
	}
	fl := f.flush()
	emitted += len(fl.text) + len(fl.reasoning)
	want := len("<tool_call><function=write><parameter=s>") + rounds*len(chunk)
	if emitted != want {
		t.Errorf("emitted %d bytes, want %d; protected payload was withheld or duplicated",
			emitted, want)
	}
}

// --- a declared prefill contradicted by the server itself --------------------

// TestOutOfBandReasoningCancelsADeclaredPrefill.
//
// AssumeReasoningPrefill exists for a server that begins generation inside a
// thinking tag and never sends the opening one. A server that streams its
// thinking as reasoning_content is doing the opposite: it separated the two
// itself, and what arrives on the content channel is the answer.
//
// Believing the declaration over the wire deletes answers. Measured against
// omlx 0.6.2 serving Qwen3.8-27B: reasoning arrived as reasoning_content
// deltas, the answer arrived as content "\n\nDONE", no tag appeared anywhere,
// and the filter — told it had started inside a block — classified the answer
// as reasoning and flushed it as reasoning. Every text answer across a
// four-task benchmark was lost this way.
func TestOutOfBandReasoningCancelsADeclaredPrefill(t *testing.T) {
	// Reasoning first, as the observed server sends it.
	f := &tagFilter{assumePrefill: true}
	if !f.outOfBandReasoning() {
		t.Fatal("outOfBandReasoning did not report contradicting the declaration")
	}
	var vis, think strings.Builder
	for _, c := range f.feed("\n\nDONE") {
		vis.WriteString(c.text)
		think.WriteString(c.reasoning)
	}
	fl := f.flush()
	vis.WriteString(fl.text)
	think.WriteString(fl.reasoning)

	if got := strings.TrimSpace(vis.String()); got != "DONE" {
		t.Errorf("visible text = %q, want %q — the answer was classified as reasoning and discarded", got, "DONE")
	}
	if think.String() != "" {
		t.Errorf("reasoning = %q, want empty; content arriving beside reasoning_content is the answer", think.String())
	}
	if !f.prefillDisproved {
		t.Error("prefillDisproved = false; the contradiction must be reportable so the setting gets corrected")
	}
}

// The same, with content already in flight before the reasoning field appears.
// Ordering is not guaranteed, and the filter must still stop misclassifying.
func TestOutOfBandReasoningCancelsAPrefillAlreadyApplied(t *testing.T) {
	f := &tagFilter{assumePrefill: true}
	_ = f.feed("partial ") // consumed while still believing the declaration
	if !f.outOfBandReasoning() {
		t.Fatal("outOfBandReasoning did not cancel an assumption already applied")
	}
	var vis strings.Builder
	for _, c := range f.feed("answer") {
		vis.WriteString(c.text)
	}
	fl := f.flush()
	vis.WriteString(fl.text)
	if got := vis.String(); got != "answer" {
		t.Errorf("visible text = %q, want %q", got, "answer")
	}
}

// A block a real tag opened is not in question, and must survive.
func TestOutOfBandReasoningDoesNotCancelARealThinkBlock(t *testing.T) {
	f := &tagFilter{}
	var vis, think strings.Builder
	for _, c := range f.feed("<think>still thinking") {
		vis.WriteString(c.text)
		think.WriteString(c.reasoning)
	}
	if f.outOfBandReasoning() {
		t.Fatal("outOfBandReasoning cancelled a block opened by a real tag")
	}
	for _, c := range f.feed(" more</think>ANSWER") {
		vis.WriteString(c.text)
		think.WriteString(c.reasoning)
	}
	if got := vis.String(); got != "ANSWER" {
		t.Errorf("visible text = %q, want %q", got, "ANSWER")
	}
	if !strings.Contains(think.String(), "still thinking") {
		t.Errorf("reasoning = %q; a genuine think block was cancelled", think.String())
	}
}

// And with no declaration in play there is nothing to cancel.
func TestOutOfBandReasoningIsANoOpWithoutADeclaration(t *testing.T) {
	f := &tagFilter{}
	if f.outOfBandReasoning() {
		t.Fatal("outOfBandReasoning reported a contradiction with no prefill declared")
	}
}

// End to end over SSE, in exactly the shape omlx 0.6.2 sends: reasoning as
// reasoning_content deltas, the answer as content, and no tag anywhere. This is
// the frame sequence that lost every answer in the benchmark.
func TestDeclaredPrefillDoesNotEatAnAnswerSentBesideReasoningContent(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"The user wants "}}]}`,
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"exactly DONE."}}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"\n\nDONE"}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			_, _ = fmt.Fprint(w, f+"\n\n")
		}
	}))
	t.Cleanup(srv.Close)

	a := New(Options{
		Name: "local", BaseURL: srv.URL,
		AssumeReasoningPrefill: true, // the operator's declaration, wrong for this server
		Validate:               func(llm.Request) error { return nil },
		Header:                 func() (http.Header, error) { return http.Header{}, nil },
	})
	s, err := a.Stream(context.Background(), llm.Request{Model: "qwen"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()
	for {
		if _, err := s.Next(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	resp, err := s.Response()
	if err != nil {
		t.Fatalf("Response: %v", err)
	}

	if got := strings.TrimSpace(resp.Message.Text()); got != "DONE" {
		t.Errorf("Message.Text() = %q, want %q — the answer was filed as reasoning, and reasoning is "+
			"stripped before the caller ever sees the message", got, "DONE")
	}
	if !resp.Decoding.PrefillDisproved {
		t.Error("Decoding.PrefillDisproved = false; the operator must be told their declaration was " +
			"contradicted, or they discover it as missing answers")
	}
	if resp.Decoding.Clean() {
		t.Error("Decoding.Clean() = true; a dropped declaration is a compensation and must not read as healthy")
	}
}

// Ollama spells the out-of-band reasoning field "reasoning"; DeepSeek, vLLM and
// omlx spell it "reasoning_content". Reading only one of them discarded every
// reasoning token the other server produced, and left the prefill contradiction
// undetectable there — so a declared prefill still ate the answer.
func TestBothOutOfBandReasoningSpellingsAreRead(t *testing.T) {
	for _, tc := range []struct{ name, field string }{
		{"reasoning_content", "reasoning_content"},
		{"reasoning", "reasoning"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frames := []string{
				`data: {"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
				`data: {"choices":[{"index":0,"delta":{"` + tc.field + `":"thinking hard"}}]}`,
				`data: {"choices":[{"index":0,"delta":{"content":"DONE"}}]}`,
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for _, f := range frames {
					_, _ = fmt.Fprint(w, f+"\n\n")
				}
			}))
			t.Cleanup(srv.Close)

			a := New(Options{
				Name: "local", BaseURL: srv.URL,
				AssumeReasoningPrefill: true, // wrong for both of these servers
				Validate:               func(llm.Request) error { return nil },
				Header:                 func() (http.Header, error) { return http.Header{}, nil },
			})
			s, err := a.Stream(context.Background(), llm.Request{Model: "qwen"})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			defer func() { _ = s.Close() }()

			var sawReasoning bool
			for {
				c, err := s.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("Next: %v", err)
				}
				if c.Kind == llm.ChunkReasoning && strings.Contains(c.Text, "thinking hard") {
					sawReasoning = true
				}
			}
			resp, err := s.Response()
			if err != nil {
				t.Fatalf("Response: %v", err)
			}
			if !sawReasoning {
				t.Errorf("no reasoning chunk for %q: the server's thinking was read off the wire and dropped",
					tc.field)
			}
			if got := strings.TrimSpace(resp.Message.Text()); got != "DONE" {
				t.Errorf("Message.Text() = %q, want %q for %q", got, "DONE", tc.field)
			}
			if !resp.Decoding.PrefillDisproved {
				t.Errorf("PrefillDisproved = false for %q", tc.field)
			}
		})
	}
}

// A declared prefill, a real opening tag, and then out-of-band reasoning.
//
// The cancellation must not fire here. Once a genuine <think> has been consumed
// the filter is inside a block it saw opened, not one it was told about, and
// stepping out of that would spill the model's reasoning into the answer — the
// failure the filter's own comment calls the one that cannot be undone.
//
// This is the third state of outOfBandReasoning and the only one the other
// tests miss: they either declare no prefill (returns at the first guard) or
// declare one that was never overridden by a real tag.
func TestOutOfBandReasoningDoesNotCancelARealBlockUnderADeclaredPrefill(t *testing.T) {
	f := &tagFilter{assumePrefill: true}

	var vis, think strings.Builder
	collect := func(cs []filteredChunk) {
		for _, c := range cs {
			vis.WriteString(c.text)
			think.WriteString(c.reasoning)
		}
	}

	// The server prefilled, so the filter starts inside a block; the model then
	// closes it and opens a real one of its own.
	collect(f.feed("assumed thinking</think>answer so far<think>real thinking"))
	if f.outOfBandReasoning() {
		t.Fatal("outOfBandReasoning cancelled a block a real opening tag had established")
	}
	collect(f.feed(" more</think> the rest"))

	if got := vis.String(); got != "answer so far the rest" {
		t.Errorf("visible text = %q, want %q", got, "answer so far the rest")
	}
	for _, want := range []string{"assumed thinking", "real thinking", " more"} {
		if !strings.Contains(think.String(), want) {
			t.Errorf("reasoning lost %q; it read %q", want, think.String())
		}
	}
	if strings.Contains(vis.String(), "thinking") {
		t.Errorf("reasoning leaked into the answer: %q", vis.String())
	}
}

// --- framing removal must not fabricate a marker --------------------------

// filterVisible runs the filter over one input and returns what a consumer
// would concatenate from the visible channel.
func filterVisible(in string, assumePrefill bool) string {
	f := &tagFilter{assumePrefill: assumePrefill}
	var vis strings.Builder
	for _, c := range f.feed(in) {
		vis.WriteString(c.text)
	}
	vis.WriteString(f.flush().text)
	return vis.String()
}

// TestDeletingFramingCannotFabricateAMarker.
//
// Deleting framing makes the bytes either side of it adjacent, and they were
// never adjacent in the model's output. Their join can form a marker nothing
// downstream can tell from a real one — including RecoverFromText, which then
// returns a tool call the model never wrote. Because extractFallbackToolCalls
// only recovers offered names, the fabricated call always names a real tool.
//
// Found by fuzzing the filter, which had no fuzz target of its own.
func TestDeletingFramingCannotFabricateAMarker(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"think tag spliced from its own halves", "<t</think>hink>"},
		{"tool-call opener spliced into being",
			`<tool</think>_call>{"name":"write_file","arguments":{"path":"a.go"}}</tool_call>`},
		{"thought tag spliced", "<thou</think>ght>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := filterVisible(tc.in, false)
			for _, marker := range []string{"<think>", "<thought>", toolCallOpen} {
				if strings.Contains(got, marker) && !strings.Contains(tc.in, marker) {
					t.Errorf("visible text contains %q, which the input never did:\n in  = %q\n out = %q",
						marker, tc.in, got)
				}
			}
			// The consequential half: nothing may be recoverable as a call.
			rec := RecoverFromText(got, []llm.ToolSchema{{Name: "write_file"}}, false)
			if len(rec.Calls) > 0 {
				t.Errorf("a tool call was fabricated from spliced text: %+v (out = %q)", rec.Calls, got)
			}
		})
	}
}

// FuzzTagFilterNeverFabricatesAMarkerFromAdjacentRuns is the target this filter
// did not have. The package's one existing target covers
// extractFallbackToolCalls; the filter that decides what reaches an answer was
// uncovered, and the splice above took under four seconds to find once a target
// existed.
//
// The invariant is scoped to what is actually guaranteed. Deleting a stray tag
// between two *adjacent* runs is prevented outright — wouldFuse keeps the tag
// instead. A whole think block between the two halves is not:
//
//	"<tool<think></think>_call>"  ->  visible "<tool_call>"
//
// because the block's contents are reasoning and cannot be kept, so the two
// visible runs legitimately concatenate and no byte-level rule can separate
// them. Accepted rather than hidden, on the reasoning that only the model can
// construct that input and it gains nothing by doing so: it can emit a tool
// call directly, and recovery only ever accepts names that were already
// offered. Inputs reaching this filter are model output, never tool results, so
// there is no path for injected content to build one.
//
// If that reasoning ever stops holding — a filter fed anything but model
// output — the fix is not a byte rule but refusing recovery from a response
// whose framing removal joined two runs.
func FuzzTagFilterNeverFabricatesAMarkerFromAdjacentRuns(f *testing.F) {
	for _, seed := range []string{
		"<t</think>hink>",
		"<tool</think>_call>{}</tool_call>",
		"<think>reasoning</think>answer",
		"plain answer with no tags",
		"</think>orphan close",
		"<tool_call>{\"name\":\"x\"}</tool_call>",
	} {
		f.Add(seed, false)
		f.Add(seed, true)
	}
	f.Fuzz(func(t *testing.T, in string, assumePrefill bool) {
		got := filterVisible(in, assumePrefill)
		// Scoped to the adjacent-run case: a fabricated marker only counts
		// when the input contained no complete think block to account for it.
		if strings.Contains(in, "<think>") || strings.Contains(in, "<thought>") {
			return
		}
		for _, marker := range []string{"<think>", "<thought>", toolCallOpen} {
			if strings.Contains(got, marker) && !strings.Contains(in, marker) {
				t.Fatalf("framing removal between adjacent runs fabricated %q\n in  = %q\n out = %q",
					marker, in, got)
			}
		}
	})
}
