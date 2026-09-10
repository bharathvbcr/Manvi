package computer

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

func TestLadderWalkRecordsStrategyIndex(t *testing.T) {
	balance := "$1,250.00"
	o := observationFixture(t)
	o.Nodes = []Node{
		{ID: "find", Role: "button", Name: "Find member", Enabled: true, Actions: []string{"press"}},
		{ID: "balance", Role: "text_field", Name: "Balance", Value: &balance, Enabled: true},
	}
	spec := workflow.Capability{
		SchemaVersion: 1, ID: "bank", Revision: "1", Application: "bank",
		Targets: map[string]workflow.Selector{
			"search": {
				Strategies: []workflow.Selector{
					{Role: "button", Name: "Search"},
					{Role: "button", Name: "Find member"},
				},
				Rationale: "tenant label drift",
				Stability: "semantic",
			},
			"balance": {Name: "Balance"},
		},
		Limits: workflow.DefaultLimits(),
		Steps: []workflow.Step{
			{ID: "search", Kind: "press", Target: "search", Effect: "read"},
			{ID: "read", Kind: "extract", Target: "balance", Effect: "read", Output: "balance", OutputType: "money", Currency: "USD"},
		},
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	p, err := workflow.Compile(raw)
	if err != nil {
		t.Fatal(err)
	}
	d := &testDesktop{observation: o}
	var records []Record
	var mu sync.Mutex
	opts := RunOptions{
		Program: p, Desktop: d, Session: Session{ID: "session", RunID: "run", Epoch: 1, Window: o.Window},
		OnRecord: func(r Record) error {
			mu.Lock()
			records = append(records, r)
			mu.Unlock()
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(ctx)
	if err != nil || result.State.Phase != workflow.Completed {
		t.Fatalf("%+v %v", result, err)
	}
	if result.Result.Kind != workflow.ResultSuccess {
		t.Fatalf("result kind=%s", result.Result.Kind)
	}
	found := false
	mu.Lock()
	defer mu.Unlock()
	for _, r := range records {
		if r.Kind == "event" && r.Event != nil && r.Event.Kind == "observed" && r.Event.Observation.Target == "search" && r.Event.Observation.Matches == 1 {
			if r.Event.Observation.StrategyIndex != 1 {
				t.Fatalf("expected strategy_index=1, got %d", r.Event.Observation.StrategyIndex)
			}
			found = true
		}
		if r.Kind == "observation" && r.StrategyIndex == 1 {
			found = true
		}
	}
	if !found {
		t.Fatal("ladder fallback strategy_index not recorded")
	}
}

func TestDeclaredRecoveryExecutesAndRecordsRecoveryID(t *testing.T) {
	balance := "$1,250.00"
	o := observationFixture(t)
	o.Nodes = []Node{
		{ID: "dismiss", Role: "button", Name: "Dismiss", Enabled: true, Actions: []string{"press"}},
		{ID: "balance", Role: "text_field", Name: "Balance", Value: &balance, Enabled: true},
	}
	spec := workflow.Capability{
		SchemaVersion: 1, ID: "bank", Revision: "1", Application: "bank",
		Targets: map[string]workflow.Selector{
			"search":  {Role: "button", Name: "Search"},
			"dismiss": {Role: "button", Name: "Dismiss"},
			"balance": {Name: "Balance"},
		},
		Recoveries: []workflow.Recovery{{
			ID:     "dismiss_overlay",
			When:   workflow.RecoveryWhen{Target: "search", Predicate: workflow.Predicate{Op: "exists"}},
			Action: workflow.RecoveryAction{Kind: "press", Target: "dismiss"},
			Max:    1,
		}},
		Limits: workflow.Limits{MaxActions: 40, ActiveSeconds: 300, ObservationAttempts: 1, InterventionSeconds: 600},
		Steps: []workflow.Step{
			{ID: "search", Kind: "press", Target: "search", Effect: "read"},
			{ID: "read", Kind: "extract", Target: "balance", Effect: "read", Output: "balance", OutputType: "money", Currency: "USD"},
		},
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	p, err := workflow.Compile(raw)
	if err != nil {
		t.Fatal(err)
	}
	d := &countingDesktop{testDesktop: testDesktop{observation: o}, afterAct: func(d *countingDesktop) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.observation.Nodes = []Node{
			{ID: "search", Role: "button", Name: "Search", Enabled: true, Actions: []string{"press"}},
			{ID: "balance", Role: "text_field", Name: "Balance", Value: &balance, Enabled: true},
		}
	}}
	var records []Record
	opts := RunOptions{
		Program: p, Desktop: d, Session: Session{ID: "session", RunID: "run", Epoch: 1, Window: o.Window},
		OnRecord: func(r Record) error { records = append(records, r); return nil },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	run, err := StartRun(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(ctx)
	if err != nil || result.State.Phase != workflow.Completed {
		t.Fatalf("%+v %v", result, err)
	}
	if result.State.RecoveryCounts["dismiss_overlay"] != 1 {
		t.Fatalf("recovery counts=%v", result.State.RecoveryCounts)
	}
	seen := false
	for _, r := range records {
		if r.RecoveryID == "dismiss_overlay" {
			seen = true
			break
		}
	}
	if !seen {
		t.Fatal("recovery_id missing from records")
	}
}

type countingDesktop struct {
	testDesktop
	afterAct func(*countingDesktop)
}

func (d *countingDesktop) Act(ctx context.Context, s Session, a Action) (Receipt, error) {
	receipt, err := d.testDesktop.Act(ctx, s, a)
	if err == nil && d.afterAct != nil {
		d.afterAct(d)
	}
	return receipt, err
}
