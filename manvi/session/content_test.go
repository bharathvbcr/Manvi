package session

import (
	"encoding/json"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"reflect"
	"testing"
)

func TestToolContentSurvivesWarmColdAndRestore(t *testing.T) {
	log := NewLog()
	if _, err := log.DeriveMessages(); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, log, ToolResult, ToolResultData{ToolCallID: "c1", Content: llm.Content{llm.TextBlock{Text: "screen"}, llm.ImageBlock{MediaType: "image/png", Data: []byte{1, 2, 3}}}})
	warm, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	if len(warm) != 1 || len(warm[0].Content[0].(llm.ToolResultBlock).Content) != 2 {
		t.Fatalf("projection=%v", warm)
	}
	raw, err := json.Marshal(log.Events())
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	if err := json.Unmarshal(raw, &events); err != nil {
		t.Fatal(err)
	}
	loaded, err := RestoreLog(events)
	if err != nil {
		t.Fatal(err)
	}
	cold, err := loaded.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(warm, cold) {
		t.Fatal("warm and restored image projections diverge")
	}
}

func TestAmbiguousToolContentRejectedWithoutAppending(t *testing.T) {
	log := NewLog()
	_, err := log.Append(ToolResult, ToolResultData{Text: "one", Content: llm.Content{llm.TextBlock{Text: "two"}}})
	if err == nil || len(log.Events()) != 0 {
		t.Fatal("ambiguous tool result entered journal")
	}
}
