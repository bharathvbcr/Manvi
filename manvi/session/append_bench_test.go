package session

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

func BenchmarkAppendToolResult(b *testing.B) {
	text := strings.Repeat("func main() { fmt.Println(\"hello <world> & co\") }\n", 20000) // ~1 MB
	for _, scrubbed := range []bool{false, true} {
		name := "plain"
		if scrubbed {
			name = "scrubbed"
		}
		b.Run(name, func(b *testing.B) {
			log := NewLog()
			if scrubbed {
				log.SetScrubber(func(s string) string { return strings.ReplaceAll(s, "sk-secret-value", "[redacted]") })
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(text)))
			for b.Loop() {
				if _, err := log.Append(ToolResult, ToolResultData{ToolCallID: llm.CallID("c"), Text: text}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
