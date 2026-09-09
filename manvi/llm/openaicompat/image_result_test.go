package openaicompat

import (
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"testing"
)

func TestUnsupportedNestedImagesFailInsteadOfDisappearing(t *testing.T) {
	msg := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultBlock{ToolCallID: "c", Content: []llm.ContentBlock{llm.TextBlock{Text: "screen"}, llm.ImageBlock{MediaType: "image/png", Data: []byte{1}}}}}}
	if _, err := toWireMessages(msg, "test"); err == nil {
		t.Fatal("unsupported image silently removed from tool result")
	}
}
