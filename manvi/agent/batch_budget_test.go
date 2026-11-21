package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// A handler is told, in its View, how much of its answer the batch will show.
// read_file trusts that number to decide a file was read in full, and the
// write guard trusts read_file. So the fold must never cut an answer that fits
// the view it was given: five answers that each fill their view all arrive
// whole. The fold used to give each call 32 KiB in the view and only what the
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
		n := min(view.MaxLines, view.MaxBytes/len(line))
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
