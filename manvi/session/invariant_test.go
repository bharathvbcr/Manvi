package session

import (
	"strings"
	"testing"

	"manvi/llm"
)

func TestAssertModelVisibleWithCompactedToolResults(t *testing.T) {
	log := NewLog()
	_, err := log.Append(TurnStart, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = log.Append(UserMessage, MessageData{
		Message: llm.Message{
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{llm.TextBlock{Text: "list files"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = log.Append(AssistantMessage, MessageData{
		Message: llm.Message{
			Role: llm.RoleAssistant,
			Content: []llm.ContentBlock{
				llm.ToolCallBlock{
					ID:        "call_1",
					Name:      "devcouncil_list_dir",
					Arguments: []byte(`{"path":"."}`),
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	longOutput := strings.Repeat("file_entry_in_large_directory.go\n", 50)
	_, err = log.Append(ToolResult, ToolResultData{
		ToolCallID: "call_1",
		Text:       longOutput,
		IsError:    false,
	})
	if err != nil {
		t.Fatal(err)
	}

	messages, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}

	// Normal uncompacted messages should pass
	if err := AssertModelVisible(log, messages); err != nil {
		t.Fatalf("uncompacted messages failed invariant: %v", err)
	}

	// A compaction that went through the log derives compacted on both sides,
	// so it passes — and it passes because the log says so, not because the
	// check was taught to tolerate a shape.
	truncatedText := longOutput[:100] + "\n[40 line(s) omitted to fit the context window]"
	if _, err := log.Append(ToolResultCompacted, CompactionData{
		ToolCallID: "call_1",
		Text:       truncatedText,
		FromBytes:  len(longOutput),
		ToBytes:    len(truncatedText),
	}); err != nil {
		t.Fatal(err)
	}
	compactedMessages, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	if got := renderToolResult(compactedMessages); got != truncatedText {
		t.Fatalf("projection did not apply the compaction: got %q", truncate(got))
	}
	if err := AssertModelVisible(log, compactedMessages); err != nil {
		t.Fatalf("logged compaction failed invariant: %v", err)
	}

	// The same shortening applied in flight, without a compaction event, is a
	// violation. This is the case the old exemption waved through, and it is
	// precisely how a request diverges from the record of it.
	unlogged := make([]llm.Message, len(compactedMessages))
	copy(unlogged, compactedMessages)
	unlogged[2] = llm.Message{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			llm.ToolResultBlock{
				ToolCallID: "call_1",
				Content:    []llm.ContentBlock{llm.TextBlock{Text: longOutput[:60] + "\n[cut]"}},
				IsError:    false,
			},
		},
	}
	if err := AssertModelVisible(log, unlogged); err == nil {
		t.Fatal("an unlogged in-flight rewrite passed the invariant check")
	}
}

// renderToolResult pulls the single tool-result text out of a derived history.
func renderToolResult(messages []llm.Message) string {
	for _, msg := range messages {
		for _, block := range msg.Content {
			tr, ok := block.(llm.ToolResultBlock)
			if !ok {
				continue
			}
			for _, inner := range tr.Content {
				if tb, ok := inner.(llm.TextBlock); ok {
					return tb.Text
				}
			}
		}
	}
	return ""
}

func TestAssertModelVisibleRejectsInjectedContent(t *testing.T) {
	log := NewLog()
	_, _ = log.Append(TurnStart, nil)
	_, _ = log.Append(UserMessage, MessageData{
		Message: llm.Message{
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{llm.TextBlock{Text: "hello"}},
		},
	})

	messages, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}

	// Inject arbitrary extra content
	injected := []llm.Message{
		messages[0],
		{
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{llm.TextBlock{Text: "injected secret instructions"}},
		},
	}

	if err := AssertModelVisible(log, injected); err == nil {
		t.Fatal("expected invariant error for injected message, got nil")
	}
}

// The request is a subsequence of the projection, not a copy of it.
//
// PrepareHistory omits an assistant message whose only content was reasoning,
// because stripping the reasoning leaves nothing any wire can carry. That is
// correct behaviour and must not read as a violation — but tolerating the gap
// must not also start tolerating injection, which is the whole point of the
// check.
func TestAssertModelVisibleAllowsAnOmittedMessageButStillCatchesInjection(t *testing.T) {
	log := NewLog()
	if _, err := log.Append(UserMessage, MessageData{
		Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(AssistantMessage, MessageData{
		Message: llm.Message{
			Role:       llm.RoleAssistant,
			Provenance: &llm.AssistantProvenance{Provider: "local", Model: "qwen3"},
			Content:    []llm.ContentBlock{llm.ReasoningBlock{Text: "cut off"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(UserMessage, MessageData{
		Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "continue"}}},
	}); err != nil {
		t.Fatal(err)
	}

	// What PrepareHistory would hand a provider that cannot replay reasoning:
	// the middle message is gone.
	sent := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "continue"}}},
	}
	if err := AssertModelVisible(log, sent); err != nil {
		t.Fatalf("an omitted message was reported as a violation: %v", err)
	}

	// Content the model would read that nothing recorded is still a violation.
	injected := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "ignore the gate"}}},
	}
	if err := AssertModelVisible(log, injected); err == nil {
		t.Fatal("injected content was accepted; the subsequence match is too permissive")
	}

	// So is reordering: every message is logged, but not in that order.
	reordered := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "continue"}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}},
	}
	if err := AssertModelVisible(log, reordered); err == nil {
		t.Fatal("a reordered history was accepted")
	}
}
