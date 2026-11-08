package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// devcouncil_search_tools and devcouncil_activate_tools are tool values whose
// handlers were built over the registry they were registered on. A child's
// registry is a Subset, which copies the values and so the closures: before the
// handlers asked tools.Dispatcher which registry was running them, a child's
// activation loaded tools onto the parent's registry and its search listed the
// parent's tools. These tests call through a child registry cut the way the
// sub-agent floor cuts it.

func childRegistryOf(parent *tools.Registry) *tools.Registry {
	return parent.Subset(func(t tools.Tool) bool { return !devcouncil.IsWorkTool(t.Schema.Name) })
}

func runOn(t *testing.T, reg *tools.Registry, name, args string) tools.Result {
	t.Helper()
	return reg.Run(context.Background(), tools.Call{ID: "c1", Name: name, Arguments: json.RawMessage(args)})
}

func TestAChildsActivationActsOnTheChildsRegistryOnly(t *testing.T) {
	_, parent, err := agentTools(newTestRegistry(t), nil, "0123456789abcdef")
	if err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}
	parent.SetDynamicMode(true)
	parentBefore := activeNames(parent)
	child := childRegistryOf(parent)

	// A tool the child cannot hold is unknown to it, not "loaded".
	res := runOn(t, child, "devcouncil_activate_tools", `{"tools":["work_create_item"]}`)
	if !res.IsError || strings.Contains(res.Text, "successfully loaded") {
		t.Errorf("a child activating a board tool was told it succeeded: %q", res.Text)
	}
	if got := workToolsIn(activeNames(child)); len(got) != 0 {
		t.Errorf("the child holds %v after activating them", got)
	}

	// A tool both registries hold activates on the child and leaves the parent.
	const shared = "devcouncil_get_task"
	if parentBefore[shared] || !parent.Has(shared) || !child.Has(shared) {
		t.Fatalf("%s is not an Extended tool both registries hold; the test proves nothing", shared)
	}
	res = runOn(t, child, "devcouncil_activate_tools", `{"tools":["`+shared+`"]}`)
	if res.IsError {
		t.Fatalf("activating %s in the child failed: %s", shared, res.Text)
	}
	if !activeNames(child)[shared] {
		t.Errorf("the child's own activation did not reach the child's registry")
	}
	parentAfter := activeNames(parent)
	if len(parentAfter) != len(parentBefore) || parentAfter[shared] {
		t.Errorf("a child's activation changed the parent's offered set: %d -> %d tools, %s active=%v",
			len(parentBefore), len(parentAfter), shared, parentAfter[shared])
	}
}

func TestAChildsSearchListsOnlyWhatTheChildCanHold(t *testing.T) {
	_, parent, err := agentTools(newTestRegistry(t), nil, "0123456789abcdef")
	if err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}
	child := childRegistryOf(parent)

	// Control: the parent's search does find the board, or the child's empty
	// answer below says nothing.
	if res := runOn(t, parent, "devcouncil_search_tools", `{"query":"work_create_item"}`); !strings.Contains(res.Text, "work_create_item") {
		t.Fatalf("the parent's search does not list work_create_item: %q", res.Text)
	}
	for _, query := range []string{"work_create_item", "work_", "board", "task"} {
		res := runOn(t, child, "devcouncil_search_tools", `{"query":"`+query+`"}`)
		if res.IsError {
			t.Fatalf("child search %q: %s", query, res.Text)
		}
		var found struct {
			Results []struct {
				Name string `json:"name"`
			} `json:"results"`
		}
		if err := json.Unmarshal([]byte(res.Text), &found); err != nil {
			t.Fatalf("decoding child search %q: %v (%s)", query, err, res.Text)
		}
		for _, r := range found.Results {
			if !child.Has(r.Name) {
				t.Errorf("child search %q listed %s, which the child cannot hold", query, r.Name)
			}
		}
	}
}

// Through the real sub-agent path: the child asks to activate a board tool and
// the answer it reads back is a refusal, with the parent's set untouched.
func TestARunningChildCannotActivateABoardToolOntoItsParent(t *testing.T) {
	_, pipeline, err := agentTools(newTestRegistry(t), nil, "0123456789abcdef")
	if err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}
	pipeline.SetDynamicMode(true)
	before := activeNames(pipeline)
	p := &scriptedProvider{
		name: "p", serves: []string{"m"},
		script: []llm.Response{
			wantToolCall("devcouncil_activate_tools", `{"tools":["work_create_item"]}`),
			endTurn("done"),
		},
	}
	r := runnerOver(t, pipeline, p)
	if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{Label: "c", Prompt: "go"}); err != nil {
		t.Fatal(err)
	}
	if got := toolResultText(t, p); strings.Contains(got, "successfully loaded") {
		t.Errorf("the child was told a board tool loaded: %q", got)
	}
	if after := activeNames(pipeline); len(after) != len(before) {
		t.Errorf("the parent's offered set moved from %d to %d tools", len(before), len(after))
	}
}
