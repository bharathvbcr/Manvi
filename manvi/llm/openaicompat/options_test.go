package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"manvi/llm"
)

// These cover the declared Options that reach the wire or the decoder. Each one
// is a statement an operator made about their own server, and a declaration
// that silently does nothing is worse than one that refuses: the operator sets
// it, sees no change, and concludes the server ignores it.

// TestEveryDeclaredSamplingFieldReachesTheWire. The penalties in particular had
// no coverage at all: frequency_penalty could have been dropped between the
// neutral request and the payload and nothing would have failed.
func TestEveryDeclaredSamplingFieldReachesTheWire(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	a := New(Options{
		Name: "local", BaseURL: srv.URL,
		Validate: func(llm.Request) error { return nil },
		Header:   func() (http.Header, error) { return http.Header{}, nil },
	})

	temp, topP, minP := 0.7, 0.8, 0.05
	repPen, presence, frequency := 1.05, 0.25, 0.75
	topK, seed := 20, 1234
	s, err := a.Stream(context.Background(), llm.Request{
		Model:             "qwen",
		Temperature:       &temp,
		TopP:              &topP,
		TopK:              &topK,
		MinP:              &minP,
		RepetitionPenalty: &repPen,
		PresencePenalty:   &presence,
		FrequencyPenalty:  &frequency,
		Seed:              &seed,
		Stop:              []string{"<|im_end|>"},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_ = s.Close()

	for _, want := range []struct {
		key string
		val any
	}{
		{"temperature", 0.7},
		{"top_p", 0.8},
		{"top_k", float64(20)},
		{"min_p", 0.05},
		{"repetition_penalty", 1.05},
		{"presence_penalty", 0.25},
		{"frequency_penalty", 0.75},
		{"seed", float64(1234)},
	} {
		if got, ok := body[want.key]; !ok || got != want.val {
			t.Errorf("%s = %v (present=%v), want %v", want.key, got, ok, want.val)
		}
	}
	stop, _ := body["stop"].([]any)
	if len(stop) != 1 || stop[0] != "<|im_end|>" {
		t.Errorf("stop = %v", body["stop"])
	}
}

// TestAZeroValuedSamplingFieldIsSentRatherThanOmitted. Zero is a real setting —
// temperature 0 is greedy decoding, a zero seed is a seed — and omitting it
// hands the server its own default instead. The pointer types exist for exactly
// this distinction, and omitempty on a pointer honours it.
func TestAZeroValuedSamplingFieldIsSentRatherThanOmitted(t *testing.T) {
	var raw map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	a := New(Options{
		Name: "local", BaseURL: srv.URL,
		Validate: func(llm.Request) error { return nil },
		Header:   func() (http.Header, error) { return http.Header{}, nil },
	})
	zeroF := 0.0
	zeroI := 0
	s, err := a.Stream(context.Background(), llm.Request{
		Model:            "qwen",
		Temperature:      &zeroF,
		FrequencyPenalty: &zeroF,
		PresencePenalty:  &zeroF,
		TopK:             &zeroI,
		Seed:             &zeroI,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_ = s.Close()

	for _, key := range []string{"temperature", "frequency_penalty", "presence_penalty", "top_k", "seed"} {
		got, ok := raw[key]
		if !ok {
			t.Errorf("%s was omitted; an explicit zero is a decision, not an absence", key)
			continue
		}
		if string(got) != "0" {
			t.Errorf("%s = %s, want 0", key, got)
		}
	}
}

// TestTheDeclaredPrefillReachesTheStream. The filter's own behaviour is covered
// elsewhere; what is covered here is that the Options field is wired to it. The
// difference is visible live: a declared prefill labels the first byte as
// reasoning, where an undeclared one emits it as text and corrects the settled
// message afterwards.
func TestTheDeclaredPrefillReachesTheStream(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"delta":{"content":"First I check the imports."}}]}`,
		`data: {"choices":[{"delta":{"content":"</think>The unused import is fmt."}}]}`,
		`data: {"choices":[{"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}

	for _, tc := range []struct {
		name         string
		declared     bool
		firstKind    llm.ChunkKind
		reclassified bool
	}{
		{"declared", true, llm.ChunkReasoning, false},
		{"undeclared", false, llm.ChunkText, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for _, f := range frames {
					_, _ = fmt.Fprint(w, f+"\n\n")
				}
			}))
			t.Cleanup(srv.Close)

			a := New(Options{
				Name: "local", BaseURL: srv.URL,
				AssumeReasoningPrefill: tc.declared,
				Validate:               func(llm.Request) error { return nil },
				Header:                 func() (http.Header, error) { return http.Header{}, nil },
			})
			s, err := a.Stream(context.Background(), llm.Request{Model: "qwen"})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			defer func() { _ = s.Close() }()

			var chunks []llm.Chunk
			for {
				c, err := s.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("Next: %v", err)
				}
				chunks = append(chunks, c)
			}
			if len(chunks) == 0 {
				t.Fatal("no chunks")
			}
			if chunks[0].Kind != tc.firstKind {
				t.Errorf("first chunk kind = %v, want %v — the declaration did not reach the filter",
					chunks[0].Kind, tc.firstKind)
			}
			resp, err := s.Response()
			if err != nil {
				t.Fatalf("Response: %v", err)
			}
			if resp.Decoding.ReasoningReclassified != tc.reclassified {
				t.Errorf("ReasoningReclassified = %v, want %v",
					resp.Decoding.ReasoningReclassified, tc.reclassified)
			}
			// The tag never survives into the answer either way. What differs
			// is the split: a declared prefill separates the chain of thought,
			// while an undeclared one keeps the text — the stream cannot tell a
			// prefill from prose about think tags, and guessing wrong deleted
			// answers.
			got := resp.Message.Text()
			if strings.Contains(got, "</think>") {
				t.Errorf("the closing tag survived into the answer: %q", got)
			}
			if tc.reclassified {
				if !strings.Contains(got, "The unused import is fmt.") {
					t.Errorf("the answer was lost: %q", got)
				}
				if !strings.Contains(got, "I check the imports") {
					t.Errorf("undeclared prefill discarded the leading text: %q", got)
				}
			} else if got != "The unused import is fmt." {
				t.Errorf("declared prefill did not separate the reasoning: %q", got)
			}
		})
	}
}
