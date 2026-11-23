package local

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestANamedAddressStillReceivesTheCredential is the control: an operator who
// fronts their server with a proxy that checks a key said where it is, and the
// rule that withholds the key from guesses must not have stopped sending it.
func TestANamedAddressStillReceivesTheCredential(t *testing.T) {
	srv := newAuthRecorder(t, http.StatusOK)
	servers := Scan(context.Background(), ScanOptions{
		Endpoints:  []Endpoint{{BaseURL: srv.URL + "/v1", Named: true}},
		Credential: borrowedKey,
		Timeout:    5 * time.Second,
	})
	if len(servers) != 1 {
		t.Fatalf("found %d servers, want 1", len(servers))
	}
	want := "Bearer sk-not-a-real-key"
	got := srv.headers()
	if len(got) == 0 || got[0] != want {
		t.Fatalf("Authorization seen = %q, want %q on the first request", got, want)
	}
}

// TestAResolutionNeverSendsACredential: ResolveEndpoint only scans when the
// operator named no address, so everything it probes is a guess.
func TestAResolutionNeverSendsACredential(t *testing.T) {
	srv := newAuthRecorder(t, http.StatusOK)
	res := ResolveEndpoint(context.Background(), ResolveOptions{
		Declared:  "http://127.0.0.1:1/v1",
		Endpoints: endpointsOf(srv.URL),
		Timeout:   5 * time.Second,
	})
	if res.Found == nil {
		t.Fatal("the lone listener was not found")
	}
	for _, h := range srv.headers() {
		if h != "" {
			t.Fatalf("a resolution sent Authorization %q to an address nobody named", h)
		}
	}
}

// TestA401Or403IsAServerThatIsHere: a protected server answered, and calling it
// absent would hide it from the operator who has to pin it.
func TestA401Or403IsAServerThatIsHere(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := newAuthRecorder(t, status)
		servers := Scan(context.Background(), ScanOptions{
			Endpoints: endpointsOf(srv.URL), Timeout: 5 * time.Second,
		})
		if len(servers) != 1 {
			t.Fatalf("status %d: found %d servers, want the protected one reported", status, len(servers))
		}
		got := servers[0]
		if !got.AuthRequired || len(got.Models) != 0 {
			t.Errorf("status %d: %+v, want AuthRequired with no models", status, got)
		}
		if !strings.Contains(got.Describe(), "requires a credential") {
			t.Errorf("status %d: Describe() = %q does not say a credential is required", status, got.Describe())
		}
		if _, why := SoleUsableModel(got); !strings.Contains(why, "requires a credential") {
			t.Errorf("status %d: SoleUsableModel explains %q; it must not say the server serves nothing", status, why)
		}
	}
}

// TestAdoptingAProtectedListenerIsProposedNotSilent: the resolution says what
// was found, that it wants a key, that none was sent, and the one command that
// confirms it.
func TestAdoptingAProtectedListenerIsProposedNotSilent(t *testing.T) {
	srv := newAuthRecorder(t, http.StatusUnauthorized)
	res := ResolveEndpoint(context.Background(), ResolveOptions{
		Declared:  "http://127.0.0.1:1/v1",
		Endpoints: endpointsOf(srv.URL),
		Timeout:   5 * time.Second,
	})
	if res.Found == nil || !res.Found.AuthRequired {
		t.Fatalf("Found = %+v, want the protected listener", res.Found)
	}
	note := res.Note()
	for _, want := range []string{"requires a credential", "none is sent to an address you did not name", "manvi use local --base-url " + srv.URL + "/v1"} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q lacks %q", note, want)
		}
	}
}

// TestStringsAListenerChoseAreNeutralisedWhereTheyAreDecoded: a model id and a
// version come off the wire from whatever answered on a port. Terminal control
// bytes in them would be executed by whatever later prints them.
func TestStringsAListenerChoseAreNeutralisedWhereTheyAreDecoded(t *testing.T) {
	const esc = "\x1b"
	srv := ollamaFleet(t, "0.1"+esc+"]52;c;AAAA\a", map[string][]string{
		"evil" + esc + "[2Jmodel":  {"completion", "tools"},
		"two\nlines":               {"completion", "tools"},
		"org/model:7b-q4_K_M.gguf": {"completion", "tools"},
		`C:\models\llama`:          {"completion", "tools"},
	})
	servers := Scan(context.Background(), ScanOptions{
		Endpoints: endpointsOf(srv.URL), Capabilities: true, Timeout: 10 * time.Second,
	})
	if len(servers) != 1 {
		t.Fatalf("found %d servers, want 1", len(servers))
	}
	got := servers[0]
	if strings.ContainsAny(got.Describe(), "\x1b\a") || strings.ContainsAny(got.Version, "\x1b\a") {
		t.Errorf("the version reached a caller unneutralised: %q", got.Describe())
	}
	ids := map[string]bool{}
	for _, m := range got.Models {
		ids[m.ID] = true
		if strings.ContainsAny(m.ID, "\x1b\n\t\a") {
			t.Errorf("model id %q still carries a control byte", m.ID)
		}
	}
	for _, want := range []string{"org/model:7b-q4_K_M.gguf", `C:\models\llama`} {
		if !ids[want] {
			t.Errorf("a legitimate id %q was altered; got %v", want, ids)
		}
	}
	if len(ids) != 4 {
		t.Errorf("got %d ids, want the 4 models served: %v", len(ids), ids)
	}
	// A request the server answers is still built from a neutralised id: the
	// listing is the one source every later step reads.
	_ = httptest.NewRecorder
}
