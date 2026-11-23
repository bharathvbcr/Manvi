package adaptertest_test

import (
	"fmt"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
)

// Probe 3: cross-wire table. Stop semantics: end, tool, limit, filter, none
// (no stop reason, only the stream's terminal frame).
func TestAdvB31SettleStopReasonTable(t *testing.T) {
	type arg struct {
		name   string
		deltas []string
		closed bool
		whole  bool // a complete JSON object
	}
	args := []arg{
		{"incomplete object, open", []string{"{\"a\":"}, false, false},
		{"incomplete object, closed", []string{"{\"a\":"}, true, false},
		{"complete object, open", []string{"{\"a\":1}"}, false, true},
		{"complete object, closed", []string{"{\"a\":1}"}, true, true},
		{"no bytes, open", nil, false, false},
		{"no bytes, closed", nil, true, false},
	}
	stops := []string{"end", "tool", "limit", "filter", "none"}
	var table strings.Builder
	for _, a := range args {
		for _, stop := range stops {
			row := fmt.Sprintf("%-26s stop=%-6s", a.name, stop)
			outs := map[string]advOut{}
			for _, name := range advWireNames() {
				o := advRun(t, name, advBody(name, []advSpec{{ID: "c1", Name: "bash", Deltas: a.deltas, Closed: a.closed}}, stop))
				outs[name] = o
				row += fmt.Sprintf(" | %s: %s", name, o.class())
				// R1: nothing that is not a complete object is ever dispatched.
				if !a.whole && len(a.deltas) > 0 {
					for _, c := range o.calls {
						if string(c.Arguments) != "" && !advIsObject(c.Arguments) {
							t.Errorf("%s/%s/%s: dispatched non-object %q", name, a.name, stop, c.Arguments)
						}
					}
					if len(o.calls) > 0 {
						t.Errorf("%s/%s/%s: incomplete arguments %q were dispatched as %s", name, a.name, stop, a.deltas, o.class())
					}
				}
				// R2: the output limit is the model's own; every wire keeps the
				// turn and reports exactly one malformed call.
				if stop == "limit" && !a.whole && len(o.calls) == 0 {
					if o.err != nil || len(o.resp.Malformed) != 1 || o.resp.StopReason != llm.StopMaxTokens {
						// "no bytes, closed" is a legitimate {} call
						if !(a.deltas == nil && a.closed) {
							t.Errorf("%s/%s/limit: want one malformed call and a settled turn, got %s", name, a.name, o.class())
						}
					}
				}
				// R3: otherwise an incomplete call is refused or reported, never silent.
				if stop != "limit" && !a.whole && len(a.deltas) > 0 && o.err == nil && len(o.resp.Malformed) == 0 {
					t.Errorf("%s/%s/%s: incomplete call vanished silently: %s", name, a.name, stop, o.class())
				}
			}
			// R4: where the semantics match (a whole, closed call), the three
			// wires must agree on whether it runs, whatever the stop reason.
			if a.whole && a.closed && stop != "filter" { // gemini's "failed" refuses the whole interaction by design
				run := map[string]bool{}
				for n, o := range outs {
					run[n] = len(o.calls) == 1
				}
				if run["anthropic"] != run["gemini"] || run["gemini"] != run["openaicompat"] {
					t.Errorf("whole closed call, stop=%s: wires disagree on dispatch: %v", stop, run)
				}
			}
			// R5: a call with no argument bytes that was never closed, on a stream
			// the provider's filter ended or that carried no stop reason at all,
			// must not run as {} on any wire (the hosted wires refuse it).
			if a.deltas == nil && !a.closed && (stop == "filter" || stop == "none") {
				for n, o := range outs {
					if len(o.calls) > 0 {
						t.Errorf("%s: unclosed call with no argument bytes under stop=%s dispatched: %s", n, stop, o.class())
					}
				}
			}
			table.WriteString(row + "\n")
		}
	}
	t.Logf("cross-wire table:\n%s", table.String())
}

// Probe 3: an open anthropic tool_use block cut by max_tokens whose arguments
// already parse as a whole object is reported as a cut (the framing is
// required there; the other wires dispatch the same bytes). The framing
// difference is by design; what is not is the reason text, which tells the
// model its arguments are not a complete JSON object when they are.
func TestAdvB31SettleLimitWholeUnclosedObjectReasonIsTrue(t *testing.T) {
	for _, name := range advWireNames() {
		o := advRun(t, name, advBody(name, []advSpec{{ID: "c1", Name: "bash", Deltas: []string{"{\"a\":1}"}, Closed: false}}, "limit"))
		t.Logf("%s: %s", name, o.class())
		for _, m := range o.resp.Malformed {
			if strings.Contains(m.Reason, "not a complete JSON object") {
				t.Errorf("%s: reported to the model as %q although the arguments {\"a\":1} are a complete object", name, m.Reason)
			}
		}
	}
}

// openaicompat only: the repair step runs before the rule, so a stream cut off
// by the output limit can be repaired into a runnable call.
func TestAdvB31SettleOpenAICompatLimitNeverRunsRepairedArguments(t *testing.T) {
	for _, args := range []string{
		"{\"a\":1}{\"b\":2",                  // second object cut: first extracted by repair
		"{\"cmd\":\"x\"} and then more {\"q", // trailing prose
	} {
		o := advRun(t, "openaicompat", advBody("openaicompat", []advSpec{{ID: "c1", Name: "bash", Deltas: []string{args}}}, "limit"))
		t.Logf("%q -> %s", args, o.class())
		if len(o.calls) != 0 {
			t.Errorf("%q: ended by the output limit and dispatched as %s (repaired=%v)", args, o.class(), o.resp.Decoding.ArgumentsRepaired)
		}
	}
}

// openaicompat only: the reason text goes to the model; it must be bounded.
func TestAdvB31SettleMalformedReasonIsBoundedOnEveryWire(t *testing.T) {
	big := strings.Repeat("x", 1<<20)
	for _, name := range advWireNames() {
		o := advRun(t, name, advBody(name, []advSpec{{ID: "c1", Name: "bash", Deltas: []string{"{" + big}, Closed: true}}, "tool"))
		n := 0
		if o.err != nil {
			n = len(o.err.Error())
		}
		for _, m := range o.resp.Malformed {
			if len(m.Reason) > n {
				n = len(m.Reason)
			}
		}
		t.Logf("%s: %s, reason/error bytes=%d", name, o.class(), n)
		if n > 8<<10 {
			t.Errorf("%s: a 1 MB broken argument string produced a %d-byte reason/error", name, n)
		}
	}
}

func advUnknownBody(wireName, kind string) string {
	k := jsonString(kind)
	if wireName == "anthropic" {
		return anthropicStart +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":" + k + "}}\n\n" +
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"hi\"}}\n\n" +
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" + anthropicFinish("end_turn")
	}
	return "event: step.start\ndata: {\"event_type\":\"step.start\",\"index\":0,\"step\":{\"type\":" + k + "}}\n\n" +
		"event: step.stop\ndata: {\"event_type\":\"step.stop\",\"index\":0,\"step\":{\"type\":" + k + "}}\n\n" +
		"event: step.start\ndata: {\"event_type\":\"step.start\",\"index\":1,\"step\":{\"type\":\"model_output\"}}\n\n" +
		"event: step.delta\ndata: {\"event_type\":\"step.delta\",\"index\":1,\"delta\":{\"type\":\"text\",\"text\":\"hi\"}}\n\n" +
		"event: step.stop\ndata: {\"event_type\":\"step.stop\",\"index\":1}\n\n" + geminiFinish("completed")
}

// Probe 4 end to end: hostile type names on the hosted wires. A name reaches
// either the report (settled) or the refusal (error); neither may carry
// control characters, invalid UTF-8, or an unbounded name.
func TestAdvB31SettleUnknownKindsHostileNamesEndToEnd(t *testing.T) {
	for _, name := range []string{"anthropic", "gemini"} {
		for label, kind := range map[string]string{
			"escape":    "evil\x1b[2J\x1b]0;pwned\x07",
			"newline":   "a\nb\rc",
			"1MB name":  strings.Repeat("z", 1<<20),
			"multibyte": strings.Repeat("\u20ac", 500),
		} {
			t.Run(name+"/"+label, func(t *testing.T) {
				o := advRun(t, name, advUnknownBody(name, kind))
				var texts []string
				if o.err != nil {
					texts = append(texts, o.err.Error())
				}
				texts = append(texts, o.resp.Decoding.UnknownKinds...)
				if len(texts) == 0 {
					t.Fatal("unknown type dropped without a report")
				}
				for _, k := range texts {
					if len(k) > 1<<10 {
						t.Errorf("%d bytes of attacker-chosen type name in one report/error", len(k))
					}
					if !utf8.ValidString(k) {
						t.Errorf("invalid UTF-8 in %.100q", k)
					}
					for _, r := range k {
						if unicode.IsControl(r) {
							t.Errorf("control char %U reaches the report: %.100q", r, k)
							break
						}
					}
				}
			})
		}
	}
}

// Probe 4: 10,000 unknown blocks in one hosted response.
func TestAdvB31SettleUnknownKindsTenThousandBlocksEndToEnd(t *testing.T) {
	var b strings.Builder
	b.WriteString(anthropicStart)
	for i := 0; i < 10000; i++ {
		fmt.Fprintf(&b, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"t%05d\"}}\n\n", i, i)
		fmt.Fprintf(&b, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", i)
	}
	b.WriteString(anthropicFinish("end_turn"))
	o := advRun(t, "anthropic", b.String())
	if o.err != nil {
		t.Skipf("refused by a decode cap, which is acceptable: %v", o.err)
	}
	d := o.resp.Decoding
	if len(d.UnknownKinds) != 16 || d.UnknownKindsMore != 10000-16 {
		t.Fatalf("len=%d more=%d", len(d.UnknownKinds), d.UnknownKindsMore)
	}
}

// Probe 5: gemini.
func TestAdvB31SettleGeminiIDlessMixedWithIDBearing(t *testing.T) {
	o := advRun(t, "gemini", advBody("gemini", []advSpec{
		{ID: "", Name: "bash", Deltas: []string{"{\"a\":1}"}, Closed: true},
		{ID: "c2", Name: "bash", Deltas: []string{"{\"a\":2}"}, Closed: true},
		{ID: "", Name: "bash", Deltas: []string{"{\"a\":3}"}, Closed: true},
	}, "tool"))
	if o.err != nil || len(o.calls) != 3 {
		t.Fatalf("want 3 calls, got %s", o.class())
	}
	for i, c := range o.calls {
		t.Logf("call %d id=%q name=%s args=%s", i, c.ID, c.Name, c.Arguments)
	}
	// Two calls with an empty id must still be told apart downstream: the
	// loop pairs a result with a call by id.
	if o.calls[0].ID == "" && o.calls[2].ID == "" {
		t.Errorf("two id-less calls of the same tool (bash) both dispatched with ID \"\"; neither id nor name tells their results apart")
	}
}

func TestAdvB31SettleDuplicateIDsAreNeverBothDispatched(t *testing.T) {
	for _, name := range advWireNames() {
		o := advRun(t, name, advBody(name, []advSpec{
			{ID: "dup", Name: "bash", Deltas: []string{"{\"a\":1}"}, Closed: true},
			{ID: "dup", Name: "read", Deltas: []string{"{\"b\":2}"}, Closed: true},
		}, "tool"))
		t.Logf("%s: %s", name, o.class())
		seen := map[llm.CallID]int{}
		for _, c := range o.calls {
			seen[c.ID]++
		}
		for id, n := range seen {
			if id != "" && n > 1 {
				t.Errorf("%s: %d calls dispatched with the same id %q; the results answering them are ambiguous and the next request is rejected", name, n, id)
			}
		}
		if o.err == nil && len(o.calls)+len(o.resp.Malformed) < 2 {
			t.Errorf("%s: one of two calls vanished: %s", name, o.class())
		}
	}
}

func TestAdvB31SettleGeminiKnownStepWithUnknownFieldsStaysClean(t *testing.T) {
	body := "event: step.start\ndata: {\"event_type\":\"step.start\",\"index\":0,\"step\":{\"type\":\"function_call\",\"id\":\"c1\",\"name\":\"bash\",\"arguments\":{},\"future_field\":{\"x\":1},\"signature\":\"s\"}}\n\n" +
		"event: step.delta\ndata: {\"event_type\":\"step.delta\",\"index\":0,\"delta\":{\"type\":\"arguments_delta\",\"arguments\":\"{\\\"a\\\":1}\",\"extra\":true}}\n\n" +
		"event: step.stop\ndata: {\"event_type\":\"step.stop\",\"index\":0,\"more\":1}\n\n" + geminiFinish("requires_action")
	o := advRun(t, "gemini", body)
	if o.err != nil || len(o.calls) != 1 {
		t.Fatalf("known step with unknown fields: %s", o.class())
	}
	if !o.resp.Decoding.Clean() {
		t.Errorf("unknown fields on a known step reported as unknown kinds: %+v", o.resp.Decoding)
	}
}

func TestAdvB31SettleGeminiStepTypeCaseIsNotSilentlyDropped(t *testing.T) {
	w := advWire("gemini")
	adaptertest.AssertSettlement(t, adaptertest.SettlementCase{
		Name: "gemini/Function_Call", Provider: w.provider, Request: w.request,
		Body: w.unknown("Function_Call"), Expect: adaptertest.ExpectUnknownNamed, Unknown: "Function_Call",
	})
	// The same case as a call with arguments: it must not be dispatched, and
	// must not vanish.
	body := "event: step.start\ndata: {\"event_type\":\"step.start\",\"index\":0,\"step\":{\"type\":\"Function_Call\",\"id\":\"c1\",\"name\":\"bash\",\"arguments\":{\"a\":1}}}\n\n" +
		"event: step.stop\ndata: {\"event_type\":\"step.stop\",\"index\":0}\n\n" + geminiFinish("requires_action")
	o := advRun(t, "gemini", body)
	t.Logf("%s decoding=%+v", o.class(), o.resp.Decoding)
	if len(o.calls) != 0 {
		t.Errorf("a step of unknown type was dispatched: %s", o.class())
	}
	if o.err == nil && len(o.resp.Decoding.UnknownKinds) == 0 {
		t.Errorf("a function_call-shaped step with a differently-cased type was dropped without a trace; stop=%s", o.resp.StopReason)
	}
}
