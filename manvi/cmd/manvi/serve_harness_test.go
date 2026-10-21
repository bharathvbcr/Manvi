package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/serve"
)

func harnessResolveLine(t *testing.T, provider, spec string) serve.Response {
	t.Helper()
	reg := specRegistry(t, nil)
	params, err := json.Marshal(map[string]any{
		"session_id": "tab", "provider": provider,
		"spec": map[string]any{"kind": "inline", "inline": json.RawMessage(spec)},
	})
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(serve.Request{ID: "1", Op: serve.OpHarnessResolve, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	srv := serve.New(&out, serve.Options{HardRules: true, Modules: []serve.Module{serve.HarnessModule{Resolver: serveHarnessResolver(reg)}}})
	if err := srv.Serve(context.Background(), strings.NewReader(string(line)+"\n")); err != nil {
		t.Fatal(err)
	}
	var resp serve.Response
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("undecodable response %q: %v", out.String(), err)
	}
	return resp
}

// The serve plane judges a host's spec against the section names of this
// process's own prompt assembler, the one source dh-1 keeps: an Essential
// section of the real prompt is refused as a spec section name, for every
// provider, and a free name resolves.
func TestServeHarnessRefusesEverySectionNameTheRealPromptOwns(t *testing.T) {
	reg := specRegistry(t, nil)
	owned := map[string]bool{}
	for _, provider := range []string{"anthropic", "local"} {
		for _, src := range promptSources(reg, PromptOptions{Provider: provider, TaskToolsOffered: true}) {
			owned[src.Name()] = true
		}
	}
	if len(owned) < 5 {
		t.Fatalf("the prompt reports only %d sections; the fixture is not reading the real assembler", len(owned))
	}
	reserved := serveHarnessResolver(reg).Reserved("anthropic")
	for name := range owned {
		if !slices.Contains(reserved, name) {
			t.Errorf("the prompt owns section %q and the serve plane would let a spec take it", name)
		}
		doc := fmt.Sprintf(`{"name":"host","sections":[{"name":%q,"order":70,"text":"t"}]}`, name)
		resp := harnessResolveLine(t, "anthropic", doc)
		if resp.OK || resp.Error.Code != serve.ErrHarnessRefused {
			t.Errorf("section %q was not refused with %s: %+v", name, serve.ErrHarnessRefused, resp)
		}
	}
	if resp := harnessResolveLine(t, "anthropic", `{"name":"host","sections":[{"name":"house-style","order":70,"text":"t"}]}`); !resp.OK {
		t.Fatalf("a free section name was refused: %+v", resp.Error)
	}
}

func TestServeHarnessBudgetIsThePromptsOwn(t *testing.T) {
	reg := specRegistry(t, nil)
	r := serveHarnessResolver(reg)
	for _, provider := range []string{"anthropic", "local"} {
		got := r.Budget(provider)
		want := promptTokenBudget(reg, provider)
		if got.Prompt != want || got.Skills != want/skillsBudgetShare {
			t.Errorf("%s: budget = %+v, want prompt %d and its skills share", provider, got, want)
		}
	}
}
