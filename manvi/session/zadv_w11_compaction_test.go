package session

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// Adversarial tests for lane w11-compaction (card ft-f7993787), session half:
// Log.StripStaleReasoning. Fixture helpers (staleFixture, staleRound, compact,
// signaturesOf, strip) come from stale_test.go.
//
// Layout of staleFixture(n): message 0 is the user turn; round i is the
// assistant message at 1+2i and its result at 2+2i.

func advSigned(msgs []llm.Message) map[int]string { return signaturesOf(msgs) }

func advWant(t *testing.T, got map[int]string, want map[int]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("surviving signed blocks = %v, want %v", got, want)
	}
	for i, sig := range want {
		if got[i] != sig {
			t.Fatalf("surviving signed blocks = %v, want %v", got, want)
		}
	}
}

// An edit of the very first result: everything signed before the result stays,
// everything between it and the edit goes.
func TestAdvW11StripEditOfFirstResult(t *testing.T) {
	log := NewLog()
	staleFixture(t, log, 3)
	compact(t, log, "c0", true)
	out, n := strip(t, log)
	if n != 2 {
		t.Fatalf("stripped %d, want 2 (rounds 1 and 2)", n)
	}
	advWant(t, advSigned(out), map[int]string{1: "sig-0"})
}

// Two edits: the cut starts at the earliest edited position.
func TestAdvW11StripMultipleEdits(t *testing.T) {
	log := NewLog()
	staleFixture(t, log, 2) // rounds 0,1
	compact(t, log, "c0", true)
	staleRound(t, log, 2)
	compact(t, log, "c1", true)
	staleRound(t, log, 3)
	out, _ := strip(t, log)
	// Everything from the first edited position on goes, including round 3's
	// block (msg 7), which was recorded after both edits: keeping it would be a
	// removal from the middle of the chain of signed blocks.
	advWant(t, advSigned(out), map[int]string{1: "sig-0"})
}

// An edit recorded after the last thinking block still invalidates every block
// between the edited result and itself.
func TestAdvW11StripEditAfterLastThinking(t *testing.T) {
	log := NewLog()
	staleFixture(t, log, 4)
	compact(t, log, "c1", true)
	out, _ := strip(t, log)
	advWant(t, advSigned(out), map[int]string{1: "sig-0", 3: "sig-1"})
}

// redacted_thinking carries its payload in Signature and must be stripped like
// a signed thinking block.
func TestAdvW11StripRedactedThinking(t *testing.T) {
	log := NewLog()
	if _, err := log.Append(UserMessage, MessageData{Message: llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}}}); err != nil {
		t.Fatal(err)
	}
	staleRound(t, log, 0)
	if _, err := log.Append(AssistantMessage, MessageData{Message: llm.Message{
		Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ReasoningBlock{Redacted: true, Signature: "opaque-data"},
			llm.ToolCallBlock{ID: "c1", Name: "read", Arguments: json.RawMessage(`{}`)},
		}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(ToolResult, ToolResultData{ToolCallID: "c1", Text: "o"}); err != nil {
		t.Fatal(err)
	}
	compact(t, log, "c0", true)
	out, n := strip(t, log)
	if n != 1 {
		t.Fatalf("stripped %d, want the one redacted block", n)
	}
	for _, m := range out {
		for _, b := range m.Content {
			if r, ok := b.(llm.ReasoningBlock); ok && r.Redacted {
				t.Fatalf("redacted_thinking survived behind an edit: %+v", r)
			}
		}
	}
}

// Positions that do not line up with the log (a listener dropped a message)
// mean the answer is unknown, so every signed block goes, including ones that
// precede the edit.
func TestAdvW11StripMisalignedHistoryStripsEverything(t *testing.T) {
	log := NewLog()
	staleFixture(t, log, 3)
	compact(t, log, "c1", true)
	msgs, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	out, n, err := log.StripStaleReasoning(msgs[1:]) // lose the user turn
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 || len(advSigned(out)) != 0 {
		t.Fatalf("misaligned history: stripped %d, surviving %v; want all 3 gone", n, advSigned(out))
	}
	// And a longer history than the log (a listener appended a message).
	extra := append(append([]llm.Message{}, msgs...), llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "x"}}})
	out, _, err = log.StripStaleReasoning(extra)
	if err != nil {
		t.Fatal(err)
	}
	if len(advSigned(out)) != 0 {
		t.Fatalf("over-long history kept signed blocks %v", advSigned(out))
	}
}

// Stripping reasoning must leave tool_use/tool_result pairing intact and must
// not mutate the caller's messages or the log.
func TestAdvW11StripKeepsToolPairingAndDoesNotMutateInput(t *testing.T) {
	log := NewLog()
	staleFixture(t, log, 4)
	compact(t, log, "c0", true)
	msgs, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	before := signaturesOf(msgs)
	out, n, err := log.StripStaleReasoning(msgs)
	if err != nil || n == 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := signaturesOf(msgs); fmt.Sprint(got) != fmt.Sprint(before) {
		t.Fatalf("StripStaleReasoning mutated its input: %v -> %v", before, got)
	}
	for i, m := range out {
		if m.Role != llm.RoleAssistant {
			continue
		}
		for _, c := range m.ToolCalls() {
			if i+1 >= len(out) {
				t.Fatalf("tool call %s at the end with no result", c.ID)
			}
			found := false
			for _, b := range out[i+1].Content {
				if r, ok := b.(llm.ToolResultBlock); ok && r.ToolCallID == c.ID {
					found = true
				}
			}
			if !found {
				t.Fatalf("tool call %s in message %d lost its result in message %d", c.ID, i, i+1)
			}
		}
	}
}

// A compaction naming a result the log does not hold applies to nothing; the
// strip must not panic or strip on its account.
func TestAdvW11StripCompactionOfAbsentCallIsInert(t *testing.T) {
	log := NewLog()
	staleFixture(t, log, 2)
	msgs, _ := log.DeriveMessages()
	if _, err := log.Append(ToolResultCompacted, CompactionData{ToolCallID: "nope", Text: "short"}); err != nil {
		t.Skipf("Append refuses a compaction of an absent call (%v); nothing to attack", err)
	}
	out, n, err := log.StripStaleReasoning(msgs)
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v out=%v, want nothing stripped", n, err, advSigned(out))
	}
}

// The doc rule for which thinking removals keep later blocks valid:
// "removing a *leading* run of thinking blocks, oldest first"; removing a block
// "from anywhere other than the start of the run" invalidates later ones
// (claude-api shared/model-migration.md, Breaking change 3, lines 1630 and 1635).
// StripStaleReasoning strips the blocks between an edit and its recording and
// keeps the blocks recorded after the recording. For a log written by a build
// that compacted client-side, those later blocks were signed with the stale
// blocks still in their prefix, so removing the stale blocks while keeping the
// later ones is a non-leading removal: the retained blocks are then bound to a
// prefix that no longer exists. Invariant checked here: after the strip, no
// retained signed block follows a removed one.
func TestAdvW11StripRemovesOnlyALeadingRun(t *testing.T) {
	log := NewLog()
	staleFixture(t, log, 3)
	compact(t, log, "c1", true)
	staleRound(t, log, 3) // produced by the legacy build with the stale block still in its prefix
	msgs, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	before := signaturesOf(msgs)
	out, _, err := log.StripStaleReasoning(msgs)
	if err != nil {
		t.Fatal(err)
	}
	after := signaturesOf(out)
	removedAt := -1
	for i := 0; i < len(msgs); i++ {
		if _, was := before[i]; was {
			if _, still := after[i]; !still && removedAt < 0 {
				removedAt = i
			}
		}
	}
	if removedAt < 0 {
		t.Fatal("fixture stripped nothing")
	}
	for i := removedAt + 1; i < len(msgs); i++ {
		if _, still := after[i]; still {
			t.Fatalf("signed block at message %d is retained after the block at message %d was removed: non-leading removal", i, removedAt)
		}
	}
}
