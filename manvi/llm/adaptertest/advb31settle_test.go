package adaptertest_test

// Adversarial pass on the b31-settle change (one settlement rule for LLM tool
// calls). Each test PASSES if the change holds; a failure is a break.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
)

type advSpec struct {
	ID, Name string
	Deltas   []string
	Closed   bool
}

// stops: end, tool, limit, filter, none.
func advBody(wireName string, specs []advSpec, stop string) string {
	var b strings.Builder
	switch {
	case wireName == "anthropic":
		b.WriteString(anthropicStart)
		for i, s := range specs {
			fmt.Fprintf(&b, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"tool_use\",\"id\":%q,\"name\":%q}}\n\n", i, s.ID, s.Name)
			for _, d := range s.Deltas {
				fmt.Fprintf(&b, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":%s}}\n\n", i, jsonString(d))
			}
			if s.Closed {
				fmt.Fprintf(&b, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", i)
			}
		}
		m := map[string]string{"end": "end_turn", "tool": "tool_use", "limit": "max_tokens", "filter": "refusal"}
		if stop == "none" {
			b.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		} else {
			b.WriteString(anthropicFinish(m[stop]))
		}
	case wireName == "gemini":
		for i, s := range specs {
			idf := ""
			if s.ID != "" {
				idf = fmt.Sprintf("\"id\":%q,", s.ID)
			}
			fmt.Fprintf(&b, "event: step.start\ndata: {\"event_type\":\"step.start\",\"index\":%d,\"step\":{\"type\":\"function_call\",%s\"name\":%q,\"arguments\":{}}}\n\n", i, idf, s.Name)
			for _, d := range s.Deltas {
				fmt.Fprintf(&b, "event: step.delta\ndata: {\"event_type\":\"step.delta\",\"index\":%d,\"delta\":{\"type\":\"arguments_delta\",\"arguments\":%s}}\n\n", i, jsonString(d))
			}
			if s.Closed {
				fmt.Fprintf(&b, "event: step.stop\ndata: {\"event_type\":\"step.stop\",\"index\":%d}\n\n", i)
			}
		}
		m := map[string]string{"end": "completed", "tool": "requires_action", "limit": "max_tokens", "filter": "failed"}
		if stop == "none" {
			b.WriteString("event: done\ndata: [DONE]\n\n")
		} else {
			b.WriteString(geminiFinish(m[stop]))
		}
	default:
		for i, s := range specs {
			for j, d := range s.Deltas {
				fn := "{}"
				idField := ""
				if j == 0 {
					fn = fmt.Sprintf("{\"name\":%q,\"arguments\":%s}", s.Name, jsonString(d))
					if s.ID != "" {
						idField = fmt.Sprintf("\"id\":%q,", s.ID)
					}
				} else {
					fn = fmt.Sprintf("{\"arguments\":%s}", jsonString(d))
				}
				fmt.Fprintf(&b, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":%d,%s\"function\":%s}]}}]}\n\n", i, idField, fn)
			}
			if len(s.Deltas) == 0 {
				idField := ""
				if s.ID != "" {
					idField = fmt.Sprintf("\"id\":%q,", s.ID)
				}
				fmt.Fprintf(&b, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":%d,%s\"function\":{\"name\":%q}}]}}]}\n\n", i, idField, s.Name)
			}
		}
		m := map[string]string{"end": "stop", "tool": "tool_calls", "limit": "length", "filter": "content_filter"}
		if stop != "none" {
			fmt.Fprintf(&b, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":%q}]}\n\n", m[stop])
		}
		b.WriteString("data: [DONE]\n\n")
	}
	return b.String()
}

func advWire(name string) wire {
	for _, w := range wires() {
		if strings.HasPrefix(w.name, name) {
			return w
		}
	}
	panic("no wire " + name)
}

func advWireNames() []string { return []string{"anthropic", "gemini", "openaicompat"} }

type advOut struct {
	calls []llm.ToolCallBlock
	resp  llm.Response
	err   error
}

func advRun(t *testing.T, name, body string) advOut {
	t.Helper()
	w := advWire(name)
	server := adaptertest.NewServer(t, body)
	stream, err := w.provider(server.URL).Stream(adaptertest.Ctx(), w.request)
	if err != nil {
		return advOut{err: err}
	}
	_, resp, err := adaptertest.Drain(stream)
	return advOut{calls: resp.Message.ToolCalls(), resp: resp, err: err}
}

func (o advOut) class() string {
	switch {
	case len(o.calls) > 0:
		a := make([]string, 0, len(o.calls))
		for _, c := range o.calls {
			s := string(c.Arguments)
			if len(s) > 40 {
				s = s[:40] + "..."
			}
			a = append(a, fmt.Sprintf("%s(%s)", c.ID, s))
		}
		return "DISPATCH " + strings.Join(a, ",")
	case o.err != nil:
		e := o.err.Error()
		if len(e) > 90 {
			e = e[:90] + "..."
		}
		return "ERROR " + e
	case len(o.resp.Malformed) > 0:
		return fmt.Sprintf("MALFORMED x%d stop=%s", len(o.resp.Malformed), o.resp.StopReason)
	}
	return "NOTHING stop=" + string(o.resp.StopReason)
}

func advIsObject(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(raw, &m) == nil
}

// Probe 2: edge arguments, on every wire.
func TestAdvB31SettleEdgeArgumentsAcrossWires(t *testing.T) {
	huge := "{\"k\":\"" + strings.Repeat("A", 1<<20) + "\"}"
	var hugeDeltas []string
	for i := 0; i < len(huge); i += 16 << 10 {
		e := i + 16<<10
		if e > len(huge) {
			e = len(huge)
		}
		hugeDeltas = append(hugeDeltas, huge[i:e])
	}
	many := []string{"{\"k\":\""}
	for i := 0; i < 1000; i++ {
		many = append(many, "a")
	}
	many = append(many, "\"}")

	type tc struct {
		name   string
		deltas []string
		want   string // "run" or "refuse"
		check  func(t *testing.T, args json.RawMessage)
	}
	cases := []tc{
		{"whitespace-only args, closed", []string{"  \n\t "}, "run", nil},
		{"object with trailing whitespace", []string{"{}  \n\t"}, "run", nil},
		{"two objects", []string{"{} {}"}, "refuse", nil},
		{"BOM-prefixed object", []string{"\ufeff{}"}, "refuse-or-clean", nil},
		{"JSON null", []string{"null"}, "refuse", nil},
		{"top-level exponent number", []string{"1e5"}, "refuse", nil},
		{"top-level negative exponent", []string{"-1.5E+3"}, "refuse", nil},
		{"1 MB object in 16 KB deltas", hugeDeltas, "run", func(t *testing.T, a json.RawMessage) {
			if len(a) != len(huge) {
				t.Errorf("args length %d, want %d: bytes were lost or altered", len(a), len(huge))
			}
		}},
		{"1000 deltas", many, "run", func(t *testing.T, a json.RawMessage) {
			var m map[string]string
			if err := json.Unmarshal(a, &m); err != nil || len(m["k"]) != 1000 {
				t.Errorf("reassembled args wrong: err=%v len=%d", err, len(m["k"]))
			}
		}},
		{"unicode escape split mid-sequence", []string{"{\"k\":\"\\u00", "e9\"}"}, "run", func(t *testing.T, a json.RawMessage) {
			var m map[string]string
			if err := json.Unmarshal(a, &m); err != nil || m["k"] != "\u00e9" {
				t.Errorf("k=%q err=%v, want \u00e9", m["k"], err)
			}
		}},
		{"surrogate pair escape split mid-pair", []string{"{\"k\":\"\\ud83d", "\\ude00\"}"}, "run", func(t *testing.T, a json.RawMessage) {
			var m map[string]string
			if err := json.Unmarshal(a, &m); err != nil || m["k"] != "\U0001F600" {
				t.Errorf("k=%q err=%v", m["k"], err)
			}
		}},
	}
	for _, c := range cases {
		for _, name := range advWireNames() {
			t.Run(name+"/"+c.name, func(t *testing.T) {
				o := advRun(t, name, advBody(name, []advSpec{{ID: "c1", Name: "bash", Deltas: c.deltas, Closed: true}}, "tool"))
				t.Logf("%s", o.class())
				switch c.want {
				case "run":
					if len(o.calls) != 1 {
						t.Fatalf("not dispatched: %s", o.class())
					}
					if !advIsObject(o.calls[0].Arguments) {
						t.Fatalf("dispatched non-object args %q", o.calls[0].Arguments)
					}
					if c.check != nil {
						c.check(t, o.calls[0].Arguments)
					}
				case "refuse":
					if len(o.calls) != 0 {
						t.Fatalf("dispatched %q", o.calls[0].Arguments)
					}
					if o.err == nil && len(o.resp.Malformed) == 0 {
						t.Fatalf("vanished silently: %s", o.class())
					}
				case "refuse-or-clean":
					for _, call := range o.calls {
						if strings.HasPrefix(string(call.Arguments), "\ufeff") || !advIsObject(call.Arguments) {
							t.Fatalf("dispatched BOM/non-object args %q", call.Arguments)
						}
					}
				}
			})
		}
	}
}
