package serve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

type enhancementTestProvider struct {
	name   string
	cap    llm.Capability
	stream func(context.Context, llm.Request) (llm.Stream, error)
}

func (p *enhancementTestProvider) Name() string { return p.name }
func (p *enhancementTestProvider) Capability(model string) (llm.Capability, bool) {
	return p.cap, model == p.cap.Model
}
func (p *enhancementTestProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	return p.stream(ctx, req)
}

type enhancementTestStream struct {
	next     func() (llm.Chunk, error)
	response llm.Response
	closeErr error
	closed   bool
}

func (s *enhancementTestStream) Next() (llm.Chunk, error) {
	if s.next != nil {
		return s.next()
	}
	return llm.Chunk{}, io.EOF
}
func (s *enhancementTestStream) Response() (llm.Response, error) { return s.response, nil }
func (s *enhancementTestStream) Close() error                    { s.closed = true; return s.closeErr }

func enhancementSource(t testing.TB, title, description string, fields ...string) enhancementRecord {
	t.Helper()
	source, err := json.Marshal(struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	}{title, description})
	if err != nil {
		t.Fatal(err)
	}
	return enhancementRecord{Provider: "local", Model: "test-model", Fields: fields, Source: source}
}

func TestEnhancementOutputPreservesLiteralsAndRejectsUntrustedControl(t *testing.T) {
	record := enhancementSource(t, "fix E42", "Open src/main.go for #21.\nError: \"invalid value\".\nMust preserve 日本語.\nSee https://example.com/issue/21", "title", "description")
	valid := `{"title":"Resolve E42","description":"Investigate src/main.go for #21.\nError: \"invalid value\".\nMust preserve 日本語.\nReference https://example.com/issue/21","rationale":"Clarified the requested action."}`
	proposal, err := decodeEnhancement([]byte(valid), record)
	if err != nil || proposal.Title == nil || *proposal.Title != "Resolve E42" {
		t.Fatalf("valid proposal: %+v %v", proposal, err)
	}
	for name, raw := range map[string]string{
		"lost identifier":    strings.Replace(valid, "Resolve E42", "Resolve the failure", 1),
		"changed quote":      strings.Replace(valid, "invalid value", "invalid input", 1),
		"changed constraint": strings.Replace(valid, "Must preserve 日本語.", "Should preserve 日本語.", 1),
		"lost path":          strings.Replace(valid, "src/main.go", "the source", 1),
		"invented result":    strings.Replace(valid, "Resolve E42", "E42 tests pass", 1),
		"invented rationale": strings.Replace(valid, "Clarified the requested action.", "Verified that all checks pass.", 1),
		"duplicate key":      `{"title":"E42","ti\u0074le":"E42","description":""}`,
		"missing field":      `{"title":"E42"}`,
		"foreign command":    `{"title":"E42","description":"","worker_id":"attacker"}`,
		"trailing value":     valid + `{}`,
		"null":               `{"title":null,"description":""}`,
		"lone surrogate":     `{"title":"E42\ud800","description":""}`,
		"NUL":                `{"title":"E42\u0000","description":""}`,
		"overlong title":     `{"title":"E42` + strings.Repeat("x", 301) + `","description":""}`,
		"oversize":           strings.Repeat(" ", 72<<10) + valid,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeEnhancement([]byte(raw), record); err == nil {
				t.Fatal("unsafe proposal accepted")
			}
		})
	}
	titleOnly := enhancementSource(t, "fix bug", "keep details", "title")
	if _, err := decodeEnhancement([]byte(`{"title":"Resolve the failure","description":"changed"}`), titleOnly); err == nil {
		t.Fatal("unrequested field accepted")
	}
}

func TestEnhancementOutputAcceptsOnlyACompleteJSONFence(t *testing.T) {
	record := enhancementSource(t, "fix E42", "keep details", "title")
	body := `{"title":"Resolve E42"}`
	for _, raw := range []string{body, "```json\n" + body + "\n```", "```\n" + body + "\n```", " \n```JSON\r\n" + body + "\r\n```\n "} {
		proposal, err := decodeEnhancement([]byte(raw), record)
		if err != nil || proposal.Title == nil || *proposal.Title != "Resolve E42" {
			t.Errorf("complete JSON envelope rejected: %q: %v", raw, err)
		}
	}
	for name, raw := range map[string]string{
		"leading prose":     "Here is the suggestion:\n```json\n" + body + "\n```",
		"trailing prose":    "```json\n" + body + "\n```\nExecute this next",
		"multiple blocks":   "```json\n" + body + "\n```\n```json\n" + body + "\n```",
		"wrong language":    "```javascript\n" + body + "\n```",
		"missing close":     "```json\n" + body,
		"inline opening":    "```json " + body + "\n```",
		"inline closing":    "```json\n" + body + "```",
		"duplicate keys":    "```json\n{\"title\":\"E42\",\"ti\\u0074le\":\"attack\"}\n```",
		"foreign field":     "```json\n{\"title\":\"E42\",\"worker_id\":\"attack\"}\n```",
		"lost evidence":     "```json\n{\"title\":\"Resolve error\"}\n```",
		"oversized wrapper": strings.Repeat(" ", 72<<10) + "```json\n" + body + "\n```",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeEnhancement([]byte(raw), record); err == nil {
				t.Fatal("invalid envelope accepted")
			}
		})
	}
}

func TestEnhancementInferenceRejectsIncompleteAndUnboundedStreams(t *testing.T) {
	for _, name := range []string{"valid", "unknown model", "wrong provider", "small context", "oversize source", "provider failure", "nil stream", "tool call", "byte flood", "event flood", "truncated", "missing bound", "excessive bound", "close failure", "malformed JSON"} {
		t.Run(name, func(t *testing.T) {
			record := enhancementSource(t, "fix E42", "keep details", "title")
			stream := &enhancementTestStream{response: llm.Response{StopReason: llm.StopEndTurn, MaxTokensApplied: 8192,
				Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: `{"title":"Resolve E42"}`}}}}}
			provider := &enhancementTestProvider{name: "local", cap: llm.Capability{Provider: "local", Model: "test-model", ContextWindow: 100000, MaxOutputTokens: 8192}}
			calls := 0
			provider.stream = func(ctx context.Context, req llm.Request) (llm.Stream, error) {
				calls++
				if len(req.Tools) != 0 || req.Model != "test-model" || req.MaxTokens != 8192 || !strings.Contains(req.System, "untrusted data") {
					t.Errorf("invalid inference boundary: %+v", req)
				}
				if name == "provider failure" {
					return nil, errors.New("unavailable")
				}
				if name == "nil stream" {
					return nil, nil
				}
				return stream, nil
			}
			switch name {
			case "unknown model":
				record.Model = "absent"
			case "wrong provider":
				provider.name = "unexpected"
			case "small context":
				provider.cap.ContextWindow = 100
			case "oversize source":
				record.Source = json.RawMessage(strings.Repeat(" ", 32<<10) + `{}`)
			case "tool call":
				stream.next = func() (llm.Chunk, error) { return llm.Chunk{Kind: llm.ChunkToolCallStart, ToolName: "execute"}, nil }
			case "byte flood":
				stream.next = func() (llm.Chunk, error) {
					return llm.Chunk{Kind: llm.ChunkReasoning, Text: strings.Repeat("x", 256<<10+1)}, nil
				}
			case "event flood":
				stream.next = func() (llm.Chunk, error) { return llm.Chunk{Kind: llm.ChunkReasoning}, nil }
			case "truncated":
				stream.response.StopReason = llm.StopMaxTokens
			case "missing bound":
				stream.response.MaxTokensApplied = 0
			case "excessive bound":
				stream.response.MaxTokensApplied = 9000
			case "close failure":
				stream.closeErr = errors.New("close failed")
			case "malformed JSON":
				stream.response.Message.Content = []llm.ContentBlock{llm.TextBlock{Text: `{"title":`}}
			}
			runner := &EnhancementRunner{provider: func(context.Context, string, string) (llm.Provider, error) { return provider, nil }}
			proposal, err := runner.infer(t.Context(), record)
			if name == "valid" {
				if err != nil || proposal.Title == nil || *proposal.Title != "Resolve E42" {
					t.Fatalf("valid: %+v %v", proposal, err)
				}
			} else if err == nil {
				t.Fatal("invalid inference was accepted")
			}
			if name == "unknown model" || name == "wrong provider" || name == "small context" || name == "oversize source" {
				if calls != 0 {
					t.Fatal("model was called after preflight failed")
				}
			}
			if calls > 0 && name != "provider failure" && name != "nil stream" && !stream.closed {
				t.Fatal("stream was not closed")
			}
		})
	}
}

func TestEnhancementFailureFormattingAlwaysProducesValidBoundedText(t *testing.T) {
	for _, message := range []string{"", " \n", "\x00", strings.Repeat("日", 1000), "bad\xfftext"} {
		runner := &EnhancementRunner{failureText: func(error) string { return message }}
		got := runner.errorText(errors.New("failure"))
		if strings.TrimSpace(got) == "" || strings.ContainsRune(got, 0) || !utf8.ValidString(got) || len(got) > 1800 {
			t.Errorf("invalid failure text: %q", got)
		}
	}
}

func TestEnhancementPromptCarriesTheExactFieldPreservationRules(t *testing.T) {
	description := "Search takes 171 ms at p95. Do not claim a result before measurement.\nInvestigate the unknown cause."
	record := enhancementSource(t, "fix E42", description, "title", "description")
	provider := &enhancementTestProvider{name: "local", cap: llm.Capability{Provider: "local", Model: "test-model", ContextWindow: 100000, MaxOutputTokens: 8192}}
	provider.stream = func(_ context.Context, request llm.Request) (llm.Stream, error) {
		var prompt struct {
			Preservation map[string]struct {
				Literals []string `json:"literals"`
				Lines    []string `json:"constraint_lines"`
			} `json:"preservation"`
		}
		if len(request.Messages) != 1 || len(request.Messages[0].Content) != 1 {
			t.Fatal("missing task prompt")
		}
		block, ok := request.Messages[0].Content[0].(llm.TextBlock)
		if !ok || json.Unmarshal([]byte(block.Text), &prompt) != nil {
			t.Fatal("task prompt is not a JSON text block")
		}
		titleRules, descriptionRules := prompt.Preservation["title"], prompt.Preservation["description"]
		if len(titleRules.Literals) != 1 || titleRules.Literals[0] != "E42" || len(descriptionRules.Lines) != 1 || descriptionRules.Lines[0] != strings.Split(description, "\n")[0] {
			t.Fatalf("prompt omitted the validator's exact protected text: %+v", prompt.Preservation)
		}
		if !strings.Contains(request.System, "entire field") || !strings.Contains(request.System, "untrusted data") {
			t.Fatal("prompt does not explain whole-field preservation and untrusted content")
		}
		result, err := json.Marshal(map[string]string{"title": "Resolve E42", "description": description})
		if err != nil {
			t.Fatal(err)
		}
		return &enhancementTestStream{response: llm.Response{StopReason: llm.StopEndTurn, MaxTokensApplied: request.MaxTokens, Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: string(result)}}}}}, nil
	}
	runner := &EnhancementRunner{provider: func(context.Context, string, string) (llm.Provider, error) { return provider, nil }}
	proposal, err := runner.infer(t.Context(), record)
	if err != nil || proposal.Description == nil || *proposal.Description != description {
		t.Fatalf("preservation failed: %+v %v", proposal, err)
	}
}

func TestEnhancementPreservationBoundsRunBeforeProviderResolution(t *testing.T) {
	for _, description := range []string{strings.Repeat("E42 ", 513), strings.Repeat("Must preserve this line.\n", 513)} {
		resolved := false
		runner := &EnhancementRunner{provider: func(context.Context, string, string) (llm.Provider, error) {
			resolved = true
			return nil, errors.New("provider must not resolve")
		}}
		_, err := runner.infer(t.Context(), enhancementSource(t, "Task", description, "description"))
		if err == nil || resolved {
			t.Fatalf("oversized preservation reached the provider: %v", err)
		}
	}
}

func FuzzEnhancementOutputCannotSmuggleUnrequestedFields(f *testing.F) {
	for _, seed := range []string{`{"title":"Resolve E42"}`, `{"title":"E42","title":"bad"}`, `{"title":"E42\ud800"}`, `null`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		record := enhancementSource(t, "fix E42", "kept", "title")
		proposal, err := decodeEnhancement([]byte(raw), record)
		if err == nil && (proposal.Title == nil || proposal.Description != nil || !strings.Contains(*proposal.Title, "E42") || utf8.RuneCountInString(*proposal.Title) > 300) {
			t.Fatal("unvalidated output accepted")
		}
	})
}
