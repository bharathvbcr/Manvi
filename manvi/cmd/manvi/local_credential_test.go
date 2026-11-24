package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/credentials"
	"github.com/bharathvbcr/Manvi/manvi/llm/local"
)

// TestShowLocalNeutralisesAModelIdFromTheListener: the id is whatever the
// scanned server said it was, and showLocal prints it to a terminal.
func TestShowLocalNeutralisesAModelIdFromTheListener(t *testing.T) {
	srv := fakeOllama(t, map[string][]string{
		"evil\x1b]52;c;AAAA\amodel": {"completion", "tools"},
		"plain:7b":                  {"completion", "tools"},
	})
	var out bytes.Buffer
	if err := showLocal(context.Background(), &out, pinnedTo(t, srv.URL+"/v1"), nil); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.ContainsAny(got, "\x1b\a") {
		t.Fatalf("showLocal wrote a terminal control byte a listener supplied:\n%q", got)
	}
	if !strings.Contains(got, "plain:7b") {
		t.Errorf("a legitimate model id is missing from the output:\n%s", got)
	}
}

func borrowedOpenAIKey() (credentials.Secret, error) {
	return credentials.NewSecret("sk-not-a-real-key", "OPENAI_API_KEY"), nil
}

// TestAnAdoptedServerIsNotSentABorrowedKey covers the run's adapter, the half
// the scan cannot see: adopting a listener must not also hand it the key.
func TestAnAdoptedServerIsNotSentABorrowedKey(t *testing.T) {
	const addr = "http://127.0.0.1:11434/v1"
	t.Run("a server that needs no key gets none, and the operator is told", func(t *testing.T) {
		var notes bytes.Buffer
		got := unnamedEndpointCredential(local.Resolution{Found: &local.Server{BaseURL: addr}}, borrowedOpenAIKey, &notes)
		if got != nil {
			t.Fatal("an adopted server was given a credential resolver")
		}
		for _, want := range []string{"OPENAI_API_KEY", addr, "manvi use local --base-url " + addr} {
			if !strings.Contains(notes.String(), want) {
				t.Errorf("note %q lacks %q", notes.String(), want)
			}
		}
		if strings.Contains(notes.String(), "sk-not-a-real-key") {
			t.Error("the note quoted the key")
		}
	})
	t.Run("a server that wants a key refuses with the command that fixes it", func(t *testing.T) {
		got := unnamedEndpointCredential(local.Resolution{Found: &local.Server{BaseURL: addr, AuthRequired: true}}, borrowedOpenAIKey, &bytes.Buffer{})
		if got == nil {
			t.Fatal("no resolver: the turn would fail with an unexplained 401")
		}
		_, err := got()
		if err == nil || !strings.Contains(err.Error(), "manvi use local --base-url "+addr) {
			t.Fatalf("err = %v, want the refusal to give the confirming command", err)
		}
		if strings.Contains(err.Error(), "sk-not-a-real-key") {
			t.Error("the refusal quoted the key")
		}
	})
	t.Run("nothing adopted or no key configured says nothing", func(t *testing.T) {
		var notes bytes.Buffer
		if got := unnamedEndpointCredential(local.Resolution{}, borrowedOpenAIKey, &notes); got != nil || notes.Len() != 0 {
			t.Errorf("nothing adopted: resolver %v, note %q", got != nil, notes.String())
		}
		none := func() (credentials.Secret, error) { return credentials.Secret{}, &credentials.ErrMissing{} }
		if got := unnamedEndpointCredential(local.Resolution{Found: &local.Server{BaseURL: addr, AuthRequired: true}}, none, &notes); got != nil || notes.Len() != 0 {
			t.Errorf("no key configured: resolver %v, note %q", got != nil, notes.String())
		}
		failing := func() (credentials.Secret, error) { return credentials.Secret{}, errors.New("keychain is locked") }
		if got := unnamedEndpointCredential(local.Resolution{Found: &local.Server{BaseURL: addr}}, failing, &notes); got != nil {
			t.Error("a failing resolver produced a credential resolver for an address nobody named")
		}
	})
}
