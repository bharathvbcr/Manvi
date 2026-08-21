// Package replay is a deterministic provider that plays back recorded
// fixtures.
//
// It is built before the real adapters on purpose. Everything above the
// provider seam — the turn driver, the tool pipeline, the policy gate, fan-out
// — is then testable offline, for free, and with byte-identical results run to
// run. A harness whose loop can only be exercised by spending tokens is a
// harness whose loop is under-tested.
//
// The fixture format is deliberately plain JSON so a real session can be
// recorded into one and replayed as a regression test.
package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"manvi/llm"
)

// Turn is one recorded model response.
type Turn struct {
	// Chunks are streamed in order before the response settles.
	Chunks []llm.Chunk `json:"chunks"`
	// Message is the settled assistant message.
	Message llm.Message `json:"message"`
	// StopReason and Usage complete the response.
	StopReason llm.StopReason `json:"stop_reason"`
	Usage      llm.Usage      `json:"usage"`
	// MaxTokensApplied replays the output bound the recorded request carried,
	// so a captured truncation replays as one. Without it every replayed turn
	// looks unbounded and the loop's cap check cannot be exercised.
	MaxTokensApplied int `json:"max_tokens_applied,omitempty"`
	// Malformed replays tool calls an adapter could not reconstruct, so the
	// loop's recovery path is exercised by the same mechanism as success.
	Malformed []llm.MalformedCall `json:"malformed,omitempty"`
	// Decoding replays adapter compensations.
	Decoding llm.DecodingReport `json:"decoding,omitempty"`
	// Err, when set, makes this turn fail instead of returning — so error
	// handling in the loop is exercised by the same mechanism as success.
	Err string `json:"error,omitempty"`
}

// Fixture is a recorded session.
type Fixture struct {
	Provider     string           `json:"provider"`
	Capabilities []llm.Capability `json:"capabilities"`
	Turns        []Turn           `json:"turns"`
}

// Provider replays a fixture.
type Provider struct {
	mu      sync.Mutex
	fixture Fixture
	cursor  int
	// Requests records what the loop actually asked for, which is usually the
	// thing under test.
	requests []llm.Request
}

// New returns a replay provider over a fixture.
func New(fixture Fixture) *Provider {
	if fixture.Provider == "" {
		fixture.Provider = "replay"
	}
	return &Provider{fixture: fixture}
}

// Load reads a fixture from disk.
func Load(path string) (*Provider, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("replay: %w", err)
	}
	var fixture Fixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		return nil, fmt.Errorf("replay: %s: %w", path, err)
	}
	return New(fixture), nil
}

// Name identifies the adapter.
func (p *Provider) Name() string { return p.fixture.Provider }

// Capability reports what the fixture declares. A model the fixture does not
// describe is unknown rather than permissively accepted — the same rule real
// adapters follow.
func (p *Provider) Capability(model string) (llm.Capability, bool) {
	for _, c := range p.fixture.Capabilities {
		if c.Model == model {
			if c.Provider == "" {
				c.Provider = p.Name()
			}
			return c, true
		}
	}
	return llm.Capability{}, false
}

// Requests returns every request the provider was asked to serve.
func (p *Provider) Requests() []llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]llm.Request(nil), p.requests...)
}

// Remaining reports how many recorded turns are unplayed. A test that ends with
// turns left over usually means the loop stopped earlier than intended.
func (p *Provider) Remaining() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.fixture.Turns) - p.cursor
}

// Stream plays the next recorded turn.
func (p *Provider) Stream(_ context.Context, req llm.Request) (llm.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.requests = append(p.requests, req)
	if p.cursor >= len(p.fixture.Turns) {
		// Running off the end of a fixture is an error, never an empty
		// response: a silent empty turn would look like a model that chose to
		// stop, and the test would pass for the wrong reason.
		return nil, fmt.Errorf("replay: fixture exhausted after %d turn(s); the loop asked for one more",
			len(p.fixture.Turns))
	}
	turn := p.fixture.Turns[p.cursor]
	p.cursor++

	if turn.Err != "" {
		return nil, fmt.Errorf("replay: %s", turn.Err)
	}

	message := turn.Message
	if message.Role == "" {
		message.Role = llm.RoleAssistant
	}
	if message.Provenance == nil {
		message.Provenance = &llm.AssistantProvenance{Provider: p.Name(), Model: req.Model}
	}

	return &stream{
		chunks: turn.Chunks,
		response: llm.Response{
			Message:          message,
			StopReason:       turn.StopReason,
			Usage:            turn.Usage,
			MaxTokensApplied: turn.MaxTokensApplied,
			Malformed:        turn.Malformed,
			Decoding:         turn.Decoding,
		},
	}, nil
}

type stream struct {
	chunks   []llm.Chunk
	cursor   int
	response llm.Response
	done     bool
	closed   bool
}

func (s *stream) Next() (llm.Chunk, error) {
	if s.closed {
		return llm.Chunk{}, fmt.Errorf("replay: stream is closed")
	}
	if s.cursor >= len(s.chunks) {
		s.done = true
		return llm.Chunk{}, io.EOF
	}
	chunk := s.chunks[s.cursor]
	s.cursor++
	return chunk, nil
}

func (s *stream) Response() (llm.Response, error) {
	if !s.done {
		return llm.Response{}, fmt.Errorf("replay: Response called before the stream was drained")
	}
	return s.response, nil
}

func (s *stream) Close() error {
	s.closed = true
	return nil
}

// Record captures a live provider's output into a fixture, so a real session
// becomes an offline regression test. The wrapped provider is used exactly
// once per Stream call; nothing is retried or reordered.
type Record struct {
	inner   llm.Provider
	mu      sync.Mutex
	fixture Fixture
}

// NewRecord wraps a provider.
func NewRecord(inner llm.Provider) *Record {
	return &Record{inner: inner, fixture: Fixture{Provider: inner.Name()}}
}

func (r *Record) Name() string { return r.inner.Name() }

func (r *Record) Capability(model string) (llm.Capability, bool) {
	capability, ok := r.inner.Capability(model)
	if ok {
		r.mu.Lock()
		r.fixture.Capabilities = appendUnique(r.fixture.Capabilities, capability)
		r.mu.Unlock()
	}
	return capability, ok
}

func (r *Record) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	inner, err := r.inner.Stream(ctx, req)
	if err != nil {
		r.mu.Lock()
		r.fixture.Turns = append(r.fixture.Turns, Turn{Err: err.Error()})
		r.mu.Unlock()
		return nil, err
	}
	return &recordingStream{parent: r, inner: inner}, nil
}

// Fixture returns what has been recorded so far.
func (r *Record) Fixture() Fixture {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fixture
}

// Save writes the fixture to disk.
func (r *Record) Save(path string) error {
	encoded, err := json.MarshalIndent(r.Fixture(), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o644)
}

type recordingStream struct {
	parent *Record
	inner  llm.Stream
	turn   Turn
}

func (s *recordingStream) Next() (llm.Chunk, error) {
	chunk, err := s.inner.Next()
	if err == nil {
		s.turn.Chunks = append(s.turn.Chunks, chunk)
	}
	return chunk, err
}

func (s *recordingStream) Response() (llm.Response, error) {
	resp, err := s.inner.Response()
	if err != nil {
		return resp, err
	}
	s.turn.Message = resp.Message
	s.turn.StopReason = resp.StopReason
	s.turn.Usage = resp.Usage
	s.turn.MaxTokensApplied = resp.MaxTokensApplied
	s.parent.mu.Lock()
	s.parent.fixture.Turns = append(s.parent.fixture.Turns, s.turn)
	s.parent.mu.Unlock()
	return resp, nil
}

func (s *recordingStream) Close() error { return s.inner.Close() }

func appendUnique(list []llm.Capability, item llm.Capability) []llm.Capability {
	for _, existing := range list {
		if existing.Model == item.Model && existing.Provider == item.Provider {
			return list
		}
	}
	return append(list, item)
}
