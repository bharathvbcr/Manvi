package session

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// Save reuses the per-event encodings it measured with. That is only sound if
// joining them is exactly what marshalling the kept slice produces — escaping,
// empty payloads and all — at every trim point.
func TestJoinedEncodingsMatchMarshallingTheSlice(t *testing.T) {
	log := NewLog()
	for turn := 0; turn < 4; turn++ {
		if _, err := log.Append(TurnStart, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := log.Append(UserMessage, MessageData{Message: llm.Message{
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{llm.TextBlock{Text: "<html> &   " + strings.Repeat("x", 200)}},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	events := log.snapshot()
	for _, budget := range []int{1 << 20, 1200, 600, 10} {
		from, _, _, perEvent, err := trimToBudget(events, budget)
		if err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal(events[from:])
		if err != nil {
			t.Fatal(err)
		}
		if got := joinArray(perEvent[from:]); !bytes.Equal(got, want) {
			t.Fatalf("budget %d: joined encoding differs\n got %s\nwant %s", budget, got, want)
		}
	}
	if got := joinArray(nil); string(got) != "[]" {
		t.Fatalf("empty join = %s", got)
	}
}
