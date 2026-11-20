package main

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/agent"
	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// sensorConstants reads every SensorVerdict constant out of the agent package's
// source, so a value added there without a row below fails here rather than
// being read by a default branch as a pass.
func sensorConstants(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "../../agent/loop.go", nil, 0)
	if err != nil {
		t.Fatalf("parse loop.go: %v", err)
	}
	var names []string
	for _, d := range f.Decls {
		gen, ok := d.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); ok && id.Name == "SensorVerdict" {
				for _, n := range vs.Names {
					names = append(names, n.Name)
				}
			}
		}
	}
	sort.Strings(names)
	return names
}

// Exit statuses are decided by the turn's own endings, not by the end-of-turn
// verdict, and adding "interrupted" must not move any of them. Each shape below
// has a status and an escalation reason with no verdict; every verdict must
// leave both exactly where they were.
func TestZadv_w24va_EverySensorValueLeavesTheExitStatusAndEscalationUnchanged(t *testing.T) {
	verdicts := map[string]agent.SensorVerdict{
		"SensorNone": agent.SensorNone, "SensorSkipped": agent.SensorSkipped, "SensorPassed": agent.SensorPassed,
		"SensorFailed": agent.SensorFailed, "SensorDegraded": agent.SensorDegraded, "SensorInterrupted": agent.SensorInterrupted,
	}
	var have []string
	for n := range verdicts {
		have = append(have, n)
	}
	sort.Strings(have)
	want := sensorConstants(t)
	if len(have) != len(want) {
		t.Fatalf("agent declares %v, this table covers %v", want, have)
	}
	for i := range want {
		if want[i] != have[i] {
			t.Fatalf("agent declares %v, this table covers %v", want, have)
		}
	}

	shapes := map[string]agent.Outcome{
		"finished":     {Steps: 3, Mutated: true, StopReason: llm.StopEndTurn},
		"step ceiling": {Steps: 3, Mutated: true, TruncatedBySteps: true},
		"output cap":   {Steps: 3, Mutated: true, FinalTruncated: true},
		"no answer":    {Steps: 3, Mutated: true, FinalEmpty: true},
		"refusal":      {Steps: 3, Mutated: true, StopReason: llm.StopRefusal},
		"bounced out":  {Steps: 3, Mutated: true, BouncesExhausted: true, Bounces: 2, StopReason: llm.StopEndTurn},
	}
	for shape, base := range shapes {
		wantStatus := outcomeStatus(base)
		wantEvidence := escalationEvidence(base, "an answer", 500)
		for name, v := range verdicts {
			o := base
			o.Sensor = v
			got := outcomeStatus(o)
			if (got == nil) != (wantStatus == nil) || (got != nil && !errors.Is(got, wantStatus)) {
				t.Errorf("%s/%s: exit status %v, was %v without a verdict", shape, name, got, wantStatus)
			}
			if got := escalationEvidence(o, "an answer", 500); got != wantEvidence &&
				!(v == agent.SensorFailed && shape == "bounced out") {
				t.Errorf("%s/%s: escalation evidence %q, was %q without a verdict", shape, name, got, wantEvidence)
			}
		}
	}
}

// The notice is what a person reads, and the two non-passes that are not a
// finding must still say something; a pass says nothing.
func TestZadv_w24va_EverySensorValueIsEitherSilentOnlyWhenItIsNotAnObjection(t *testing.T) {
	silent := map[agent.SensorVerdict]bool{agent.SensorNone: true, agent.SensorSkipped: true, agent.SensorPassed: true}
	for _, v := range []agent.SensorVerdict{agent.SensorNone, agent.SensorSkipped, agent.SensorPassed,
		agent.SensorFailed, agent.SensorDegraded, agent.SensorInterrupted, agent.SensorVerdict("from-the-future")} {
		degraded := false
		for _, n := range outcomeNotices(agent.Outcome{Steps: 2, Mutated: true, Sensor: v}, 500) {
			degraded = degraded || len(n.Degraded) > 0
		}
		if silent[v] == degraded {
			t.Errorf("verdict %q: degraded notice = %v", v, degraded)
		}
	}
}
