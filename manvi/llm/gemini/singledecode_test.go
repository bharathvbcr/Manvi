package gemini

import (
	"bufio"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// decodeRun is everything observable about one stream drained to its end: what
// Next returned, what Response settled, and the state the continuation will
// replay to the provider.
type decodeRun struct {
	chunks   []llm.Chunk
	errs     []string
	response llm.Response
	respErr  string
	steps    []string
	stepsErr string
	state    string
}

func errText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

func drainFrames(t *testing.T, body string, twice bool) decodeRun {
	t.Helper()
	s := newStream(io.NopCloser(strings.NewReader(body)), "gemini-3.7-flash", 0)
	s.decodeTwice = twice
	var run decodeRun
	for i := 0; ; i++ {
		if i > 100000 {
			t.Fatal("stream did not terminate")
		}
		chunk, err := s.Next()
		run.chunks = append(run.chunks, chunk)
		run.errs = append(run.errs, errText(err))
		if err != nil {
			break
		}
	}
	resp, err := s.Response()
	run.response, run.respErr = resp, errText(err)
	steps, err := s.continuation.steps()
	for _, step := range steps {
		run.steps = append(run.steps, string(step))
	}
	run.stepsErr = errText(err)
	run.state = streamState(s)
	_ = s.Close()
	return run
}

// streamState renders the decoder and continuation state a later frame or the
// settled response can read, in a form two runs can be compared by.
func streamState(s *stream) string {
	var b strings.Builder
	acc := func(a *callAccumulator) string {
		if a == nil {
			return "nil"
		}
		return fmt.Sprintf("{id=%q name=%q args=%q block=%d sig=%q sealed=%v}",
			a.id, a.name, a.args.String(), a.block, a.signature, a.sealed)
	}
	fmt.Fprintf(&b, "text=%q reasoning=%q stop=%v usage=%+v done=%v failure=%q\n",
		s.text.String(), s.reasoning.String(), s.stopReason, s.usage, s.done, errText(s.failure))
	fmt.Fprintf(&b, "next=%d text=%d reason=%d sig=%q decoded=%d\n",
		s.nextIndex, s.textIndex, s.reasonIdx, s.lastSignature, s.decodedBytes())
	for _, a := range s.order {
		fmt.Fprintf(&b, "order %s\n", acc(a))
	}
	for _, k := range sortedInts(s.stepKinds) {
		fmt.Fprintf(&b, "kind %d=%q\n", k, s.stepKinds[k])
	}
	for _, k := range sortedInts(s.calls) {
		fmt.Fprintf(&b, "call %d=%s\n", k, acc(s.calls[k]))
	}

	c := &s.continuation
	fmt.Fprintf(&b, "continuation bytes=%d\n", c.bytes)
	for _, raw := range c.complete {
		fmt.Fprintf(&b, "complete %q\n", raw)
	}
	position := map[*retainedStep]int{}
	for i, st := range c.order {
		position[st] = i
		fmt.Fprintf(&b, "step %d kind=%q final=%v text=%q thought=%q call=%s\n",
			i, st.kind, st.final, st.text.String(), st.thought.String(), acc(st.call))
		keys := make([]string, 0, len(st.fields))
		for k := range st.fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			// %q of the raw bytes: these are replayed to the provider, so
			// they must match to the byte, whitespace included.
			fmt.Fprintf(&b, "  field %q=%q\n", k, []byte(st.fields[k]))
		}
		fmt.Fprintf(&b, "  fields nil=%v\n", st.fields == nil)
	}
	for _, k := range sortedInts(c.indexed) {
		fmt.Fprintf(&b, "indexed %d -> step %d\n", k, position[c.indexed[k]])
	}
	fmt.Fprintf(&b, "indexed nil=%v\n", c.indexed == nil)
	return b.String()
}

func sortedInts[V any](m map[int]V) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

// prelude opens a thought step at 0 and a call at 1, so a frame under test
// has steps to land on.
const prelude = `event: step.start
data: {"index":0,"step":{"type":"thought"},"event_type":"step.start"}

event: step.start
data: {"index":1,"step":{"type":"function_call","id":"c1","name":"read_file","arguments":{}},"event_type":"step.start"}

`

// coda gives the stream an answer and an end, so what the frame under test
// did to the state is carried through to the settled response.
const coda = `event: step.delta
data: {"index":1,"delta":{"arguments":"{\"path\":\"a.go\"}","type":"arguments_delta"},"event_type":"step.delta"}

event: step.stop
data: {"index":1,"event_type":"step.stop"}

event: step.start
data: {"index":2,"step":{"type":"model_output"},"event_type":"step.start"}

event: step.delta
data: {"index":2,"delta":{"text":"done","type":"text"},"event_type":"step.delta"}

event: interaction.completed
data: {"interaction":{"status":"requires_action","usage":{"total_input_tokens":3,"total_output_tokens":4}},"event_type":"interaction.completed"}

event: done
data: [DONE]

`

// adversarialFrames are frames chosen to find a difference between one
// decode and two: every key the two readers type differently, given every
// shape json will accept or refuse for it, duplicated, re-cased, nulled after
// a value and a value after null.
var adversarialFrames = []struct{ event, data string }{
	// index
	{"step.delta", `{"index":null,"delta":{"type":"text","text":"x"},"event_type":"step.delta"}`},
	{"step.delta", `{"delta":{"type":"text","text":"no index"},"event_type":"step.delta"}`},
	{"step.delta", `{"index":-1,"delta":{"type":"text","text":"x"},"event_type":"step.delta"}`},
	{"step.start", `{"index":-1,"step":{"type":"thought"},"event_type":"step.start"}`},
	{"step.delta", `{"index":-0,"delta":{"type":"text","text":"x"},"event_type":"step.delta"}`},
	{"step.delta", `{"index":0.0,"delta":{"type":"text","text":"x"},"event_type":"step.delta"}`},
	{"step.delta", `{"index":1.5,"delta":{"type":"text","text":"x"},"event_type":"step.delta"}`},
	{"step.delta", `{"index":1e2,"delta":{"type":"text","text":"x"},"event_type":"step.delta"}`},
	{"step.delta", `{"index":"1","delta":{"type":"text","text":"x"},"event_type":"step.delta"}`},
	{"step.delta", `{"index":true,"delta":{"type":"text","text":"x"},"event_type":"step.delta"}`},
	{"step.delta", `{"index":{},"delta":{"type":"text","text":"x"},"event_type":"step.delta"}`},
	{"step.delta", `{"index":[0],"delta":{"type":"text","text":"x"},"event_type":"step.delta"}`},
	{"step.delta", `{"index":9223372036854775807,"delta":{"type":"text","text":"x"},"event_type":"step.delta"}`},
	{"step.delta", `{"index":9223372036854775808,"delta":{"type":"text","text":"x"},"event_type":"step.delta"}`},
	{"step.delta", `{"index":-9223372036854775809,"delta":{"type":"text","text":"x"},"event_type":"step.delta"}`},
	{"step.delta", `{"index":0,"index":null,"delta":{"type":"text","text":"x"},"event_type":"step.delta"}`},
	{"step.delta", `{"index":1,"index":null,"delta":{"type":"arguments","arguments":"{}"},"event_type":"step.delta"}`},
	{"step.stop", `{"index":1,"index":null,"step":{"type":"function_call","id":"c1","name":"read_file"},"event_type":"step.stop"}`},
	{"step.delta", `{"index":null,"index":0,"delta":{"type":"text","text":"x"},"event_type":"step.delta"}`},
	{"step.delta", `{"index":1,"index":0,"delta":{"type":"text","text":"x"},"event_type":"step.delta"}`},
	{"step.delta", `{"INDEX":0,"delta":{"type":"text","text":"upper"},"event_type":"step.delta"}`},
	{"step.delta", `{"Index":1,"index":0,"delta":{"type":"thought","thought":"mixed"},"event_type":"step.delta"}`},
	{"step.delta", `{"index" : 0 ,"delta":{"type":"text","text":"spaced"},"event_type":"step.delta"}`},
	{"step.delta", `{"index":0,"delta":{"type":"thought_signature","signature":"c2ln"},"event_type":"step.delta"}`},
	{"step.delta", `{"index":0,"delta":{"type":"something_new"},"event_type":"step.delta"}`},
	{"step.delta", `{"index":7,"delta":{"type":"text","text":"unopened"},"event_type":"step.delta"}`},

	// step
	{"step.start", `{"index":3,"step":null,"event_type":"step.start"}`},
	{"step.start", `{"index":3,"step":5,"event_type":"step.start"}`},
	{"step.start", `{"index":3,"step":"thought","event_type":"step.start"}`},
	{"step.start", `{"index":3,"step":[],"event_type":"step.start"}`},
	{"step.start", `{"index":3,"step":{"type":5},"event_type":"step.start"}`},
	{"step.start", `{"index":3,"step":{"type":"thought","content":"not a list"},"event_type":"step.start"}`},
	{"step.start", `{"index":3,"step":{"type":"thought","id":{"nested":true}},"event_type":"step.start"}`},
	{"step.start", `{"index":3,"step":{"Type":"function_call","type":"thought","name":"n"},"event_type":"step.start"}`},
	{"step.start", `{"index":3,"step":{"type":"thought","type":"function_call","id":"dup","name":"read_file"},"event_type":"step.start"}`},
	{"step.start", `{"index":3,"step":{"type":"function_call","id":"a"},"step":{"name":"read_file","arguments":{}},"event_type":"step.start"}`},
	{"step.start", `{"index":3,"step":{"type":"thought"},"step":null,"event_type":"step.start"}`},
	{"step.start", `{"index":3,"step":null,"step":{"type":"thought"},"event_type":"step.start"}`},
	{"step.start", `{"index":3,"STEP":{"type":"thought","x":1},"event_type":"step.start"}`},
	{"step.start", "{ \"index\" : 3 , \"step\" : { \"type\" : \"thought\" ,\n \"signature\":\"abc\" , \"extra\": [1, 2 ,3], \"esc\":\"\\u0041\\n\" } , \"event_type\":\"step.start\" }"},
	{"step.start", `{"index":3,"step":{},"event_type":"step.start"}`},
	{"step.stop", `{"index":0,"step":{"type":"thought","signature":"final-sig","summary":[{"type":"text","text":"s"}]},"event_type":"step.stop"}`},
	{"step.stop", `{"index":1,"step":{"type":"function_call","id":"c1","name":"read_file","arguments":{"path":"z.go"}},"event_type":"step.stop"}`},
	{"step.stop", `{"index":1,"step":{},"event_type":"step.stop"}`},
	{"step.stop", `{"index":1,"step":null,"event_type":"step.stop"}`},
	{"step.stop", `{"index":0,"step":{"type":"thought","signature":"x"},"step":null,"event_type":"step.stop"}`},
	{"step.stop", `{"index":0,"step":null,"step":{"type":"thought","signature":"y"},"event_type":"step.stop"}`},
	{"step.stop", `{"index":1,"step":{"type":"function_call","id":7},"event_type":"step.stop"}`},
	{"step.delta", `{"index":0,"step":{"type":"thought","content":[{"type":"thought","thought":"nested"},{"type":"text","text":"t"}]},"event_type":"step.delta"}`},
	{"step.delta", `{"index":0,"step":{"type":"thought","content":[{"type":"thought","text":"via text"}]},"event_type":"step.delta"}`},
	{"step.delta", `{"index":0,"step":{"content":[{"type":5}]},"event_type":"step.delta"}`},
	{"step.delta", `{"index":0,"delta":{"type":"text","text":"both"},"step":{"type":"thought"},"event_type":"step.delta"}`},

	// interaction
	{"interaction.completed", `{"interaction":{"status":"completed","steps":[{"type":"thought","signature":"s"} , {"type":"model_output","content":[{"type":"text","text":"snap"}]}]},"event_type":"interaction.completed"}`},
	{"interaction.completed", `{"interaction":{"status":"completed","steps":[]},"event_type":"interaction.completed"}`},
	{"interaction.completed", `{"interaction":{"status":"completed","steps":null},"event_type":"interaction.completed"}`},
	{"interaction.completed", `{"interaction":{"status":"completed","steps":"no"},"event_type":"interaction.completed"}`},
	{"interaction.completed", `{"interaction":{"status":"completed","steps":[1,"two",null]},"event_type":"interaction.completed"}`},
	{"interaction.completed", `{"interaction":{"status":5},"event_type":"interaction.completed"}`},
	{"interaction.completed", `{"interaction":{"usage":{"total_input_tokens":"x"}},"event_type":"interaction.completed"}`},
	{"interaction.completed", `{"interaction":{"usage":{"total_input_tokens":1.5}},"event_type":"interaction.completed"}`},
	{"interaction.completed", `{"interaction":null,"event_type":"interaction.completed"}`},
	{"interaction.completed", `{"interaction":"done","event_type":"interaction.completed"}`},
	{"interaction.completed", `{"interaction":{"status":"completed"},"interaction":{"steps":[{"type":"thought"}]},"event_type":"interaction.completed"}`},
	{"interaction.completed", `{"interaction":{"steps":[{"type":"thought"}]},"interaction":null,"event_type":"interaction.completed"}`},
	{"interaction.completed", `{"Interaction":{"STATUS":"completed","Steps":[{"type":"thought"}]},"event_type":"interaction.completed"}`},
	{"step.delta", `{"index":0,"interaction":{"steps":[{"type":"x"}]},"delta":{"type":"text","text":"x"},"event_type":"step.delta"}`},
	{"interaction.status_update", `{"status":"completed","event_type":"interaction.status_update"}`},
	{"interaction.status_update", `{"status":7,"event_type":"interaction.status_update"}`},

	// whole frames
	{"step.delta", `null`},
	{"step.delta", `[]`},
	{"step.delta", `"text"`},
	{"step.delta", `{}`},
	{"step.delta", `{"index":0`},
	{"step.delta", `{"index":0,"delta":{"type":"text","text":5},"event_type":"step.delta"}`},
	{"step.delta", `{"index":0,"delta":"x","event_type":"step.delta"}`},
	{"step.delta", `{"index":1,"delta":{"type":"arguments","arguments":"\"un"},"event_type":"step.delta"}`},
	{"step.start", `{"index":0,"step":{"type":"thought"},"event_type":"step.delta"}`},
	{"", `{"index":0,"delta":{"type":"text","text":"unnamed"},"event_type":"step.delta"}`},
	{"error", `{"error":{"message":"overloaded","code":"unavailable"},"event_type":"error"}`},
	{"error", `{"error":{"message":5},"event_type":"error"}`},
	{"step.delta", `{"index":0,"delta":{"type":"text","text":"x"},"event_type":"step.delta","unknown":{"deep":[1,{"a":null}]}}`},
}

func frameStream(event, data string) string {
	var b strings.Builder
	b.WriteString(prelude)
	if event != "" {
		fmt.Fprintf(&b, "event: %s\n", event)
	}
	fmt.Fprintf(&b, "data: %s\n\n", data)
	b.WriteString(coda)
	return b.String()
}

// TestOneDecodeMatchesTwo holds the single-pass decode to the two-pass decode
// it replaced, on every fixture this package has and on frames built to find a
// difference. The comparison is the whole observable outcome: each chunk and
// error Next returned, the settled response, the continuation steps that are
// replayed to the provider byte for byte, and the decoder's internal state,
// byte accounting included.
func TestOneDecodeMatchesTwo(t *testing.T) {
	corpus := map[string]string{
		"happyStream":         happyStream,
		"liveStream":          liveStream,
		"reusedIDStream":      reusedIDStream,
		"splitArgumentStream": splitArgumentStream,
		"overloadedFrames":    overloadedFrames,
		"benchStream":         string(benchStream()),
		"opaqueThought": strings.Replace(liveStream, `"type":"thought"`,
			`"type":"thought","opaque_extension":{"token":"opaque-a"}`, 1),
	}
	for i, f := range adversarialFrames {
		corpus[fmt.Sprintf("adversarial-%02d %s", i, f.data)] = frameStream(f.event, f.data)
	}

	for name, body := range corpus {
		t.Run(name, func(t *testing.T) {
			twice := drainFrames(t, body, true)
			once := drainFrames(t, body, false)
			if !reflect.DeepEqual(once.errs, twice.errs) {
				t.Errorf("errors differ:\none:  %q\ntwo:  %q", once.errs, twice.errs)
			}
			if !reflect.DeepEqual(once.chunks, twice.chunks) {
				t.Errorf("chunks differ:\none:  %+v\ntwo:  %+v", once.chunks, twice.chunks)
			}
			if once.respErr != twice.respErr || !reflect.DeepEqual(once.response, twice.response) {
				t.Errorf("responses differ:\none:  %+v (%s)\ntwo:  %+v (%s)",
					once.response, once.respErr, twice.response, twice.respErr)
			}
			if once.stepsErr != twice.stepsErr || !reflect.DeepEqual(once.steps, twice.steps) {
				t.Errorf("replayed steps differ:\none:  %q (%s)\ntwo:  %q (%s)",
					once.steps, once.stepsErr, twice.steps, twice.stepsErr)
			}
			if once.state != twice.state {
				t.Errorf("stream state differs:\none:\n%s\ntwo:\n%s", once.state, twice.state)
			}
		})
	}
}

// The equivalence above would hold trivially if the single pass refused every
// frame. It must take every frame of an ordinary stream, or nothing was
// saved.
func TestOrdinaryFramesTakeTheSinglePass(t *testing.T) {
	for name, body := range map[string]string{
		"liveStream":  liveStream,
		"happyStream": happyStream,
		"benchStream": string(benchStream()),
	} {
		scanner := bufio.NewScanner(strings.NewReader(body))
		for scanner.Scan() {
			data, ok := strings.CutPrefix(scanner.Text(), "data: ")
			if !ok || data == DoneSentinel {
				continue
			}
			if _, _, single := decodeFrame([]byte(data)); !single {
				t.Errorf("%s: an ordinary frame was refused by the single pass: %s", name, data)
			}
		}
	}
}

// FuzzOneDecodeMatchesTwo is TestOneDecodeMatchesTwo over arbitrary frames.
func FuzzOneDecodeMatchesTwo(f *testing.F) {
	for _, frame := range adversarialFrames {
		// A seed the body below would skip is a skip every plain `go test` run
		// records, and verify.sh names recorded skips; it is not a case.
		if strings.ContainsAny(frame.event, "\r\n") || strings.ContainsAny(frame.data, "\r\n") {
			continue
		}
		f.Add(frame.event, frame.data)
	}
	f.Fuzz(func(t *testing.T, event, data string) {
		if strings.ContainsAny(event, "\r\n") || strings.ContainsAny(data, "\r\n") {
			t.Skip("a line break would split the frame into SSE fields of its own")
		}
		body := frameStream(event, data)
		twice := drainFrames(t, body, true)
		once := drainFrames(t, body, false)
		if !reflect.DeepEqual(once, twice) {
			t.Errorf("one decode and two disagree on %q / %s:\none: %+v\ntwo: %+v", event, data, once, twice)
		}
	})
}
