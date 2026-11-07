package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
)

// Adversarial tests for the multi-part step.delta queue (s.pending) and for
// calls withheld at max_tokens. See the w7 review.

func advStream(t *testing.T, body string) llm.Stream {
	t.Helper()
	adapter, _ := adapterFor(t, body)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	return stream
}

// advDrain reads every chunk, then Response. It returns the emitted text and
// reasoning concatenated, so the stream can be compared with the settled message.
func advDrain(t *testing.T, stream llm.Stream) (text, reasoning string, chunks int, resp llm.Response, err error) {
	t.Helper()
	var tb, rb strings.Builder
	for {
		c, nerr := stream.Next()
		if nerr != nil {
			if errors.Is(nerr, io.EOF) {
				break
			}
			return tb.String(), rb.String(), chunks, llm.Response{}, nerr
		}
		chunks++
		switch c.Kind {
		case llm.ChunkText:
			tb.WriteString(c.Text)
		case llm.ChunkReasoning:
			reasoning := c.Text
			rb.WriteString(reasoning)
		}
	}
	resp, err = stream.Response()
	return tb.String(), rb.String(), chunks, resp, err
}

func nestedDelta(contentJSON string) [2]string {
	return [2]string{"step.delta", `{"event_type":"step.delta","step":{"type":"model_output","content":` + contentJSON + `}}`}
}

func reasoningOf(m llm.Message) string {
	var b strings.Builder
	for _, blk := range m.Content {
		if r, ok := blk.(llm.ReasoningBlock); ok {
			b.WriteString(r.Text)
		}
	}
	return b.String()
}

// Attack 1/2: whatever shape a step's content takes, the chunks that streamed
// are exactly the text and reasoning the settled message holds.
func TestZAdvStreamAndMessageAgreeOnHostileContent(t *testing.T) {
	var ten strings.Builder
	ten.WriteString("[")
	for i := 0; i < 10000; i++ {
		if i > 0 {
			ten.WriteString(",")
		}
		switch i % 3 {
		case 0:
			fmt.Fprintf(&ten, `{"type":"text","text":"t%d"}`, i)
		case 1:
			fmt.Fprintf(&ten, `{"type":"thought","thought":"r%d"}`, i)
		default:
			fmt.Fprintf(&ten, `{"type":"mystery_%d","text":"SHOULD-NOT-APPEAR"}`, i)
		}
	}
	ten.WriteString("]")

	cases := map[string]string{
		"empty content array":        `[]`,
		"null content":               `null`,
		"null parts":                 `[null,{"type":"text","text":"a"},null,{"type":"text","text":"b"}]`,
		"unknown types interleaved":  `[{"type":"x","text":"NO"},{"type":"text","text":"a"},{"type":"image","text":"NO"},{"type":"text","text":"b"}]`,
		"empty strings interleaved":  `[{"type":"text","text":""},{"type":"text","text":"a"},{"type":"thought","thought":""},{"type":"text","text":"b"}]`,
		"text and thought mixed":     `[{"type":"thought","thought":"r1"},{"type":"text","text":"a"},{"type":"thought","text":"r2"},{"type":"text","text":"b"}]`,
		"ten thousand parts":         ten.String(),
		"combining marks and RTL":    "[{\"type\":\"text\",\"text\":\"e\\u0301\\u0301\"},{\"type\":\"text\",\"text\":\"\\u202eabc\\u202c\"},{\"type\":\"text\",\"text\":\"\\u0000nul\\u0000\"}]",
		"lone surrogate escapes":     `[{"type":"text","text":"\ud800"},{"type":"text","text":"\udc00x"},{"type":"text","text":"\ud83d"},{"type":"text","text":"\ude00"}]`,
		"raw invalid utf-8 in JSON":  "[{\"type\":\"text\",\"text\":\"a\xff\xfe\"},{\"type\":\"text\",\"text\":\"\xc3\"}]",
		"text part of wrong json ty": `[{"type":"text","text":5},{"type":"text","text":"a"}]`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			body := sse(nestedDelta(content), completedWith("completed"), doneFrame)
			text, reasoning, _, resp, err := advDrain(t, advStream(t, body))
			if err != nil {
				// A refusal is acceptable only if it is a refusal, not a mismatch.
				t.Logf("settled with error (acceptable): %v", err)
				return
			}
			if text != resp.Message.Text() {
				t.Errorf("streamed text %q != settled text %q", clip(text), clip(resp.Message.Text()))
			}
			if reasoning != reasoningOf(resp.Message) {
				t.Errorf("streamed reasoning %q != settled reasoning %q", clip(reasoning), clip(reasoningOf(resp.Message)))
			}
			if strings.Contains(text, "SHOULD-NOT-APPEAR") || strings.Contains(text, "NO") && name == "unknown types interleaved" {
				t.Errorf("an unknown part leaked into text: %q", clip(text))
			}
		})
	}
}

func clip(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

// Attack 1: one multi-MB text part, then a few more parts. The decode limit
// must trip, every chunk of the frame must still arrive before the refusal, and
// nothing may arrive after it.
func TestZAdvHugePartThenMorePartsThenLimit(t *testing.T) {
	big := strings.Repeat("a", 5<<20)
	body := sse(
		nestedDelta(`[{"type":"text","text":"`+big+`"},{"type":"text","text":"tail1"},{"type":"text","text":"tail2"}]`),
		nestedDelta(`[{"type":"text","text":"SHOULD-NOT-BE-READ"}]`),
		completedWith("completed"), doneFrame)
	stream := advStream(t, body)
	var got []string
	var final error
	for i := 0; i < 20; i++ {
		c, err := stream.Next()
		if err != nil {
			final = err
			break
		}
		got = append(got, clip(c.Text))
	}
	if final == nil || errors.Is(final, io.EOF) {
		t.Fatalf("expected the decode-limit refusal, got %v after %v", final, got)
	}
	if len(got) != 3 || got[2] != "tail2" {
		t.Errorf("chunks before the refusal = %v, want the big part and both tails", got)
	}
	// after the refusal Next must keep refusing and never produce a chunk
	for i := 0; i < 3; i++ {
		if c, err := stream.Next(); err == nil {
			t.Errorf("Next after the refusal returned a chunk: %+v", c)
		}
	}
}

// Attack 2: a frame that yields several chunks and whose continuation
// observation then fails (negative index). The error is returned by Next; a
// later Next must not hand out the chunks that frame queued.
func TestZAdvNoChunksAfterAnError(t *testing.T) {
	body := sse(
		[2]string{"step.delta", `{"event_type":"step.delta","index":-1,"step":{"type":"model_output","content":[{"type":"text","text":"one"},{"type":"text","text":"two"},{"type":"text","text":"three"}]}}`},
		completedWith("completed"), doneFrame)
	stream := advStream(t, body)
	sawErr := false
	for i := 0; i < 10; i++ {
		c, err := stream.Next()
		if err != nil {
			if sawErr && !errors.Is(err, io.EOF) {
				continue
			}
			sawErr = true
			continue
		}
		if sawErr {
			t.Fatalf("Next returned chunk %+v after it had already returned an error", c)
		}
	}
	if !sawErr {
		t.Fatal("the negative step index was not refused")
	}
}

// Attack 2: Close, then Next, must not hand out queued chunks.
func TestZAdvNextAfterCloseDoesNotYieldQueuedChunks(t *testing.T) {
	body := sse(
		nestedDelta(`[{"type":"text","text":"one"},{"type":"text","text":"two"},{"type":"text","text":"three"}]`),
		completedWith("completed"), doneFrame)
	stream := advStream(t, body)
	c, err := stream.Next()
	if err != nil || c.Text != "one" {
		t.Fatalf("first chunk = %+v, %v", c, err)
	}
	if err := stream.Close(); err != nil {
		t.Logf("close: %v", err)
	}
	if c, err := stream.Next(); err == nil {
		t.Errorf("Next after Close returned a chunk: %+v", c)
	}
}

// Attack 2: Response with chunks still queued must not settle as if the stream
// had been exhausted, and must not report text the caller never saw.
func TestZAdvResponseBeforePendingDrained(t *testing.T) {
	body := sse(
		nestedDelta(`[{"type":"text","text":"one"},{"type":"text","text":"two"}]`),
		completedWith("completed"), doneFrame)
	stream := advStream(t, body)
	if _, err := stream.Next(); err != nil {
		t.Fatal(err)
	}
	if resp, err := stream.Response(); err == nil {
		t.Errorf("Response settled with a chunk still undelivered: %q", resp.Message.Text())
	}
}

// Attack 2: a frame that carries more parts than the stream ever drains must
// not leave later frames' chunks reordered: pending is FIFO and drained before
// the next frame.
func TestZAdvOrderAcrossFrames(t *testing.T) {
	body := sse(
		nestedDelta(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`),
		nestedDelta(`[{"type":"text","text":"c"},{"type":"text","text":"d"}]`),
		completedWith("completed"), doneFrame)
	text, _, n, resp, err := advDrain(t, advStream(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if text != "abcd" || resp.Message.Text() != "abcd" || n != 4 {
		t.Errorf("text=%q settled=%q chunks=%d", text, resp.Message.Text(), n)
	}
}

// Attack 2: queued chunks must not be returned once the context is cancelled.
func TestZAdvCancelDropsQueuedChunks(t *testing.T) {
	body := sse(
		nestedDelta(`[{"type":"text","text":"one"},{"type":"text","text":"two"}]`),
		completedWith("completed"), doneFrame)
	adapter, _ := adapterFor(t, body)
	ctx, cancel := contextWithCancel()
	stream, err := adapter.Stream(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Next(); err != nil {
		t.Fatal(err)
	}
	cancel()
	if c, err := stream.Next(); err == nil {
		t.Errorf("Next after cancel returned %+v", c)
	}
}

// ---- Attack 3: malformed-call settlement ----

func idxCall(index int, id, name string) [2]string {
	return [2]string{"step.start", fmt.Sprintf(`{"event_type":"step.start","index":%d,"step":{"type":"function_call","id":%q,"name":%q,"arguments":{}}}`, index, id, name)}
}

func idxArgs(index int, frag string) [2]string {
	return [2]string{"step.delta", fmt.Sprintf(`{"event_type":"step.delta","index":%d,"delta":{"type":"arguments_delta","arguments":%s}}`, index, jsonString(frag))}
}

func idxStop(index int) [2]string {
	return [2]string{"step.stop", fmt.Sprintf(`{"event_type":"step.stop","index":%d}`, index)}
}

func TestZAdvTwoCutCallsBothReported(t *testing.T) {
	resp, err := settle(t, sse(
		idxCall(0, "a", "read_file"), idxArgs(0, `{"p`),
		idxCall(1, "b", "read_file"), idxArgs(1, `{"q`),
		completedWith("max_tokens"), doneFrame))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Malformed) != 2 || resp.Malformed[0].ID != "a" || resp.Malformed[1].ID != "b" || len(resp.Message.ToolCalls()) != 0 {
		t.Errorf("malformed=%+v calls=%+v", resp.Malformed, resp.Message.ToolCalls())
	}
}

func TestZAdvCutThenWholeCall(t *testing.T) {
	resp, err := settle(t, sse(
		idxCall(0, "a", "read_file"), idxArgs(0, `{"p`), idxStop(0),
		idxCall(1, "b", "read_file"), idxArgs(1, `{"path":"x"}`), idxStop(1),
		completedWith("max_tokens"), doneFrame))
	if err != nil {
		t.Fatal(err)
	}
	calls := resp.Message.ToolCalls()
	if len(calls) != 1 || calls[0].ID != "b" || len(resp.Malformed) != 1 || resp.Malformed[0].ID != "a" {
		t.Errorf("malformed=%+v calls=%+v", resp.Malformed, calls)
	}
}

func TestZAdvSameIDWholeThenCutIsNotDroppedTogether(t *testing.T) {
	// Both calls share id and name. The first is whole and sealed; the second is
	// cut. The whole one must survive in the replay state.
	resp, err := settle(t, sse(
		idxCall(0, "dup", "read_file"), idxArgs(0, `{"path":"x"}`), idxStop(0),
		idxCall(1, "dup", "read_file"), idxArgs(1, `{"p`),
		completedWith("max_tokens"), doneFrame))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Message.ToolCalls()) != 1 || len(resp.Malformed) != 1 {
		t.Fatalf("calls=%+v malformed=%+v", resp.Message.ToolCalls(), resp.Malformed)
	}
	state := string(resp.Message.Provenance.ReplayState)
	if !strings.Contains(state, "dup") || !strings.Contains(state, `path`) {
		t.Errorf("the whole call was dropped from the replay state: %s", state)
	}
}

func TestZAdvSameIDWholeThenCutViaSnapshot(t *testing.T) {
	snap := `"steps":[` +
		`{"type":"function_call","id":"dup","name":"read_file","arguments":{"path":"x"}},` +
		`{"type":"function_call","id":"dup","name":"read_file","arguments":{}}]`
	resp, err := settle(t, sse(
		idxCall(0, "dup", "read_file"), idxArgs(0, `{"path":"x"}`), idxStop(0),
		idxCall(1, "dup", "read_file"), idxArgs(1, `{"p`),
		[2]string{"interaction.completed", `{"event_type":"interaction.completed","interaction":{"status":"max_tokens",` + snap + `}}`},
		doneFrame))
	if err != nil {
		t.Fatalf("turn died: %v", err)
	}
	if len(resp.Message.ToolCalls()) != 1 || len(resp.Malformed) != 1 {
		t.Fatalf("calls=%+v malformed=%+v", resp.Message.ToolCalls(), resp.Malformed)
	}
	if n := strings.Count(string(resp.Message.Provenance.ReplayState), `"dup"`); n != 1 {
		t.Errorf("replay state holds %d dup calls, want exactly the whole one: %s", n, resp.Message.Provenance.ReplayState)
	}
}

func TestZAdvEmptyIDsWholeAndCutViaSnapshot(t *testing.T) {
	snap := `"steps":[` +
		`{"type":"function_call","id":"","name":"read_file","arguments":{"path":"x"}},` +
		`{"type":"function_call","id":"","name":"read_file","arguments":{}}]`
	resp, err := settle(t, sse(
		idxCall(0, "", "read_file"), idxArgs(0, `{"path":"x"}`), idxStop(0),
		idxCall(1, "", "read_file"), idxArgs(1, `{"p`),
		[2]string{"interaction.completed", `{"event_type":"interaction.completed","interaction":{"status":"max_tokens",` + snap + `}}`},
		doneFrame))
	if err != nil {
		t.Fatalf("turn died: %v", err)
	}
	if len(resp.Message.ToolCalls()) != 1 || len(resp.Malformed) != 1 {
		t.Fatalf("calls=%+v malformed=%+v", resp.Message.ToolCalls(), resp.Malformed)
	}
	if n := strings.Count(string(resp.Message.Provenance.ReplayState), `"function_call"`); n != 1 {
		t.Errorf("replay state holds %d function_call steps, want the one whole call: %s", n, resp.Message.Provenance.ReplayState)
	}
}

func TestZAdvEmptyArgsObjectThenMoreBytes(t *testing.T) {
	// `{}` then more bytes is two things run together; at max_tokens it is cut.
	resp, err := settle(t, sse(
		idxCall(0, "a", "read_file"), idxArgs(0, `{}`), idxArgs(0, `{"p`),
		completedWith("max_tokens"), doneFrame))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Malformed) != 1 || len(resp.Message.ToolCalls()) != 0 {
		t.Errorf("malformed=%+v calls=%+v", resp.Malformed, resp.Message.ToolCalls())
	}
}

func TestZAdvObjectThenTrailingGarbageAtMaxTokens(t *testing.T) {
	resp, err := settle(t, sse(
		idxCall(0, "a", "read_file"), idxArgs(0, `{"path":"x"}garbage`), idxStop(0),
		completedWith("max_tokens"), doneFrame))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Message.ToolCalls()) != 0 || len(resp.Malformed) != 1 {
		t.Errorf("a call with trailing garbage was dispatched: calls=%+v malformed=%+v", resp.Message.ToolCalls(), resp.Malformed)
	}
}

func TestZAdvNullArgsAtMaxTokens(t *testing.T) {
	resp, err := settle(t, sse(
		idxCall(0, "a", "read_file"), idxArgs(0, `null`), idxStop(0),
		completedWith("max_tokens"), doneFrame))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Message.ToolCalls()) != 0 {
		t.Errorf("null arguments were dispatched: %+v", resp.Message.ToolCalls())
	}
}

func TestZAdvMaxTokensNoCallsIsPlainTruncation(t *testing.T) {
	resp, err := settle(t, sse(nestedDelta(`[{"type":"text","text":"hi"}]`), completedWith("max_tokens"), doneFrame))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != llm.StopMaxTokens || len(resp.Malformed) != 0 || resp.Message.Text() != "hi" {
		t.Errorf("%+v", resp)
	}
}

func TestZAdvIncompleteWithErrorFrameIsAnError(t *testing.T) {
	_, err := settle(t, sse(
		idxCall(0, "a", "read_file"), idxArgs(0, `{"p`),
		[2]string{"error", `{"event_type":"error","error":{"message":"boom","status":"INTERNAL"}}`},
		completedWith("incomplete"), doneFrame))
	if err == nil {
		t.Fatal("an error frame was absorbed into a malformed call")
	}
}

func TestZAdvToolUseStatusWithCutCallStaysError(t *testing.T) {
	// requires_action with an unsealed, cut call is not an output-budget cut.
	_, err := settle(t, sse(
		idxCall(0, "a", "read_file"), idxArgs(0, `{"p`),
		completedWith("requires_action"), doneFrame))
	if err == nil {
		t.Fatal("a cut call under requires_action settled")
	}
}

func TestZAdvMixedWholeAndCutStopReason(t *testing.T) {
	// Pin the stop reason a loop sees for whole+cut. It is max_tokens; the whole
	// call must still be dispatched by the loop (see the agent test).
	resp, err := settle(t, sse(
		idxCall(0, "a", "read_file"), idxArgs(0, `{"path":"x"}`), idxStop(0),
		idxCall(1, "b", "read_file"), idxArgs(1, `{"p`),
		completedWith("max_tokens"), doneFrame))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != llm.StopMaxTokens {
		t.Errorf("stop = %q", resp.StopReason)
	}
}

// A cut call whose signature rides in the replay state: the signature of the
// withheld call must not remain in CallSignatures.
func TestZAdvWithheldCallSignatureNotReplayed(t *testing.T) {
	resp, err := settle(t, sse(
		[2]string{"step.delta", `{"event_type":"step.delta","index":0,"delta":{"type":"thought_signature","signature":"SIG-CUT"}}`},
		idxCall(1, "a", "read_file"), idxArgs(1, `{"p`),
		completedWith("max_tokens"), doneFrame))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Provenance != nil && strings.Contains(string(resp.Message.Provenance.ReplayState), "SIG-CUT") {
		t.Errorf("a withheld call's signature stayed in replay state: %s", resp.Message.Provenance.ReplayState)
	}
}

func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(adaptertest.Ctx())
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
