package local

import (
	"context"
	"strings"
	"testing"
	"time"

	"manvi/credentials"
	"manvi/llm"
)

// remoteBaseURL is an address in TEST-NET-3 (RFC 5737), reserved for
// documentation and guaranteed never to be a real host. It stands in for "an
// inference server somewhere else", which is what an operator produces the
// moment they point llm.local.base_url off this machine.
const remoteBaseURL = "http://198.51.100.7:8000/v1"

// TestABorrowedCredentialIsNotSentOffThisMachine.
//
// The "local" provider accepts OPENAI_API_KEY as a convenience, and
// llm.local.base_url does not have to name this machine. Nothing tied those two
// facts together: transport had a loopback predicate and used it to choose a
// connection pool, not to decide whether to attach a credential. An operator
// who pointed this provider at a remote inference host therefore shipped their
// real OpenAI key to that host as a bearer token, silently.
//
// The assertion that this returns without reaching the network is the fix, not
// a detail of it: the point is that the key never leaves the process. Before
// the check existed the request was built, dialled and only failed on the
// connection, by which time the header had been assembled to send.
func TestABorrowedCredentialIsNotSentOffThisMachine(t *testing.T) {
	cfg := Config{BaseURL: remoteBaseURL, SupportsTools: true, AssumeModelServed: true}
	a := New(cfg, func() (credentials.Secret, error) {
		return credentials.NewSecret("sk-not-a-real-key", "OPENAI_API_KEY"), nil
	})

	// Short, so a build that does dial fails as a timeout rather than hanging
	// the suite — and so "it came back instantly" is a fact the test can rely
	// on rather than an impression.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	started := time.Now()
	_, err := a.Stream(ctx, llm.Request{Model: "some-local-model", MaxTokens: 64,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "hi"}}}}})
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("a borrowed credential was sent to a host that is not this machine")
	}
	if !strings.Contains(err.Error(), "refusing to send the credential in OPENAI_API_KEY") {
		t.Fatalf("Stream failed with %v; want the refusal naming the borrowed variable", err)
	}
	if strings.Contains(err.Error(), "sk-not-a-real-key") {
		t.Fatal("the refusal quoted the credential it was refusing to send")
	}
	if elapsed > time.Second {
		t.Errorf("took %s to refuse; the request reached the network before the check ran", elapsed)
	}
}

// TestTheDestinationCheckOnlyHoldsBackABorrowedKey.
//
// The check has to be narrow or it breaks the arrangements it is not about: a
// key set for this provider is deliberate wherever it is pointed, a key
// supplied in-process by an embedding program is deliberate by construction,
// and a loopback server is the case the OPENAI_API_KEY convenience exists for.
func TestTheDestinationCheckOnlyHoldsBackABorrowedKey(t *testing.T) {
	for _, tc := range []struct {
		name    string
		baseURL string
		source  string
		refuse  bool
	}{
		{name: "borrowed key, remote host", baseURL: remoteBaseURL, source: "OPENAI_API_KEY", refuse: true},
		{name: "borrowed key, loopback by name", baseURL: "http://localhost:8000/v1", source: "OPENAI_API_KEY"},
		{name: "borrowed key, loopback by address", baseURL: "http://127.0.0.1:8000/v1", source: "OPENAI_API_KEY"},
		{name: "borrowed key, loopback over v6", baseURL: "http://[::1]:8000/v1", source: "OPENAI_API_KEY"},
		{name: "this provider's own variable, remote host", baseURL: remoteBaseURL, source: DedicatedCredentialEnvVar},
		{name: "supplied in-process, remote host", baseURL: remoteBaseURL, source: "Resolver.Set"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkCredentialDestination(tc.baseURL, credentials.NewSecret("sk-not-a-real-key", tc.source))
			if tc.refuse && err == nil {
				t.Errorf("checkCredentialDestination(%q, from %s) allowed it", tc.baseURL, tc.source)
			}
			if !tc.refuse && err != nil {
				t.Errorf("checkCredentialDestination(%q, from %s) = %v, want it allowed",
					tc.baseURL, tc.source, err)
			}
		})
	}
}

// TestALoopbackServerStillReceivesTheCredential is the control on the wire. Some
// operators front a loopback server with a proxy that does check the key, and
// the refusal above must not have quietly stopped sending it to them.
func TestALoopbackServerStillReceivesTheCredential(t *testing.T) {
	s := newServer(t, []string{"qwen-local"}, 200)
	a := New(cfgFor(s), func() (credentials.Secret, error) {
		return credentials.NewSecret("sk-not-a-real-key", "OPENAI_API_KEY"), nil
	})

	if _, ok := a.Capability("qwen-local"); !ok {
		t.Fatal("the server lists this model")
	}
	select {
	case got := <-s.authSeen:
		if got != "Bearer sk-not-a-real-key" {
			t.Errorf("Authorization = %q; a loopback server stopped receiving the credential", got)
		}
	default:
		t.Fatal("the server saw no request at all")
	}
}
