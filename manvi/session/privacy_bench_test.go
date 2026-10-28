package session

// Benchmarks for the privacy walk: Public is the nil-scrubber path every message
// append takes, Append adds an armed scrubber and a key-heavy tool call. The
// key rewrite must stay within a few percent of the walk it extended.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

func benchBigArgs() json.RawMessage {
	m := map[string]any{}
	for i := 0; i < 50; i++ {
		inner := map[string]any{}
		for j := 0; j < 4; j++ {
			inner[fmt.Sprintf("field_%d", j)] = strings.Repeat("v", 60)
		}
		m[fmt.Sprintf("key_%02d", i)] = map[string]any{
			"name":   fmt.Sprintf("item-%d", i),
			"body":   strings.Repeat("lorem ipsum ", 25),
			"nested": inner,
			"list":   []any{map[string]any{"a": 1, "b": "x"}, map[string]any{"a": 2, "b": "y"}},
		}
	}
	b, _ := json.Marshal(m)
	return b
}

func benchMsg(args json.RawMessage) MessageData {
	return MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.TextBlock{Text: "running"},
		llm.ToolCallBlock{ID: "c1", Name: "http", Arguments: args},
	}}}
}

func benchArmed(s string) string {
	s = strings.ReplaceAll(s, "sk-secret-value-1", "[redacted]")
	s = strings.ReplaceAll(s, "sk-secret-value-22", "[redacted]")
	return strings.ReplaceAll(s, "ghp_secretvalue333", "[redacted]")
}

func BenchmarkScrubPublic(b *testing.B) {
	big, small := benchBigArgs(), json.RawMessage(`{"path":"a/b/c.go","opts":{"n":1}}`)
	b.Logf("big args %d bytes", len(big))
	for name, args := range map[string]json.RawMessage{"big": big, "small": small} {
		data, _ := json.Marshal(benchMsg(args))
		ev := Event{Type: AssistantMessage, Data: data}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := ev.Public(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkScrubAppend(b *testing.B) {
	big, small := benchBigArgs(), json.RawMessage(`{"path":"a/b/c.go","opts":{"n":1}}`)
	text := strings.Repeat("func main() { fmt.Println(\"hi\") }\n", 600) // ~20KB
	for _, armed := range []bool{false, true} {
		mode := "plain"
		if armed {
			mode = "armed"
		}
		for _, sz := range []string{"big", "small"} {
			args := big
			if sz == "small" {
				args = small
			}
			b.Run(sz+"/"+mode, func(b *testing.B) {
				l := NewLog()
				if armed {
					l.SetScrubber(benchArmed)
				}
				p := benchMsg(args)
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if _, err := l.Append(AssistantMessage, p); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
		b.Run("toolresult20k/"+mode, func(b *testing.B) {
			l := NewLog()
			if armed {
				l.SetScrubber(benchArmed)
			}
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := l.Append(ToolResult, ToolResultData{ToolCallID: "c1", Text: text}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
