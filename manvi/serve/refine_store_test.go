package serve

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/Manvi/manvi/llm"
)

const refineStoreAnswer = `{"acceptance_criteria":["E42 no longer fails EnhancementRunner"],
	"split_into":[` + `{"title":"Diagnose E42","description":"Part of E42. Open src/serve/refine.go. Keep \"invalid value\" and ` + "`EnhancementRunner`" + `."},` +
	`{"title":"Fix E42","description":"Part of E42. Open src/serve/refine.go. Keep \"invalid value\" and ` + "`EnhancementRunner`" + `."}],
	"duplicate_of":[{"id":"d","reason":"same E42 failure"}],"rationale":"grounded"}`

// resultID reads the stored proposal id from the JSON a host sees.
func resultID(t *testing.T, result *RefineResult) string {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		ID string `json:"enhancement_id"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil || wire.ID == "" {
		t.Fatalf("result carries no stored proposal id: %s", raw)
	}
	return wire.ID
}

func storedProposal(t *testing.T, env *refineEnv, id string) map[string]any {
	t.Helper()
	var got struct {
		Item map[string]any `json:"item"`
	}
	if err := json.Unmarshal(enhancementCall(t, env.client, "enhancements.get", `{"id":"`+id+`"}`), &got); err != nil {
		t.Fatal(err)
	}
	return got.Item
}

func TestRefineStoresItsProposalAndAcceptApplies(t *testing.T) {
	env := refineFixture(t, &refineDevmap{status: builtIndex(), known: map[string]bool{"EnhancementRunner": true}})
	env.answer(refineStoreAnswer)
	got := env.mustRefine(t, "store")
	id := resultID(t, got)
	item := storedProposal(t, env, id)
	if item["kind"] != "refine" || item["state"] != "ready" || item["task_id"] != "t" {
		t.Fatalf("stored proposal = %v", item)
	}
	source, _ := item["source"].(map[string]any)
	if source["description"] != refineDescription {
		t.Fatalf("the full source snapshot was not stored: %v", source)
	}
	proposed, _ := item["proposed"].(map[string]any)
	if n := len(proposed["split_into"].([]any)); n != 2 {
		t.Fatalf("proposed = %v", proposed)
	}
	if ground, _ := item["grounding"].(map[string]any); ground["status"] != "grounded" {
		t.Fatalf("grounding was not stored with the proposal: %v", item["grounding"])
	}
	// Nothing changed the board yet.
	if task := enhancementCall(t, env.client, "items.get", `{"id":"t"}`); !strings.Contains(string(task), `"revision":2`) {
		t.Fatalf("refine modified the task: %s", task)
	}
	// The ordinary accept op applies it: criteria, a duplicate link and new tasks.
	module := WorkbenchModule{Client: env.client, Runner: env.runner, Configuration: &refineConfig, Devmap: env.devmap}
	accept := `{"id":"` + id + `","request_id":"acc","expected_revision":2,"expected_task_revision":2,"fields":["acceptance_criteria","split_into","duplicate_of"],"split_ids":["sa","sb"]}`
	responses := roundTrip(t, Options{Modules: []Module{module}}, Request{ID: "a", Op: "work.enhancements.accept", Params: json.RawMessage(accept)})
	if !responses[0].OK {
		t.Fatalf("accept over the host boundary: %+v", responses[0])
	}
	task := string(enhancementCall(t, env.client, "items.get", `{"id":"t"}`))
	for _, want := range []string{`"revision":3`, "E42 no longer fails EnhancementRunner", `"duplicate_of"`} {
		if !strings.Contains(task, want) {
			t.Fatalf("accepted refinement missing %s: %s", want, task)
		}
	}
	if sb := string(enhancementCall(t, env.client, "items.get", `{"id":"sb"}`)); !strings.Contains(sb, `"title":"Fix E42"`) || !strings.Contains(sb, `"kind":"parent"`) {
		t.Fatalf("split task = %s", sb)
	}
}

func TestRefineRejectLeavesTheTaskAndReleasesNothingElse(t *testing.T) {
	env := refineFixture(t, &refineDevmap{status: builtIndex()})
	env.answer(refineStoreAnswer)
	id := resultID(t, env.mustRefine(t, "reject"))
	enhancementCall(t, env.client, "enhancements.dismiss", `{"request_id":"rej","id":"`+id+`","expected_revision":2}`)
	if storedProposal(t, env, id)["state"] != "dismissed" {
		t.Fatal("dismiss did not reject the refinement")
	}
	if task := enhancementCall(t, env.client, "items.get", `{"id":"t"}`); !strings.Contains(string(task), `"revision":2`) {
		t.Fatalf("reject changed the task: %s", task)
	}
}

func TestRefineIsRefusedBeforeTheModelWhenAnotherGenerationHoldsTheSlot(t *testing.T) {
	env := refineFixture(t, &refineDevmap{status: builtIndex()})
	enhancementCall(t, env.client, "enhancements.create", `{"request_id":"hold","id":"hold","expected_revision":0,"task_id":"t","source_revision":2,"fields":["title"],"provider":"local","model":"test-model"}`)
	var refused *store.WorkbenchError
	if _, err := env.refine(t.Context(), "busy", 2); !errors.As(err, &refused) || refused.Code != "busy" {
		t.Fatalf("refine while another generation is pending = %v", err)
	}
	if env.calls.Load() != 0 {
		t.Fatalf("a refused refinement still spent %d model calls", env.calls.Load())
	}
}

func TestRefineRecordsAFailureAndReleasesTheSlot(t *testing.T) {
	env := refineFixture(t, &refineDevmap{status: builtIndex()})
	env.answer(`not json`)
	if _, err := env.refine(t.Context(), "bad", 2); err == nil {
		t.Fatal("malformed model output was accepted")
	}
	list := string(enhancementCall(t, env.client, "enhancements.list", `{"task_id":"t","states":["failed"]}`))
	if !strings.Contains(list, `"kind":"refine"`) {
		t.Fatalf("the failed refinement was not recorded: %s", list)
	}
	env.answer(refineStoreAnswer)
	if _, err := env.refine(t.Context(), "retry", 2); err != nil {
		t.Fatalf("the slot was not released by the failure: %v", err)
	}
}

func TestRefineCancellationStillReleasesTheSlot(t *testing.T) {
	env := refineFixture(t, &refineDevmap{status: builtIndex()})
	ctx, cancel := context.WithCancel(t.Context())
	env.reply.Store(func(llm.Request) string {
		cancel()
		return refineStoreAnswer
	})
	_, _ = env.refine(ctx, "cancel", 2)
	env.answer(refineStoreAnswer)
	if _, err := env.refine(t.Context(), "after", 2); err != nil {
		t.Fatalf("a cancelled refinement left the slot held: %v", err)
	}
}

func TestRefineReplayReturnsTheSameStoredProposalWithoutAnotherOne(t *testing.T) {
	env := refineFixture(t, &refineDevmap{status: builtIndex()})
	env.answer(refineStoreAnswer)
	first := resultID(t, env.mustRefine(t, "same"))
	if again := resultID(t, env.mustRefine(t, "same")); again != first {
		t.Fatalf("replay stored a second proposal: %s then %s", first, again)
	}
	list := string(enhancementCall(t, env.client, "enhancements.list", `{"task_id":"t","states":["ready"]}`))
	if strings.Count(list, `"kind":"refine"`) != 1 {
		t.Fatalf("expected one stored refinement: %s", list)
	}
}

func TestGenerateRefusesARefinementProposal(t *testing.T) {
	env := refineFixture(t, &refineDevmap{status: builtIndex()})
	enhancementCall(t, env.client, "enhancements.create", `{"request_id":"gen","id":"gen","expected_revision":0,"task_id":"t","source_revision":2,"kind":"refine","fields":["split_into"],"provider":"local","model":"test-model"}`)
	if _, err := env.runner.Generate(t.Context(), json.RawMessage(`{"id":"gen","request_id":"g","expected_revision":1}`)); err == nil || !strings.Contains(err.Error(), "refinement") {
		t.Fatalf("Generate on a refinement proposal = %v", err)
	}
	if env.calls.Load() != 0 {
		t.Fatal("a refinement proposal reached the text generator")
	}
}
