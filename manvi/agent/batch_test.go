package agent

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
	"github.com/bharathvbcr/Manvi/manvi/session"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

func registerBatchable(t *testing.T, h *harness, name string, handler tools.Handler) {
	t.Helper()
	if err := h.tools.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: name, Description: name,
			InputSchema: []byte(`{"type":"object","properties":{}}`)},
		ReadOnly: true, Batchable: true, Handler: handler,
	}); err != nil {
		t.Fatal(err)
	}
}

func withBatch(t *testing.T, h *harness) {
	t.Helper()
	if err := h.tools.Register(tools.BatchTool()); err != nil {
		t.Fatal(err)
	}
}

// toolResults is every tool result the session projects onto the wire, by call.
func toolResults(t *testing.T, h *harness) map[llm.CallID]string {
	t.Helper()
	msgs, err := h.log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	out := map[llm.CallID]string{}
	for _, m := range msgs {
		for _, block := range m.Content {
			if r, ok := block.(llm.ToolResultBlock); ok {
				var text strings.Builder
				for _, c := range r.Content {
					if tb, ok := c.(llm.TextBlock); ok {
						text.WriteString(tb.Text)
					}
				}
				out[r.ToolCallID] = text.String()
			}
		}
	}
	return out
}

// The point of a batch: its lookups run at the same time, and the model gets
// one result for one call — inner calls never become wire messages.
func TestBatchRunsLookupsConcurrentlyBehindOneResult(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("b1", tools.BatchToolName, `{"calls":[{"tool":"left"},{"tool":"right"}]}`),
		textTurn("done"),
	}, nil)
	withBatch(t, h)
	var started sync.WaitGroup
	started.Add(2)
	both := make(chan struct{})
	go func() { started.Wait(); close(both) }()
	meet := func(answer string) tools.Handler {
		return func(context.Context, tools.Call) tools.Result {
			started.Done()
			select {
			case <-both:
				return tools.Result{Text: answer}
			case <-time.After(5 * time.Second):
				return tools.Errorf("the other lookup never started: the batch ran serially")
			}
		}
	}
	registerBatchable(t, h, "left", meet("left answer"))
	registerBatchable(t, h, "right", meet("right answer"))

	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatal(err)
	}
	results := toolResults(t, h)
	if len(results) != 1 {
		t.Fatalf("the wire carries %d tool results, want exactly the batch's: %v", len(results), results)
	}
	text := results["b1"]
	for _, want := range []string{"left answer", "right answer", "## [1] left", "## [2] right"} {
		if !strings.Contains(text, want) {
			t.Errorf("batch result lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "serially") {
		t.Fatalf("lookups did not overlap:\n%s", text)
	}
	if out.ToolCalls != 3 {
		t.Errorf("ToolCalls = %d, want 3 (the batch and its two lookups)", out.ToolCalls)
	}
}

// Anything that is not a registered, batchable, offered lookup is refused
// inside a batch and never runs.
func TestBatchRefusesWhatIsNotABatchableLookup(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("b1", tools.BatchToolName, `{"calls":[{"tool":"writer"},{"tool":"devcouncil_batch"},{"tool":"ghost"},{"tool":""},{"tool":"look"}]}`),
		textTurn("done"),
	}, nil)
	withBatch(t, h)
	wrote := 0
	if err := h.tools.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "writer", Description: "writer",
			InputSchema: []byte(`{"type":"object","properties":{}}`)},
		ReadOnly: true, // ReadOnly alone does not admit a tool
		Handler:  func(context.Context, tools.Call) tools.Result { wrote++; return tools.Result{Text: "wrote"} },
	}); err != nil {
		t.Fatal(err)
	}
	registerBatchable(t, h, "look", func(context.Context, tools.Call) tools.Result { return tools.Result{Text: "looked"} })

	if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
		t.Fatal(err)
	}
	if wrote != 0 {
		t.Fatalf("a tool that is not batchable ran %d time(s) inside a batch", wrote)
	}
	text := toolResults(t, h)["b1"]
	for _, want := range []string{"writer cannot run inside a batch", "a batch cannot contain a batch",
		"ghost", "needs a tool name", "looked"} {
		if !strings.Contains(text, want) {
			t.Errorf("batch result lacks %q:\n%s", want, text)
		}
	}
}

func TestBatchKeepsOnlyTheLinesAskedFor(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("b1", tools.BatchToolName, `{"calls":[{"tool":"lines","match":"^keep"},{"tool":"lines","args":{"n":2},"max_lines":2}]}`),
		textTurn("done"),
	}, nil)
	withBatch(t, h)
	registerBatchable(t, h, "lines", func(context.Context, tools.Call) tools.Result {
		return tools.Result{Text: "keep one\ndrop\nkeep two\ndrop again\n"}
	})
	if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
		t.Fatal(err)
	}
	text := toolResults(t, h)["b1"]
	first, second, _ := strings.Cut(text, "## [2]")
	if strings.Contains(first, "drop") || !strings.Contains(first, "keep two") ||
		!strings.Contains(first, "[2 of 4 lines matched; 2 shown]") {
		t.Errorf("match filter wrong:\n%s", first)
	}
	if strings.Contains(second, "keep two") || !strings.Contains(second, "[2 of 4 lines shown") {
		t.Errorf("max_lines cap wrong:\n%s", second)
	}
}

// A batch is not a way round the repeat ledger.
func TestBatchCountsInTheRepeatLedger(t *testing.T) {
	call := `{"tool":"probe"}`
	h := build(t, []replay.Turn{
		toolTurn("b1", tools.BatchToolName, fmt.Sprintf(`{"calls":[%s,%s,%s,%s]}`, call, call, call, call)),
		textTurn("done"),
	}, nil)
	withBatch(t, h)
	var ran atomic.Int32
	registerBatchable(t, h, "probe", func(context.Context, tools.Call) tools.Result {
		ran.Add(1)
		return tools.Result{Text: "same"}
	})
	if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
		t.Fatal(err)
	}
	if got := ran.Load(); got != RepeatLimit {
		t.Fatalf("the identical call ran %d times, want %d", got, RepeatLimit)
	}
	if !strings.Contains(toolResults(t, h)["b1"], "already made") {
		t.Error("the repeat refusal is not in the batch result")
	}
}

func TestBatchArgumentsAreBounded(t *testing.T) {
	var many []string
	for range tools.MaxBatchCalls + 1 {
		many = append(many, `{"tool":"probe"}`)
	}
	for name, args := range map[string]string{
		"empty":            `{"calls":[]}`,
		"too many":         `{"calls":[` + strings.Join(many, ",") + `]}`,
		"calls not a list": `{"calls":"read everything"}`,
		"bad re":           `{"calls":[{"tool":"probe","match":"("}]}`,
		"bad cap":          `{"calls":[{"tool":"probe","max_lines":99999}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := build(t, []replay.Turn{toolTurn("b1", tools.BatchToolName, args), textTurn("done")}, nil)
			withBatch(t, h)
			ran := 0
			registerBatchable(t, h, "probe", func(context.Context, tools.Call) tools.Result { ran++; return tools.Result{Text: "x"} })
			if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
				t.Fatal(err)
			}
			if ran != 0 {
				t.Errorf("a malformed batch ran %d lookup(s)", ran)
			}
		})
	}
}

// Three steps of one lookup each earn one note pointing at the batch — but
// only when there is a batch tool to point at.
func TestSerialLookupsEarnANoteOnlyWhenABatchIsOffered(t *testing.T) {
	for _, offered := range []bool{true, false} {
		t.Run(fmt.Sprint("batch offered=", offered), func(t *testing.T) {
			h := build(t, []replay.Turn{
				toolTurn("s1", "probe", `{"n":1}`),
				toolTurn("s2", "probe", `{"n":2}`),
				toolTurn("s3", "probe", `{"n":3}`),
				textTurn("done"),
			}, nil)
			if offered {
				withBatch(t, h)
			}
			n := 0
			registerBatchable(t, h, "probe", func(context.Context, tools.Call) tools.Result {
				n++
				return tools.Result{Text: fmt.Sprintf("answer %d", n)}
			})
			if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
				t.Fatal(err)
			}
			results := toolResults(t, h)
			for _, early := range []llm.CallID{"s1", "s2"} {
				if strings.Contains(results[early], "[harness:") {
					t.Errorf("%s carries the note before the third serial lookup", early)
				}
			}
			if got := strings.Contains(results["s3"], "[harness:"); got != offered {
				t.Errorf("note on the third lookup = %v, want %v: %q", got, offered, results["s3"])
			}
		})
	}
}

// The reason inner calls record their gate verdicts through recordPolicy: a
// batch whose lookup the gate refused must leave the same evidence a top-level
// refusal does, under an ID that never reaches the wire, and the log must still
// project into a valid conversation.
func TestBatchRecordsTheGatesVerdictOnEachInnerCall(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("b1", tools.BatchToolName, `{"calls":[{"tool":"secret"},{"tool":"qualified"},{"tool":"plain"}]}`),
		textTurn("done"),
	}, nil)
	withBatch(t, h)
	registerBatchable(t, h, "secret", func(context.Context, tools.Call) tools.Result {
		return tools.Result{IsError: true, Blocked: true, Rule: "path.secret", Severity: "hard", Text: "refused: credential path"}
	})
	registerBatchable(t, h, "qualified", func(context.Context, tools.Call) tools.Result {
		return tools.Result{Text: "answered", Rule: "scope.unplanned", Degraded: []string{"index stale"}}
	})
	registerBatchable(t, h, "plain", func(context.Context, tools.Call) tools.Result { return tools.Result{Text: "fine"} })

	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Denied != 1 || out.Qualified != 1 {
		t.Fatalf("Denied=%d Qualified=%d, want 1 and 1", out.Denied, out.Qualified)
	}
	var denied, qualified []string
	for _, e := range h.log.Events() {
		switch e.Type {
		case session.PolicyDenied:
			var d session.DenialData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatal(err)
			}
			denied = append(denied, string(d.ToolCallID)+"/"+d.Tool+"/"+d.Rule)
		case session.PolicyQualified:
			var q session.QualificationData
			if err := json.Unmarshal(e.Data, &q); err != nil {
				t.Fatal(err)
			}
			qualified = append(qualified, string(q.ToolCallID)+"/"+q.Tool)
		}
	}
	if len(denied) != 1 || denied[0] != "b1#1/secret/path.secret" {
		t.Errorf("denials recorded = %v, want the inner call's under b1#1", denied)
	}
	if len(qualified) != 1 || qualified[0] != "b1#2/qualified" {
		t.Errorf("qualifications recorded = %v, want the inner call's under b1#2", qualified)
	}
	results := toolResults(t, h)
	if len(results) != 1 || !strings.Contains(results["b1"], "refused: credential path") {
		t.Errorf("the wire must carry one result, holding the refusal: %v", results)
	}
}

// Line caps do not bound a result whose answer is one enormous line.
func TestBatchBoundsBytesAsWellAsLines(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("b1", tools.BatchToolName, `{"calls":[{"tool":"minified"}]}`),
		textTurn("done"),
	}, nil)
	withBatch(t, h)
	registerBatchable(t, h, "minified", func(context.Context, tools.Call) tools.Result {
		return tools.Result{Text: strings.Repeat("x", 4*maxBatchCallBytes) + "\nsecond line\n"}
	})
	if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
		t.Fatal(err)
	}
	text := toolResults(t, h)["b1"]
	if len(text) > maxBatchCallBytes+1024 {
		t.Errorf("a one-line answer of %d bytes produced a %d-byte batch result", 4*maxBatchCallBytes, len(text))
	}
	if !strings.Contains(text, "byte bound") || !strings.Contains(text, "second line") {
		t.Errorf("the clip must be named and the lines that fit kept:\n%.300s", text)
	}
}

// Each inner handler is told how much of its answer will reach the model, so a
// side effect that assumes the whole answer was seen can decline.
func TestBatchTellsEachHandlerWhatItsAnswerWillBeCutTo(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("b1", tools.BatchToolName, `{"calls":[{"tool":"see","args":{"n":1}},{"tool":"see","args":{"n":2},"match":"x","max_lines":7}]}`),
		textTurn("done"),
	}, nil)
	withBatch(t, h)
	var mu sync.Mutex
	views := map[string]tools.View{}
	registerBatchable(t, h, "see", func(ctx context.Context, call tools.Call) tools.Result {
		v, ok := tools.ViewFrom(ctx)
		if !ok {
			return tools.Errorf("no view")
		}
		mu.Lock()
		views[string(call.Arguments)] = v
		mu.Unlock()
		return tools.Result{Text: "x"}
	})
	if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
		t.Fatal(err)
	}
	if v := views[`{"n":1}`]; v.MaxLines != defaultBatchLines || v.Filtered || v.MaxBytes != maxBatchCallBytes {
		t.Errorf("default view = %+v", v)
	}
	if v := views[`{"n":2}`]; v.MaxLines != 7 || !v.Filtered {
		t.Errorf("filtered view = %+v", v)
	}
	if (tools.View{MaxLines: 10, MaxBytes: 100}).Whole(10, 100) != true ||
		(tools.View{MaxLines: 10, MaxBytes: 100, Filtered: true}).Whole(1, 1) != false ||
		(tools.View{MaxLines: 10, MaxBytes: 100}).Whole(11, 1) != false {
		t.Error("View.Whole is wrong at its edges")
	}
}

// serialProbe registers a batchable "probe" whose every answer differs, so
// each run reads as progress, and returns how many times it ran.
func serialProbe(t *testing.T, h *harness) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	registerBatchable(t, h, "probe", func(context.Context, tools.Call) tools.Result {
		return tools.Result{Text: fmt.Sprintf("answer %d", n.Add(1))}
	})
	return &n
}

func probeTurns(ids ...string) []replay.Turn {
	var turns []replay.Turn
	for i, id := range ids {
		turns = append(turns, toolTurn(id, "probe", fmt.Sprintf(`{"n":%d}`, i)))
	}
	return turns
}

func longTurn(c *Config) { c.MaxSteps = 40 }

// The fifth lone lookup in a row is refused once, without running, and the
// same call sent again runs. The refusal is the harness's decision, so it must
// not enter the repeat ledger (three more identical calls still run) and must
// be charged as one step, not as a stall.
func TestSerialLookupsRefuseTheFifthOnceThenRun(t *testing.T) {
	turns := probeTurns("s1", "s2", "s3", "s4")
	for _, id := range []string{"s5", "s6", "s7", "s8"} {
		turns = append(turns, toolTurn(id, "probe", `{"n":"again"}`))
	}
	h := build(t, append(turns, textTurn("done")), longTurn)
	withBatch(t, h)
	ran := serialProbe(t, h)

	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatal(err)
	}
	results := toolResults(t, h)
	if !strings.Contains(results["s3"], "refused once") {
		t.Errorf("the note at three does not warn of the refusal: %q", results["s3"])
	}
	if !strings.Contains(results["s5"], "refused, not run") {
		t.Fatalf("the fifth lone lookup ran: %q", results["s5"])
	}
	for _, id := range []llm.CallID{"s6", "s7", "s8"} {
		if !strings.HasPrefix(results[id], "answer ") {
			t.Errorf("%s did not run after the refusal: %q", id, results[id])
		}
	}
	if ran.Load() != 7 {
		t.Errorf("probe ran %d times, want 7 (s5 refused, s6-s8 not stopped by the repeat ledger)", ran.Load())
	}
	if out.BudgetSpent != 9 || out.NoProgressSteps != 0 {
		t.Errorf("BudgetSpent=%d NoProgressSteps=%d, want 9 and 0: the refused step is one step, not a stall",
			out.BudgetSpent, out.NoProgressSteps)
	}
}

// Wrapping a lone lookup in a one-call batch is still a lone lookup.
func TestSerialLookupsCountAOneCallBatch(t *testing.T) {
	turns := append(probeTurns("s1", "s2", "s3", "s4"),
		toolTurn("b5", tools.BatchToolName, `{"calls":[{"tool":"probe","args":{"n":"wrapped"}}]}`),
		textTurn("done"))
	h := build(t, turns, longTurn)
	withBatch(t, h)
	ran := serialProbe(t, h)
	if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
		t.Fatal(err)
	}
	if got := toolResults(t, h)["b5"]; !strings.Contains(got, "refused, not run") || ran.Load() != 4 {
		t.Errorf("a one-call batch escaped the rule (ran %d): %q", ran.Load(), got)
	}
}

// A step that batches two lookups, or makes a call that is not a lookup, ends
// the run: the count starts again and nothing is refused.
func TestSerialLookupsResetOnABatchOrANonLookup(t *testing.T) {
	for name, breaker := range map[string]replay.Turn{
		"two-call batch": toolTurn("x", tools.BatchToolName, `{"calls":[{"tool":"probe","args":{"n":"a"}},{"tool":"probe","args":{"n":"b"}}]}`),
		"non-lookup":     toolTurn("x", "edit", `{}`),
	} {
		t.Run(name, func(t *testing.T) {
			turns := append(probeTurns("s1", "s2", "s3", "s4"), breaker)
			turns = append(turns, probeTurns("t1", "t2", "t3", "t4")...)
			h := build(t, append(turns, textTurn("done")), longTurn)
			withBatch(t, h)
			serialProbe(t, h)
			if err := h.tools.Register(tools.Tool{
				Schema:  llm.ToolSchema{Name: "edit", Description: "edit", InputSchema: []byte(`{"type":"object","properties":{}}`)},
				Handler: func(context.Context, tools.Call) tools.Result { return tools.Result{Text: "edited"} },
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
				t.Fatal(err)
			}
			for id, text := range toolResults(t, h) {
				if strings.Contains(text, "refused, not run") {
					t.Errorf("%s was refused although the run was broken: %q", id, text)
				}
			}
		})
	}
}

// off says nothing, note notes every three and never refuses, and with no
// batch offered there is nothing to point at, so nothing is refused either.
func TestSerialLookupModes(t *testing.T) {
	ids := []string{"s1", "s2", "s3", "s4", "s5", "s6"}
	for _, c := range []struct {
		name    string
		mode    SerialLookups
		offered bool
		notes   int
	}{
		{"off", SerialLookupsOff, true, 0},
		{"note", SerialLookupsNote, true, 2},
		{"refuse without a batch", SerialLookupsRefuse, false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := build(t, append(probeTurns(ids...), textTurn("done")), func(cfg *Config) {
				cfg.MaxSteps = 40
				cfg.SerialLookups = c.mode
			})
			if c.offered {
				withBatch(t, h)
			}
			ran := serialProbe(t, h)
			if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
				t.Fatal(err)
			}
			notes := 0
			for _, text := range toolResults(t, h) {
				notes += strings.Count(text, "[harness:")
			}
			if notes != c.notes || ran.Load() != int64(len(ids)) {
				t.Errorf("notes=%d ran=%d, want %d and %d", notes, ran.Load(), c.notes, len(ids))
			}
		})
	}
}

func TestNewLoopRefusesAnUnknownSerialLookupMode(t *testing.T) {
	_, err := NewLoop(Config{Provider: replay.New(replay.Fixture{Provider: "replay"}), Model: "m", MaxSteps: 1,
		SerialLookups: "always"}, bus.New(), session.NewLog(), nil)
	if err == nil || !strings.Contains(err.Error(), "SerialLookups") {
		t.Errorf("an unknown mode was accepted: %v", err)
	}
}
