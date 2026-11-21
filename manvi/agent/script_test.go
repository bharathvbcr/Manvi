package agent

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
	"github.com/bharathvbcr/Manvi/manvi/session"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

func withScript(t *testing.T, h *harness) {
	t.Helper()
	withBatch(t, h)
	if err := h.tools.Register(tools.ScriptTool()); err != nil {
		t.Fatal(err)
	}
}

func scriptArgs(t *testing.T, script string, maxCalls int) string {
	t.Helper()
	m := map[string]any{"script": script}
	if maxCalls != 0 {
		m["max_calls"] = maxCalls
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// scriptHarness is a loop whose one step is a script call, answered by "done".
func scriptHarness(t *testing.T, script string, maxCalls int) *harness {
	t.Helper()
	h := build(t, []replay.Turn{
		toolTurn("s1", tools.ScriptToolName, scriptArgs(t, script, maxCalls)),
		textTurn("done"),
	}, nil)
	withScript(t, h)
	return h
}

func runScriptHarness(t *testing.T, h *harness) (Outcome, string) {
	t.Helper()
	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatal(err)
	}
	return out, toolResults(t, h)["s1"]
}

func shortWallTime(t *testing.T, d time.Duration) {
	t.Helper()
	old := scriptWallTime
	scriptWallTime = d
	t.Cleanup(func() { scriptWallTime = old })
}

// The point of a script: the second call's arguments come from the first
// call's answer, in one step, behind one wire result.
func TestScriptChainsOneCallsAnswerIntoTheNext(t *testing.T) {
	h := scriptHarness(t, `
first = find_it()
found = json.decode(first.text)
second = use_it(path=found["path"], n=len(found["lines"]))
print("second saw:", second.text)
`, 0)
	registerBatchable(t, h, "find_it", func(context.Context, tools.Call) tools.Result {
		return tools.Result{Text: `{"path":"a/b.go","lines":[1,2,3]}`}
	})
	var got string
	registerBatchable(t, h, "use_it", func(_ context.Context, c tools.Call) tools.Result {
		got = string(c.Arguments)
		return tools.Result{Text: "used " + string(c.Arguments)}
	})

	out, text := runScriptHarness(t, h)
	if !strings.Contains(got, `"path":"a/b.go"`) || !strings.Contains(got, `"n":3`) {
		t.Fatalf("the second call did not get the first call's answer: args = %q\n%s", got, text)
	}
	if !strings.Contains(text, "script: 2 call(s) made") || !strings.Contains(text, "second saw: used") {
		t.Errorf("script result = %q", text)
	}
	if n := len(toolResults(t, h)); n != 1 {
		t.Errorf("the wire carries %d tool results, want only the script's", n)
	}
	if out.ToolCalls != 3 {
		t.Errorf("ToolCalls = %d, want 3 (the script and its two calls)", out.ToolCalls)
	}
}

// A refusal or a failed call is data the script can branch on, not a crash:
// Starlark has no try.
func TestScriptSeesRefusalsAndErrorsAsValues(t *testing.T) {
	h := scriptHarness(t, `
bad = fails()
print("error:", bad.error, "refused:", bad.refused)
w = writer()
print("writer refused:", w.refused, w.text)
b = devcouncil_batch(calls=[])
print("batch refused:", b.refused, b.text)
s = devcouncil_script(script="print(1)")
print("script refused:", s.refused, s.text)
`, 0)
	registerBatchable(t, h, "fails", func(context.Context, tools.Call) tools.Result { return tools.Errorf("nope") })
	wrote := 0
	if err := h.tools.Register(tools.Tool{
		Schema:   llm.ToolSchema{Name: "writer", Description: "writer", InputSchema: []byte(`{"type":"object","properties":{}}`)},
		ReadOnly: true,
		Handler:  func(context.Context, tools.Call) tools.Result { wrote++; return tools.Result{Text: "wrote"} },
	}); err != nil {
		t.Fatal(err)
	}
	_, text := runScriptHarness(t, h)
	if wrote != 0 {
		t.Fatalf("a tool that is not batchable ran inside a script")
	}
	for _, want := range []string{"error: True refused: False", "writer refused: True writer cannot run inside a script",
		"batch refused: True a script cannot contain a batch", "script refused: True a script cannot contain a script"} {
		if !strings.Contains(text, want) {
			t.Errorf("script result lacks %q:\n%s", want, text)
		}
	}
}

// The repeat ledger a top-level call is counted in covers a script's calls.
func TestScriptCannotWalkAroundTheRepeatRefusal(t *testing.T) {
	h := scriptHarness(t, fmt.Sprintf(`
for i in range(%d):
    r = probe(n="same")
    print(i, r.refused, r.text)
`, RepeatLimit+2), 0)
	ran := serialProbe(t, h)
	_, text := runScriptHarness(t, h)
	if int(ran.Load()) != RepeatLimit {
		t.Errorf("probe ran %d times, want %d: the ledger did not count the script's calls\n%s", ran.Load(), RepeatLimit, text)
	}
	if !strings.Contains(text, "was already made") {
		t.Errorf("no repeat refusal reached the script's output:\n%s", text)
	}
}

func TestScriptRefusesAToolThatWasNotOffered(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("s1", tools.ScriptToolName, scriptArgs(t, "r = hidden()\nprint(r.refused, r.text)", 0)),
		textTurn("done"),
	}, func(c *Config) { c.CoreToolsOnly = true })
	withScript(t, h)
	ran := 0
	if err := h.tools.Register(tools.Tool{
		Schema:   llm.ToolSchema{Name: "hidden", Description: "hidden", InputSchema: []byte(`{"type":"object","properties":{}}`)},
		ReadOnly: true, Batchable: true, Extended: true,
		Handler: func(context.Context, tools.Call) tools.Result { ran++; return tools.Result{Text: "x"} },
	}); err != nil {
		t.Fatal(err)
	}
	_, text := runScriptHarness(t, h)
	if ran != 0 || !strings.Contains(text, "was not offered") {
		t.Fatalf("a tool outside the offered set ran (%d) or was not refused: %s", ran, text)
	}
}

// Every inner call's verdict is recorded under a derived ID, exactly as a
// batch item's is.
func TestScriptRecordsTheGatesVerdictOnEachCall(t *testing.T) {
	h := scriptHarness(t, `
secret()
qualified()
plain()
`, 0)
	registerBatchable(t, h, "secret", func(context.Context, tools.Call) tools.Result {
		return tools.Result{IsError: true, Blocked: true, Rule: "path.secret", Severity: "hard", Text: "refused: credential path"}
	})
	registerBatchable(t, h, "qualified", func(context.Context, tools.Call) tools.Result {
		return tools.Result{Text: "answered", Rule: "scope.unplanned", Degraded: []string{"index stale"}}
	})
	registerBatchable(t, h, "plain", func(context.Context, tools.Call) tools.Result { return tools.Result{Text: "fine"} })
	out, _ := runScriptHarness(t, h)
	if out.Denied != 1 || out.Qualified != 1 {
		t.Fatalf("Denied=%d Qualified=%d, want 1 and 1", out.Denied, out.Qualified)
	}
	var seen []string
	for _, e := range h.log.Events() {
		switch e.Type {
		case session.PolicyDenied:
			var d session.DenialData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatal(err)
			}
			seen = append(seen, "denied:"+string(d.ToolCallID)+"/"+d.Tool+"/"+d.Rule)
		case session.PolicyQualified:
			var q session.QualificationData
			if err := json.Unmarshal(e.Data, &q); err != nil {
				t.Fatal(err)
			}
			seen = append(seen, "qualified:"+string(q.ToolCallID)+"/"+q.Tool)
		}
	}
	want := []string{"denied:s1#1/secret/path.secret", "qualified:s1#2/qualified"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Errorf("recorded %v, want %v", seen, want)
	}
}

func TestScriptBounds(t *testing.T) {
	probeCalls := func(n int) string {
		return fmt.Sprintf("print('before')\nfor i in range(%d):\n    probe(n=i)\nprint('after')", n)
	}
	t.Run("max_calls ends the script and keeps its output", func(t *testing.T) {
		h := scriptHarness(t, probeCalls(10), 3)
		ran := serialProbe(t, h)
		out, text := runScriptHarness(t, h)
		if ran.Load() != 3 || !strings.Contains(text, "more than its 3 calls") || !strings.Contains(text, "before") ||
			strings.Contains(text, "after") || !strings.Contains(text, "script: 3 call(s) made") {
			t.Errorf("ran %d, result:\n%s", ran.Load(), text)
		}
		if out.Steps == 0 {
			t.Errorf("the turn did not go on")
		}
	})
	t.Run("default is 16", func(t *testing.T) {
		h := scriptHarness(t, probeCalls(30), 0)
		ran := serialProbe(t, h)
		_, text := runScriptHarness(t, h)
		if ran.Load() != tools.DefaultScriptCalls || !strings.Contains(text, "more than its 16 calls") {
			t.Errorf("ran %d, result:\n%s", ran.Load(), text)
		}
	})
	t.Run("hard max is 32", func(t *testing.T) {
		for _, n := range []int{33, 0 - 1} {
			h := scriptHarness(t, probeCalls(2), n)
			ran := serialProbe(t, h)
			_, text := runScriptHarness(t, h)
			if ran.Load() != 0 || !strings.Contains(text, "max_calls must be in 1..32") {
				t.Errorf("max_calls=%d: ran %d, result:\n%s", n, ran.Load(), text)
			}
		}
	})
	t.Run("script size", func(t *testing.T) {
		h := scriptHarness(t, "print(1)\n"+strings.Repeat("#", tools.MaxScriptBytes), 0)
		_, text := runScriptHarness(t, h)
		if !strings.Contains(text, "the limit is") {
			t.Errorf("an oversized script was not refused: %.200s", text)
		}
	})
	t.Run("interpreter steps", func(t *testing.T) {
		h := scriptHarness(t, "print('before')\nfor i in range(100000000):\n    pass\nprint('after')", 0)
		start := time.Now()
		_, text := runScriptHarness(t, h)
		if !strings.Contains(text, "interpreter steps") || !strings.Contains(text, "before") || strings.Contains(text, "after") {
			t.Errorf("result:\n%s", text)
		}
		if time.Since(start) > 20*time.Second {
			t.Errorf("the step bound took %s", time.Since(start))
		}
	})
	t.Run("wall time covers a call that blocks", func(t *testing.T) {
		shortWallTime(t, 150*time.Millisecond)
		h := scriptHarness(t, "print('before')\nblock()\nprint('after')", 0)
		registerBatchable(t, h, "block", func(ctx context.Context, _ tools.Call) tools.Result {
			<-ctx.Done()
			return tools.Errorf("interrupted")
		})
		_, text := runScriptHarness(t, h)
		if !strings.Contains(text, "wall-time bound") || !strings.Contains(text, "before") || strings.Contains(text, "after") {
			t.Errorf("result:\n%s", text)
		}
	})
	t.Run("wall time covers pure computation", func(t *testing.T) {
		shortWallTime(t, 150*time.Millisecond)
		h := scriptHarness(t, "print('before')\nwhile True:\n    pass", 0)
		_, text := runScriptHarness(t, h)
		if !strings.Contains(text, "wall-time bound") && !strings.Contains(text, "interpreter steps") {
			t.Errorf("result:\n%s", text)
		}
	})
	t.Run("one answer is cut at the per-call bound", func(t *testing.T) {
		h := scriptHarness(t, "r = big()\nprint(len(r.text), r.truncated)", 0)
		registerBatchable(t, h, "big", func(context.Context, tools.Call) tools.Result {
			return tools.Result{Text: strings.Repeat("é", 40<<10)}
		})
		_, text := runScriptHarness(t, h)
		want := fmt.Sprintf("%d True", maxBatchCallBytes)
		if !strings.Contains(text, want) && !strings.Contains(text, fmt.Sprintf("%d True", maxBatchCallBytes-1)) {
			t.Errorf("result:\n%s", text)
		}
	})
	t.Run("total output", func(t *testing.T) {
		h := scriptHarness(t, "line = 'x' * 1000\nfor i in range(500):\n    print(line)\nprint('after')", 0)
		_, text := runScriptHarness(t, h)
		if len(text) > maxScriptOutput+1024 || !strings.Contains(text, "printed more than") || strings.Contains(text, "after") {
			t.Errorf("result is %d bytes: %.120s ... %s", len(text), text, text[max(0, len(text)-200):])
		}
	})
}

// A read made inside a script is never a read in full: the answer's deferred
// side effect (read_file's overwrite-guard mark) is not committed, whatever the
// script printed. The same call in a batch, shown whole, commits it.
func TestScriptNeverCommitsAReadsOnShown(t *testing.T) {
	register := func(h *harness, marked *atomic.Int64) {
		registerBatchable(t, h, "reader", func(ctx context.Context, _ tools.Call) tools.Result {
			if _, inView := tools.ViewFrom(ctx); !inView {
				marked.Add(1)
				return tools.Result{Text: "whole file"}
			}
			return tools.Result{Text: "whole file", OnShown: func() { marked.Add(1) }}
		})
	}
	var inScript atomic.Int64
	h := scriptHarness(t, "r = reader()\nprint(r.text)", 0)
	register(h, &inScript)
	_, text := runScriptHarness(t, h)
	if !strings.Contains(text, "whole file") || inScript.Load() != 0 {
		t.Fatalf("script run marked the read %d time(s) (want 0); result:\n%s", inScript.Load(), text)
	}

	var inBatch atomic.Int64
	hb := build(t, []replay.Turn{toolTurn("b1", tools.BatchToolName, `{"calls":[{"tool":"reader"}]}`), textTurn("done")}, nil)
	withScript(t, hb)
	register(hb, &inBatch)
	if _, err := hb.loop.Run(context.Background(), userMsg("go")); err != nil {
		t.Fatal(err)
	}
	if inBatch.Load() != 1 {
		t.Fatalf("control: the batch committed the read %d time(s), want 1", inBatch.Load())
	}
}

// The script has no way out of the interpreter: no load, no files, no clock,
// no network, no randomness.
func TestScriptIsHermetic(t *testing.T) {
	for name, tc := range map[string]struct{ script, want string }{
		"load":    {"load('other.star', 'x')\nprint(x)", "load not implemented"},
		"open":    {"open('/etc/passwd')", "undefined: open"},
		"time":    {"print(time.now())", "undefined: time"},
		"random":  {"print(random.random())", "undefined: random"},
		"import":  {"import os", "got import"},
		"getenv":  {"print(os.getenv('HOME'))", "undefined: os"},
		"http":    {"http.get('http://example.com')", "undefined: http"},
		"recurse": {"def f(n):\n    return f(n)\nf(1)", "called recursively"},
	} {
		t.Run(name, func(t *testing.T) {
			h := scriptHarness(t, tc.script, 0)
			_, text := runScriptHarness(t, h)
			if !strings.Contains(text, "script error") || !strings.Contains(strings.ToLower(text), tc.want) {
				t.Errorf("want a script error containing %q, got:\n%s", tc.want, text)
			}
		})
	}
}

// Cancelling the turn ends a script that never yields.
func TestScriptEndsWhenTheTurnIsCancelled(t *testing.T) {
	h := scriptHarness(t, "while True:\n    pass", 0)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = h.loop.Run(ctx, userMsg("go"))
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("a cancelled turn did not end the script")
	}
	if got := toolResults(t, h)["s1"]; got != "" && !strings.Contains(got, "cancelled") && !strings.Contains(got, "interpreter steps") {
		t.Errorf("result = %q", got)
	}
}

// A step holding a script call is not a lone lookup: it resets the run, so a
// script is not counted toward the serial-lookup refusal.
func TestScriptCallResetsTheSerialLookupRun(t *testing.T) {
	turns := append(probeTurns("s1", "s2", "s3", "s4"),
		toolTurn("sc", tools.ScriptToolName, scriptArgs(t, "probe(n='in script')", 0)))
	turns = append(turns, probeTurns("t1", "t2", "t3", "t4")...)
	h := build(t, append(turns, textTurn("done")), longTurn)
	withScript(t, h)
	serialProbe(t, h)
	if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
		t.Fatal(err)
	}
	for id, text := range toolResults(t, h) {
		if strings.Contains(text, "refused, not run") {
			t.Errorf("%s was refused although a script call broke the run: %q", id, text)
		}
	}
}
