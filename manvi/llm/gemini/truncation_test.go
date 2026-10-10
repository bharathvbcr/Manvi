package gemini

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
	"github.com/bharathvbcr/Manvi/manvi/llm/transport"
)

const createdFrame = `event: interaction.created
data: {"event_type":"interaction.created","interaction":{"status":"in_progress"}}

`

const textFrame = `event: step.delta
data: {"event_type":"step.delta","step":{"type":"model_output","content":[{"type":"text","text":"The answer is"}]}}

`

func drainFor(t *testing.T, body string) error {
	t.Helper()
	adapter, _ := adapterFor(t, body)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		return err
	}
	defer stream.Close()
	for {
		if _, err := stream.Next(); err != nil {
			if err == io.EOF {
				_, err = stream.Response()
			}
			return err
		}
	}
}

// TestEOFBeforeInteractionCompletedIsNotASuccess: without interaction.completed
// or done the content is a prefix and the usage was never reported.
func TestEOFBeforeInteractionCompletedIsNotASuccess(t *testing.T) {
	err := drainFor(t, createdFrame+textFrame)
	if err == nil || err == io.EOF {
		t.Fatalf("truncated stream settled as a success (err=%v)", err)
	}
	var te *transport.Error
	if !errors.As(err, &te) || !te.Retryable() {
		t.Fatalf("err = %#v, want a retryable transport error", err)
	}
	if !strings.Contains(err.Error(), "interaction.completed") {
		t.Fatalf("err = %v, want it to name the missing terminal event", err)
	}
}

// TestEOFAfterStepStartDoesNotDispatchAnEmptyCall: the call opened and the
// stream ended; its arguments never arrived and must not become {}.
func TestEOFAfterStepStartDoesNotDispatchAnEmptyCall(t *testing.T) {
	body := createdFrame + `event: step.start
data: {"event_type":"step.start","index":0,"step":{"type":"function_call","id":"fc_1","name":"read_file","arguments":{}}}

`
	if err := drainFor(t, body); err == nil {
		t.Fatal("a call with no arguments, from a stream that never finished, was dispatched")
	}
}

// TestAnUnsealedEmptyCallIsRefusedEvenWhenCompleted: interaction.completed
// arrived, but the call was never framed as complete by step.stop and carries
// no argument bytes, so {} would be invented.
func TestAnUnsealedEmptyCallIsRefusedEvenWhenCompleted(t *testing.T) {
	body := createdFrame + `event: step.start
data: {"event_type":"step.start","index":0,"step":{"type":"function_call","id":"fc_1","name":"read_file","arguments":{}}}

event: interaction.completed
data: {"event_type":"interaction.completed","interaction":{"status":"requires_action","usage":{"total_input_tokens":3,"total_output_tokens":1}}}

event: done
data: [DONE]

`
	if err := drainFor(t, body); err == nil {
		t.Fatal("an unsealed, argument-less call was surfaced as {}")
	}
}

// TestASealedCallWithNoArgumentsIsLegitimate is the other direction.
func TestASealedCallWithNoArgumentsIsLegitimate(t *testing.T) {
	body := createdFrame + `event: step.start
data: {"event_type":"step.start","index":0,"step":{"type":"function_call","id":"fc_1","name":"read_file","arguments":{}}}

event: step.stop
data: {"event_type":"step.stop","index":0}

event: interaction.completed
data: {"event_type":"interaction.completed","interaction":{"status":"requires_action","usage":{"total_input_tokens":3,"total_output_tokens":1}}}

event: done
data: [DONE]

`
	adapter, _ := adapterFor(t, body)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	_, resp, err := adaptertest.Drain(stream)
	if err != nil {
		t.Fatal(err)
	}
	calls := resp.Message.ToolCalls()
	if len(calls) != 1 || string(calls[0].Arguments) != "{}" {
		t.Fatalf("calls = %+v", calls)
	}
}

// TestFailedAndCancelledInteractionsAreErrors: a terminal failure status, in
// either frame that carries one, names the status and settles nothing.
func TestFailedAndCancelledInteractionsAreErrors(t *testing.T) {
	for _, status := range []string{"failed", "cancelled"} {
		completed := createdFrame + textFrame + `event: interaction.completed
data: {"event_type":"interaction.completed","interaction":{"status":"` + status + `","usage":{"total_input_tokens":3,"total_output_tokens":1}}}

event: done
data: [DONE]

`
		update := createdFrame + textFrame + `event: interaction.status_update
data: {"event_type":"interaction.status_update","status":"` + status + `"}

event: interaction.completed
data: {"event_type":"interaction.completed","interaction":{"status":"completed"}}

`
		for name, body := range map[string]string{"completed": completed, "status_update": update} {
			t.Run(status+"/"+name, func(t *testing.T) {
				err := drainFor(t, body)
				if err == nil || err == io.EOF {
					t.Fatalf("a %s interaction settled as a success", status)
				}
				if !strings.Contains(err.Error(), status) {
					t.Fatalf("err = %v, want it to name %q", err, status)
				}
			})
		}
	}
}

// TestDoneWithoutTrailingBlankLineIsACleanEnd: the final `done` frame is
// terminated by EOF rather than a blank line.
func TestDoneWithoutTrailingBlankLineIsACleanEnd(t *testing.T) {
	body := strings.TrimSuffix(happyStream, "\n")
	adapter, _ := adapterFor(t, body)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := adaptertest.Drain(stream); err != nil {
		t.Fatalf("a finished stream ending without a blank line failed: %v", err)
	}
}
