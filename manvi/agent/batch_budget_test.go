package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// A handler is told, in its View, the most of its answer a batch can show.
// Answers that fit the batch must arrive whole: five answers of 20 KiB all
// arrive whole. The fold used to give each call 32 KiB in the view and only what the
// earlier calls had left of 128 KiB when cutting, so the later ones were cut
// after being told they would not be.
func TestBatchNeverCutsAnAnswerThatFitsItsView(t *testing.T) {
	var calls []string
	for i := range 5 {
		calls = append(calls, fmt.Sprintf(`{"tool":"fill","args":{"n":%d},"max_lines":2000}`, i))
	}
	h := build(t, []replay.Turn{
		toolTurn("b1", tools.BatchToolName, `{"calls":[`+strings.Join(calls, ",")+`]}`),
		textTurn("done"),
	}, nil)
	withBatch(t, h)
	registerBatchable(t, h, "fill", func(ctx context.Context, _ tools.Call) tools.Result {
		view, ok := tools.ViewFrom(ctx)
		if !ok {
			return tools.Errorf("no view")
		}
		line := strings.Repeat("x", 63) + "\n"
		n := min(view.MaxLines, min(view.MaxBytes, 20<<10)/len(line))
		return tools.Result{Text: strings.Repeat(line, n)}
	})
	if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
		t.Fatal(err)
	}
	got := toolResults(t, h)["b1"]
	if strings.Contains(got, "byte bound") || strings.Contains(got, "lines shown") {
		t.Fatalf("an answer that fit its view was cut: %s", got[strings.LastIndex(got, "## ["):])
	}
}

// A short name a model read somewhere is told the full one, not that its
// lookup is not batchable.
func TestBatchNamesTheFullToolForAShortName(t *testing.T) {
	h := build(t, []replay.Turn{
		toolTurn("b1", tools.BatchToolName, `{"calls":[{"tool":"probe"},{"tool":"nothing"}]}`),
		textTurn("done"),
	}, nil)
	withBatch(t, h)
	registerBatchable(t, h, "devcouncil_probe", func(context.Context, tools.Call) tools.Result {
		return tools.Result{Text: "ok"}
	})
	if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
		t.Fatal(err)
	}
	got := toolResults(t, h)["b1"]
	for _, want := range []string{"no tool is named probe; the lookup is devcouncil_probe", "no tool is named nothing"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
}

// markRead registers a batchable tool whose answer is text and whose deferred
// side effect records its name in *shown, standing in for read_file's mark.
func markRead(t *testing.T, h *harness, name string, answer func() string, mu *sync.Mutex, shown *[]string) {
	t.Helper()
	registerBatchable(t, h, name, func(context.Context, tools.Call) tools.Result {
		return tools.Result{Text: answer(), OnShown: func() {
			mu.Lock()
			defer mu.Unlock()
			*shown = append(*shown, name)
		}}
	})
}

// A pending read mark is committed only for an answer the fold showed whole:
// the later files of a batch that overruns its byte bound are cut, so they must
// stay unmarked, and so must an answer a filter or a line cap shortened.
func TestBatchCommitsOnlyTheMarksOfAnswersItShowedWhole(t *testing.T) {
	big := strings.Repeat(strings.Repeat("x", 1023)+"\n", 30) // 30 KiB: fits a call, five do not fit 128 KiB
	var mu sync.Mutex
	var shown []string
	h := build(t, []replay.Turn{
		toolTurn("b1", tools.BatchToolName, `{"calls":[
			{"tool":"f1"},{"tool":"f2"},{"tool":"f3"},{"tool":"f4"},{"tool":"f5"},
			{"tool":"lines","max_lines":2},{"tool":"match","match":"zzz"}]}`),
		textTurn("done"),
	}, nil)
	withBatch(t, h)
	for i := 1; i <= 5; i++ {
		markRead(t, h, fmt.Sprintf("f%d", i), func() string { return big }, &mu, &shown)
	}
	markRead(t, h, "lines", func() string { return "a\nb\nc\n" }, &mu, &shown)
	markRead(t, h, "match", func() string { return "alpha\n" }, &mu, &shown)
	if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
		t.Fatal(err)
	}
	got := toolResults(t, h)["b1"]
	if !strings.Contains(got, "byte bound") {
		t.Fatalf("the fixture no longer overruns the batch: %s", got[len(got)-200:])
	}
	want := []string{"f1", "f2", "f3", "f4"}
	if strings.Join(shown, ",") != strings.Join(want, ",") {
		t.Errorf("committed marks = %v, want %v (f5 was cut, lines was capped, match was filtered)", shown, want)
	}
}

// The budget follows what remains: sixteen calls, fifteen of them tiny and one
// of 30 KiB, show every answer whole and commit every mark. A fixed share of
// 128 KiB over sixteen calls was 8 KiB, which cut the large one.
func TestBatchBudgetAdaptsToWhatRemains(t *testing.T) {
	big := strings.Repeat(strings.Repeat("x", 1023)+"\n", 30)
	var calls []string
	for i := range 16 {
		calls = append(calls, fmt.Sprintf(`{"tool":"f%d"}`, i))
	}
	var mu sync.Mutex
	var shown []string
	h := build(t, []replay.Turn{
		toolTurn("b1", tools.BatchToolName, `{"calls":[`+strings.Join(calls, ",")+`]}`),
		textTurn("done"),
	}, nil)
	withBatch(t, h)
	for i := range 16 {
		answer := "tiny\n"
		if i == 7 {
			answer = big
		}
		markRead(t, h, fmt.Sprintf("f%d", i), func() string { return answer }, &mu, &shown)
	}
	if _, err := h.loop.Run(context.Background(), userMsg("go")); err != nil {
		t.Fatal(err)
	}
	if got := toolResults(t, h)["b1"]; strings.Contains(got, "byte bound") || strings.Contains(got, "lines shown") {
		t.Fatalf("an answer that fit the batch was cut: %s", got[len(got)-300:])
	}
	if len(shown) != 16 {
		t.Errorf("committed %d marks, want 16: %v", len(shown), shown)
	}
}

// A line that does not fit the byte budget ends what the batch shows. Skipping
// it and keeping a later, shorter line left a hole the model could not see: the
// kept lines read as consecutive.
func TestBatchKeepStopsAtTheFirstLineThatDoesNotFit(t *testing.T) {
	slot := &batchSlot{limit: 100, result: tools.Result{Text: "one\n" + strings.Repeat("x", 200) + "\nthree\n"}}
	text, whole := batchKeep(slot, 50)
	if whole {
		t.Error("an answer cut at the byte bound was reported whole")
	}
	if !strings.Contains(text, "one\n") {
		t.Errorf("the line before the oversized one was dropped:\n%s", text)
	}
	if strings.Contains(text, "three") {
		t.Errorf("a line after the one that did not fit was kept, leaving a silent hole:\n%s", text)
	}
	if !strings.Contains(text, "1 of 3 lines shown") || !strings.Contains(text, "stops after 1 line(s)") {
		t.Errorf("the note does not say where the answer stopped and why:\n%s", text)
	}
}
