package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
	"io"
	"strings"
	"testing"
)

func TestStatelessContinuationPreservesOrderedThoughtCallAndProse(t *testing.T) {
	a, _ := adapterFor(t, liveStream)
	stream, err := a.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	_, resp, err := adaptertest.Drain(stream)
	if err != nil {
		t.Fatal(err)
	}
	req := request()
	req.Messages = append(req.Messages, resp.Message, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultBlock{ToolCallID: "call_1168334", Content: []llm.ContentBlock{llm.TextBlock{Text: "observed"}}}}})
	body, err := a.buildRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Input []struct {
			Type      string          `json:"type"`
			Signature string          `json:"signature"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"input"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{"user_input", "thought", "function_call", "model_output", "function_result"}
	if len(got.Input) != len(want) {
		t.Fatalf("continuation has %d steps, want %v; %s", len(got.Input), want, raw)
	}
	for i, w := range want {
		if got.Input[i].Type != w {
			t.Errorf("step %d=%q want %q", i, got.Input[i].Type, w)
		}
	}
	if got.Input[1].Signature != "EuYBCuMBARFNMg" || got.Input[2].Signature != "" {
		t.Errorf("thought signature moved to another step: %s", raw)
	}
}

func TestIndexedStepStopSealsFunctionCall(t *testing.T) {
	var frames strings.Builder
	for i, path := range []string{"a.go", "b.go"} {
		fmt.Fprintf(&frames, "event: step.start\ndata: {\"index\":%d,\"event_type\":\"step.start\",\"step\":{\"type\":\"function_call\",\"id\":\"same\",\"name\":\"read_file\",\"arguments\":{}}}\n\n", i)
		fmt.Fprintf(&frames, "event: step.delta\ndata: {\"index\":%d,\"event_type\":\"step.delta\",\"delta\":{\"type\":\"arguments\",\"arguments\":{\"path\":%q}}}\n\n", i, path)
		fmt.Fprintf(&frames, "event: step.stop\ndata: {\"index\":%d,\"event_type\":\"step.stop\"}\n\n", i)
	}
	frames.WriteString("event: interaction.completed\ndata: {\"event_type\":\"interaction.completed\",\"interaction\":{\"status\":\"requires_action\"}}\n\nevent: done\ndata: [DONE]\n\n")
	s := newStream(io.NopCloser(strings.NewReader(frames.String())), "m", 0)
	_, response, err := adaptertest.Drain(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Message.ToolCalls()) != 2 {
		t.Fatal("indexed stop failed to seal its call")
	}
}

func TestContinuationPreservesOpaqueFieldsAndRejectsEditedCalls(t *testing.T) {
	input := strings.Replace(liveStream, `"type":"thought"`, `"type":"thought","opaque_extension":{"token":"opaque-a"}`, 1)
	a, _ := adapterFor(t, input)
	s, err := a.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	_, response, err := adaptertest.Drain(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(response.Message.Provenance.ReplayState), `"opaque_extension":{"token":"opaque-a"}`) {
		t.Fatal("unknown provider field lost")
	}
	req := request()
	req.Messages = append(req.Messages, response.Message)
	for i, b := range req.Messages[1].Content {
		if call, ok := b.(llm.ToolCallBlock); ok {
			call.Arguments = json.RawMessage(`{"path":"other.go"}`)
			req.Messages[1].Content[i] = call
		}
	}
	if _, err := a.buildRequest(req); err == nil {
		t.Fatal("opaque continuation overwrote changed journal arguments")
	}
}

func TestCanceledGeminiStreamCannotSettle(t *testing.T) {
	a, _ := adapterFor(t, liveStream)
	ctx, cancel := context.WithCancel(context.Background())
	s, err := a.Stream(ctx, request())
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	defer s.Close()
	if _, err := s.Next(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Next error=%v", err)
	}
	if _, err := s.Response(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Response error=%v", err)
	}
}

func TestImagesEncodeAtTopLevelAndInsideToolResult(t *testing.T) {
	for _, nested := range []bool{false, true} {
		a, _ := adapterFor(t, happyStream)
		req := request()
		img := llm.ImageBlock{MediaType: "image/png", Data: []byte{1, 2, 3}}
		if nested {
			req.Messages = append(req.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ToolCallBlock{ID: "c", Name: "capture", Arguments: json.RawMessage(`{}`)}}}, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultBlock{ToolCallID: "c", Content: []llm.ContentBlock{llm.TextBlock{Text: "screen"}, img}}}})
		} else {
			req.Messages[0].Content = append(req.Messages[0].Content, img)
		}
		body, err := a.buildRequest(req)
		if err != nil {
			t.Errorf("nested=%v: %v", nested, err)
			continue
		}
		raw, _ := json.Marshal(body)
		var got struct {
			Input []struct {
				Content []struct {
					Type, Data string
					MIME       string `json:"mime_type"`
				} `json:"content"`
				Result []struct {
					Type, Data string
					MIME       string `json:"mime_type"`
				} `json:"result"`
			} `json:"input"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, step := range got.Input {
			for _, c := range append(step.Content, step.Result...) {
				if c.Type == "image" && c.Data == "AQID" && c.MIME == "image/png" {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("nested=%v image disappeared: %s", nested, raw)
		}
	}
}

func TestPrivateContinuationMismatchDoesNotDisclosePayload(t *testing.T) {
	secret := "sensitive-literal"
	msg := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ToolCallBlock{ID: "c", Name: "enter", Arguments: json.RawMessage(`{"text":"[redacted]"}`)}}}
	steps := []json.RawMessage{json.RawMessage(`{"type":"function_call","id":"c","name":"enter","arguments":{"text":"sensitive-literal"}}`)}
	err := validateContinuation(msg, steps)
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "parameter references") {
		t.Fatalf("missing safe continuation diagnostic: %v", err)
	}
}
