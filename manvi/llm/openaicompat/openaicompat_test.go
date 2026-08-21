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

func TestOpenAICompatStreamingText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		events := []string{
			`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
			`data: {"choices":[{"delta":{"content":" world!"}}]}`,
			`data: {"choices":[{"finish_reason":"stop"}]}`,
			`data: {"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
			`data: [DONE]`,
		}
		for _, e := range events {
			_, _ = w.Write([]byte(e + "\n\n"))
		}
	}))
	defer server.Close()

	adapter := New(Options{
		Name:     "test-provider",
		BaseURL:  server.URL,
		Validate: func(req llm.Request) error { return nil },
		Header:   func() (http.Header, error) { return http.Header{}, nil },
	})

	stream, err := adapter.Stream(context.Background(), llm.Request{
		Model: "test-model",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "hi"}}},
		},
	})
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	defer stream.Close()

	var chunks []llm.Chunk
	for {
		chunk, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next failed: %v", err)
		}
		chunks = append(chunks, chunk)
	}

	if len(chunks) != 2 {
		t.Fatalf("expected 2 text chunks, got %d", len(chunks))
	}
	if chunks[0].Text != "Hello" || chunks[1].Text != " world!" {
		t.Errorf("unexpected chunks: %+v", chunks)
	}

	resp, err := stream.Response()
	if err != nil {
		t.Fatalf("Response failed: %v", err)
	}
	if resp.Message.Text() != "Hello world!" {
		t.Errorf("expected 'Hello world!', got %q", resp.Message.Text())
	}
	if resp.StopReason != llm.StopEndTurn {
		t.Errorf("expected StopEndTurn, got %s", resp.StopReason)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 5 {
		t.Errorf("unexpected usage: %+v", resp.Usage)
	}
}

func TestOpenAICompatThinkTagExtraction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		events := []string{
			`data: {"choices":[{"delta":{"content":"<think>I should calculate 2+2"}}]}`,
			`data: {"choices":[{"delta":{"content":" which is 4</think>The answer is 4."}}]}`,
			`data: {"choices":[{"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}
		for _, e := range events {
			_, _ = w.Write([]byte(e + "\n\n"))
		}
	}))
	defer server.Close()

	adapter := New(Options{
		Name:     "local",
		BaseURL:  server.URL,
		Validate: func(req llm.Request) error { return nil },
		Header:   func() (http.Header, error) { return http.Header{}, nil },
	})

	stream, err := adapter.Stream(context.Background(), llm.Request{
		Model: "qwen-coder-r1",
	})
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	defer stream.Close()

	var textChunks, reasoningChunks []llm.Chunk
	for {
		chunk, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next failed: %v", err)
		}
		if chunk.Kind == llm.ChunkReasoning {
			reasoningChunks = append(reasoningChunks, chunk)
		} else if chunk.Kind == llm.ChunkText {
			textChunks = append(textChunks, chunk)
		}
	}

	if len(reasoningChunks) == 0 {
		t.Fatal("expected reasoning chunks from <think> tags, got none")
	}
	if len(textChunks) == 0 {
		t.Fatal("expected text chunks, got none")
	}

	resp, err := stream.Response()
	if err != nil {
		t.Fatalf("Response failed: %v", err)
	}

	if resp.Message.Text() != "The answer is 4." {
		t.Errorf("expected visible text 'The answer is 4.', got %q", resp.Message.Text())
	}

	// Verify reasoning block in response message
	foundReasoning := false
	for _, b := range resp.Message.Content {
		if rb, ok := b.(llm.ReasoningBlock); ok {
			foundReasoning = true
			if !strings.Contains(rb.Text, "I should calculate 2+2 which is 4") {
				t.Errorf("unexpected reasoning text: %q", rb.Text)
			}
		}
	}
	if !foundReasoning {
		t.Fatal("expected ReasoningBlock in Message.Content, found none")
	}
}

func TestOpenAICompatToolCallWithMarkdownSanitizationAndSyntheticID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// Model sends empty id and markdown-wrapped json with trailing comma
		events := []string{
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"","function":{"name":"devcouncil_read_file","arguments":"` + "```json\\n{\\\"path\\\": \\\"main.go\\\",}\\n```" + `"}}]}}]}`,
			`data: {"choices":[{"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}
		for _, e := range events {
			_, _ = w.Write([]byte(e + "\n\n"))
		}
	}))
	defer server.Close()

	adapter := New(Options{
		Name:     "local",
		BaseURL:  server.URL,
		Validate: func(req llm.Request) error { return nil },
		Header:   func() (http.Header, error) { return http.Header{}, nil },
	})

	stream, err := adapter.Stream(context.Background(), llm.Request{
		Model: "local-coder",
	})
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	defer stream.Close()

	for {
		_, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next failed: %v", err)
		}
	}

	resp, err := stream.Response()
	if err != nil {
		t.Fatalf("Response failed: %v", err)
	}

	if resp.StopReason != llm.StopToolUse {
		t.Errorf("expected StopToolUse, got %s", resp.StopReason)
	}

	calls := resp.Message.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	call := calls[0]
	if call.Name != "devcouncil_read_file" {
		t.Errorf("expected devcouncil_read_file, got %s", call.Name)
	}
	if call.ID == "" {
		t.Error("expected non-empty synthetic tool call ID")
	}

	var args map[string]any
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		t.Fatalf("tool call arguments are not valid json: %v (raw: %s)", err, string(call.Arguments))
	}
	if args["path"] != "main.go" {
		t.Errorf("expected path 'main.go', got %v", args["path"])
	}
}

func TestOpenAICompatSamplingParametersInWirePayload(t *testing.T) {
	var receivedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &receivedBody)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	adapter := New(Options{
		Name:     "local",
		BaseURL:  server.URL,
		Validate: func(req llm.Request) error { return nil },
		Header:   func() (http.Header, error) { return http.Header{}, nil },
	})

	temp := 0.2
	topP := 0.9
	minP := 0.05
	repPen := 1.1
	req := llm.Request{
		Model:             "qwen",
		Temperature:       &temp,
		TopP:              &topP,
		MinP:              &minP,
		RepetitionPenalty: &repPen,
		Stop:              []string{"<|im_end|>"},
	}

	stream, err := adapter.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	defer stream.Close()
	for {
		if _, err := stream.Next(); err == io.EOF {
			break
		}
	}

	if receivedBody["temperature"] != 0.2 {
		t.Errorf("expected temperature 0.2, got %v", receivedBody["temperature"])
	}
	if receivedBody["top_p"] != 0.9 {
		t.Errorf("expected top_p 0.9, got %v", receivedBody["top_p"])
	}
	if receivedBody["min_p"] != 0.05 {
		t.Errorf("expected min_p 0.05, got %v", receivedBody["min_p"])
	}
	if receivedBody["repetition_penalty"] != 1.1 {
		t.Errorf("expected repetition_penalty 1.1, got %v", receivedBody["repetition_penalty"])
	}
}

func TestOpenAICompatFragmentedThinkTagStreaming(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// Split <think> and </think> across multiple SSE deltas
		events := []string{
			`data: {"choices":[{"delta":{"content":"<thi"}}]}`,
			`data: {"choices":[{"delta":{"content":"nk>Step 1: plan."}}]}`,
			`data: {"choices":[{"delta":{"content":" Step 2: verify.</thi"}}]}`,
			`data: {"choices":[{"delta":{"content":"nk>Done."}}]}`,
			`data: {"choices":[{"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}
		for _, e := range events {
			_, _ = w.Write([]byte(e + "\n\n"))
		}
	}))
	defer server.Close()

	adapter := New(Options{
		Name:     "local",
		BaseURL:  server.URL,
		Validate: func(req llm.Request) error { return nil },
		Header:   func() (http.Header, error) { return http.Header{}, nil },
	})

	stream, err := adapter.Stream(context.Background(), llm.Request{Model: "qwen-coder"})
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	defer stream.Close()

	var textParts, reasoningParts []string
	for {
		chunk, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next failed: %v", err)
		}
		if chunk.Kind == llm.ChunkReasoning {
			reasoningParts = append(reasoningParts, chunk.Text)
		} else if chunk.Kind == llm.ChunkText {
			textParts = append(textParts, chunk.Text)
		}
	}

	resp, err := stream.Response()
	if err != nil {
		t.Fatalf("Response failed: %v", err)
	}

	joinedReasoning := strings.Join(reasoningParts, "")
	if !strings.Contains(joinedReasoning, "Step 1: plan. Step 2: verify.") {
		t.Errorf("expected full reasoning, got %q", joinedReasoning)
	}
	if strings.Contains(resp.Message.Text(), "<think>") || strings.Contains(resp.Message.Text(), "</think>") {
		t.Errorf("think tags leaked into visible text: %q", resp.Message.Text())
	}
	if resp.Message.Text() != "Done." {
		t.Errorf("expected text 'Done.', got %q", resp.Message.Text())
	}
}

func TestOpenAICompatFallbackHermesToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// Model outputs Hermes format inside content instead of tool_calls array
		events := []string{
			`data: {"choices":[{"delta":{"content":"I will read the file now.\n<tool_call>\n{\"name\": \"devcouncil_read_file\", \"arguments\": {\"path\": \"README.md\"}}\n</tool_call>"}}]}`,
			`data: {"choices":[{"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}
		for _, e := range events {
			_, _ = w.Write([]byte(e + "\n\n"))
		}
	}))
	defer server.Close()

	adapter := New(Options{
		Name:     "local",
		BaseURL:  server.URL,
		Validate: func(req llm.Request) error { return nil },
		Header:   func() (http.Header, error) { return http.Header{}, nil },
	})

	stream, err := adapter.Stream(context.Background(), llm.Request{
		Model: "qwen-coder",
		Tools: []llm.ToolSchema{{Name: "devcouncil_read_file"}},
	})
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	defer stream.Close()

	for {
		if _, err := stream.Next(); err == io.EOF {
			break
		}
	}

	resp, err := stream.Response()
	if err != nil {
		t.Fatalf("Response failed: %v", err)
	}

	if resp.StopReason != llm.StopToolUse {
		t.Errorf("expected StopToolUse, got %s", resp.StopReason)
	}

	calls := resp.Message.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call extracted, got %d", len(calls))
	}
	if calls[0].Name != "devcouncil_read_file" {
		t.Errorf("expected devcouncil_read_file, got %s", calls[0].Name)
	}
	if !strings.Contains(string(calls[0].Arguments), "README.md") {
		t.Errorf("expected README.md in arguments, got %s", string(calls[0].Arguments))
	}
	if resp.Message.Text() != "I will read the file now." {
		t.Errorf("expected cleaned text, got %q", resp.Message.Text())
	}
}

func TestOpenAICompatFallbackMarkdownToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		events := []string{
			`data: {"choices":[{"delta":{"content":"Writing the patch:\n` + "```json\\n{\\n  \\\"name\\\": \\\"devcouncil_patch_file\\\",\\n  \\\"arguments\\\": {\\n    \\\"path\\\": \\\"foo.go\\\",\\n    \\\"target_content\\\": \\\"a\\\",\\n    \\\"replacement_content\\\": \\\"b\\\"\\n  }\\n}\\n```" + `"}}]}`,
			`data: {"choices":[{"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}
		for _, e := range events {
			_, _ = w.Write([]byte(e + "\n\n"))
		}
	}))
	defer server.Close()

	adapter := New(Options{
		Name:     "local",
		BaseURL:  server.URL,
		Validate: func(req llm.Request) error { return nil },
		Header:   func() (http.Header, error) { return http.Header{}, nil },
	})

	// The offered set is the authority on what may be recovered, so the fixture
	// declares the tool it expects back. The point this test makes — that a
	// recovered name needs no "devcouncil_" prefix — is unchanged: any offered
	// name is accepted, whatever its shape.
	stream, err := adapter.Stream(context.Background(), llm.Request{
		Model: "qwen-coder",
		Tools: []llm.ToolSchema{{Name: "devcouncil_patch_file"}},
	})
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	defer stream.Close()

	for {
		_, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next failed: %v", err)
		}
	}

	resp, err := stream.Response()
	if err != nil {
		t.Fatalf("Response failed: %v", err)
	}

	if resp.StopReason != llm.StopToolUse {
		t.Errorf("expected StopToolUse, got %s", resp.StopReason)
	}

	calls := resp.Message.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call extracted, got %d", len(calls))
	}
	if calls[0].Name != "devcouncil_patch_file" {
		t.Errorf("expected devcouncil_patch_file, got %s", calls[0].Name)
	}
	if resp.Message.Text() != "Writing the patch:" {
		t.Errorf("expected cleaned text, got %q", resp.Message.Text())
	}
}

func TestOpenAICompatFallbackArbitraryToolNameAndPythonLiterals(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		events := []string{
			`data: {"choices":[{"delta":{"content":"Invoking custom tool:\n` + "```json\\n{\\n  \\\"name\\\": \\\"read_custom_file\\\",\\n  \\\"arguments\\\": {\\n    \\\"path\\\": \\\"config.yaml\\\",\\n    \\\"enabled\\\": True,\\n    \\\"limit\\\": None,\\n    \\\"tags\\\": [\\\"v1\\\", \\\"v2\\\",]\\n  }\\n}\\n```" + `"}}]}`,
			`data: {"choices":[{"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}
		for _, e := range events {
			_, _ = w.Write([]byte(e + "\n\n"))
		}
	}))
	defer server.Close()

	adapter := New(Options{
		Name:     "local",
		BaseURL:  server.URL,
		Validate: func(req llm.Request) error { return nil },
		Header:   func() (http.Header, error) { return http.Header{}, nil },
	})

	// The offered set is the authority on what may be recovered, so the fixture
	// declares the tool it expects back. The point this test makes — that a
	// recovered name needs no "devcouncil_" prefix — is unchanged: any offered
	// name is accepted, whatever its shape.
	stream, err := adapter.Stream(context.Background(), llm.Request{
		Model: "qwen-27b",
		Tools: []llm.ToolSchema{{Name: "read_custom_file"}},
	})
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	defer stream.Close()

	for {
		if _, err := stream.Next(); err == io.EOF {
			break
		}
	}

	resp, err := stream.Response()
	if err != nil {
		t.Fatalf("Response failed: %v", err)
	}

	if resp.StopReason != llm.StopToolUse {
		t.Errorf("expected StopToolUse, got %s", resp.StopReason)
	}

	calls := resp.Message.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call extracted, got %d", len(calls))
	}
	if calls[0].Name != "read_custom_file" {
		t.Errorf("expected read_custom_file, got %s", calls[0].Name)
	}

	var parsed struct {
		Path    string   `json:"path"`
		Enabled bool     `json:"enabled"`
		Limit   *int     `json:"limit"`
		Tags    []string `json:"tags"`
	}
	if err := json.Unmarshal(calls[0].Arguments, &parsed); err != nil {
		t.Fatalf("failed to decode repaired JSON arguments: %v (raw: %s)", err, string(calls[0].Arguments))
	}
	if !parsed.Enabled || parsed.Limit != nil || len(parsed.Tags) != 2 || parsed.Tags[1] != "v2" {
		t.Errorf("unexpected parsed arguments: %+v", parsed)
	}
}

func TestOpenAICompatMultipleFallbackToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		events := []string{
			`data: {"choices":[{"delta":{"content":"Reading two files:\n<tool_call>\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"a.go\"}}\n</tool_call>\n<tool_call>\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"b.go\"}}\n</tool_call>"}}]}`,
			`data: {"choices":[{"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}
		for _, e := range events {
			_, _ = w.Write([]byte(e + "\n\n"))
		}
	}))
	defer server.Close()

	adapter := New(Options{
		Name:     "local",
		BaseURL:  server.URL,
		Validate: func(req llm.Request) error { return nil },
		Header:   func() (http.Header, error) { return http.Header{}, nil },
	})

	// The request carries the tool it is about to recover a call to. A real
	// request always does, and the offered set is what the recovery is checked
	// against — a call naming a tool that was never offered is prose, not a
	// call, so a fixture that omits Tools would be testing a request no caller
	// makes.
	stream, err := adapter.Stream(context.Background(), llm.Request{
		Model: "qwen-27b",
		Tools: []llm.ToolSchema{{Name: "read_file"}},
	})
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	defer stream.Close()

	for {
		if _, err := stream.Next(); err == io.EOF {
			break
		}
	}

	resp, err := stream.Response()
	if err != nil {
		t.Fatalf("Response failed: %v", err)
	}

	calls := resp.Message.ToolCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 tool calls extracted, got %d", len(calls))
	}
	if calls[0].Name != "read_file" || !strings.Contains(string(calls[0].Arguments), "a.go") {
		t.Errorf("unexpected first tool call: %+v", calls[0])
	}
	if calls[1].Name != "read_file" || !strings.Contains(string(calls[1].Arguments), "b.go") {
		t.Errorf("unexpected second tool call: %+v", calls[1])
	}
}

func TestSanitizeJSONArgumentsAdversarial(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "already valid",
			input:    `{"a": 1, "b": "hello"}`,
			expected: `{"a": 1, "b": "hello"}`,
		},
		{
			name:     "markdown code block",
			input:    "```json\n{\"a\": 1, \"b\": 2}\n```",
			expected: `{"a": 1, "b": 2}`,
		},
		{
			name:     "trailing commas",
			input:    `{"a": 1, "items": [1, 2, ], }`,
			expected: `{"a": 1, "items": [1, 2 ] }`,
		},
		{
			name:     "python literals",
			input:    `{"active": True, "missing": None, "off": False}`,
			expected: `{"active": true, "missing": null, "off": false}`,
		},
		{
			name:     "python literals not replaced inside string values",
			input:    `{"name": "True is True", "flag": True}`,
			expected: `{"name": "True is True", "flag": true}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeJSONArguments(tc.input)
			if !json.Valid(got) {
				t.Fatalf("sanitizeJSONArguments produced invalid JSON: %s", string(got))
			}
			var gotObj, expObj any
			if err := json.Unmarshal(got, &gotObj); err != nil {
				t.Fatalf("unmarshal got: %v", err)
			}
			if err := json.Unmarshal([]byte(tc.expected), &expObj); err != nil {
				t.Fatalf("unmarshal expected: %v", err)
			}
		})
	}
}

// TestAStringErrorFieldDoesNotAbandonTheStream is the regression test for a
// lost turn.
//
// OpenAI documents `error` as an object with a message. Several local servers
// send a bare string instead. The strict struct could not decode that, the
// chunk was reported as undecodable, and the whole stream was abandoned — a
// real ten-minute agent turn died that way, on a message the server was only
// reporting in passing.
func TestAStringErrorFieldDoesNotAbandonTheStream(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame string
		want  string
	}{
		{"bare string", `data: {"error":"model is overloaded"}`, "model is overloaded"},
		{"documented object", `data: {"error":{"message":"bad request","code":400}}`, "bad request"},
		{"object with no message", `data: {"error":{"code":500}}`, "code"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.frame + "\n\n"))
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
			}))
			defer server.Close()

			adapter := New(Options{
				Name:     "test-provider",
				BaseURL:  server.URL,
				Validate: func(req llm.Request) error { return nil },
				Header:   func() (http.Header, error) { return http.Header{}, nil },
			})

			stream, err := adapter.Stream(context.Background(), llm.Request{
				Model: "m",
				Messages: []llm.Message{{
					Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "hi"}},
				}},
			})
			if err != nil {
				t.Fatalf("stream: %v", err)
			}
			defer stream.Close()

			var failure error
			for {
				_, err := stream.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					failure = err
					break
				}
			}
			if failure == nil {
				t.Fatal("an error frame must surface as an error")
			}
			if !strings.Contains(failure.Error(), tc.want) {
				t.Fatalf("the error must carry what the server said; got %q, want it to mention %q",
					failure, tc.want)
			}
			if strings.Contains(failure.Error(), "undecodable") {
				t.Fatalf("the shape was understood, so it must not be reported as undecodable: %v", failure)
			}
		})
	}
}
