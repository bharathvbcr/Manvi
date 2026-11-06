package gemini

import (
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
)

func drainChunks(t *testing.T, body string) ([]llm.Chunk, llm.Response, error) {
	t.Helper()
	adapter, _ := adapterFor(t, body)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	return adaptertest.Drain(stream)
}

// TestAnIndexedStepDeltaKeepsEveryTextPart.
//
// applyStep returned on the first non-empty part, so a step.delta carrying two
// text parts lost the second from the message while the continuation, which
// reads every part, kept it; Response then refused the turn because the private
// continuation differed from the journaled assistant content.
func TestAnIndexedStepDeltaKeepsEveryTextPart(t *testing.T) {
	chunks, resp, err := drainChunks(t, sse(
		[2]string{"step.start", `{"event_type":"step.start","index":0,"step":{"type":"model_output"}}`},
		[2]string{"step.delta", `{"event_type":"step.delta","index":0,"step":{"type":"model_output","content":[{"type":"text","text":"first "},{"type":"text","text":"second"}]}}`},
		[2]string{"step.stop", `{"event_type":"step.stop","index":0}`},
		completedWith("completed"), doneFrame))
	if err != nil {
		t.Fatalf("an indexed step with two text parts failed: %v", err)
	}
	if got := resp.Message.Text(); got != "first second" {
		t.Errorf("Message.Text() = %q, want both parts", got)
	}
	if got := adaptertest.TextOf(chunks); got != "first second" {
		t.Errorf("streamed text = %q, want both parts", got)
	}
}

// TestAnUnindexedStepDeltaKeepsEveryTextPart: with no index there is no
// continuation to disagree, so the second part was dropped without any error.
func TestAnUnindexedStepDeltaKeepsEveryTextPart(t *testing.T) {
	chunks, resp, err := drainChunks(t, sse(
		[2]string{"step.delta", `{"event_type":"step.delta","step":{"type":"model_output","content":[{"type":"text","text":"first "},{"type":"text","text":"second"}]}}`},
		completedWith("completed"), doneFrame))
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Message.Text(); got != "first second" {
		t.Errorf("Message.Text() = %q, want both parts", got)
	}
	if got := adaptertest.TextOf(chunks); got != "first second" {
		t.Errorf("streamed text = %q, want both parts", got)
	}
}

// TestAStepDeltaKeepsMixedPartsInOrder: text and thought parts in one step each
// reach their accumulator, as their own chunk and in the order they arrived.
func TestAStepDeltaKeepsMixedPartsInOrder(t *testing.T) {
	chunks, resp, err := drainChunks(t, sse(
		[2]string{"step.delta", `{"event_type":"step.delta","step":{"type":"model_output","content":[{"type":"text","text":"a"},{"type":"thought","thought":"t1"},{"type":"text","text":""},{"type":"text","text":"b"},{"type":"thought","text":"t2"}]}}`},
		completedWith("completed"), doneFrame))
	if err != nil {
		t.Fatal(err)
	}
	type part struct {
		kind llm.ChunkKind
		text string
	}
	want := []part{{llm.ChunkText, "a"}, {llm.ChunkReasoning, "t1"}, {llm.ChunkText, "b"}, {llm.ChunkReasoning, "t2"}}
	if len(chunks) != len(want) {
		t.Fatalf("chunks = %+v, want %d", chunks, len(want))
	}
	for i, w := range want {
		if chunks[i].Kind != w.kind || chunks[i].Text != w.text {
			t.Errorf("chunk %d = %v %q, want %v %q", i, chunks[i].Kind, chunks[i].Text, w.kind, w.text)
		}
	}
	if chunks[0].BlockIndex != chunks[2].BlockIndex || chunks[1].BlockIndex != chunks[3].BlockIndex ||
		chunks[0].BlockIndex == chunks[1].BlockIndex {
		t.Errorf("block indexes = %d %d %d %d; each kind should keep one block, distinct from the other",
			chunks[0].BlockIndex, chunks[1].BlockIndex, chunks[2].BlockIndex, chunks[3].BlockIndex)
	}
	if got := resp.Message.Text(); got != "ab" {
		t.Errorf("Message.Text() = %q", got)
	}
}

// TestASinglePartStepDeltaEmitsExactlyOneChunk guards the common shape against
// the queue that multi-part steps need.
func TestASinglePartStepDeltaEmitsExactlyOneChunk(t *testing.T) {
	chunks, resp, err := drainChunks(t, sse(
		[2]string{"step.delta", `{"event_type":"step.delta","step":{"type":"model_output","content":[{"type":"text","text":"only"}]}}`},
		completedWith("completed"), doneFrame))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || chunks[0].Kind != llm.ChunkText || chunks[0].Text != "only" {
		t.Errorf("chunks = %+v, want exactly one text chunk %q", chunks, "only")
	}
	if got := resp.Message.Text(); got != "only" {
		t.Errorf("Message.Text() = %q", got)
	}
}
