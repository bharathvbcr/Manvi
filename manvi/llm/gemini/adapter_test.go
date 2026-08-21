package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"manvi/llm"
	"manvi/llm/adaptertest"
)

const happyStream = `event: interaction.created
data: {"event_type":"interaction.created","interaction":{"status":"in_progress"}}

event: step.delta
data: {"event_type":"step.delta","step":{"type":"model_output","content":[{"type":"thought","thought":"planning"}]}}

event: step.delta
data: {"event_type":"step.delta","step":{"type":"model_output","content":[{"type":"text","text":"Reading "}]}}

event: step.delta
data: {"event_type":"step.delta","step":{"type":"model_output","content":[{"type":"text","text":"the file."}]}}

event: step.start
data: {"event_type":"step.start","step":{"type":"function_call","id":"fc_1","name":"read_file"}}

event: step.delta
data: {"event_type":"step.delta","step":{"type":"function_call","id":"fc_1","name":"read_file","arguments":{"path":"a.go"}}}

event: interaction.completed
data: {"event_type":"interaction.completed","interaction":{"status":"requires_action","usage":{"total_input_tokens":19,"total_output_tokens":41,"total_thought_tokens":8}}}

event: done
data: [DONE]

`

func adapterFor(t *testing.T, body string) (*Adapter, *adaptertest.Server) {
	t.Helper()
	server := adaptertest.NewServer(t, body)
	return New(server.URL, adaptertest.Secret("gemini-test-key-value")), server
}

func request() llm.Request {
	return llm.Request{
		Model:    "gemini-3.7-flash",
		System:   "You are the builder.",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "read a.go"}}}},
		Tools:    []llm.ToolSchema{{Name: "read_file", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Effort:   "low",
	}
}

func TestAFullTurnDecodes(t *testing.T) {
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
		t.Errorf("text = %q", got)
	}
	if got := adaptertest.ReasoningOf(chunks); got != "planning" {
		t.Errorf("reasoning = %q", got)
	}
	if resp.StopReason != llm.StopToolUse {
		t.Errorf("stop = %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 19 || resp.Usage.OutputTokens != 41 || resp.Usage.ReasoningTokens != 8 {
		t.Errorf("usage = %+v", resp.Usage)
	}

	calls := resp.Message.ToolCalls()
	if len(calls) != 1 || calls[0].Name != "read_file" || calls[0].ID != "fc_1" {
		t.Fatalf("calls = %+v", calls)
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil || args.Path != "a.go" {
		t.Fatalf("arguments = %s (%v)", calls[0].Arguments, err)
	}
}

// TestStoreIsSentExplicitlyAsFalse.
//
// The API defaults store to true, which keeps conversation state on the server
// — and the harness's central invariant is that the session log is the complete
// record of what the model saw. An omitted field would opt into the opposite
// silently. See StoreInteractions for why this briefly said true and what the
// measurement error was.
func TestStoreIsSentExplicitlyAsFalse(t *testing.T) {
	adapter, server := adapterFor(t, happyStream)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	adaptertest.Drain(stream)

	var body map[string]any
	if err := json.Unmarshal([]byte(server.Requests[0]), &body); err != nil {
		t.Fatal(err)
	}
	value, present := body["store"]
	if !present {
		t.Fatal("store was omitted; a field left to the provider's default is a decision " +
			"this adapter did not make and cannot report")
	}
	if value != false {
		t.Fatalf("store = %v, want false — server-side conversation state would mean the log "+
			"no longer holds everything the model read", value)
	}
	// The user's own message must be in the request, not assumed to be held by
	// the provider from an earlier interaction.
	if !strings.Contains(server.Requests[0], "read a.go") {
		t.Errorf("the request did not carry the conversation: %s", server.Requests[0])
	}
	if body["system_instruction"] != "You are the builder." {
		t.Errorf("system_instruction = %v", body["system_instruction"])
	}
	if got := server.Headers[0].Get(APIKeyHeader); got != "gemini-test-key-value" {
		t.Errorf("%s = %q — this API uses a bespoke header, not a bearer token", APIKeyHeader, got)
	}
	if server.Headers[0].Get("Authorization") != "" {
		t.Error("an Authorization header was sent; copying the bearer form from another adapter is the trap here")
	}
}

// TestToolResultIsSentAsAnObject: this wire wants an object, and a bare string
// is rejected.
// TestToolResultReferencesItsCallByCallID pins the shape a tool result goes out
// in, and it replaces a test that pinned the wrong one.
//
// The previous version asserted `id` and a `response` object with an `output`
// field, and passed against a scripted server that validated nothing about the
// request. The live API rejects that outright:
//
//	http 400: Unknown parameter 'id' at 'input[2]'
//
// Two things made it survive. The shape is only reachable once history holds a
// completed tool call, so `manvi probe gemini` — one request, one step — never
// built one. And a test that asserts what the encoder already does, against a
// server that accepts anything, only ever proves the encoder is consistent with
// itself.
//
// The documented shape is asymmetric on purpose: a function_call carries its
// own `id`, and a function_result references it through `call_id`. The payload
// field is `result`, and the documented example carries an array of typed
// content blocks.
func TestToolResultReferencesItsCallByCallID(t *testing.T) {
	adapter, server := adapterFor(t, happyStream)
	req := request()
	req.Messages = append(req.Messages,
		llm.Message{
			Role:       llm.RoleAssistant,
			Provenance: &llm.AssistantProvenance{Provider: Name, Model: "gemini-3.7-flash"},
			Content: []llm.ContentBlock{llm.ToolCallBlock{
				ID: "fc_1", Name: "read_file", Arguments: []byte(`{"path":"a.go"}`),
			}},
		},
		llm.Message{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{llm.ToolResultBlock{
				ToolCallID: "fc_1", Content: []llm.ContentBlock{llm.TextBlock{Text: "file contents"}},
			}},
		})
	stream, err := adapter.Stream(adaptertest.Ctx(), req)
	if err != nil {
		t.Fatal(err)
	}
	adaptertest.Drain(stream)

	// Decoded into a bare map so a field the encoder should no longer send is
	// visible rather than silently ignored by a typed struct.
	var body struct {
		Input []map[string]json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal([]byte(server.Requests[0]), &body); err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, item := range body.Input {
		var kind string
		json.Unmarshal(item["type"], &kind)
		if kind != TypeFunctionResult {
			continue
		}
		found = true

		if _, present := item["id"]; present {
			t.Errorf("a function_result carried `id`; the live API answers "+
				"`Unknown parameter 'id'` and fails the turn: %v", item)
		}
		if _, present := item["response"]; present {
			t.Errorf("a function_result carried `response`; the payload field is `result`: %v", item)
		}

		var callID, name string
		json.Unmarshal(item["call_id"], &callID)
		if callID != "fc_1" {
			t.Errorf("call_id = %q, want fc_1", callID)
		}
		json.Unmarshal(item["name"], &name)
		if name != "read_file" {
			t.Errorf("name = %q, want read_file — a result without it is refused", name)
		}

		var result []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(item["result"], &result); err != nil {
			t.Fatalf("result is not an array of content blocks: %s (%v)", item["result"], err)
		}
		if len(result) != 1 || result[0].Type != DeltaText || result[0].Text != "file contents" {
			t.Errorf("result = %+v", result)
		}
	}
	if !found {
		t.Fatalf("no function_result was sent: %s", server.Requests[0])
	}
}

// TestAFailedToolResultSaysSoInItsText.
//
// The error is carried in the text because the two documentation sources do not
// agree that an is_error field exists on this wire, and an unknown parameter
// here is not a degraded result — it is a 400 that fails the whole turn. The
// model still has to be able to tell a failure from a result.
func TestAFailedToolResultSaysSoInItsText(t *testing.T) {
	adapter, server := adapterFor(t, happyStream)
	req := request()
	req.Messages = append(req.Messages,
		llm.Message{
			Role:       llm.RoleAssistant,
			Provenance: &llm.AssistantProvenance{Provider: Name, Model: "gemini-3.7-flash"},
			Content: []llm.ContentBlock{llm.ToolCallBlock{
				ID: "fc_1", Name: "read_file", Arguments: []byte(`{}`),
			}},
		},
		llm.Message{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{llm.ToolResultBlock{
				ToolCallID: "fc_1", IsError: true,
				Content: []llm.ContentBlock{llm.TextBlock{Text: "no such file"}},
			}},
		})
	stream, err := adapter.Stream(adaptertest.Ctx(), req)
	if err != nil {
		t.Fatal(err)
	}
	adaptertest.Drain(stream)

	sent := server.Requests[0]
	if strings.Contains(sent, `"is_error"`) {
		t.Errorf("is_error was sent; no documented example carries it and an unknown "+
			"parameter fails the request: %s", sent)
	}
	if !strings.Contains(sent, "ERROR: no such file") {
		t.Errorf("a failed tool result is indistinguishable from a successful one: %s", sent)
	}
}

func TestEventNameAndPayloadTypeMustAgree(t *testing.T) {
	body := `event: step.delta
data: {"event_type":"interaction.completed","step":{"type":"model_output","content":[{"type":"text","text":"x"}]}}

`
	adapter, _ := adapterFor(t, body)
	stream, _ := adapter.Stream(adaptertest.Ctx(), request())
	if _, _, err := adaptertest.Drain(stream); err == nil {
		t.Fatal("a disagreeing event name and payload type were accepted")
	}
}

// TestAnErrorFrameFailsTheTurn.
func TestAnErrorFrameFailsTheTurn(t *testing.T) {
	body := `event: step.delta
data: {"event_type":"step.delta","step":{"type":"model_output","content":[{"type":"text","text":"partial"}]}}

event: error
data: {"event_type":"error","error":{"message":"quota exhausted","status":"RESOURCE_EXHAUSTED"}}

`
	adapter, _ := adapterFor(t, body)
	stream, _ := adapter.Stream(adaptertest.Ctx(), request())
	_, _, err := adaptertest.Drain(stream)
	if err == nil || !strings.Contains(err.Error(), "quota exhausted") {
		t.Fatalf("err = %v, want the provider's error", err)
	}
}

// TestUnstrippedReasoningIsRefused: thought replay here goes through
// TestReasoningTextIsNeverPutOnTheWire replaces a test that required an
// unstripped reasoning block to be an error.
//
// That was right while nothing about an assistant turn was replayable, so a
// reasoning block reaching the encoder meant PrepareHistory had failed. It is
// wrong now: the assistant message is deliberately kept, because it carries the
// call signatures without which a tool call cannot be replayed at all — and the
// reasoning rides along with it.
//
// What must stay true is the thing the old test was protecting: reasoning text
// never reaches the wire. There is no input field for it, and sending it as
// ordinary text would put the model's private thinking into the prompt as
// though it had said it aloud.
func TestReasoningTextIsNeverPutOnTheWire(t *testing.T) {
	adapter, server := adapterFor(t, happyStream)
	req := request()
	req.Messages = append(req.Messages, llm.Message{
		Role:       llm.RoleAssistant,
		Provenance: &llm.AssistantProvenance{Provider: Name, Model: "gemini-3.7-flash"},
		Content: []llm.ContentBlock{
			llm.ReasoningBlock{Text: "PRIVATE-THINKING"},
			llm.TextBlock{Text: "visible answer"},
		},
	})
	stream, err := adapter.Stream(adaptertest.Ctx(), req)
	if err != nil {
		t.Fatalf("a message carrying reasoning was refused outright: %v", err)
	}
	adaptertest.Drain(stream)

	if strings.Contains(server.Requests[0], "PRIVATE-THINKING") {
		t.Errorf("reasoning text reached the wire: %s", server.Requests[0])
	}
	if !strings.Contains(server.Requests[0], "visible answer") {
		t.Errorf("the visible text was dropped along with the reasoning: %s", server.Requests[0])
	}
}

func TestAnUnknownModelIsRefusedAtAssembly(t *testing.T) {
	adapter, server := adapterFor(t, happyStream)
	req := request()
	req.Model = "gemini-9.9-ultra"
	if _, err := adapter.Stream(adaptertest.Ctx(), req); err == nil {
		t.Fatal("an unknown model was sent")
	}
	if len(server.Requests) != 0 {
		t.Fatal("an unknown model reached the network")
	}
}

// TestThinkingLevelCarriesTheRequestedEffort pins the field the harness's
// llm.effort setting ultimately writes to. The name is the whole risk: an
// effort that reaches the adapter but lands in a mistyped or renamed key is
// accepted by the API and silently ignored, which looks identical to a model
// that simply did not think much.
func TestThinkingLevelCarriesTheRequestedEffort(t *testing.T) {
	adapter, server := adapterFor(t, happyStream)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	adaptertest.Drain(stream)

	var body struct {
		GenerationConfig *struct {
			ThinkingLevel string `json:"thinking_level"`
		} `json:"generation_config"`
	}
	if err := json.Unmarshal([]byte(server.Requests[0]), &body); err != nil {
		t.Fatal(err)
	}
	if body.GenerationConfig == nil {
		t.Fatal("generation_config was omitted from a request that named an effort")
	}
	if body.GenerationConfig.ThinkingLevel != "low" {
		t.Fatalf("thinking_level = %q, want %q", body.GenerationConfig.ThinkingLevel, "low")
	}
}

// TestNoEffortSendsNoThinkingLevel is the other half of the contract, and the
// reason llm.effort defaults to empty: with nothing set the field is absent
// rather than pinned to a level this harness picked, so the provider's own
// default applies.
func TestNoEffortSendsNoThinkingLevel(t *testing.T) {
	adapter, server := adapterFor(t, happyStream)
	req := request()
	req.Effort = ""
	stream, err := adapter.Stream(adaptertest.Ctx(), req)
	if err != nil {
		t.Fatal(err)
	}
	adaptertest.Drain(stream)

	var body map[string]any
	if err := json.Unmarshal([]byte(server.Requests[0]), &body); err != nil {
		t.Fatal(err)
	}
	conf, present := body["generation_config"]
	if !present {
		return
	}
	if level, set := conf.(map[string]any)["thinking_level"]; set {
		t.Fatalf("thinking_level = %v was sent for a request with no effort", level)
	}
}

// TestAnUnsignedCallIsDroppedRatherThanRefusingTheRequest.
//
// Measured live: a replayed function_call without a signature is refused
// ("Request contains an invalid argument."), and with one is accepted 3/3.
// A call this adapter has no signature for — history from before signatures
// were captured, or a cross-model handoff that stripped them — is therefore
// dropped rather than sent, so the turn continues from the results alone. That
// is degraded and it works; sending it would fail the request outright.
func TestAnUnsignedCallIsDroppedRatherThanRefusingTheRequest(t *testing.T) {
	adapter, server := adapterFor(t, happyStream)
	req := request()
	req.Messages = append(req.Messages,
		llm.Message{
			Role:       llm.RoleAssistant,
			Provenance: &llm.AssistantProvenance{Provider: Name, Model: "gemini-3.7-flash"},
			Content: []llm.ContentBlock{llm.ToolCallBlock{
				ID: "fc_1", Name: "read_file", Arguments: []byte(`{"path":"a.go"}`),
			}},
		},
		llm.Message{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{llm.ToolResultBlock{
				ToolCallID: "fc_1", Content: []llm.ContentBlock{llm.TextBlock{Text: "ok"}},
			}},
		})
	stream, err := adapter.Stream(adaptertest.Ctx(), req)
	if err != nil {
		t.Fatal(err)
	}
	adaptertest.Drain(stream)

	if strings.Contains(server.Requests[0], `"function_call"`) {
		t.Errorf("an unsigned function_call was replayed; the live API refuses it: %s",
			server.Requests[0])
	}
	if !strings.Contains(server.Requests[0], `"function_result"`) {
		t.Errorf("the tool result was dropped along with the call: %s", server.Requests[0])
	}
}

// TestAssistantProseIsDroppedFromAToolUsingHistory.
//
// Measured live, each repeated until stable: user + model_output +
// function_result was refused 0/2, and so was user + function_result +
// model_output — while user + function_result + function_result was accepted
// 2/2 and user + model_output + user (no results at all) was accepted 3/3.
//
// So a conversation that carries any tool result may not carry model_output
// anywhere in the same input. That is a real loss — on a turn that has used a
// tool the model does not get its own earlier prose back — and it is better
// than the alternative this replaced, which was the request refused outright.
func TestAssistantProseIsDroppedFromAToolUsingHistory(t *testing.T) {
	adapter, server := adapterFor(t, happyStream)
	req := request()
	req.Messages = append(req.Messages,
		llm.Message{
			Role:       llm.RoleAssistant,
			Provenance: &llm.AssistantProvenance{Provider: Name, Model: "gemini-3.7-flash"},
			Content: []llm.ContentBlock{
				llm.TextBlock{Text: "PROSE-THAT-MUST-NOT-BE-SENT"},
				llm.ToolCallBlock{ID: "fc_1", Name: "read_file", Arguments: []byte(`{}`)},
			},
		},
		llm.Message{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{llm.ToolResultBlock{
				ToolCallID: "fc_1", Content: []llm.ContentBlock{llm.TextBlock{Text: "ok"}},
			}},
		})
	stream, err := adapter.Stream(adaptertest.Ctx(), req)
	if err != nil {
		t.Fatal(err)
	}
	adaptertest.Drain(stream)

	if strings.Contains(server.Requests[0], "PROSE-THAT-MUST-NOT-BE-SENT") {
		t.Errorf("assistant prose was sent alongside a tool result; the live API refuses "+
			"model_output in that company: %s", server.Requests[0])
	}
	// The user's own instruction is never dropped: without it this is a
	// different request.
	if !strings.Contains(server.Requests[0], "read a.go") {
		t.Errorf("the user's instruction was dropped too: %s", server.Requests[0])
	}
}

// TestAssistantProseSurvivesWhenNoToolWasUsed is the other half: the rule is
// conditional, and applying it always would strip a plain conversation of the
// model's own replies for no reason.
func TestAssistantProseSurvivesWhenNoToolWasUsed(t *testing.T) {
	adapter, server := adapterFor(t, happyStream)
	req := request()
	req.Messages = append(req.Messages,
		llm.Message{
			Role:       llm.RoleAssistant,
			Provenance: &llm.AssistantProvenance{Provider: Name, Model: "gemini-3.7-flash"},
			Content:    []llm.ContentBlock{llm.TextBlock{Text: "KEEP-THIS-PROSE"}},
		},
		llm.Message{
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{llm.TextBlock{Text: "and now?"}},
		})
	stream, err := adapter.Stream(adaptertest.Ctx(), req)
	if err != nil {
		t.Fatal(err)
	}
	adaptertest.Drain(stream)

	if !strings.Contains(server.Requests[0], "KEEP-THIS-PROSE") {
		t.Errorf("assistant prose was dropped from a history with no tool results at all: %s",
			server.Requests[0])
	}
}

// TestAResultWhoseCallIsMissingIsRefusedByName.
//
// The name on a result comes from the call it answers, because
// llm.ToolResultBlock does not carry one. A result whose call is not in the
// history cannot be named, and this wire refuses an unnamed result — so it is
// better to say which result and why than to send it and read back
// "Invalid input received."
func TestAResultWhoseCallIsMissingIsRefusedByName(t *testing.T) {
	adapter, _ := adapterFor(t, happyStream)
	req := request()
	req.Messages = append(req.Messages, llm.Message{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{llm.ToolResultBlock{
			ToolCallID: "orphan", Content: []llm.ContentBlock{llm.TextBlock{Text: "ok"}},
		}},
	})
	_, err := adapter.Stream(adaptertest.Ctx(), req)
	if err == nil {
		t.Fatal("a result with no matching call was sent")
	}
	if !strings.Contains(err.Error(), "orphan") {
		t.Errorf("the refusal does not name the result: %v", err)
	}
}

// TestASignedCallIsReplayedSoTheModelSeesItsOwnWork.
//
// This is the difference between a model that can continue and one that goes
// blank. Measured live, both the shape and the consequence:
//
//	user + call(signed) + result           3/3 accepted, and it answers
//	user + call(unsigned) + result         0/3 "Request contains an invalid argument."
//	user + result only                     3/3 accepted, and after a few rounds
//	                                       the model returns nothing at all
//
// The third row is why this matters. Dropping the call is accepted by the API
// and quietly ruins the conversation: the model is handed a list of results for
// calls it never sees. In one recorded benchmark that produced runs of four
// consecutive responses with no content whatsoever, at identical input sizes —
// deterministic, not flaky.
func TestASignedCallIsReplayedSoTheModelSeesItsOwnWork(t *testing.T) {
	adapter, server := adapterFor(t, happyStream)
	req := request()
	req.Messages = append(req.Messages,
		llm.Message{
			Role: llm.RoleAssistant,
			Provenance: &llm.AssistantProvenance{
				Provider: Name, Model: "gemini-3.7-flash",
				ReplayState: json.RawMessage(`{"call_signatures":{"fc_1":"SIG-ABC"}}`),
			},
			Content: []llm.ContentBlock{llm.ToolCallBlock{
				ID: "fc_1", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.go"}`),
			}},
		},
		llm.Message{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{llm.ToolResultBlock{
				ToolCallID: "fc_1", Content: []llm.ContentBlock{llm.TextBlock{Text: "ok"}},
			}},
		})
	stream, err := adapter.Stream(adaptertest.Ctx(), req)
	if err != nil {
		t.Fatal(err)
	}
	adaptertest.Drain(stream)

	var body struct {
		Input []map[string]json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal([]byte(server.Requests[0]), &body); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, item := range body.Input {
		var kind string
		json.Unmarshal(item["type"], &kind)
		if kind != TypeFunctionCall {
			continue
		}
		found = true
		var sig string
		json.Unmarshal(item["signature"], &sig)
		if sig != "SIG-ABC" {
			t.Errorf("signature = %q, want SIG-ABC — an unsigned call is refused", sig)
		}
	}
	if !found {
		t.Fatalf("the signed call was not replayed: %s", server.Requests[0])
	}
}

// TestTheStreamCarriesSignaturesOutOfTheThoughtStep.
//
// The signature arrives on a thought delta, before the call it authorises. It
// was being discarded as "opaque and not replayable", which was true of its
// content and wrong about its purpose.
func TestTheStreamCarriesSignaturesOutOfTheThoughtStep(t *testing.T) {
	adapter, _ := adapterFor(t, liveStream)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	_, resp, err := adaptertest.Drain(stream)
	if err != nil {
		t.Fatal(err)
	}
	calls := resp.Message.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	if got := signatureFor(resp.Message, calls[0].ID); got != "EuYBCuMBARFNMg" {
		t.Errorf("signature carried = %q; without it the call cannot be replayed", got)
	}
	// And it must not have become visible content.
	if strings.Contains(resp.Message.Text(), "EuYBCuMBARFNMg") {
		t.Error("the signature leaked into the answer text")
	}
}
