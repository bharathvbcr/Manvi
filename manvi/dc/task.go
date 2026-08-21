// Package dc holds the DevCouncil domain types the harness executes against.
//
// These are ports of the shapes DevCouncil persists, not a second source of
// truth. The harness reads and writes DevCouncil's own state; these structs are
// how that state looks in memory while a turn is running.
package dc

import (
	"bytes"
	"encoding/json"
)

// AllowedChange is what a task's plan permits for one planned file.
type AllowedChange string

const (
	ChangeCreate   AllowedChange = "create"
	ChangeModify   AllowedChange = "modify"
	ChangeDelete   AllowedChange = "delete"
	ChangeReadOnly AllowedChange = "read_only"
)

// Operation is what a tool call is attempting.
type Operation string

const (
	OpCreate Operation = "create"
	OpModify Operation = "modify"
	OpDelete Operation = "delete"
	// OpWrite is the unspecialised write a tool reports when it does not know
	// whether the file already exists. It satisfies create or modify.
	OpWrite Operation = "write"
)

// PlannedFile is one entry in a task's declared file scope.
type PlannedFile struct {
	Path          string        `json:"path"`
	AllowedChange AllowedChange `json:"allowed_change"`
}

// UnmarshalJSON accepts either the object form DevCouncil writes or a bare
// path string.
//
// The bare form appears in hand-written plans and in older exports. Accepting
// it is not laxity: rejecting it would make the gate refuse a scope the task
// genuinely declares, and a path with no stated change defaults to modify —
// the same thing DevCouncil assumes when the field is absent.
func (p *PlannedFile) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var path string
		if err := json.Unmarshal(trimmed, &path); err != nil {
			return err
		}
		p.Path = path
		p.AllowedChange = ChangeModify
		return nil
	}
	type raw PlannedFile
	var decoded raw
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return err
	}
	*p = PlannedFile(decoded)
	if p.AllowedChange == "" {
		p.AllowedChange = ChangeModify
	}
	return nil
}

// Task is the unit of work a builder holds a lease on.
type Task struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// PlannedFiles is the scope the *planner* declared. It is deliberately not
	// the whole of what the gate will allow — see AgentAppendedPlannedFiles —
	// because the two answer different questions, and a single merged list
	// cannot answer the second one at all.
	PlannedFiles []PlannedFile `json:"planned_files"`
	// AgentAppendedPlannedFiles is scope an executor added to its own task
	// while it worked, by arguing a blocked write through the override seam.
	//
	// It is in scope for the gate exactly as PlannedFiles is, and it is not the
	// same thing: one is a decision someone made before the work started, the
	// other is a decision the worker made about itself. Keeping them apart is
	// what lets a write authorised by the second report as a widening rather
	// than as a clean pass against the plan.
	AgentAppendedPlannedFiles []PlannedFile `json:"agent_appended_planned_files,omitempty"`
	ForbiddenChanges          []string      `json:"forbidden_changes"`
	AllowedCommands           []string      `json:"allowed_commands"`
	Difficulty                string        `json:"difficulty"`
	Status                    string        `json:"status"`
}

// AllPlannedFiles is everything in the task's file scope: the plan it was
// created with, followed by whatever an executor appended to it.
//
// Consumers asking "is this file in scope" want this. Consumers asking "did the
// planner authorise this" want PlannedFiles, and must not use this instead.
func (t *Task) AllPlannedFiles() []PlannedFile {
	if t == nil {
		return nil
	}
	if len(t.AgentAppendedPlannedFiles) == 0 {
		return t.PlannedFiles
	}
	all := make([]PlannedFile, 0, len(t.PlannedFiles)+len(t.AgentAppendedPlannedFiles))
	all = append(all, t.PlannedFiles...)
	return append(all, t.AgentAppendedPlannedFiles...)
}

// LeaseCode classifies a lease token, matching DevCouncil's four documented
// outcomes so an agent can recover without parsing prose.
type LeaseCode string

const (
	LeaseValid       LeaseCode = "valid"
	LeaseExpired     LeaseCode = "lease_expired"
	LeaseInvalid     LeaseCode = "invalid_lease"
	LeaseHeldByOther LeaseCode = "lease_held_by_other"
)

// Recovery is the action an agent should take for a lease failure. It mirrors
// DevCouncil's suggested_action / suggested_tool contract.
func (c LeaseCode) Recovery() (action, tool string) {
	switch c {
	case LeaseExpired:
		return "checkout_again", "devcouncil_checkout_task"
	case LeaseHeldByOther:
		return "pick_other_task", "devcouncil_next_task"
	case LeaseInvalid:
		return "checkout_again", "devcouncil_checkout_task"
	default:
		return "", ""
	}
}
