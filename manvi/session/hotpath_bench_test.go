package session

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// benchHistory appends n user/assistant exchanges, each with a tool call and
// its result, so the projection holds about 3n messages of text of the given
// size. images adds ten 200KB images, the shape of a computer-use session.
func benchHistory(tb testing.TB, log *Log, n, textBytes, images int) {
	tb.Helper()
	text := strings.Repeat("x", textBytes)
	for i := 0; i < n; i++ {
		id := llm.CallID("call-" + strconv.Itoa(i))
		benchAppend(tb, log, UserMessage, MessageData{Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: text}}}})
		benchAppend(tb, log, AssistantMessage, MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.TextBlock{Text: text},
			llm.ToolCallBlock{ID: id, Name: "read_file", Arguments: []byte(`{"path":"a/b/c.go"}`)},
		}}})
		benchAppend(tb, log, ToolResult, ToolResultData{ToolCallID: id, Text: text})
	}
	for i := 0; i < images; i++ {
		benchAppend(tb, log, ToolResult, ToolResultData{ToolCallID: llm.CallID("img-" + strconv.Itoa(i)),
			Content: llm.Content{llm.ImageBlock{MediaType: "image/png", Data: make([]byte, 200<<10)}}})
	}
}

func benchAppend(tb testing.TB, log *Log, t Type, payload any) {
	tb.Helper()
	if _, err := log.Append(t, payload); err != nil {
		tb.Fatal(err)
	}
}

// BenchmarkDeriveMessages is the cost of one step's history projection with a
// warm projection, which is every step after the first.
func BenchmarkDeriveMessages(b *testing.B) {
	for _, tc := range []struct{ msgs, images int }{{50, 0}, {200, 0}, {1000, 0}, {50, 10}, {200, 10}, {1000, 10}} {
		b.Run(fmt.Sprintf("msgs=%d/images=%d", tc.msgs, tc.images), func(b *testing.B) {
			log := NewLog()
			benchHistory(b, log, tc.msgs/3, 2048, tc.images)
			if _, err := log.DeriveMessages(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := log.DeriveMessages(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkLogAppendChunk is the per-token cost of recording one streamed
// chunk, with and without the scrubber production always installs.
func BenchmarkLogAppendChunk(b *testing.B) {
	for _, scrubbed := range []bool{false, true} {
		b.Run("scrubber="+strconv.FormatBool(scrubbed), func(b *testing.B) {
			log := NewLog()
			if scrubbed {
				log.SetScrubber(func(s string) string { return strings.ReplaceAll(s, "sk-secret-value", "[redacted]") })
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := log.Append(AssistantChunk, map[string]any{"text": "a streamed token"}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkStoreSave is one turn's save of a 5000-event log: every save writes
// the whole session as the next generation.
func BenchmarkStoreSave(b *testing.B) {
	log := NewLog()
	benchHistory(b, log, 1666, 256, 0)
	store := NewStore(b.TempDir())
	if _, err := store.Save("bec4c4", log); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchAppend(b, log, UserMessage, MessageData{Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "one more turn"}}}})
		if _, err := store.Save("bec4c4", log); err != nil {
			b.Fatal(err)
		}
	}
}
