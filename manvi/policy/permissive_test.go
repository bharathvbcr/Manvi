package policy

import (
	"slices"
	"testing"

	"manvi/dc"
)

// A neighbour allow reached through a near-total neighbour relation must not
// look like one reached through a tight relation.
//
// The map has always been able to compute this — repomap.Stats.Permissive —
// and the only thing that ever consulted it was `manvi map status`. So the
// condition was visible to an operator who thought to run a diagnostic, and
// invisible in the decision it actually changes: a repository where one area
// neighbours most of the others answers "is the target next door to the plan"
// with almost one answer, and every such write recorded a clean pass.
func TestAPermissiveNeighbourAllowIsQualified(t *testing.T) {
	task := &dc.Task{
		ID: "TASK-1",
		PlannedFiles: []dc.PlannedFile{
			{Path: "src/calc.go", AllowedChange: dc.ChangeModify},
		},
	}
	mapping := areas{
		of:        map[string]string{"src": "core", "web": "ui"},
		neighbors: map[string][]string{"ui": {"core"}, "core": {"ui"}},
	}

	for _, tc := range []struct {
		name          string
		permissive    bool
		wantQualified bool
	}{
		{name: "tight relation is a clean allow", permissive: false, wantQualified: false},
		{name: "permissive relation is recorded", permissive: true, wantQualified: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := mapping
			m.permissive = tc.permissive
			g := FileGate{
				Root:           t.TempDir(),
				Subsystems:     m,
				AllowNeighbors: true,
				AllowSameDir:   true,
				HardRules:      true,
			}

			d := g.EvaluateFileChange("web/page.go", task, dc.OpModify, false)
			if d.Action != Allow {
				t.Fatalf("the neighbour rung did not allow: %+v", d)
			}
			got := slices.Contains(d.Degraded, "scope.neighbors.permissive")
			if got != tc.wantQualified {
				t.Fatalf("degraded=%v, want the permissive marker present=%v", d.Degraded, tc.wantQualified)
			}
			// Decision.Clean is the predicate evidence reporting uses, so this
			// is what decides whether a run built on permissive allows can be
			// summarised as strict.
			if d.Clean() == tc.wantQualified {
				t.Fatalf("Clean()=%v with permissive=%v; a permissive allow must not read as an ordinary pass",
					d.Clean(), tc.permissive)
			}
		})
	}
}

// The marker must not appear on an allow the neighbour rung did not produce.
// A same-subsystem write is authorised by the plan itself, and marking it would
// make the signal mean nothing.
func TestASameSubsystemAllowIsNotMarkedPermissive(t *testing.T) {
	task := &dc.Task{
		ID:           "TASK-1",
		PlannedFiles: []dc.PlannedFile{{Path: "src/calc.go", AllowedChange: dc.ChangeModify}},
	}
	g := FileGate{
		Root: t.TempDir(),
		Subsystems: areas{
			of:         map[string]string{"src": "core"},
			neighbors:  map[string][]string{},
			permissive: true,
		},
		AllowNeighbors: true,
		AllowSameDir:   true,
		HardRules:      true,
	}

	d := g.EvaluateFileChange("src/other.go", task, dc.OpModify, false)
	if d.Action != Allow {
		t.Fatalf("a same-subsystem write was not allowed: %+v", d)
	}
	if slices.Contains(d.Degraded, "scope.neighbors.permissive") {
		t.Fatalf("a same-subsystem allow was marked permissive: %+v", d.Degraded)
	}
}

// A denial must not carry the marker either: the rung refused, so how wide the
// relation is did not decide anything.
func TestADenialIsNotMarkedPermissive(t *testing.T) {
	task := &dc.Task{
		ID:           "TASK-1",
		PlannedFiles: []dc.PlannedFile{{Path: "src/calc.go", AllowedChange: dc.ChangeModify}},
	}
	g := FileGate{
		Root: t.TempDir(),
		Subsystems: areas{
			of:         map[string]string{"src": "core", "far": "unrelated"},
			neighbors:  map[string][]string{},
			permissive: true,
		},
		AllowNeighbors: true,
		HardRules:      true,
	}

	d := g.EvaluateFileChange("far/thing.go", task, dc.OpModify, false)
	if d.Action == Allow {
		t.Fatalf("an unrelated subsystem was allowed: %+v", d)
	}
	if slices.Contains(d.Degraded, "scope.neighbors.permissive") {
		t.Fatalf("a denial was marked permissive: %+v", d.Degraded)
	}
}
