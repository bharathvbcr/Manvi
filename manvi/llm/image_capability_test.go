package llm

import "testing"

func TestImageCapabilityIncludesNestedToolContent(t *testing.T) {
	c := Capability{Provider: "text", Model: "m"}
	r := Request{Messages: []Message{{Role: RoleUser, Content: []ContentBlock{ToolResultBlock{ToolCallID: "c", Content: []ContentBlock{ImageBlock{MediaType: "image/png", Data: []byte{1}}}}}}}}
	if err := c.Validate(r); err == nil {
		t.Fatal("image inside tool result bypassed image capability")
	}
}
