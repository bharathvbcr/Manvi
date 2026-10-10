package tools

import (
	"context"
	"fmt"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// BenchmarkSchemas is what the agent loop pays to ask for the offered tools,
// which it does at least twice a step.
func BenchmarkSchemas(b *testing.B) {
	r := NewRegistry(nil)
	for i := 0; i < 80; i++ {
		if err := r.Register(Tool{Schema: llm.ToolSchema{Name: fmt.Sprintf("tool_%02d", (i*37)%80)}, ReadOnly: i%3 == 0, Handler: func(context.Context, Call) Result { return Result{} }}); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Schemas()
	}
}
