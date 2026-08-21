package policy

import (
	"testing"

	"manvi/dc"
)

// A secret at the repository root must be refused exactly as its nested twin
// is. The SecretPathPatterns list reaches nested copies through "**/x"
// patterns, and "**/" requires at least one leading segment — pinned as
// `**/.env  .env  false` in the fnmatch parity fixture, matching Python. So a
// root-level copy is only protected when the list *also* carries a bare entry,
// and it carries one for some secrets and not others.
//
// This is the same defect the RestrictedPathPatterns comment already describes
// for ".git", where bare entries were added for exactly this reason.
func TestRootLevelSecretsAreRefusedLikeNestedOnes(t *testing.T) {
	gate := FileGate{Root: t.TempDir(), HardRules: true}
	// A task that plans every path, so nothing below the secret rung can be
	// what refuses the write: only the secret rung itself.
	task := &dc.Task{ID: "T", PlannedFiles: []dc.PlannedFile{{Path: "**", AllowedChange: dc.ChangeModify}}}

	for _, path := range []string{
		"secrets/token.txt",
		"credentials/aws.json",
		"server.pem",
		"server.key",
		"id_dsa",
		"id_ecdsa",
	} {
		nested := "sub/" + path
		if d := gate.EvaluateFileChange(nested, task, dc.OpWrite, false); d.Action != Deny {
			t.Errorf("%s: action = %q, want deny (the nested case is the one that already worked)",
				nested, d.Action)
		}
		if d := gate.EvaluateFileChange(path, task, dc.OpWrite, false); d.Action != Deny {
			t.Errorf("%s: action = %q rule=%q — a root-level secret was writable while %s was not",
				path, d.Action, d.Rule, nested)
		}
	}
}
