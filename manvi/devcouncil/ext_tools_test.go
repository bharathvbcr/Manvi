package devcouncil

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"manvi/flags"
	"manvi/mcp"
)

func TestArtifactTools(t *testing.T) {
	f := newFixture(t)

	// 1. Create artifact
	createRes := f.call("devcouncil_create_artifact", map[string]any{
		"name":    "design_spec.md",
		"content": "# Architecture Spec\nMicroservices layout",
		"metadata": map[string]any{
			"summary":          "Core system design specification",
			"user_facing":      true,
			"request_feedback": true,
		},
	})
	if createRes.IsError {
		t.Fatalf("create artifact failed: %s", createRes.Text)
	}
	if !strings.Contains(createRes.Text, "created") {
		t.Errorf("unexpected create result: %s", createRes.Text)
	}

	// 2. Update artifact
	updateRes := f.call("devcouncil_update_artifact", map[string]any{
		"name":    "design_spec.md",
		"content": "# Architecture Spec v2\nMicroservices and event streams",
	})
	if updateRes.IsError {
		t.Fatalf("update artifact failed: %s", updateRes.Text)
	}
	if !strings.Contains(updateRes.Text, "updated") {
		t.Errorf("unexpected update result: %s", updateRes.Text)
	}

	// 3. List artifacts
	listRes := f.call("devcouncil_list_artifacts", map[string]any{})
	if listRes.IsError {
		t.Fatalf("list artifacts failed: %s", listRes.Text)
	}
	if !strings.Contains(listRes.Text, "design_spec.md") {
		t.Errorf("missing artifact in list result: %s", listRes.Text)
	}
}

func TestDynamicSubagentTools(t *testing.T) {
	// A runner, because this test invokes a sub-agent and then asserts the
	// invocation did not error. Without one the child cannot run at all, and
	// the assertion only held while a wholly failed dispatch still reported
	// success — which is the defect the failure branch below now catches.
	f := newFixtureRunner(t, &recordingRunner{})

	// 1. Define subagent
	defRes := f.call("devcouncil_define_subagent", map[string]any{
		"name":          "frontend_designer",
		"role":          "Frontend Designer",
		"description":   "Designs UI mockups and CSS styling",
		"system_prompt": "You design pixel-perfect vanilla CSS components.",
	})
	if defRes.IsError {
		t.Fatalf("define subagent failed: %s", defRes.Text)
	}
	if !strings.Contains(defRes.Text, "frontend_designer") {
		t.Errorf("unexpected define result: %s", defRes.Text)
	}

	// 2. Invoke subagent
	invokeRes := f.call("devcouncil_invoke_subagent", map[string]any{
		"subagents": []map[string]any{
			{
				"type_name": "frontend_designer",
				"role":      "UI Designer",
				"prompt":    "Create index.css with dark and light themes",
			},
		},
	})
	if invokeRes.IsError {
		t.Fatalf("invoke subagent failed: %s", invokeRes.Text)
	}
	if !strings.Contains(invokeRes.Text, "dispatched") {
		t.Errorf("unexpected invoke result: %s", invokeRes.Text)
	}
	if !strings.Contains(invokeRes.Text, `"clean":true`) {
		t.Errorf("a dispatch whose child ran must report clean: %s", invokeRes.Text)
	}

	// 3. Manage subagents (list)
	listRes := f.call("devcouncil_manage_subagents", map[string]any{
		"action": "list",
	})
	if listRes.IsError {
		t.Fatalf("manage subagents list failed: %s", listRes.Text)
	}
	if !strings.Contains(listRes.Text, "count") {
		t.Errorf("unexpected manage result: %s", listRes.Text)
	}
}

type testQuestionAsker struct {
	answered bool
}

func (q *testQuestionAsker) AskQuestions(ctx context.Context, questions []Question) ([]QuestionAnswer, error) {
	q.answered = true
	var out []QuestionAnswer
	for _, item := range questions {
		out = append(out, QuestionAnswer{
			Question: item.Question,
			Selected: []string{item.Options[0]},
		})
	}
	return out, nil
}

func TestAskQuestionTool(t *testing.T) {
	// 1. Unattended / YOLO mode auto-resolution
	fYolo := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureYolo})

	res := fYolo.call("devcouncil_ask_question", map[string]any{
		"questions": []map[string]any{
			{
				"question": "Which database backend should we configure?",
				"options":  []string{"(Recommended) SQLite WAL mode", "PostgreSQL 16"},
			},
		},
	})
	if res.IsError {
		t.Fatalf("ask question failed: %s", res.Text)
	}
	if !strings.Contains(res.Text, "(Recommended) SQLite WAL mode") {
		t.Errorf("expected recommended option selected, got %s", res.Text)
	}

	// 2. Interactive question asker attached
	asker := &testQuestionAsker{}
	f2 := newFixture(t)
	f2.reg.deps.QuestionAsker = asker

	res2 := f2.call("devcouncil_ask_question", map[string]any{
		"questions": []map[string]any{
			{
				"question": "Select CSS methodology",
				"options":  []string{"Vanilla CSS Tokens", "TailwindCSS"},
			},
		},
	})
	if res2.IsError {
		t.Fatalf("ask question interactive failed: %s", res2.Text)
	}
	if !asker.answered {
		t.Errorf("expected QuestionAsker to have been called")
	}
}

func TestMCPTools(t *testing.T) {
	f := newFixture(t)
	tmpDir := t.TempDir()
	mgr := mcp.NewManager(tmpDir)

	// Register Open Plugin with static tool
	manifest := &mcp.PluginManifest{
		SchemaVersion: "1.0",
		Name:          "demo-plugin",
		Version:       "1.0.0",
		Description:   "Demo plugin for tests",
		Runtime: mcp.PluginRuntime{
			Command: "echo",
		},
		Tools: []mcp.Tool{
			{
				Name:        "echo_tool",
				Description: "Echo back",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			},
		},
	}
	_ = mgr.RegisterPlugin(manifest)
	f.reg.deps.MCP = mgr

	res := f.call("mcp_list_tools", map[string]any{})
	if res.IsError {
		t.Fatalf("mcp_list_tools failed: %s", res.Text)
	}
	if !strings.Contains(res.Text, "demo-plugin") || !strings.Contains(res.Text, "echo_tool") {
		t.Errorf("unexpected mcp_list_tools result: %s", res.Text)
	}
}

func TestDynamicToolSearchAndActivationNative(t *testing.T) {
	f := newFixture(t)
	f.pipe.EnableDynamic()

	// 1. Search for subagent tools
	searchRes := f.call("devcouncil_search_tools", map[string]any{
		"query": "subagent",
	})
	if searchRes.IsError {
		t.Fatalf("search tools failed: %s", searchRes.Text)
	}
	if !strings.Contains(searchRes.Text, "devcouncil_define_subagent") || !strings.Contains(searchRes.Text, "devcouncil_invoke_subagent") {
		t.Errorf("expected subagent tools in search result: %s", searchRes.Text)
	}

	// 2. Activate subagent tools group
	actRes := f.call("devcouncil_activate_tools", map[string]any{
		"tools": []string{"subagent"},
	})
	if actRes.IsError {
		t.Fatalf("activate tools failed: %s", actRes.Text)
	}
	if !strings.Contains(actRes.Text, "devcouncil_define_subagent") {
		t.Errorf("expected subagent tools activated: %s", actRes.Text)
	}

	// 3. Verify active schemas include subagent tools
	var activeNames []string
	for _, s := range f.pipe.ActiveSchemas() {
		activeNames = append(activeNames, s.Name)
	}
	found := false
	for _, name := range activeNames {
		if name == "devcouncil_define_subagent" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected devcouncil_define_subagent in active schemas, got: %v", activeNames)
	}
}
