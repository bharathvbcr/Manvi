package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/agent"
	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
	"github.com/bharathvbcr/Manvi/manvi/session"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// The owner's requirement: a turn that changed files is verified to a verdict,
// and that verdict is what the exit status and the notices report.

// A failed or not-run verification used to exit 0, the status of a verified
// turn: outcomeStatus read the stop reason and the ceilings and never the
// sensor.
func TestExitStatusReflectsTheVerification(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome agent.Outcome
		want    error
	}{
		{"failed", agent.Outcome{Sensor: agent.SensorFailed}, errVerifyFailed},
		{"failed with the bounce cap spent", agent.Outcome{Sensor: agent.SensorFailed, Bounces: 2, BouncesExhausted: true}, errVerifyFailed},
		{"degraded", agent.Outcome{Sensor: agent.SensorDegraded}, errVerifyNotRun},
		{"a verdict this build does not know", agent.Outcome{Sensor: "approved"}, errVerifyNotRun},
		{"the step ceiling wins, being more specific", agent.Outcome{Sensor: agent.SensorFailed, TruncatedBySteps: true}, errTruncated},
		{"passed", agent.Outcome{Sensor: agent.SensorPassed}, nil},
		{"not owed", agent.Outcome{Sensor: agent.SensorSkipped}, nil},
		{"no check attached", agent.Outcome{Sensor: agent.SensorNone}, nil},
	} {
		got := outcomeStatus(tc.outcome)
		if !errors.Is(got, tc.want) || (tc.want == nil && got != nil) {
			t.Errorf("%s: status %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestVerificationStatusesMapToTheirOwnExitCodes(t *testing.T) {
	var notes bytes.Buffer
	if got := statusFor(errVerifyFailed, &notes); got != 10 {
		t.Errorf("verification failed exits %d, want 10", got)
	}
	if got := statusFor(errVerifyNotRun, &notes); got != 11 {
		t.Errorf("verification did not run exits %d, want 11", got)
	}
}

// The final look at a turn the ceiling ended has nowhere to send the model, so
// it must record its verdict and compose nothing.
func TestSensorFinalCheckRecordsAndNeverInjects(t *testing.T) {
	log := session.NewLog()
	s := &sensor{
		native: &fakeVerifier{report: devcouncil.PathReport{
			Verdict: devcouncil.VerdictFailed, Findings: []string{"broken"},
		}},
		log: log,
	}
	e := &agent.TurnStopping{Mutated: true, Wrote: []string{"a.go"}, Final: true}
	if err := s.check(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if e.Verdict != agent.SensorFailed {
		t.Fatalf("verdict = %q, want failed", e.Verdict)
	}
	if e.Inject != "" {
		t.Fatalf("a final check composed an injection nobody will read: %q", e.Inject)
	}
	if got := reportsIn(t, log); len(got) != 1 || got[0].Verdict != "failed" {
		t.Fatalf("reports = %+v, want the failed verdict recorded", got)
	}
}

func TestSensorOutputCapVerdictIsMarkedUnexamined(t *testing.T) {
	v := &fakeVerifier{report: devcouncil.PathReport{Verdict: devcouncil.VerdictPassed}}
	s := &sensor{native: v, log: session.NewLog()}
	e := &agent.TurnStopping{Mutated: true, Wrote: []string{"a.go"}, Truncated: true}
	if err := s.check(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if e.Verdict != agent.SensorFailed || !e.Unexamined || v.calls != 0 {
		t.Fatalf("verdict=%q unexamined=%v verifier calls=%d, want a failed verdict that says no file was read",
			e.Verdict, e.Unexamined, v.calls)
	}
}

func reportsIn(t *testing.T, log *session.Log) []session.VerifyReportData {
	t.Helper()
	var out []session.VerifyReportData
	for _, e := range log.Events() {
		if e.Type != session.VerifyReport {
			continue
		}
		var d session.VerifyReportData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func checkedReplayLoop(t *testing.T, turns []replay.Turn, maxSteps int, v *fakeVerifier) (*agent.Loop, *session.Log) {
	t.Helper()
	return checkedReplayLoopWith(t, turns, maxSteps, v, registerFakeWrite)
}

// checkedReplayLoopWith is checkedReplayLoop with the write tool supplied by the
// caller. It arms the loop the way production does: EndOfTurnCheck on the config
// and the sensor attached through OnCheck, both.
func checkedReplayLoopWith(t *testing.T, turns []replay.Turn, maxSteps int, v *fakeVerifier, register func(*testing.T, *tools.Registry)) (*agent.Loop, *session.Log) {
	t.Helper()
	b := bus.New()
	log := session.NewLog()
	registry := tools.NewRegistry(b)
	provider := replay.New(replay.Fixture{
		Provider: "replay",
		Capabilities: []llm.Capability{{
			Provider: "replay", Model: "m",
			ContextWindow: 200000, MaxOutputTokens: 8192,
			SupportsTools: true, SupportsStreaming: true,
		}},
		Turns: turns,
	})
	loop, err := agent.NewLoop(agent.Config{
		Provider: provider, Model: "m",
		SystemPrompt: "you are a builder", MaxSteps: maxSteps, MaxTokens: 1024,
		AssertInvariant: true, EndOfTurnCheck: true,
	}, b, log, registry)
	if err != nil {
		t.Fatal(err)
	}
	register(t, registry)
	if err := attachSensor(b, &sensor{native: v, log: log}); err != nil {
		t.Fatal(err)
	}
	return loop, log
}

// Production arms both declarations (EndOfTurnCheck and OnCheck). A turn
// cancelled after a write is marked interrupted by the loop's cancel path, and
// that record must be the only one: the deferred seal for "owed and did not run"
// would otherwise write a degraded report over it.
func TestCancelledCheckedTurnIsInterruptedWithOneReport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	v := &fakeVerifier{report: devcouncil.PathReport{Verdict: devcouncil.VerdictPassed}}
	loop, log := checkedReplayLoopWith(t, []replay.Turn{
		replayToolCall("c1", "write", `{"path":"a.go"}`),
		replayToolCall("c2", "write", `{"path":"b.go"}`),
	}, 8, v, func(t *testing.T, registry *tools.Registry) {
		t.Helper()
		if err := registry.Register(tools.Tool{
			Schema: llm.ToolSchema{Name: "write", Description: "write a file"},
			Handler: func(context.Context, tools.Call) tools.Result {
				cancel()
				return tools.Result{Text: "wrote a.go", Wrote: []string{"a.go"}}
			},
		}); err != nil {
			t.Fatal(err)
		}
	})
	out, err := loop.Run(ctx, llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "fix a.go"}},
	})
	if err == nil {
		t.Fatal("fixture: the cancelled turn was meant to return its error")
	}
	if out.Sensor != agent.SensorInterrupted {
		t.Fatalf("sensor = %q, want interrupted", out.Sensor)
	}
	got := reportsIn(t, log)
	if len(got) != 1 || got[0].Verdict != string(agent.SensorInterrupted) {
		t.Fatalf("reports = %+v, want exactly one, interrupted", got)
	}
	if v.calls != 0 {
		t.Fatal("a verifier was started for a cancelled turn")
	}
}

// The declaration that arms the loop is made in newTurnLoop, from whether the
// face attached a check. Every other test here sets it by hand; this one goes
// through the constructor both run and the TUI use, so deleting that line fails.
func TestNewTurnLoopArmsTheLoopOnlyWhenACheckIsAttached(t *testing.T) {
	for _, tc := range []struct {
		name     string
		attached bool
	}{{"with a check", true}, {"a sub-agent's loop, no check", false}} {
		t.Run(tc.name, func(t *testing.T) {
			v := &fakeVerifier{report: devcouncil.PathReport{Verdict: devcouncil.VerdictPassed}}
			log := session.NewLog()
			registry := tools.NewRegistry(bus.New())
			registerFakeWrite(t, registry)
			face := turnFace{maxSteps: 2}
			if tc.attached {
				face.check = &sensor{native: v, log: log}
			}
			loop, err := newTurnLoop(face, turnDeps{
				provider: replay.New(replay.Fixture{
					Provider: "replay",
					Capabilities: []llm.Capability{{
						Provider: "replay", Model: "m",
						ContextWindow: 200000, MaxOutputTokens: 8192,
						SupportsTools: true, SupportsStreaming: true,
					}},
					Turns: []replay.Turn{
						replayToolCall("c1", "write", `{"path":"a.go"}`),
						replayToolCall("c2", "write", `{"path":"b.go"}`),
					},
				}),
				model: "m", systemPrompt: "you are a builder", log: log, tools: registry,
			})
			if err != nil {
				t.Fatal(err)
			}
			out, err := runPrompt(loop)
			if err != nil {
				t.Fatal(err)
			}
			if !out.TruncatedBySteps {
				t.Fatal("fixture: the turn was meant to end on the ceiling")
			}
			if tc.attached && (v.calls != 1 || out.Sensor != agent.SensorPassed) {
				t.Fatalf("verifier calls=%d sensor=%q: the face attached a check and the ceiling skipped it", v.calls, out.Sensor)
			}
			if !tc.attached && (v.calls != 0 || out.Sensor != agent.SensorNone || len(reportsIn(t, log)) != 0) {
				t.Fatalf("calls=%d sensor=%q reports=%d: a loop with no check was given one", v.calls, out.Sensor, len(reportsIn(t, log)))
			}
		})
	}
}

func runPrompt(loop *agent.Loop) (agent.Outcome, error) {
	return loop.Run(context.Background(), llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "fix a.go"}},
	})
}

// The step ceiling used to end a turn that wrote files with no verification at
// all: no verify.report, an empty sensor, exit status 2 and nothing to say the
// files were never looked at.
func TestStepCeilingTurnIsVerifiedToAVerdict(t *testing.T) {
	v := &fakeVerifier{report: devcouncil.PathReport{
		Verdict: devcouncil.VerdictFailed, Findings: []string{"a.go does not build"},
	}}
	loop, log := checkedReplayLoop(t, []replay.Turn{
		replayToolCall("c1", "write", `{"path":"a.go"}`),
		replayToolCall("c2", "write", `{"path":"b.go"}`),
		replayToolCall("c3", "write", `{"path":"c.go"}`),
	}, 2, v)

	out, err := runPrompt(loop)
	if err != nil {
		t.Fatal(err)
	}
	if !out.TruncatedBySteps {
		t.Fatal("fixture: the turn was meant to end on the ceiling")
	}
	if v.calls != 1 {
		t.Fatalf("the verifier ran %d times, want once over the final tree", v.calls)
	}
	if out.Sensor != agent.SensorFailed {
		t.Fatalf("sensor = %q, want failed", out.Sensor)
	}
	got := reportsIn(t, log)
	if len(got) != 1 || got[0].Verdict != "failed" || len(got[0].Findings) == 0 {
		t.Fatalf("reports = %+v, want the failed verdict with its findings", got)
	}
	if out.Bounces != 0 {
		t.Fatal("the ceiling's check sent the turn back")
	}
}

// A turn that dies after writing used to leave no verification record at all.
func TestErroredTurnThatWroteFilesRecordsAnUnrunCheck(t *testing.T) {
	v := &fakeVerifier{report: devcouncil.PathReport{Verdict: devcouncil.VerdictPassed}}
	loop, log := checkedReplayLoop(t, []replay.Turn{
		replayToolCall("c1", "write", `{"path":"a.go"}`),
	}, 12, v)

	out, err := runPrompt(loop)
	if err == nil {
		t.Fatal("fixture: the provider was meant to run dry and fail the turn")
	}
	if out.Sensor != agent.SensorDegraded {
		t.Fatalf("sensor = %q, want degraded", out.Sensor)
	}
	got := reportsIn(t, log)
	if len(got) != 1 || got[0].Verdict != "degraded" || len(got[0].Degraded) == 0 || len(got[0].Paths) != 1 {
		t.Fatalf("reports = %+v, want one degraded report naming what did not run and the paths", got)
	}
	if v.calls != 0 {
		t.Fatal("a verifier was started for a turn that never reached its checkpoint")
	}
}
