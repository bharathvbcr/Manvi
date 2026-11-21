package tools

import (
	"context"
	"strings"
	"testing"
)

// The script tool names exactly the lookups it is given, sorted and without
// repeats, and its registered handler only refuses: the agent loop runs it.
func TestScriptToolDescriptionNamesTheCallableToolsAndHandlerRefuses(t *testing.T) {
	tool := ScriptTool("devcouncil_grep", "devcouncil_find_files", "devcouncil_grep")
	if tool.Schema.Name != ScriptToolName || !tool.ReadOnly {
		t.Fatalf("registration = %q read-only=%v", tool.Schema.Name, tool.ReadOnly)
	}
	if !strings.Contains(tool.Schema.Description, "devcouncil_find_files, devcouncil_grep. ") {
		t.Errorf("description does not list the callable tools once each, sorted: %s", tool.Schema.Description)
	}
	if tool.Batchable {
		t.Error("the script tool must not be Batchable: it cannot run inside a batch or a script")
	}
	res := tool.Handler(context.Background(), Call{Name: ScriptToolName})
	if !res.IsError || !strings.Contains(res.Text, "nothing ran") {
		t.Errorf("a handler reached without the loop must refuse, got %+v", res)
	}
}
