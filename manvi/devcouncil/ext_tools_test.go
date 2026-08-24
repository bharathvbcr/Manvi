package devcouncil

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"manvi/flags"
	"manvi/mcp"
	"manvi/policy"
	"manvi/tools"
)

func TestArtifactTools(t *testing.T) {
	f := newFixture(t)

	// An artifact is a record of work on a task, so the tools now require the
	// same lease every other mutation does. Under this fixture's strict posture
	// the refusal stands rather than being demoted; TestAnArtifactWriteIsJudged
	// below is what proves it.
	if out := f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"}); out["task_id"] != "TASK-001" {
		t.Fatalf("checkout = %v", out)
	}

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

// TestAnArtifactWriteIsJudgedAndSaysHowItWasJudged.
//
// The artifact tools wrote into `.devcouncil/artifacts/` with no lease, no gate
// consultation and nothing in the result to say so, while
// devcouncil_write_file refused the sibling path in the same run as a hard rule
// that "no override clears, by any authority". Two tools, one directory, two
// answers — and the ungated one left no record that anything had been skipped.
func TestAnArtifactWriteIsJudgedAndSaysHowItWasJudged(t *testing.T) {
	create := func(f *fixture, name string) tools.Result {
		return f.call("devcouncil_create_artifact", map[string]any{
			"name":     name,
			"content":  "# plan\n",
			"metadata": map[string]any{"summary": "a plan"},
		})
	}

	t.Run("strict refuses without a lease", func(t *testing.T) {
		f := newFixture(t)
		res := create(f, "unattributed.md")
		if !res.IsError || !res.Blocked {
			t.Fatalf("an artifact was written with no task checked out: %+v", res)
		}
		if res.Rule != string(policy.RuleNoTask) {
			t.Errorf("rule = %q, want the rule a repository write is refused by for the same reason", res.Rule)
		}
		if _, err := os.Stat(filepath.Join(f.root, ".devcouncil", "artifacts", "unattributed.md")); !os.IsNotExist(err) {
			t.Errorf("the refusal did not stop the write: %v", err)
		}
	})

	t.Run("a leased write is allowed and admits what did not run", func(t *testing.T) {
		f := newFixture(t)
		f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})
		res := create(f, "planned.md")
		if res.IsError {
			t.Fatalf("a leased artifact write was refused: %s", res.Text)
		}
		// Spelled out rather than taken from the constant: this string travels
		// into the run report, so the test has to pin the value and not just
		// agree with whatever the source currently says.
		if !slices.Contains(res.Degraded, "scope.artifact_store") {
			// No task plans a file under .devcouncil/artifacts, so the scope
			// rungs never ran on this path. An allow that does not say so is
			// indistinguishable from one the plan authorised.
			t.Errorf("degraded = %v, want %q", res.Degraded, "scope.artifact_store")
		}
	})

	t.Run("yolo demotes the refusal rather than dropping the rule", func(t *testing.T) {
		f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureYolo})
		res := create(f, "unattributed.md")
		if res.IsError {
			t.Fatalf("yolo refused an artifact write: %s", res.Text)
		}
		// The posture allowed it; the record must still name what would have
		// blocked it, exactly as a demoted repository write does.
		if res.Demoted == "" || !strings.Contains(res.Demoted, flags.PolicyFileMode) {
			t.Errorf("demoted = %q, want the setting that allowed this named", res.Demoted)
		}
		if res.Rule != string(policy.RuleNoTask) {
			t.Errorf("rule = %q, want the rule that would have blocked it kept", res.Rule)
		}
	})
}

// mcpProbeScript is a minimal stdio MCP server: it answers the handshake and
// every tools/call with one text block. A real subprocess over the real
// JSON-RPC framing, because the point of the assertion below is what a
// *successful* dispatch carries back, and a stubbed manager would prove only
// what the stub was told to return.
const mcpProbeScript = `
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  if [ -z "$id" ]; then continue; fi
  case "$line" in
    *tools/call*) printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"the probe server ran something"}]}}\n' "$id" ;;
    *) printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id" ;;
  esac
done
`

// TestAnMCPDispatchIsRecordedAsUnjudged.
//
// mcp_call_tool validated two strings and handed the call to the server: no
// gate, no lease, no rule, and nothing on the result to say so. An MCP server
// can expose a filesystem write or a shell, so a run that reached one through
// this tool was, in the report, indistinguishable from a run that had not —
// mcp.enabled is not a safety flag either, so nothing appeared in Weakened().
// The dispatch is not refused (see mcpDispatchUnjudged for why a lease would be
// the misleading answer rather than the strict one) but it can never again be
// silent.
func TestAnMCPDispatchIsRecordedAsUnjudged(t *testing.T) {
	unjudged := func(res tools.Result) bool {
		for _, note := range res.Degraded {
			if strings.HasPrefix(note, "mcp.dispatch.unjudged") {
				return true
			}
		}
		return false
	}

	t.Run("a dispatch that reached a server", func(t *testing.T) {
		f := newFixture(t)
		mgr := mcp.NewManager(f.root)
		if err := mgr.RegisterServer(mcp.ServerConfig{
			Name: "probe", Command: "/bin/sh", Args: []string{"-c", mcpProbeScript},
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(mgr.CloseAll)
		f.reg.deps.MCP = mgr

		res := f.call("mcp_call_tool", map[string]any{
			"server_name": "probe", "tool_name": "write_anything", "arguments": map[string]any{},
		})
		if res.IsError {
			t.Fatalf("the probe server refused: %s", res.Text)
		}
		if !strings.Contains(res.Text, "the probe server ran something") {
			t.Fatalf("the dispatch did not reach the server: %q", res.Text)
		}
		if !unjudged(res) {
			t.Errorf("degraded = %v; a dispatch nothing judged must say so", res.Degraded)
		}
		if !res.Qualified() {
			t.Error("an unjudged dispatch reported as a clean result")
		}
		// The note has to be routable: which server, which tool.
		for _, want := range []string{"probe", "write_anything"} {
			if !strings.Contains(strings.Join(res.Degraded, " "), want) {
				t.Errorf("degraded = %v, want it to name %q", res.Degraded, want)
			}
		}
	})

	t.Run("a dispatch that failed", func(t *testing.T) {
		// The call still crossed the boundary — the harness cannot know how far
		// it got — so the failure carries the same admission.
		f := newFixture(t)
		res := f.call("mcp_call_tool", map[string]any{
			"server_name": "absent", "tool_name": "t", "arguments": map[string]any{},
		})
		if !res.IsError {
			t.Fatalf("a call to an unconfigured server succeeded: %s", res.Text)
		}
		if !unjudged(res) {
			t.Errorf("degraded = %v; the attempt was still unjudged", res.Degraded)
		}
	})
}
