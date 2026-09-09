package tools

import (
	"context"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"testing"
)

func TestTypedResultContentIsScrubbedOwnedAndValidated(t *testing.T) {
	image := []byte{1, 2, 3}
	r := scrubbingRegistry(t)
	if err := r.Register(Tool{Schema: leakySchema("typed"), Handler: func(context.Context, Call) Result {
		return Result{Content: llm.Content{llm.TextBlock{Text: fakeKey}, llm.ImageBlock{MediaType: "image/png", Data: image}}}
	}}); err != nil {
		t.Fatal(err)
	}
	got := r.Run(context.Background(), Call{Name: "leaky"})
	image[0] = 9
	if got.IsError || got.Content[0].(llm.TextBlock).Text != "[redacted]" || got.Content[1].(llm.ImageBlock).Data[0] != 1 {
		t.Fatalf("invalid result %+v", got)
	}
	for _, bad := range []Result{{Text: "one", Content: llm.Content{llm.TextBlock{Text: "two"}}}, {Content: llm.Content{llm.ToolCallBlock{Name: "nested"}}}, {Content: llm.Content{llm.ImageBlock{MediaType: "image/png"}}}} {
		registry, _ := registryWith(t, func(context.Context, Call) Result { return bad })
		if got := registry.Run(context.Background(), Call{Name: "probe"}); !got.IsError || len(got.Content) != 0 {
			t.Fatalf("invalid content accepted %+v", got)
		}
	}
}
