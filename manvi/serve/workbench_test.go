package serve

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/testsupport"
	"github.com/bharathvbcr/Manvi/manvi/llm"
)

func TestEnhancementConfigurationIsNegotiatedWithoutOpeningStorageOrProviders(t *testing.T) {
	root := t.TempDir()
	client := store.New(filepath.Join(root, "missing-store"), filepath.Join(root, "profile.sqlite"))
	t.Cleanup(client.Close)
	providerCalls := 0
	runner, err := NewEnhancementRunner(client, func(context.Context, string, string) (llm.Provider, error) {
		providerCalls++
		return nil, errors.New("configuration must not resolve a provider")
	}, func(err error) string { return err.Error() })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runner.Close(); err != nil {
			t.Error(err)
		}
	})
	configuration := &EnhancementConfiguration{OK: true, Provider: "local", Model: "chosen-model", ModelSource: "llm.local.model", Providers: []string{"local", "anthropic"}}
	responses := roundTrip(t, Options{Modules: []Module{WorkbenchModule{Client: client, Runner: runner, Configuration: configuration}}},
		Request{ID: "hello", Op: OpHello},
		Request{ID: "configuration", Op: "work.enhancements.configuration"},
		Request{ID: "empty", Op: "work.enhancements.configuration", Params: json.RawMessage(`{}`)},
		Request{ID: "unknown", Op: "work.enhancements.configuration", Params: json.RawMessage(`{"provider":"other"}`)},
		Request{ID: "duplicate", Op: "work.enhancements.configuration", Params: json.RawMessage(`{"x":1,"\u0078":2}`)},
		Request{ID: "array", Op: "work.enhancements.configuration", Params: json.RawMessage(`[]`)},
	)
	if !responses[0].OK || !strings.Contains(string(responses[0].Result), `"work.enhancements.configuration"`) {
		t.Fatal("configuration was not negotiated")
	}
	for _, response := range responses[1:3] {
		var got EnhancementConfiguration
		if !response.OK || json.Unmarshal(response.Result, &got) != nil || got.Model != configuration.Model || got.ModelSource != configuration.ModelSource || len(got.Providers) != 2 {
			t.Fatalf("configuration result: %+v", response)
		}
	}
	for _, response := range responses[3:] {
		if response.OK || response.Error == nil || response.Error.Code != "invalid_input" {
			t.Fatalf("configuration accepted controls: %+v", response)
		}
	}
	if providerCalls != 0 {
		t.Fatal("configuration resolved a provider")
	}
	if _, err := os.Stat(client.DB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("configuration opened profile storage: %v", err)
	}
}

func TestWorkbenchHostProtocolReachesTheRealPersistentStore(t *testing.T) {
	client := store.New(testsupport.DCStore(t), filepath.Join(t.TempDir(), "profile.sqlite"))
	t.Cleanup(client.Close)
	options := Options{HardRules: true, Modules: []Module{WorkbenchModule{Client: client}}}
	responses := roundTrip(t, options,
		Request{ID: "hello", Op: OpHello},
		Request{ID: "repo", Op: "work.repositories.put", Params: json.RawMessage(`{"request_id":"r","id":"repo","expected_revision":0,"name":"Manvi","identity_key":"clone:r"}`)},
		Request{ID: "workspace", Op: "work.workspaces.put", Params: json.RawMessage(`{"request_id":"w","id":"ws","expected_revision":0,"name":"Devtools","repository_ids":["repo"]}`)},
		Request{ID: "task", Op: "work.items.put", Params: json.RawMessage(`{"request_id":"t","id":"task","expected_revision":0,"title":"Fix startup lag","repository_ids":["repo"],"primary_repository_id":"repo"}`)},
		Request{ID: "bad", Op: "work.items.put", Params: json.RawMessage(`{"request_id":"stale","id":"task","expected_revision":0,"title":"Overwrite","repository_ids":["repo"],"primary_repository_id":"repo"}`)},
		Request{ID: "board", Op: "work.items.list", Params: json.RawMessage(`{"workspace_id":"ws"}`)})
	for _, r := range responses {
		if r.ID == "bad" {
			if r.OK || r.Error == nil || r.Error.Code != "revision_conflict" {
				t.Fatalf("typed conflict lost: %+v", r)
			}
		} else if !r.OK {
			t.Fatalf("host request failed: %+v", r)
		}
	}
	for _, method := range store.WorkbenchMethods() {
		if !strings.Contains(string(responses[0].Result), `"work.`+method+`"`) {
			t.Fatalf("hello did not negotiate %s", method)
		}
	}
	var board struct {
		Total int `json:"total"`
		Shown int `json:"shown"`
	}
	if err := json.Unmarshal(responses[5].Result, &board); err != nil {
		t.Fatal(err)
	}
	if board.Total != 1 || board.Shown != 1 {
		t.Fatalf("board = %s", responses[5].Result)
	}
}

func TestWorkbenchRequiresAnExplicitHostModule(t *testing.T) {
	responses := roundTrip(t, Options{}, Request{ID: "hello", Op: OpHello}, Request{ID: "work", Op: "work.items.list"})
	if strings.Contains(string(responses[0].Result), `"work.items.list"`) || responses[1].OK {
		t.Fatal("default server implicitly enabled profile mutations")
	}
	if err := New(&strings.Builder{}, Options{Modules: []Module{WorkbenchModule{}}}).Serve(t.Context(), strings.NewReader("")); err == nil {
		t.Fatal("nil workbench client was accepted")
	}
}

func TestEnhancementLifecycleUsesTheHostBoundaryAndPreservesTaskEvidence(t *testing.T) {
	client := store.New(testsupport.DCStore(t), filepath.Join(t.TempDir(), "profile.sqlite"))
	t.Cleanup(client.Close)
	responses := roundTrip(t, Options{HardRules: true, Modules: []Module{WorkbenchModule{Client: client}}},
		Request{ID: "repo", Op: "work.repositories.put", Params: json.RawMessage(`{"request_id":"r","id":"r","expected_revision":0,"name":"Manvi","identity_key":"local:r"}`)},
		Request{ID: "task", Op: "work.items.put", Params: json.RawMessage(`{"request_id":"t","id":"t","expected_revision":0,"title":"fix bug","description":"Exact error E42","repository_ids":["r"],"primary_repository_id":"r"}`)},
		Request{ID: "prepare", Op: "work.enhancements.create", Params: json.RawMessage(`{"request_id":"create","id":"e","expected_revision":0,"task_id":"t","source_revision":1,"fields":["title"],"provider":"local","model":"test-model"}`)},
		Request{ID: "ready", Op: "work.enhancements.complete", Params: json.RawMessage(`{"request_id":"complete","id":"e","expected_revision":1,"title":"Resolve the E42 failure"}`)},
		Request{ID: "accept", Op: "work.enhancements.accept", Params: json.RawMessage(`{"request_id":"accept","id":"e","expected_revision":2,"expected_task_revision":1,"fields":["title"]}`)},
		Request{ID: "replay", Op: "work.enhancements.accept", Params: json.RawMessage(`{"request_id":"accept","id":"e","expected_revision":2,"expected_task_revision":1,"fields":["title"]}`)},
		Request{ID: "task-after", Op: "work.items.get", Params: json.RawMessage(`{"id":"t"}`)},
		Request{ID: "undo-stale", Op: "work.enhancements.undo", Params: json.RawMessage(`{"request_id":"undo-stale","id":"e","expected_revision":3,"expected_task_revision":1}`)},
		Request{ID: "undo", Op: "work.enhancements.undo", Params: json.RawMessage(`{"request_id":"undo","id":"e","expected_revision":3,"expected_task_revision":2}`)},
		Request{ID: "list", Op: "work.enhancements.list", Params: json.RawMessage(`{"task_id":"t","limit":1}`)},
	)
	for _, response := range responses {
		if response.ID == "undo-stale" {
			if response.OK || response.Error == nil || response.Error.Code != "revision_conflict" {
				t.Fatalf("lost stale-undo refusal: %+v", response)
			}
		} else if !response.OK {
			t.Fatalf("enhancement request failed: %+v", response)
		}
	}
	if string(responses[4].Result) != string(responses[5].Result) {
		t.Fatal("uncertain retry changed the receipt")
	}
	var task struct {
		Item struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Revision    int    `json:"revision"`
		} `json:"item"`
	}
	if err := json.Unmarshal(responses[6].Result, &task); err != nil {
		t.Fatal(err)
	}
	if task.Item.Title != "Resolve the E42 failure" || task.Item.Description != "Exact error E42" || task.Item.Revision != 2 {
		t.Fatalf("task evidence changed unexpectedly: %+v", task)
	}
	var listing struct {
		Total int `json:"total"`
		Items []struct {
			State string `json:"state"`
		} `json:"items"`
	}
	if err := json.Unmarshal(responses[9].Result, &listing); err != nil {
		t.Fatal(err)
	}
	if listing.Total != 1 || len(listing.Items) != 1 || listing.Items[0].State != "undone" {
		t.Fatalf("invalid enhancement listing: %+v", listing)
	}
}
