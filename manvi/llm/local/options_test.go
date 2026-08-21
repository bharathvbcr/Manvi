package local

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
	"time"

	"manvi/llm"
	"manvi/llm/transport"
)

// Every field on Config is a statement an operator made about their own
// machine. These tests are about whether the statement has an effect: a setting
// that silently does nothing is worse than one that refuses, because the
// operator sets it, sees no change, and blames the server.

// wireCall captures the chat-completions payload the adapter actually sent.
type wireCall struct {
	Model             string   `json:"model"`
	Temperature       *float64 `json:"temperature"`
	TopP              *float64 `json:"top_p"`
	TopK              *int     `json:"top_k"`
	MinP              *float64 `json:"min_p"`
	RepetitionPenalty *float64 `json:"repetition_penalty"`
	PresencePenalty   *float64 `json:"presence_penalty"`
	FrequencyPenalty  *float64 `json:"frequency_penalty"`
	Seed              *int     `json:"seed"`
	Stop              []string `json:"stop"`
}

// optionServer serves the model listing this adapter insists on discovering,
// and hands the chat request to a caller-supplied handler so each test can
// drive its own stream shape.
func optionServer(t *testing.T, model string, chat http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]string{{"id": model, "object": "model"}},
		})
	})
	mux.HandleFunc("/v1/chat/completions", chat)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// okStream is a minimal healthy response.
func okStream(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

func drainStream(t *testing.T, s llm.Stream) {
	t.Helper()
	for {
		_, err := s.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
}

// TestOperatorSamplingDefaultsAllReachTheServer. TopK, the two penalties and
// the seed were declared, plumbed and never asserted; any one of them could
// have been dropped in Stream and only a careful operator reading their
// server's logs would have noticed.
func TestOperatorSamplingDefaultsAllReachTheServer(t *testing.T) {
	var got wireCall
	srv := optionServer(t, "qwen-27b", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		okStream(w, r)
	})

	temp, topP, minP := 0.6, 0.95, 0.02
	repPen, presence, frequency := 1.05, 0.3, 0.9
	topK, seed := 20, 99
	a := New(Config{
		BaseURL:           srv.URL + "/v1",
		SupportsTools:     true,
		Temperature:       &temp,
		TopP:              &topP,
		TopK:              &topK,
		MinP:              &minP,
		RepetitionPenalty: &repPen,
		PresencePenalty:   &presence,
		FrequencyPenalty:  &frequency,
		Seed:              &seed,
		Stop:              []string{"<|im_end|>"},
	}, noCredential)

	s, err := a.Stream(context.Background(), llm.Request{Model: "qwen-27b"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()
	drainStream(t, s)

	if got.TopK == nil || *got.TopK != 20 {
		t.Errorf("top_k = %v; Qwen3 ships top_k 20 beside its weights and the declaration was dropped", got.TopK)
	}
	if got.PresencePenalty == nil || *got.PresencePenalty != 0.3 {
		t.Errorf("presence_penalty = %v, want 0.3", got.PresencePenalty)
	}
	if got.FrequencyPenalty == nil || *got.FrequencyPenalty != 0.9 {
		t.Errorf("frequency_penalty = %v, want 0.9", got.FrequencyPenalty)
	}
	if got.Seed == nil || *got.Seed != 99 {
		t.Errorf("seed = %v, want 99; an unreproducible run is not investigable", got.Seed)
	}
	if got.Temperature == nil || *got.Temperature != 0.6 {
		t.Errorf("temperature = %v", got.Temperature)
	}
	if got.TopP == nil || *got.TopP != 0.95 {
		t.Errorf("top_p = %v", got.TopP)
	}
	if got.MinP == nil || *got.MinP != 0.02 {
		t.Errorf("min_p = %v", got.MinP)
	}
	if got.RepetitionPenalty == nil || *got.RepetitionPenalty != 1.05 {
		t.Errorf("repetition_penalty = %v", got.RepetitionPenalty)
	}
	if len(got.Stop) != 1 || got.Stop[0] != "<|im_end|>" {
		t.Errorf("stop = %v", got.Stop)
	}
}

// TestARequestSettingBeatsTheOperatorDefault, including when the request's
// value is the zero one. A caller that asked for seed 0 or top_k 0 asked for
// something; overwriting it with the configured default because it looks empty
// is how a deliberate choice becomes invisible.
func TestARequestSettingBeatsTheOperatorDefault(t *testing.T) {
	var got wireCall
	srv := optionServer(t, "qwen-27b", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		okStream(w, r)
	})

	cfgTopK, cfgSeed := 20, 99
	cfgFreq := 0.9
	a := New(Config{
		BaseURL: srv.URL + "/v1", SupportsTools: true,
		TopK: &cfgTopK, Seed: &cfgSeed, FrequencyPenalty: &cfgFreq,
		Stop: []string{"<|im_end|>"},
	}, noCredential)

	reqTopK, reqSeed := 0, 0
	reqFreq := 0.0
	s, err := a.Stream(context.Background(), llm.Request{
		Model: "qwen-27b", TopK: &reqTopK, Seed: &reqSeed, FrequencyPenalty: &reqFreq,
		Stop: []string{"</tool_call>"},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()
	drainStream(t, s)

	if got.TopK == nil || *got.TopK != 0 {
		t.Errorf("top_k = %v; the request asked for 0 and the config overrode it", got.TopK)
	}
	if got.Seed == nil || *got.Seed != 0 {
		t.Errorf("seed = %v; an explicit zero seed is a seed", got.Seed)
	}
	if got.FrequencyPenalty == nil || *got.FrequencyPenalty != 0 {
		t.Errorf("frequency_penalty = %v; the request asked for 0", got.FrequencyPenalty)
	}
	if len(got.Stop) != 1 || got.Stop[0] != "</tool_call>" {
		t.Errorf("stop = %v; the request's stop sequences were replaced by the config's", got.Stop)
	}
}

// TestTheAdapterDoesNotShareTheOperatorsStopSlice. Config is taken by value,
// but a slice field is a window onto the caller's array: a config value reused
// or edited after the adapter was built would change what a later turn sends,
// silently and from outside the adapter.
func TestTheAdapterDoesNotShareTheOperatorsStopSlice(t *testing.T) {
	var got wireCall
	srv := optionServer(t, "qwen-27b", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		okStream(w, r)
	})
	stop := []string{"<|im_end|>"}
	a := New(Config{BaseURL: srv.URL + "/v1", SupportsTools: true, Stop: stop}, noCredential)

	// The caller still holds the slice it handed over.
	stop[0] = "MUTATED"

	s, err := a.Stream(context.Background(), llm.Request{Model: "qwen-27b"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()
	drainStream(t, s)

	if len(got.Stop) != 1 || got.Stop[0] != "<|im_end|>" {
		t.Fatalf("stop = %v; the adapter sent a value edited from outside it after construction", got.Stop)
	}
}

// TestTheDeclaredStallTimeoutAbandonsAWedgedServer is the setting's whole
// reason for existing: a real turn was lost when the server answered
// "Timed out waiting for 600s for the next generated token" after the harness
// had already spent ten minutes waiting on it.
func TestTheDeclaredStallTimeoutAbandonsAWedgedServer(t *testing.T) {
	released := make(chan struct{})
	srv := optionServer(t, "qwen-27b", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"th\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
			t.Error("the client never abandoned the wedged stream")
		}
		close(released)
	})

	a := New(Config{
		BaseURL: srv.URL + "/v1", SupportsTools: true,
		StallTimeout: 200 * time.Millisecond,
	}, noCredential)

	s, err := a.Stream(context.Background(), llm.Request{Model: "qwen-27b"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Next(); err != nil {
		t.Fatalf("the first frame should arrive: %v", err)
	}

	start := time.Now()
	_, err = s.Next()
	var stalled *transport.ErrStalled
	if !errors.As(err, &stalled) {
		t.Fatalf("Next err = %v (%T), want a stall — the declared timeout never reached the stream", err, err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("a 200ms stall limit took %s to fire", elapsed)
	}
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("the wedged connection was never torn down")
	}
}

// TestAnUnsetStallTimeoutTakesThePackageDefault. Zero must mean the documented
// default, not an instant abandonment — which would make every stream fail on
// the first pause and look like a broken server.
func TestAnUnsetStallTimeoutTakesThePackageDefault(t *testing.T) {
	srv := optionServer(t, "qwen-27b", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		w.(http.Flusher).Flush()
		// Far longer than an instant, far shorter than the default.
		time.Sleep(250 * time.Millisecond)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	})

	cfg := Config{BaseURL: srv.URL + "/v1", SupportsTools: true}
	if cfg.withDefaults().StallTimeout != 0 {
		t.Fatal("Config must leave StallTimeout alone; the wire layer owns the default")
	}
	a := New(cfg, noCredential)
	s, err := a.Stream(context.Background(), llm.Request{Model: "qwen-27b"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()
	drainStream(t, s)

	resp, err := s.Response()
	if err != nil {
		t.Fatalf("an unset stall timeout abandoned a healthy stream: %v", err)
	}
	if resp.Message.Text() != "a" {
		t.Fatalf("text = %q", resp.Message.Text())
	}
}

// TestANegativeStallTimeoutDisablesTheWatchdogEntirely. The escape hatch is
// documented on the field; an operator who turned the watchdog off must get a
// stream bounded only by their own deadline.
func TestANegativeStallTimeoutDisablesTheWatchdogEntirely(t *testing.T) {
	srv := optionServer(t, "qwen-27b", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"th\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})

	a := New(Config{
		BaseURL: srv.URL + "/v1", SupportsTools: true,
		StallTimeout: -1,
	}, noCredential)

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	s, err := a.Stream(ctx, llm.Request{Model: "qwen-27b"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Next(); err != nil {
		t.Fatalf("first frame: %v", err)
	}
	_, err = s.Next()
	var stalled *transport.ErrStalled
	if errors.As(err, &stalled) {
		t.Fatalf("the watchdog fired although the operator disabled it: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v; with the watchdog off only the caller's deadline ends this stream", err)
	}
}

// TestAStallIsNotReportedAsAnUndiscoverableServer. The two diagnoses send an
// operator to different places: one to start a process, the other to look at a
// process that is already running and stuck.
func TestAStallIsNotReportedAsAnUndiscoverableServer(t *testing.T) {
	srv := optionServer(t, "qwen-27b", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"th\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	a := New(Config{
		BaseURL: srv.URL + "/v1", SupportsTools: true,
		StallTimeout: 150 * time.Millisecond,
	}, noCredential)

	s, err := a.Stream(context.Background(), llm.Request{Model: "qwen-27b"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()
	for {
		if _, err := s.Next(); err != nil {
			var undiscoverable *ErrUndiscoverable
			var notServed *ErrNotServed
			if errors.As(err, &undiscoverable) {
				t.Fatalf("a stalled generation was reported as an unreachable server: %v", err)
			}
			if errors.As(err, &notServed) {
				t.Fatalf("a stalled generation was reported as an absent model: %v", err)
			}
			var stalled *transport.ErrStalled
			if !errors.As(err, &stalled) {
				t.Fatalf("err = %v (%T), want a stall", err, err)
			}
			return
		}
	}
}

// TestTheDeclaredPrefillReachesTheFilter. Without the declaration the chain of
// thought is emitted as text and only the settled message is corrected; with
// it, the very first chunk is labelled reasoning. That difference is the whole
// observable effect of the setting.
func TestTheDeclaredPrefillReachesTheFilter(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"delta":{"content":"I should read the file first."}}]}`,
		`data: {"choices":[{"delta":{"content":"</think>The bug is a nil map."}}]}`,
		`data: {"choices":[{"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}
	for _, tc := range []struct {
		name      string
		declared  bool
		firstKind llm.ChunkKind
	}{
		{"declared", true, llm.ChunkReasoning},
		{"undeclared", false, llm.ChunkText},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := optionServer(t, "qwen-27b", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for _, f := range frames {
					_, _ = fmt.Fprint(w, f+"\n\n")
				}
			})
			a := New(Config{
				BaseURL: srv.URL + "/v1", SupportsTools: true,
				AssumeReasoningPrefill: tc.declared,
			}, noCredential)

			s, err := a.Stream(context.Background(), llm.Request{Model: "qwen-27b"})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			defer func() { _ = s.Close() }()

			var first llm.Chunk
			seen := false
			for {
				c, err := s.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("Next: %v", err)
				}
				if !seen {
					first, seen = c, true
				}
			}
			if !seen {
				t.Fatal("no chunks arrived")
			}
			if first.Kind != tc.firstKind {
				t.Errorf("first chunk = %v, want %v — the declaration did not reach the stream",
					first.Kind, tc.firstKind)
			}
			resp, err := s.Response()
			if err != nil {
				t.Fatalf("Response: %v", err)
			}
			// The tag never survives into the answer. What differs is the
			// split: a declared prefill separates the chain of thought, while
			// an undeclared one keeps the text, because the stream cannot tell
			// a prefilled block from prose that mentions a closing tag and
			// guessing wrong deleted the answer outright.
			got := resp.Message.Text()
			if strings.Contains(got, "</think>") {
				t.Errorf("the closing tag survived into the answer: %q", got)
			}
			if tc.firstKind == llm.ChunkReasoning {
				if got != "The bug is a nil map." {
					t.Errorf("declared prefill did not separate the reasoning: %q", got)
				}
			} else if !strings.Contains(got, "The bug is a nil map.") {
				t.Errorf("the answer was lost: %q", got)
			}
		})
	}
}
