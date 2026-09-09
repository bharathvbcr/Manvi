package session

import (
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"testing"
)

func TestImageInvariantChecksBytesIncludingToolResults(t *testing.T) {
	for _, nested := range []bool{false, true} {
		image := llm.ImageBlock{MediaType: "image/png", Data: []byte{1, 2, 3}}
		block := llm.ContentBlock(image)
		if nested {
			block = llm.ToolResultBlock{ToolCallID: "c1", Content: []llm.ContentBlock{image}}
		}
		logged := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{block}}
		sent, err := llm.CloneMessages([]llm.Message{logged})
		if err != nil {
			t.Fatal(err)
		}
		if nested {
			sent[0].Content[0].(llm.ToolResultBlock).Content[0].(llm.ImageBlock).Data[0] = 9
		} else {
			sent[0].Content[0].(llm.ImageBlock).Data[0] = 9
		}
		if err := sameVisibleContent(logged, sent[0]); err == nil {
			t.Fatalf("changed image accepted; nested=%v", nested)
		}
	}
}
