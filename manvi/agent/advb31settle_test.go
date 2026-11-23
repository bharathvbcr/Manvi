package agent

import (
	"context"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
	"github.com/bharathvbcr/Manvi/manvi/session"
)

// UnknownKinds is the only evidence that a settled turn is shorter than the
// stream. It has to survive the merge across steps and reach the Outcome and
// the evidence trail, or the report exists and nobody can read it.
func TestAdvB31SettleMergeDecodingKeepsUnknownKinds(t *testing.T) {
	got := mergeDecoding(llm.DecodingReport{}, llm.DecodingReport{UnknownKinds: []string{"mystery_block"}, UnknownKindsMore: 3})
	if len(got.UnknownKinds) != 1 || got.UnknownKinds[0] != "mystery_block" || got.UnknownKindsMore != 3 {
		t.Fatalf("mergeDecoding dropped the unknown-kind report: %+v", got)
	}
	got = mergeDecoding(llm.DecodingReport{UnknownKinds: []string{"a"}}, llm.DecodingReport{UnknownKinds: []string{"b"}})
	if len(got.UnknownKinds) != 2 {
		t.Fatalf("a later step's unknown kinds erased an earlier step's: %+v", got)
	}
}

func TestAdvB31SettleLoopReportsUnknownKindsOnTheOutcomeAndLog(t *testing.T) {
	turn := textTurn("done")
	turn.Decoding = llm.DecodingReport{UnknownKinds: []string{"mystery_block"}}
	h := build(t, []replay.Turn{turn}, nil)
	out, err := h.loop.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Decoding.UnknownKinds) != 1 || out.Decoding.UnknownKinds[0] != "mystery_block" {
		t.Errorf("Outcome.Decoding = %+v; the unknown type was reported by the adapter and lost by the loop", out.Decoding)
	}
	var logged int
	for _, e := range h.log.Events() {
		if e.Type == session.DecodingCompensated {
			logged++
			t.Logf("DecodingCompensated payload: %s", e.Data)
		}
	}
	if logged == 0 {
		t.Fatal("no DecodingCompensated event at all")
	}
}
