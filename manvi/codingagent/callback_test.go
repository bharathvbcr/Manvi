package codingagent

import (
	"strings"
	"testing"
	"time"
)

type callbackState struct {
	sent, finish bool
	exp          time.Time
}

// TestEveryAdapterRejectsEachStaleCallbackField drives each field of the
// pending-callback match through both managed adapters. An adapter that stops
// calling the shared predicate, or a predicate that stops checking a field,
// fails the matching row.
func TestEveryAdapterRejectsEachStaleCallbackField(t *testing.T) {
	payload := `{"method":"item/commandExecution/requestApproval"}`
	source := Request{
		ID: "r1", ThreadID: "th", TurnID: "tu", Kind: "permission",
		Payload: payload, Digest: payloadDigest(payload),
		ExpiresAt: time.Now().Add(time.Hour),
	}
	otherPayload := payload + " "
	cases := []struct {
		name   string
		mutate func(*Request)
		finish bool
		sent   bool
	}{
		{name: "id", mutate: func(r *Request) { r.ID = "r2" }},
		{name: "payload", mutate: func(r *Request) { r.Payload = otherPayload }},
		{name: "digest", mutate: func(r *Request) { r.Digest = payloadDigest(otherPayload) }},
		{name: "recomputed digest", mutate: func(r *Request) { r.Payload, r.Digest = otherPayload, source.Digest }},
		{name: "thread", mutate: func(r *Request) { r.ThreadID = "other" }},
		{name: "turn", mutate: func(r *Request) { r.TurnID = "other" }},
		{name: "kind", mutate: func(r *Request) { r.Kind = "question" }},
		{name: "expiry", mutate: func(r *Request) { r.ExpiresAt = source.ExpiresAt }, finish: false},
		{name: "finished", mutate: func(*Request) {}, finish: true},
		{name: "sent", mutate: func(*Request) {}, sent: true},
	}
	response := Response{Decision: "deny"}
	adapters := []struct {
		name  string
		agent string
		reply func(c callbackState, request Request) error
	}{
		{"claude", "Claude", func(c callbackState, request Request) error {
			src := source
			src.ExpiresAt = c.exp
			s := &Claude{finished: c.finish, pending: map[string]*claudePending{"r1": {source: src, requestID: "r1", sent: c.sent}}}
			_, err := s.response(request, response)
			return err
		}},
		{"codex", "Codex", func(c callbackState, request Request) error {
			src := source
			src.ExpiresAt = c.exp
			s := &Codex{finished: c.finish, pending: map[string]*pendingRequest{"r1": {source: src, sent: c.sent}}}
			_, err := s.response(request, response)
			return err
		}},
	}
	for _, a := range adapters {
		for _, tc := range cases {
			t.Run(a.name+"/"+tc.name, func(t *testing.T) {
				request := source
				state := callbackState{tc.sent, tc.finish, source.ExpiresAt}
				tc.mutate(&request)
				if tc.name == "expiry" {
					state.exp = time.Now().Add(-time.Second)
				}
				err := a.reply(state, request)
				if err == nil || !strings.Contains(err.Error(), a.agent+" callback is stale, changed or already consumed") {
					t.Fatalf("%s accepted a callback with a stale %s: %v", a.agent, tc.name, err)
				}
			})
		}
		t.Run(a.name+"/unchanged is accepted", func(t *testing.T) {
			state := callbackState{false, false, source.ExpiresAt}
			if err := a.reply(state, source); err != nil {
				t.Fatalf("%s refused an unchanged live callback: %v", a.agent, err)
			}
		})
	}
}
