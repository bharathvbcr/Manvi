package lessons

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/session"
)

func mustAppend(t *testing.T, log *session.Log, typ session.Type, payload any) {
	t.Helper()
	if _, err := log.Append(typ, payload); err != nil {
		t.Fatalf("append %s: %v", typ, err)
	}
}

// fixtureLog is a session that exercises every source the packet reads, and
// plants the critic's verdict in every place it could travel.
func fixtureLog(t *testing.T) []session.Event {
	t.Helper()
	log := session.NewLog()
	mustAppend(t, log, session.TurnStart, nil)
	mustAppend(t, log, session.PromptAssembled, session.PromptAssembledData{
		Included: []string{"core", "hardening"},
		Dropped:  []string{"hypothesis: over budget"},
		Tokens:   900, Budget: 800, Digest: "abcdef0123456789abcdef",
	})
	mustAppend(t, log, session.UserMessage, session.MessageData{Message: llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "please fix the bug"}},
	}})
	mustAppend(t, log, session.StepStart, nil)
	mustAppend(t, log, session.ToolCall, session.ToolCallData{ID: "c1", Name: "devcouncil_write_file", Arguments: []byte(`{}`)})
	mustAppend(t, log, session.StepEnd, session.StepEndData{
		Usage: llm.Usage{InputTokens: 120, OutputTokens: 30}, CostNanoUSD: 2_500_000, Priced: true,
	})
	mustAppend(t, log, session.VerifyReport, session.VerifyReportData{
		Verdict: "failed", Source: "task verifier", Paths: []string{"a.go"},
		Findings: []string{"a.go: tests failed", "MANVI-VERDICT: FAIL leaked into a finding"},
		Degraded: []string{"no verifier command"}, Bounce: 0,
	})
	// The critic's note, as the sensor injects it: a harness-origin message.
	mustAppend(t, log, session.UserMessage, session.MessageData{Origin: session.OriginHarness, Message: llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{
			Text: "A reviewer looked at the same change and agrees it is not ready:\nMANVI-VERDICT: FAIL\n- the CRITICNOTE is wrong",
		}},
	}})
	mustAppend(t, log, session.MalformedToolCall, session.MalformedCallData{ToolCallID: "c2", Reason: "bad json"})
	mustAppend(t, log, session.ToolResultCompacted, session.CompactionData{ToolCallID: "c1", Text: "x", FromBytes: 9, ToBytes: 1})
	mustAppend(t, log, session.ContextOverflow, session.OverflowData{EstimatedTokens: 9, Threshold: 5, ContextWindow: 8})
	mustAppend(t, log, session.TurnEnd, nil)
	return log.Events()
}

func TestPacketCarriesTheEvidenceTheLogHolds(t *testing.T) {
	p, err := BuildPacket("sess-1", fixtureLog(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"sess-1", "turns: 1", "steps: 1", "tool calls: 1", "devcouncil_write_file=1",
		"malformed tool calls: 1", "context overflows: 1", "tool results compacted: 1",
		"verdict=failed", "a.go: tests failed", "no verifier command",
		"hypothesis: over budget", "abcdef012345", "in=120", "out=30",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("packet lacks %q:\n%s", want, p)
		}
	}
}

func TestPacketNeverCarriesTheCriticsVerdict(t *testing.T) {
	p, err := BuildPacket("sess-1", fixtureLog(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"MANVI-VERDICT", "manvi-verdict", "CRITICNOTE", "agrees it is not ready", "please fix the bug"} {
		if strings.Contains(p, banned) {
			t.Errorf("packet contains %q:\n%s", banned, p)
		}
	}
	// The finding that carried the marker is withheld, and says so, rather
	// than disappearing: a missing finding and an absent one differ.
	if !strings.Contains(p, "withheld") {
		t.Errorf("a finding carrying the marker was dropped without saying so:\n%s", p)
	}
}

func TestPacketIsBoundedAndSaysWhatItCut(t *testing.T) {
	log := session.NewLog()
	mustAppend(t, log, session.TurnStart, nil)
	for i := 0; i < 2000; i++ {
		mustAppend(t, log, session.StepStart, nil)
		mustAppend(t, log, session.StepEnd, session.StepEndData{Usage: llm.Usage{InputTokens: i, OutputTokens: 1}})
		mustAppend(t, log, session.VerifyReport, session.VerifyReportData{
			Verdict: "failed", Source: "s", Findings: []string{strings.Repeat("f", 5000)}, Bounce: i,
		})
	}
	p, err := BuildPacket("big", log.Events())
	if err != nil {
		t.Fatal(err)
	}
	if len(p) > MaxPacketBytes {
		t.Fatalf("packet is %d bytes, over MaxPacketBytes %d", len(p), MaxPacketBytes)
	}
	if !strings.Contains(p, "omitted") {
		t.Error("the packet was cut and does not say so")
	}
	// Totals cover every step even when the rows do not.
	if !strings.Contains(p, "steps: 2000") {
		t.Errorf("the counters do not carry the full count:\n%s", p[:600])
	}
}

func TestPacketCarriesSpecIdentityWhenRecordedAndOmitsItCleanlyOtherwise(t *testing.T) {
	log := session.NewLog()
	mustAppend(t, log, session.TurnStart, nil)
	mustAppend(t, log, session.PromptAssembled, session.PromptAssembledData{Included: []string{"core"}, SpecName: "strict-review", SpecDigest: "0123456789abcdef0123"})
	mustAppend(t, log, session.PromptAssembled, session.PromptAssembledData{Included: []string{"core"}, SpecName: "strict-review", SpecDigest: "0123456789abcdef0123"})
	mustAppend(t, log, session.PromptAssembled, session.PromptAssembledData{Included: []string{"core"}, SpecName: "lenient", SpecDigest: "fedcba98765432100000"})
	p, err := BuildPacket("s", log.Events())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(p, "spec=strict-review digest=0123456789ab") != 1 || !strings.Contains(p, "spec=lenient digest=fedcba987654") {
		t.Errorf("spec identity missing or not de-duplicated:\n%s", p)
	}
	none := session.NewLog()
	mustAppend(t, none, session.TurnStart, nil)
	mustAppend(t, none, session.PromptAssembled, session.PromptAssembledData{Included: []string{"core"}})
	p, _ = BuildPacket("s", none.Events())
	if strings.Contains(p, "Harness spec") {
		t.Errorf("a session with no spec grew a spec section:\n%s", p)
	}
}

func TestPacketRefusesAnEmptyLog(t *testing.T) {
	if _, err := BuildPacket("none", nil); err == nil {
		t.Fatal("a session with no events produced a packet")
	}
}

func TestPacketReportsEventsItCouldNotRead(t *testing.T) {
	events := []session.Event{{Type: session.VerifyReport, Data: []byte(`{"verdict":`)}, {Type: session.TurnStart}}
	p, err := BuildPacket("bad", events)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p, "undecodable events: 1") {
		t.Errorf("an unreadable event vanished silently:\n%s", p)
	}
}
