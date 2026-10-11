package llm

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type unknownBlock struct{}

func (unknownBlock) Kind() BlockKind { return "mystery" }

// The structural copy has to be indistinguishable from the codec round trip it
// replaced — down to nil versus empty — because the session log's projection is
// what the model is sent, and the prefix it builds must not move.
func TestCloneMessagesMatchesTheCodecRoundTrip(t *testing.T) {
	cases := map[string][]Message{
		"empty":       {},
		"nil content": {{Role: RoleUser}},
		"text and reasoning": {
			{Role: RoleUser, Content: []ContentBlock{TextBlock{Text: "hi <b>&</b>"}}},
			{Role: RoleAssistant, Content: []ContentBlock{
				ReasoningBlock{Text: "think", Signature: "sig", Redacted: true},
				TextBlock{Text: "answer   é"},
			}, Provenance: &AssistantProvenance{Provider: "p", Model: "m", ReplayState: json.RawMessage(`{ "a" : [1, 2], "h": "<x>" }`)}},
		},
		"tool round": {
			{Role: RoleAssistant, Content: []ContentBlock{
				ToolCallBlock{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path": "a<b>.go"}`)},
				ToolCallBlock{ID: "c2", Name: "noargs"},
				ToolCallBlock{ID: "c3", Name: "emptyargs", Arguments: json.RawMessage{}},
			}, Provenance: &AssistantProvenance{Provider: "p", Model: "m"}},
			{Role: RoleUser, Content: []ContentBlock{
				ToolResultBlock{ToolCallID: "c1", Content: []ContentBlock{
					TextBlock{Text: strings.Repeat("line\n", 50)},
					ImageBlock{MediaType: "image/png", Data: []byte{0, 1, 2, 255}},
					ImageBlock{MediaType: "image/png", Data: []byte{}},
				}},
				ToolResultBlock{ToolCallID: "c2", IsError: true},
				ToolResultBlock{ToolCallID: "c3", Content: []ContentBlock{}},
			}},
		},
		"invalid utf8": {{Role: RoleUser, Content: []ContentBlock{TextBlock{Text: "bad \xff\xfe bytes"}}}},
	}
	for name, msgs := range cases {
		t.Run(name, func(t *testing.T) {
			want, wantErr := cloneViaCodec(msgs)
			got, gotErr := CloneMessages(msgs)
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("error mismatch: codec %v, clone %v", wantErr, gotErr)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("clone differs from codec round trip:\n got %#v\nwant %#v", got, want)
			}
		})
	}
}

func TestCloneMessagesReportsTheCodecErrorForAnUnknownBlock(t *testing.T) {
	msgs := []Message{{Role: RoleUser, Content: []ContentBlock{
		ToolResultBlock{ToolCallID: "c", Content: []ContentBlock{unknownBlock{}}},
	}}}
	_, wantErr := cloneViaCodec(msgs)
	_, gotErr := CloneMessages(msgs)
	if wantErr == nil || gotErr == nil || wantErr.Error() != gotErr.Error() {
		t.Fatalf("errors differ: codec %v, clone %v", wantErr, gotErr)
	}
}

func TestCloneMessagesOwnsItsBytes(t *testing.T) {
	data := []byte{1, 2, 3}
	args := json.RawMessage(`{"a":1}`)
	msgs := []Message{{Role: RoleUser, Content: []ContentBlock{
		ImageBlock{MediaType: "image/png", Data: data},
		ToolCallBlock{ID: "c", Name: "n", Arguments: args},
	}}}
	got, err := CloneMessages(msgs)
	if err != nil {
		t.Fatal(err)
	}
	data[0], args[2] = 9, 'z'
	img, okImg := got[0].Content[0].(ImageBlock)
	call, okCall := got[0].Content[1].(ToolCallBlock)
	if !okImg || !okCall {
		t.Fatalf("clone changed the block types to %T and %T", got[0].Content[0], got[0].Content[1])
	}
	if img.Data[0] != 1 || string(call.Arguments) != `{"a":1}` {
		t.Fatal("clone shares memory with its source")
	}
}

func BenchmarkCloneMessages(b *testing.B) {
	var msgs []Message
	for i := 0; i < 150; i++ {
		msgs = append(msgs,
			Message{Role: RoleAssistant, Content: []ContentBlock{
				TextBlock{Text: "working on it"},
				ToolCallBlock{ID: "c", Name: "read", Arguments: json.RawMessage(`{"path":"a.go"}`)},
			}, Provenance: &AssistantProvenance{Provider: "p", Model: "m"}},
			Message{Role: RoleUser, Content: []ContentBlock{ToolResultBlock{ToolCallID: "c", Content: []ContentBlock{
				TextBlock{Text: strings.Repeat("some file content\n", 100)},
			}}}},
		)
	}
	b.Run("structural", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := CloneMessages(msgs); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("codec", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := cloneViaCodec(msgs); err != nil {
				b.Fatal(err)
			}
		}
	})
}
