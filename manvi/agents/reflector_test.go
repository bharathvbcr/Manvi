package agents

import "testing"

// The reflector reads an evidence packet and answers in text. The command that
// runs it parses the answer and stores it, so the role itself never needs a
// write: a model-driven artifact write would bypass the parser that decides
// whether an answer may be stored at all.

func TestReflectorIsReadOnlyAndReachesNoWriteTool(t *testing.T) {
	def, ok := NewRegistry().Get("reflector")
	if !ok {
		t.Fatal("the reflector role is not registered")
	}
	if def.EnableWriteTools {
		t.Fatal("the reflector is a writing role")
	}
	if len(def.WriteExceptions) != 0 {
		t.Fatalf("the reflector carries write exceptions %v", def.WriteExceptions)
	}
	surface := def.Surface()
	for _, tool := range []string{
		"devcouncil_write_file", "devcouncil_patch_file", "devcouncil_delete_file",
		"devcouncil_exec_command", "devcouncil_spawn_subagents", "mcp_call_tool",
		"devcouncil_create_artifact", "devcouncil_update_artifact",
	} {
		if surface.PermitsWrite(tool) {
			t.Errorf("the reflector may use %s", tool)
		}
	}
	if surface.MCP {
		t.Error("the reflector admits the MCP group, which reaches outside this process")
	}
}

// A reflector that could read files could read the session store, and so the
// critic's verdict the evidence packet withholds from it.
func TestReflectorToolsAreNarrowedToTheArtifactIndex(t *testing.T) {
	def, _ := NewRegistry().Get("reflector")
	surface := def.Surface()
	if !surface.Constrains() || len(surface.Allowed) == 0 {
		t.Fatal("the reflector inherits every read tool")
	}
	for _, tool := range []string{"devcouncil_read_file", "devcouncil_grep", "devcouncil_find_files"} {
		if surface.Permits(tool, "") {
			t.Errorf("the reflector may use %s", tool)
		}
	}
}
