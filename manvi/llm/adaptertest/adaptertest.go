// Package adaptertest drives provider adapters against a scripted HTTP server.
//
// It exists so all three adapters face the same hostile stream conditions. A
// decoder that is only tested on the happy path is tested on the one case that
// never causes an incident: the failures that matter are a frame split across
// packet boundaries, a tool call whose arguments stop halfway, an event type
// nobody has seen before, and a server that hangs up mid-message. Each of those
// has a correct behaviour, and for most of them the correct behaviour is a
// reported error rather than a shorter answer.
package adaptertest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"manvi/credentials"
	"manvi/llm"
)

// Server is a scripted SSE endpoint.
type Server struct {
	*httptest.Server
	// Requests holds each decoded request body, so a test can assert on what
	// was actually sent rather than on what the builder was asked to send.
	Requests []string
	// Headers holds the headers of each request.
	Headers []http.Header
}

// NewServer returns a server that replies with the given raw stream body.
//
// The body is written in small pieces with explicit flushes, so the adapter
// genuinely sees a chunked stream. Writing it in one call would make every
// frame arrive whole and would not exercise the reader at all.
func NewServer(t *testing.T, body string) *Server {
	t.Helper()
	s := &Server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		s.Requests = append(s.Requests, string(raw))
		s.Headers = append(s.Headers, r.Header.Clone())

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server cannot flush; the stream would arrive whole")
			return
		}
		for i := 0; i < len(body); i += 7 {
			end := i + 7
			if end > len(body) {
				end = len(body)
			}
			if _, err := io.WriteString(w, body[i:end]); err != nil {
				return
			}
			flusher.Flush()
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// NewStatusServer returns a server that always fails with a status and body.
func NewStatusServer(t *testing.T, status int, body string) *Server {
	t.Helper()
	s := &Server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		s.Requests = append(s.Requests, string(raw))
		s.Headers = append(s.Headers, r.Header.Clone())
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return s
}

// Secret returns a resolver function yielding a fixed test credential.
func Secret(value string) func() (credentials.Secret, error) {
	return func() (credentials.Secret, error) {
		return credentials.NewSecret(value, "TEST"), nil
	}
}

// MissingSecret returns a resolver that always fails, for asserting that a
// missing credential is refused before any request is sent.
func MissingSecret() func() (credentials.Secret, error) {
	return func() (credentials.Secret, error) {
		return credentials.Secret{}, &credentials.ErrMissing{Provider: "test", EnvVars: []string{"TEST_KEY"}}
	}
}

// Drain reads a stream to exhaustion and returns the chunks and the settled
// response, or the first error.
func Drain(s llm.Stream) ([]llm.Chunk, llm.Response, error) {
	defer s.Close()
	var chunks []llm.Chunk
	for {
		chunk, err := s.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return chunks, llm.Response{}, err
		}
		chunks = append(chunks, chunk)
	}
	resp, err := s.Response()
	return chunks, resp, err
}

// TextOf concatenates the text chunks.
func TextOf(chunks []llm.Chunk) string {
	var b strings.Builder
	for _, c := range chunks {
		if c.Kind == llm.ChunkText {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// ReasoningOf concatenates the reasoning chunks.
func ReasoningOf(chunks []llm.Chunk) string {
	var b strings.Builder
	for _, c := range chunks {
		if c.Kind == llm.ChunkReasoning {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// Ctx is a background context, for brevity at call sites.
func Ctx() context.Context { return context.Background() }
