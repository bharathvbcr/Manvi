package dc

import (
	"encoding/json"
	"testing"
)

func TestPlannedFileUnmarshalBareString(t *testing.T) {
	input := `"src/app.go"`
	var pf PlannedFile
	if err := json.Unmarshal([]byte(input), &pf); err != nil {
		t.Fatalf("unmarshal bare string: %v", err)
	}
	if pf.Path != "src/app.go" {
		t.Errorf("Path = %q, want src/app.go", pf.Path)
	}
	if pf.AllowedChange != ChangeModify {
		t.Errorf("AllowedChange = %q, want %q", pf.AllowedChange, ChangeModify)
	}
}

func TestPlannedFileUnmarshalObjectWithDefault(t *testing.T) {
	input := `{"path": "src/new.go"}`
	var pf PlannedFile
	if err := json.Unmarshal([]byte(input), &pf); err != nil {
		t.Fatalf("unmarshal object without allowed_change: %v", err)
	}
	if pf.Path != "src/new.go" {
		t.Errorf("Path = %q, want src/new.go", pf.Path)
	}
	if pf.AllowedChange != ChangeModify {
		t.Errorf("AllowedChange = %q, want %q", pf.AllowedChange, ChangeModify)
	}
}

func TestPlannedFileUnmarshalObjectExplicit(t *testing.T) {
	input := `{"path": "src/delete.go", "allowed_change": "delete"}`
	var pf PlannedFile
	if err := json.Unmarshal([]byte(input), &pf); err != nil {
		t.Fatalf("unmarshal explicit object: %v", err)
	}
	if pf.Path != "src/delete.go" {
		t.Errorf("Path = %q, want src/delete.go", pf.Path)
	}
	if pf.AllowedChange != ChangeDelete {
		t.Errorf("AllowedChange = %q, want %q", pf.AllowedChange, ChangeDelete)
	}
}

func TestPlannedFileUnmarshalInvalidJSON(t *testing.T) {
	input := `{"path": 123}`
	var pf PlannedFile
	if err := json.Unmarshal([]byte(input), &pf); err == nil {
		t.Fatalf("expected error unmarshaling invalid JSON, got nil")
	}
}

func TestTaskUnmarshal(t *testing.T) {
	raw := `{
		"id": "TASK-100",
		"title": "Add helper",
		"planned_files": ["src/helper.go", {"path": "src/helper_test.go", "allowed_change": "create"}],
		"forbidden_changes": [".env*"],
		"allowed_commands": ["go test ./..."],
		"difficulty": "medium",
		"status": "ready"
	}`
	var task Task
	if err := json.Unmarshal([]byte(raw), &task); err != nil {
		t.Fatalf("unmarshal Task: %v", err)
	}
	if task.ID != "TASK-100" || task.Title != "Add helper" {
		t.Errorf("Task metadata mismatch: %+v", task)
	}
	if len(task.PlannedFiles) != 2 {
		t.Fatalf("got %d planned files, want 2", len(task.PlannedFiles))
	}
	if task.PlannedFiles[0].Path != "src/helper.go" || task.PlannedFiles[0].AllowedChange != ChangeModify {
		t.Errorf("PlannedFiles[0] = %+v", task.PlannedFiles[0])
	}
	if task.PlannedFiles[1].Path != "src/helper_test.go" || task.PlannedFiles[1].AllowedChange != ChangeCreate {
		t.Errorf("PlannedFiles[1] = %+v", task.PlannedFiles[1])
	}
}

func TestLeaseCodeRecovery(t *testing.T) {
	tests := []struct {
		code       LeaseCode
		wantAction string
		wantTool   string
	}{
		{LeaseExpired, "checkout_again", "devcouncil_checkout_task"},
		{LeaseHeldByOther, "pick_other_task", "devcouncil_next_task"},
		{LeaseInvalid, "checkout_again", "devcouncil_checkout_task"},
		{LeaseValid, "", ""},
		{LeaseCode("unknown"), "", ""},
	}
	for _, tc := range tests {
		action, tool := tc.code.Recovery()
		if action != tc.wantAction || tool != tc.wantTool {
			t.Errorf("Recovery(%q) = (%q, %q), want (%q, %q)",
				tc.code, action, tool, tc.wantAction, tc.wantTool)
		}
	}
}
