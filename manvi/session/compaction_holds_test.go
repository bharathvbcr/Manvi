package session

import (
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// HoldsResults says whether a message list carries the tool results of the log's
// projection, by call id and order, which is what makes a place among results
// sharing an id mean the same result in both.
func TestHoldsResultsComparesCallIDsInOrder(t *testing.T) {
	l := NewLog()
	appendEvent(t, l, UserMessage, MessageData{Message: user("go")})
	for _, id := range []string{"a", "a", "b"} {
		appendEvent(t, l, AssistantMessage, MessageData{Message: callMessage(id)})
		appendEvent(t, l, ToolResult, ToolResultData{ToolCallID: llm.CallID(id), Text: id})
	}
	msgs, err := l.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	if msgs == nil {
		t.Fatal("the log projects no history")
	}
	for name, tc := range map[string]struct {
		msgs []llm.Message
		want bool
	}{
		"the projection itself":  {msgs, true},
		"the head cut off":       {msgs[3:], false},
		"the tail cut off":       {msgs[:len(msgs)-2], false},
		"no tool results at all": {msgs[:1], false},
	} {
		got, err := l.HoldsResults(tc.msgs)
		if err != nil || got != tc.want {
			t.Errorf("%s: HoldsResults = %v, %v; want %v", name, got, err, tc.want)
		}
	}
	empty := NewLog()
	if got, err := empty.HoldsResults(nil); err != nil || !got {
		t.Errorf("an empty log holds no results, and so do no messages: %v, %v", got, err)
	}
}
