package serve

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/testsupport"
	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

// The op list cannot answer "which coding agents can this build actually
// drive?". `work.runs.managed.prepare` is registered whenever a ManagedRunner
// exists, so a codex-only build and a codex+claude build advertise the same
// operation. A host that cannot tell them apart offers the lane, stores an
// attempt, takes the repository's single run slot, and only then dies on a
// refusal phrased for whichever providers that build happened to ship — which
// is exactly how a stale harness came to report "requires a fresh Codex
// attempt" to someone launching Claude Code.
//
// So the adapter set travels in the handshake, and it is read from
// `codingagent.ManagedProviders` rather than restated, because a transcribed
// list is the drift this is here to stop.
func TestHelloPublishesTheManagedAdapterSetSoAHostNeedNotGuess(t *testing.T) {
	client := store.New(testsupport.DCStore(t), filepath.Join(t.TempDir(), "profile.sqlite"))
	t.Cleanup(client.Close)
	runner, err := NewManagedRunner(client, func(context.Context, codingagent.Options) (ManagedSession, error) {
		return nil, context.Canceled
	}, func(err error) string { return err.Error() })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Close() })

	resp := roundTrip(t, Options{Modules: []Module{WorkbenchModule{Client: client, Managed: runner}}},
		Request{ID: "hello", Op: OpHello})[0]
	if !resp.OK {
		t.Fatalf("hello failed: %+v", resp.Error)
	}
	var hello HelloResult
	if err := json.Unmarshal(resp.Result, &hello); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(hello.Ops, "work.runs.managed.prepare") {
		t.Fatal("a configured managed lane did not advertise its preparation op")
	}
	// Equal as sets to the deciding list, in both directions: a published
	// provider the runner would refuse is a lane a host is invited to take and
	// then denied, and an admitted provider left unpublished is an adapter no
	// host can reach.
	for _, provider := range hello.ManagedProviders {
		if !codingagent.IsManagedProvider(provider) {
			t.Errorf("hello published %q, which the runner does not admit", provider)
		}
	}
	for _, provider := range codingagent.ManagedProviders {
		if !slices.Contains(hello.ManagedProviders, provider) {
			t.Errorf("adapter %q exists but hello did not publish it", provider)
		}
	}
	if len(hello.ManagedProviders) != len(codingagent.ManagedProviders) {
		t.Errorf("ManagedProviders = %v, want the adapter set %v", hello.ManagedProviders, codingagent.ManagedProviders)
	}

	// The published slice must be a copy. Handing out the router's own backing
	// array would let one response's decoder alias the next one's answer.
	if len(hello.ManagedProviders) > 0 {
		hello.ManagedProviders[0] = "tampered"
		second := roundTrip(t, Options{Modules: []Module{WorkbenchModule{Client: client, Managed: runner}}},
			Request{ID: "hello", Op: OpHello})[0]
		var again HelloResult
		if err := json.Unmarshal(second.Result, &again); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(again.ManagedProviders, "tampered") {
			t.Error("hello handed out the router's own slice; a caller mutated the next answer")
		}
	}
}

// Absent is not empty. A build with no managed lane omits the key entirely,
// and a host reading it must be able to tell "this harness did not say" from
// "this harness said none" — the first is a check that could not run, and a
// check that could not run must never report what a passing one would.
func TestHelloOmitsTheAdapterSetEntirelyWhenNoManagedLaneExists(t *testing.T) {
	client := store.New(testsupport.DCStore(t), filepath.Join(t.TempDir(), "profile.sqlite"))
	t.Cleanup(client.Close)

	resp := roundTrip(t, Options{Modules: []Module{WorkbenchModule{Client: client}}},
		Request{ID: "hello", Op: OpHello})[0]
	if !resp.OK {
		t.Fatalf("hello failed: %+v", resp.Error)
	}
	// Decoded into a map, because a typed decode cannot tell an absent key
	// from a null or an empty list — and that distinction is the whole point.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(resp.Result, &raw); err != nil {
		t.Fatal(err)
	}
	if _, present := raw["managed_providers"]; present {
		t.Errorf("a build with no managed lane still published managed_providers: %s", raw["managed_providers"])
	}
	var hello HelloResult
	if err := json.Unmarshal(resp.Result, &hello); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(hello.Ops, "work.runs.managed.prepare") {
		t.Error("no managed runner was configured, yet the preparation op was advertised")
	}
}
