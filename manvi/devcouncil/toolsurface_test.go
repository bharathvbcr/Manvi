package devcouncil

import (
	"encoding/json"
	"strings"
	"testing"

	"manvi/agents"
)

// A role's declared tool surface must reach the runner. Model and SystemPrompt
// already survive the dispatch; enable_mcp_tools and allowed_tools were decoded
// into a Definition nothing downstream ever read, so a role could be written
// with a tool policy that had no effect anywhere.
func TestInvokeSubagentCarriesTheRoleToolSurface(t *testing.T) {
	runner := &recordingRunner{}
	f, _ := fixtureWithRoles(t, runner, agents.Definition{
		Name:             "critic",
		Role:             "Critic",
		SystemPrompt:     "You are a critic.",
		EnableMCPTools:   false,
		EnableWriteTools: true,
		AllowedTools:     []string{"devcouncil_read_file", "devcouncil_grep"},
	})

	res := f.call("devcouncil_invoke_subagent", map[string]any{
		"subagents": []map[string]any{
			{"type_name": "critic", "role": "c-1", "prompt": "review it"},
		},
	})
	if res.IsError {
		t.Fatalf("invoke_subagent failed: %s", res.Text)
	}
	if len(runner.seen) != 1 {
		t.Fatalf("the runner was asked for %d children, want 1", len(runner.seen))
	}

	got := runner.seen[0].Surface
	if !got.Declared {
		t.Fatal("Surface.Declared = false; the role's tool policy did not survive dispatch")
	}
	if got.MCP {
		t.Error("Surface.MCP = true for a role with enable_mcp_tools:false")
	}
	if strings.Join(got.Allowed, ",") != "devcouncil_read_file,devcouncil_grep" {
		t.Errorf("Surface.Allowed = %v, want the role's allowlist", got.Allowed)
	}
}

// The untyped fan-out and an unknown type both inherit. A zero surface is the
// only encoding of "no role said anything", and reading the synthetic fallback
// as a declared surface would silently deny the MCP group to every child
// dispatched under a name that is not registered.
func TestUndeclaredSurfaceForUntypedAndUnknownDispatch(t *testing.T) {
	runner := &recordingRunner{}
	f, _ := fixtureWithRoles(t, runner)

	if res := f.call("devcouncil_invoke_subagent", map[string]any{
		"subagents": []map[string]any{
			{"type_name": "no_such_role", "role": "r", "prompt": "work"},
		},
	}); res.IsError {
		t.Fatalf("invoke_subagent failed: %s", res.Text)
	}
	if res := f.call("devcouncil_spawn_subagents", map[string]any{
		"tasks": []map[string]any{{"label": "untyped", "prompt": "work"}},
	}); res.IsError {
		t.Fatalf("spawn_subagents failed: %s", res.Text)
	}

	if len(runner.seen) != 2 {
		t.Fatalf("the runner was asked for %d children, want 2", len(runner.seen))
	}
	for _, req := range runner.seen {
		if req.Surface.Declared {
			t.Errorf("%s: Surface.Declared = true with no role behind it", req.Label)
		}
	}
}

// The role surface reaches the runner through spawn_subagents too, which is the
// tool the harness actually offers for a fan-out.
func TestSpawnSubagentsCarriesTheRoleToolSurface(t *testing.T) {
	runner := &recordingRunner{}
	f, _ := fixtureWithRoles(t, runner, agents.Definition{
		Name:             "worker",
		Role:             "Worker",
		EnableMCPTools:   true,
		EnableWriteTools: true,
		AllowedTools:     []string{"devcouncil_read_file"},
	})

	if res := f.call("devcouncil_spawn_subagents", map[string]any{
		"tasks": []map[string]any{{"label": "typed", "prompt": "do it", "type": "worker"}},
	}); res.IsError {
		t.Fatalf("spawn_subagents failed: %s", res.Text)
	}
	if len(runner.seen) != 1 {
		t.Fatalf("the runner was asked for %d children, want 1", len(runner.seen))
	}
	got := runner.seen[0].Surface
	if !got.Declared || !got.MCP || len(got.Allowed) != 1 {
		t.Fatalf("Surface = %+v, want the role's declared surface", got)
	}
}

// Nothing may advertise a workspace mode this harness does not implement.
//
// Definition.Workspace was documented as "inherit" | "branch" | "scratch", and
// both subagent schemas offered `workspace` to the model. No git-worktree or
// scratch-directory isolation exists anywhere in this harness, so a role asking
// for "branch" got the parent's working tree and was told nothing. An
// unimplemented isolation mode is the most dangerous kind of silent default:
// the caller believes the child's writes are contained.
func TestNoSchemaAdvertisesWorkspaceIsolation(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"devcouncil_define_subagent", "devcouncil_invoke_subagent"} {
		raw := schemaFor(t, f, name)
		if strings.Contains(raw, "workspace") {
			t.Errorf("%s still advertises a workspace mode: %s", name, raw)
		}
	}
}

// A child's tool registry never holds the dispatch tools, so a role toggling
// enable_subagent_tools could never be honoured — the field was a permission
// the harness structurally refuses to grant. It is gone, and nothing advertises
// it.
func TestNoSchemaAdvertisesSubagentToolPermission(t *testing.T) {
	f := newFixture(t)
	raw := schemaFor(t, f, "devcouncil_define_subagent")
	if strings.Contains(raw, "enable_subagent_tools") {
		t.Errorf("define_subagent still advertises enable_subagent_tools: %s", raw)
	}
}

// Removing a schema property is only half the fix. json.Unmarshal drops an
// unknown key without a word, so a model that keeps sending `workspace` — from
// an older transcript, or from its own priors about what a subagent definition
// looks like — would be silently ignored, which is the exact defect the removal
// was for.
func TestDefineSubagentRefusesAnUnsupportedWorkspace(t *testing.T) {
	f := newFixture(t)
	res := f.call("devcouncil_define_subagent", map[string]any{
		"name": "isolated", "description": "d", "system_prompt": "p",
		"workspace": "branch",
	})
	if !res.IsError {
		t.Fatalf("a role asking for workspace isolation was accepted: %s", res.Text)
	}
	if !strings.Contains(res.Text, "workspace") || !strings.Contains(res.Text, "branch") {
		t.Errorf("the refusal does not name what is unsupported: %s", res.Text)
	}
}

func TestDefineSubagentRefusesSubagentToolPermission(t *testing.T) {
	f := newFixture(t)
	res := f.call("devcouncil_define_subagent", map[string]any{
		"name": "recursive", "description": "d", "system_prompt": "p",
		"enable_subagent_tools": true,
	})
	if !res.IsError {
		t.Fatalf("a role granting itself sub-agent dispatch was accepted: %s", res.Text)
	}
	if !strings.Contains(res.Text, "enable_subagent_tools") {
		t.Errorf("the refusal does not name the field: %s", res.Text)
	}
}

func TestInvokeSubagentRefusesAnUnsupportedWorkspace(t *testing.T) {
	runner := &recordingRunner{}
	f, _ := fixtureWithRoles(t, runner)
	res := f.call("devcouncil_invoke_subagent", map[string]any{
		"subagents": []map[string]any{
			{"type_name": "self", "role": "r", "prompt": "work", "workspace": "scratch"},
		},
	})
	if !res.IsError {
		t.Fatalf("a call asking for workspace isolation was accepted: %s", res.Text)
	}
	if !strings.Contains(res.Text, "workspace") || !strings.Contains(res.Text, "scratch") {
		t.Errorf("the refusal does not name what is unsupported: %s", res.Text)
	}
	if len(runner.seen) != 0 {
		t.Errorf("children were dispatched anyway: %d", len(runner.seen))
	}
}

// schemaFor returns one registered tool's input schema as the model would see it.
func schemaFor(t *testing.T, f *fixture, name string) string {
	t.Helper()
	for _, s := range f.pipe.Schemas() {
		if s.Name == name {
			raw, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			return string(raw)
		}
	}
	t.Fatalf("%s is not registered", name)
	return ""
}
