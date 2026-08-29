package agents

import (
	"testing"
)

// WriteExceptions is the one field on a role that widens what a child can do,
// so every test here is an attempt to widen it further than it says.

// The planner's own prompt tells it to draft plans under .devcouncil/artifacts/
// and the role is read-only, so before the exception existed that instruction
// described work the child could not perform. This is the fix, asserted at the
// role rather than at the runner.
func TestPlannerMayWriteTheArtifactsItsPromptDescribes(t *testing.T) {
	reg := NewRegistry()
	def, ok := reg.Get("planner")
	if !ok {
		t.Fatal("the planner role is not registered")
	}
	if def.EnableWriteTools {
		t.Fatal("the planner became a writing role; the exception was meant to stay narrow")
	}
	surface := def.Surface()
	for _, tool := range []string{"devcouncil_create_artifact", "devcouncil_update_artifact"} {
		if !surface.PermitsWrite(tool) {
			t.Errorf("the planner cannot use %s, which its own system prompt tells it to use", tool)
		}
	}
}

// The narrowness is the whole safety argument. A planner that could reach
// write_file or exec_command would be a builder with a different prompt.
func TestPlannerCannotReachRepositoryWritesOrCommands(t *testing.T) {
	reg := NewRegistry()
	def, _ := reg.Get("planner")
	surface := def.Surface()
	for _, tool := range []string{
		"devcouncil_write_file",
		"devcouncil_patch_file",
		"devcouncil_delete_file",
		"devcouncil_exec_command",
		"devcouncil_spawn_subagents",
		"mcp_call_tool",
	} {
		if surface.PermitsWrite(tool) {
			t.Errorf("the planner may use %s; the exception is supposed to admit artifact "+
				"writes and nothing else", tool)
		}
	}
}

// No other shipped role gains anything from this field. A widening that spread
// silently across the catalogue would be exactly the kind nobody reviews.
func TestNoOtherShippedRoleCarriesAWriteException(t *testing.T) {
	reg := NewRegistry()
	for _, name := range []string{"research", "builder", "critic", "stress_tester", "self"} {
		def, ok := reg.Get(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if len(def.WriteExceptions) != 0 {
			t.Errorf("role %q carries write exceptions %v", name, def.WriteExceptions)
		}
	}
}

// A surface nobody declared grants nothing. This is the untyped fan-out and the
// unknown-role fallback, and both must fail closed.
func TestAnUndeclaredSurfaceGrantsNoWrites(t *testing.T) {
	var zero ToolSurface
	if zero.PermitsWrite("devcouncil_create_artifact") {
		t.Fatal("an undeclared surface permitted a write")
	}
	// Even one that lists them: Declared is what says a role was consulted at
	// all, and a list on an undeclared surface is data nobody wrote down.
	undeclared := ToolSurface{Writes: []string{"devcouncil_write_file"}}
	if undeclared.PermitsWrite("devcouncil_write_file") {
		t.Fatal("an undeclared surface honoured a write list")
	}
}

// Matching is exact. A prefix or glob would be a permission whose scope changes
// every time a tool is added, which is the opposite of reviewable.
func TestWriteExceptionsMatchExactly(t *testing.T) {
	s := ToolSurface{Declared: true, Writes: []string{"devcouncil_create_artifact"}}
	for _, near := range []string{
		"devcouncil_create_artifact_v2",
		"devcouncil_create",
		"devcouncil_create_artifac",
		"DEVCOUNCIL_CREATE_ARTIFACT",
		"",
	} {
		if s.PermitsWrite(near) {
			t.Errorf("%q was matched against an exact allowlist entry", near)
		}
	}
	if !s.PermitsWrite("devcouncil_create_artifact") {
		t.Fatal("the exact name was not matched")
	}
}

// A surface carrying only write exceptions still constrains, or the runner
// would skip the narrowing pass and the exception would never be consulted.
func TestASurfaceWithOnlyWriteExceptionsStillConstrains(t *testing.T) {
	s := ToolSurface{Declared: true, MCP: true, Writes: []string{"devcouncil_create_artifact"}}
	if !s.Constrains() {
		t.Fatal("a surface with write exceptions reported that it changes nothing, so the " +
			"runner would never apply it")
	}
}
