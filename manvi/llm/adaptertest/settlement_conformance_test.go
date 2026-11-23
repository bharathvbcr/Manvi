package adaptertest_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/llm/gemini"
	"github.com/bharathvbcr/Manvi/manvi/llm/xai"
)

// wire builds one scripted stream for one provider.
type wire struct {
	name     string
	provider func(baseURL string) llm.Provider
	request  llm.Request
	// call scripts one tool call and the end of the response. closed is whether
	// the wire's own end-of-call frame is sent; limit ends the response on the
	// model's output limit. args is the argument text, sent in one fragment.
	call func(id, name, args string, closed, limit bool) string
	// unknown scripts a response carrying one block or step type nobody knows,
	// or nil when this wire has no such thing to script.
	unknown func(kind string) string
	// idOptional is true where results are correlated by name, so a call with
	// no id is answerable (gemini).
	idOptional bool
}

func jsonString(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

const anthropicStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":4}}}\n\n"

func anthropicFinish(stop string) string {
	return "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"" + stop + "\"},\"usage\":{\"output_tokens\":9}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
}

func geminiFinish(status string) string {
	return "event: interaction.completed\ndata: {\"event_type\":\"interaction.completed\",\"interaction\":{\"status\":\"" + status + "\"}}\n\n" +
		"event: done\ndata: [DONE]\n\n"
}

func wires() []wire {
	schema := json.RawMessage("{\"type\":\"object\"}")
	user := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}}}
	tools := []llm.ToolSchema{{Name: "bash", InputSchema: schema}}
	return []wire{
		{
			name:     "anthropic",
			provider: func(u string) llm.Provider { return anthropic.New(u, adaptertest.Secret("k")) },
			request:  llm.Request{Model: "claude-opus-5", MaxTokens: 100, Messages: user, Tools: tools},
			call: func(id, name, args string, closed, limit bool) string {
				stop := "tool_use"
				if limit {
					stop = "max_tokens"
				}
				b := anthropicStart +
					fmt.Sprintf("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":%q,\"name\":%q}}\n\n", id, name)
				if args != "" {
					b += fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":%s}}\n\n", jsonString(args))
				}
				if closed {
					b += "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
				}
				return b + anthropicFinish(stop)
			},
			unknown: func(kind string) string {
				return anthropicStart +
					fmt.Sprintf("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":%q}}\n\n", kind) +
					"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
					"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"hi\"}}\n\n" +
					"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
					anthropicFinish("end_turn")
			},
		},
		{
			name:       "gemini",
			idOptional: true,
			provider:   func(u string) llm.Provider { return gemini.New(u, adaptertest.Secret("k")) },
			request:    llm.Request{Model: "gemini-3.7-flash", Messages: user, Tools: tools},
			call: func(id, name, args string, closed, limit bool) string {
				status := "requires_action"
				if limit {
					status = "max_tokens"
				}
				b := fmt.Sprintf("event: step.start\ndata: {\"event_type\":\"step.start\",\"index\":0,\"step\":{\"type\":\"function_call\",\"id\":%q,\"name\":%q,\"arguments\":{}}}\n\n", id, name)
				if args != "" {
					b += fmt.Sprintf("event: step.delta\ndata: {\"event_type\":\"step.delta\",\"index\":0,\"delta\":{\"type\":\"arguments_delta\",\"arguments\":%s}}\n\n", jsonString(args))
				}
				if closed {
					b += "event: step.stop\ndata: {\"event_type\":\"step.stop\",\"index\":0}\n\n"
				}
				return b + geminiFinish(status)
			},
			unknown: func(kind string) string {
				return fmt.Sprintf("event: step.start\ndata: {\"event_type\":\"step.start\",\"index\":0,\"step\":{\"type\":%q}}\n\n", kind) +
					fmt.Sprintf("event: step.stop\ndata: {\"event_type\":\"step.stop\",\"index\":0,\"step\":{\"type\":%q}}\n\n", kind) +
					"event: step.start\ndata: {\"event_type\":\"step.start\",\"index\":1,\"step\":{\"type\":\"model_output\"}}\n\n" +
					"event: step.delta\ndata: {\"event_type\":\"step.delta\",\"index\":1,\"delta\":{\"type\":\"text\",\"text\":\"hi\"}}\n\n" +
					"event: step.stop\ndata: {\"event_type\":\"step.stop\",\"index\":1}\n\n" +
					geminiFinish("completed")
			},
		},
		{
			name:     "openaicompat (xai)",
			provider: func(u string) llm.Provider { return xai.New(u, adaptertest.Secret("k")) },
			request:  llm.Request{Model: "grok-4.3", Messages: user, Tools: tools},
			// No per-call framing on this wire, so closed has nothing to send.
			call: func(id, name, args string, _, limit bool) string {
				finish := "tool_calls"
				if limit {
					finish = "length"
				}
				fn := fmt.Sprintf("{\"name\":%q", name)
				if args != "" {
					fn += ",\"arguments\":" + jsonString(args)
				}
				fn += "}"
				idField := ""
				if id != "" {
					idField = fmt.Sprintf("\"id\":%q,", id)
				}
				return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,%s\"function\":%s}]}}]}\n\n", idField, fn) +
					fmt.Sprintf("data: {\"choices\":[{\"delta\":{},\"finish_reason\":%q}]}\n\n", finish) +
					"data: [DONE]\n\n"
			},
		},
	}
}

// TestEveryDecoderSettlesACallByTheSameRule runs the one definition of a
// complete tool call, llm.SettleCall, against every wire through one table.
func TestEveryDecoderSettlesACallByTheSameRule(t *testing.T) {
	type scenario struct {
		name           string
		id, tool, args string
		closed, limit  bool
		expect         adaptertest.SettlementExpect
	}
	for _, sc := range []scenario{
		// A named call with zero argument bytes, ended by the output limit, is a
		// call whose arguments never arrived. It is never {}.
		{name: "named call, no argument bytes, output limit", id: "c1", tool: "bash", limit: true, expect: adaptertest.ExpectCutCall},
		{name: "named call, cut arguments, output limit", id: "c1", tool: "bash", args: "{\"command\":\"rm ", closed: true, limit: true, expect: adaptertest.ExpectCutCall},
		// Whole JSON that is not an object is not arguments.
		{name: "array arguments", id: "c1", tool: "bash", args: "[1,2]", closed: true, expect: adaptertest.ExpectNoDispatch},
		{name: "string arguments", id: "c1", tool: "bash", args: "\"ls\"", closed: true, expect: adaptertest.ExpectNoDispatch},
		{name: "array arguments, output limit", id: "c1", tool: "bash", args: "[1,2]", closed: true, limit: true, expect: adaptertest.ExpectNoDispatch},
		{name: "two objects run together", id: "c1", tool: "bash", args: "{\"a\":1}{\"b\":2}", closed: true, expect: adaptertest.ExpectNoDispatch},
		// A call that cannot be routed is not run, whatever else is true of it.
		{name: "empty name", id: "c1", tool: "", args: "{}", closed: true, expect: adaptertest.ExpectNoDispatch},
		{name: "empty name, output limit", id: "c1", tool: "", args: "{\"p", limit: true, expect: adaptertest.ExpectNoDispatch},
		// A whole, closed call settles. Whatever runs has an id and a name.
		{name: "a complete call", id: "c1", tool: "bash", args: "{\"command\":\"ls\"}", closed: true, expect: adaptertest.ExpectRunnableCalls},
	} {
		for _, w := range wires() {
			t.Run(w.name+"/"+sc.name, func(t *testing.T) {
				adaptertest.AssertSettlement(t, adaptertest.SettlementCase{
					Name: w.name + "/" + sc.name, Provider: w.provider, Request: w.request,
					Body: w.call(sc.id, sc.tool, sc.args, sc.closed, sc.limit), Expect: sc.expect,
				})
			})
		}
	}
}

// TestEveryDecoderNeverRunsACallWithNoId: a call with no id leaves the result
// answering it referencing nothing. The OpenAI-compatible wire is allowed to
// invent one, because local servers omit ids routinely; the others refuse.
// What none may do is run a call that has none, except where the wire answers
// by name.
func TestEveryDecoderNeverRunsACallWithNoId(t *testing.T) {
	for _, w := range wires() {
		t.Run(w.name, func(t *testing.T) {
			server := adaptertest.NewServer(t, w.call("", "bash", "{\"command\":\"ls\"}", true, false))
			stream, err := w.provider(server.URL).Stream(adaptertest.Ctx(), w.request)
			if err != nil {
				t.Fatal(err)
			}
			_, resp, err := adaptertest.Drain(stream)
			for _, call := range resp.Message.ToolCalls() {
				if strings.TrimSpace(string(call.ID)) == "" && !w.idOptional {
					t.Fatalf("a call with no id was dispatched: %+v (err=%v)", call, err)
				}
			}
			if err == nil && len(resp.Message.ToolCalls())+len(resp.Malformed) == 0 {
				t.Fatal("the call vanished: neither dispatched, reported nor refused")
			}
		})
	}
}

// TestEveryDecoderNamesAnUnknownBlockType: a block or step type the adapter has
// no reading for is content the settled message cannot hold. Dropping it
// silently leaves a turn that looks complete and is not.
func TestEveryDecoderNamesAnUnknownBlockType(t *testing.T) {
	for _, w := range wires() {
		if w.unknown == nil {
			continue
		}
		t.Run(w.name, func(t *testing.T) {
			adaptertest.AssertSettlement(t, adaptertest.SettlementCase{
				Name: w.name, Provider: w.provider, Request: w.request,
				Body: w.unknown("mystery_block"), Expect: adaptertest.ExpectUnknownNamed, Unknown: "mystery_block",
			})
		})
	}
}
