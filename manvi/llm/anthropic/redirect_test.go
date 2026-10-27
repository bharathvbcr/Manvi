package anthropic

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
	"github.com/bharathvbcr/Manvi/manvi/llm/transport"
)

// x-api-key is a custom header: net/http strips only Authorization, Cookie and
// WWW-Authenticate when a redirect leaves the host, so the provider key used to
// follow a 307/308 to any other origin, with the prompt as the body.
func TestTheAPIKeyIsNeverSentToAnotherOriginByARedirect(t *testing.T) {
	var reached atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(minimalStream))
	}))
	defer other.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+MessagesPath, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	a := New(redirector.URL, adaptertest.Secret("sk-FIXTURE-not-a-real-key"))
	a.client.Retry.BaseDelay = 0
	_, err := a.Stream(adaptertest.Ctx(), llm.Request{Model: "claude-opus-5-5", MaxTokens: 10,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "hi"}}}}})
	var refused *transport.ErrCrossOriginRedirect
	if !errors.As(err, &refused) {
		t.Fatalf("Stream error = %v, want *transport.ErrCrossOriginRedirect", err)
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("the redirect target (a different origin) received %d request(s)", n)
	}
}
