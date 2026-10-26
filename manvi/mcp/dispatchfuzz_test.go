package mcp

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// FuzzDispatchNeverAnswersUnderSomeoneElsesID fuzzes the one function in this
// package that reads bytes a stranger wrote.
//
// An MCP server is a process from somewhere else entirely, and everything it
// says arrives here as a line on stdout. dispatch is what decides, per line,
// whether that line is a reply, whose call it is a reply to, and whether the
// stream can still be trusted — and the results it routes go straight back to
// whichever tool call is blocked waiting for them. Six decode sites in this
// package read that input and not one of them had a fuzz target.
//
// The property that matters is the one `serve` had a defect in: a reply must
// reach the call it names and no other. Two tool calls are in flight at once as
// a matter of course, so a frame delivered to the wrong pending id hands call A
// the result of call B — and the caller cannot tell, because a result is just a
// JSON document and both calls were expecting one.
//
// The invariants:
//
//   - dispatch never panics, whatever the bytes.
//   - A frame resolves either nothing, exactly one call, or — when correlation
//     is judged untrustworthy — all of them. Never a strict subset, which would
//     leave the unresolved callers waiting on a stream this client has already
//     decided it cannot follow.
//   - When exactly one call is resolved, it is the one the frame's own id names
//     under numericID. Not a neighbour, and not whichever happened to be first.
//   - A resolved call is removed from the pending map, so a second frame
//     carrying the same id cannot deliver into a channel nobody is reading and
//     nothing is left to be resolved twice.
//   - A frame that is not JSON, or that carries a method, never resolves a
//     call: the first is server chatter and the second is a notification, and
//     neither is a reply to anything.
func FuzzDispatchNeverAnswersUnderSomeoneElsesID(f *testing.F) {
	for _, seed := range []string{
		`{"jsonrpc":"2.0","id":1,"result":{}}`,
		`{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}`,
		`{"jsonrpc":"2.0","id":3,"error":{"code":-32601,"message":"no such method"}}`,
		`{"jsonrpc":"2.0","id":"2","result":{}}`,
		`{"jsonrpc":"2.0","id":" 2 ","result":{}}`,
		`{"jsonrpc":"2.0","id":"+2","result":{}}`,
		`{"jsonrpc":"2.0","id":02,"result":{}}`,
		`{"jsonrpc":"2.0","id":2.0,"result":{}}`,
		`{"jsonrpc":"2.0","id":2.5,"result":{}}`,
		`{"jsonrpc":"2.0","id":null,"result":{}}`,
		`{"jsonrpc":"2.0","id":true,"result":{}}`,
		`{"jsonrpc":"2.0","id":[2],"result":{}}`,
		`{"jsonrpc":"2.0","id":{"n":2},"result":{}}`,
		`{"jsonrpc":"2.0","id":99,"result":{}}`,
		`{"jsonrpc":"2.0","id":-1,"result":{}}`,
		`{"jsonrpc":"2.0","id":9223372036854775807,"result":{}}`,
		`{"jsonrpc":"2.0","id":9223372036854775808,"result":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/message","params":{}}`,
		`{"method":"sampling/createMessage","id":1}`,
		`{"jsonrpc":"2.0","id":7,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":"r-1","method":"roots/list"}`,
		`{"jsonrpc":"2.0","id":null,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":1}`,
		`{"result":{}}`,
		`{"log":"a server writing structured chatter to stdout"}`,
		`plain server chatter on stdout`,
		``,
		`   `,
		`{`,
		`{"jsonrpc":"2.0","id":1,"result":`,
		"\x00\x01\x02",
		`[{"jsonrpc":"2.0","id":1,"result":{}}]`,
		`{"jsonrpc":"2.0","id":1,"result":{}}{"jsonrpc":"2.0","id":2,"result":{}}`,
		`{"jsonrpc":"2.0","id":1,"result":{},"id":2}`,
		`{"jsonrpc":"2.0","ID":1,"result":{}}`,
		`{"jsonrpc":"2.0","id":1,"result":` + strings.Repeat("[", 200) + strings.Repeat("]", 200) + `}`,
	} {
		f.Add(seed)
	}

	// The ids this client is pretending to have issued. Three rather than one
	// so "delivered to the wrong call" is expressible at all, and non-adjacent
	// so an off-by-one lands on a gap rather than on a neighbour by luck.
	inFlight := []int64{1, 2, 3, 17}

	f.Fuzz(func(t *testing.T, line string) {
		// A real stdin, so a reply the client writes is observable. With none,
		// answerServerFrame's write failed into a diagnostic and the target
		// could not tell a request that was answered from one that was not.
		in := &recordingStdin{}
		c := &Client{
			cfg:     ServerConfig{Name: "fuzz-server"},
			stdin:   in,
			pending: make(map[int64]chan *Response, len(inFlight)),
		}
		// Buffered with room for one, which is what Call allocates and what
		// failAllPending's comment relies on to say it cannot block.
		chans := make(map[int64]chan *Response, len(inFlight))
		for _, id := range inFlight {
			ch := make(chan *Response, 1)
			chans[id] = ch
			c.pending[id] = ch
		}

		c.dispatch([]byte(line))

		checkServerRequestAnswered(t, line, in.frames())

		resolved := []int64{}
		for _, id := range inFlight {
			if len(chans[id]) > 0 {
				resolved = append(resolved, id)
			}
		}

		switch len(resolved) {
		case 0:
			// Nothing routed. Every pending call must still be pending, or a
			// caller is waiting on a channel this client has forgotten.
			if len(c.pending) != len(inFlight) {
				t.Fatalf("dispatch(%q) resolved nothing but left %d of %d calls pending",
					line, len(c.pending), len(inFlight))
			}
		case len(inFlight):
			// The correlation-is-untrustworthy path. It empties the map by
			// design, so every caller is answered rather than left waiting.
			if len(c.pending) != 0 {
				t.Fatalf("dispatch(%q) failed every call but left %d pending", line, len(c.pending))
			}
		case 1:
			// Chatter and notifications are not replies, and must never have a
			// result delivered to a call. This is scoped to the single-delivery
			// case on purpose: failing every call is not delivering a result to
			// one, it is telling all of them the stream can no longer be
			// followed, and an RPC-shaped frame that does not parse is exactly
			// when that is right.
			var probe struct {
				Method string `json:"method"`
			}
			if json.Unmarshal([]byte(line), &probe) != nil {
				t.Fatalf("dispatch(%q) delivered to call %d from bytes that are not JSON at all",
					line, resolved[0])
			}
			if probe.Method != "" {
				t.Fatalf("dispatch(%q) delivered to call %d from a frame carrying method %q, "+
					"which is a notification and not a reply to anything", line, resolved[0], probe.Method)
			}

			want, ok := numericID(decodedID(line))
			if !ok {
				t.Fatalf("dispatch(%q) delivered to call %d, but the frame carries no id this "+
					"client can match; the result reached a call the frame does not name",
					line, resolved[0])
			}
			if resolved[0] != want {
				t.Fatalf("dispatch(%q) delivered to call %d, but the frame names %d; "+
					"that call receives another call's result and cannot tell",
					line, resolved[0], want)
			}
			if _, still := c.pending[resolved[0]]; still {
				t.Fatalf("dispatch(%q) delivered to call %d and left it pending; a second frame "+
					"with the same id would deliver into a channel nobody reads", line, resolved[0])
			}
		default:
			t.Fatalf("dispatch(%q) resolved %v — a strict subset of the %d calls in flight. "+
				"The rest are waiting on a stream this client has already judged unusable",
				line, resolved, len(inFlight))
		}

	})
}

// checkServerRequestAnswered asserts the other half of dispatch's contract: a
// server-initiated request — a method and a non-null id, in a frame the client
// can decode at all — is answered exactly once, under the id it carried, and
// nothing else is ever written. A server waits on its own request forever
// otherwise, and a reply written for a notification or for somebody's reply
// would be a frame the server never asked for.
func checkServerRequestAnswered(t *testing.T, line string, frames []string) {
	t.Helper()
	var probe struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	var envelope Response
	isRequest := json.Unmarshal([]byte(line), &probe) == nil &&
		json.Unmarshal([]byte(line), &envelope) == nil &&
		probe.Method != "" && len(probe.ID) > 0 && string(probe.ID) != "null"

	if !isRequest {
		if len(frames) != 0 {
			t.Fatalf("dispatch(%q) wrote %q for a frame that is not a server request", line, frames)
		}
		return
	}
	if len(frames) != 1 {
		t.Fatalf("dispatch(%q) is a server request and was answered %d times: %q", line, len(frames), frames)
	}
	var reply struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err := json.Unmarshal([]byte(frames[0]), &reply); err != nil {
		t.Fatalf("dispatch(%q) wrote a reply that is not JSON: %q", line, frames[0])
	}
	var wantID, gotID any
	if json.Unmarshal(probe.ID, &wantID) != nil || json.Unmarshal(reply.ID, &gotID) != nil || !reflect.DeepEqual(wantID, gotID) {
		t.Fatalf("dispatch(%q) answered under id %s, want %s", line, reply.ID, probe.ID)
	}
	if (reply.Result == nil) == (reply.Error == nil) {
		t.Fatalf("dispatch(%q) wrote a reply with neither or both of result and error: %q", line, frames[0])
	}
	if probe.Method == "ping" && reply.Error != nil {
		t.Fatalf("dispatch(%q): ping must get the empty result, got %q", line, frames[0])
	}
}

// decodedID returns the raw id a frame carries, decoded the way Response
// decodes it, or nil when the line is not a JSON object at all. It exists so
// the assertions above can ask "which call does this frame name" without
// reusing dispatch's own answer to that question.
func decodedID(line string) any {
	var probe struct {
		ID any `json:"id"`
	}
	if json.Unmarshal([]byte(line), &probe) != nil {
		return nil
	}
	return probe.ID
}
