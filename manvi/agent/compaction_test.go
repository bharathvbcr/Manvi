package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"manvi/core/bus"
	"manvi/llm"
	"manvi/session"
	"manvi/tools"
)

func toolResultMsg(id llm.CallID, text string) llm.Message {
	return llm.Message{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{llm.ToolResultBlock{
			ToolCallID: id,
			Content:    []llm.ContentBlock{llm.TextBlock{Text: text}},
		}},
	}
}

func bigOutput(lines int, tag string) string {
	var b strings.Builder
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&b, "%s line %03d: some reasonably long tool output content here\n", tag, i)
	}
	return b.String()
}

// history builds n tool-result messages plus a protected tail.
func history(n int, linesEach int) []llm.Message {
	var msgs []llm.Message
	msgs = append(msgs, llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{llm.TextBlock{Text: "the original task"}},
	})
	for i := 0; i < n; i++ {
		msgs = append(msgs, toolResultMsg(llm.CallID(fmt.Sprintf("call_%d", i)), bigOutput(linesEach, fmt.Sprintf("t%d", i))))
	}
	return msgs
}

func testBudget(window int) Budget {
	return Budget{ContextWindow: window, ReservedOutput: window / 8, Overhead: 512}
}

func TestCompactionKeepsHeadAndTailAndSaysWhatItDropped(t *testing.T) {
	original := bigOutput(60, "x")
	short := CompactToolResultText(original, 400)

	if len(short) >= len(original) {
		t.Fatalf("compaction did not shorten: %d >= %d", len(short), len(original))
	}
	if !strings.Contains(short, "x line 000:") {
		t.Error("the head of the result was not preserved")
	}
	if !strings.Contains(short, "x line 059:") {
		t.Error("the tail of the result was not preserved")
	}
	if !strings.Contains(short, "omitted to fit the context window") {
		t.Error("the elision was silent; a shortened result must say it was shortened")
	}
}

func TestCompactionCutsOnRuneBoundaries(t *testing.T) {
	// Multi-byte content with too few lines for the head/tail path, forcing the
	// rune-wise branch. A byte slice here would produce invalid UTF-8.
	original := strings.Repeat("日本語のテキスト", 200)
	short := CompactToolResultText(original, 100)
	if !strings.HasPrefix(short, "日本語") {
		t.Fatalf("unexpected prefix: %q", short[:20])
	}
	for i, r := range short {
		if r == '�' {
			t.Fatalf("compaction produced an invalid rune at byte %d", i)
		}
	}
}

func TestCompactionIsStickyAcrossSteps(t *testing.T) {
	msgs := history(24, 40)
	budget := testBudget(8192)
	already := map[llm.CallID]struct{}{}

	first := PlanCompaction(msgs, "sys", nil, budget, already)
	if first.Empty() {
		t.Fatal("expected the first plan to compact something")
	}
	for _, step := range first.Steps {
		already[step.ToolCallID] = struct{}{}
	}

	// Apply the first plan to the history, as the log projection would.
	applied := applyPlan(msgs, first)

	second := PlanCompaction(applied, "sys", nil, budget, already)
	for _, step := range second.Steps {
		if _, seen := already[step.ToolCallID]; seen {
			t.Fatalf("tool result %s was compacted twice; the prompt prefix would move again", step.ToolCallID)
		}
	}
}

func TestCompactionLeavesTheRecentWorkingSetAlone(t *testing.T) {
	msgs := history(20, 40)
	plan := PlanCompaction(msgs, "sys", nil, testBudget(8192), map[llm.CallID]struct{}{})

	protectedFrom := len(msgs) - ProtectedTail
	protected := map[llm.CallID]bool{}
	for i := protectedFrom; i < len(msgs); i++ {
		for _, b := range msgs[i].Content {
			if tr, ok := b.(llm.ToolResultBlock); ok {
				protected[tr.ToolCallID] = true
			}
		}
	}
	for _, step := range plan.Steps {
		if protected[step.ToolCallID] {
			t.Fatalf("%s is inside the protected tail and must not be compacted", step.ToolCallID)
		}
	}
}

func TestCompactionAimsPastTheThresholdSoItFiresRarely(t *testing.T) {
	msgs := history(30, 40)
	budget := testBudget(8192)
	plan := PlanCompaction(msgs, "sys", nil, budget, map[llm.CallID]struct{}{})

	if plan.Empty() {
		t.Fatal("expected compaction")
	}
	if plan.After > budget.Threshold() {
		t.Fatalf("compaction landed over threshold: %d > %d", plan.After, budget.Threshold())
	}
	// Landing exactly on the threshold means the next result crosses it again
	// and the prefix moves again. Headroom is the whole point.
	if plan.After > budget.Target() {
		t.Errorf("compaction landed at %d, above the %d target: it will re-trigger almost immediately",
			plan.After, budget.Target())
	}
}

func TestCompactionReportsWhenItCannotFit(t *testing.T) {
	// A protected tail alone that dwarfs the window: nothing eligible can save
	// enough, and the shortfall must be stated rather than swallowed.
	var msgs []llm.Message
	for i := 0; i < ProtectedTail; i++ {
		msgs = append(msgs, toolResultMsg(llm.CallID(fmt.Sprintf("keep_%d", i)), bigOutput(400, "k")))
	}
	msgs = append([]llm.Message{toolResultMsg("old", bigOutput(10, "o"))}, msgs...)

	plan := PlanCompaction(msgs, "sys", nil, testBudget(8192), map[llm.CallID]struct{}{})
	if !plan.Insufficient {
		t.Fatalf("expected the plan to report it could not fit; after=%d threshold=%d",
			plan.After, testBudget(8192).Threshold())
	}
}

func TestToolSchemasAreCountedInTheBudget(t *testing.T) {
	schemas := []llm.ToolSchema{
		{Name: "devcouncil_read_file", Description: "Read a file from the repository.",
			InputSchema: []byte(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)},
		{Name: "devcouncil_grep", Description: "Search for pattern matches across files.",
			InputSchema: []byte(`{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"]}`)},
	}
	msgs := history(2, 5)

	without := CountRequestTokens("sys", nil, msgs)
	with := CountRequestTokens("sys", schemas, msgs)
	if with <= without {
		t.Fatal("tool schemas contributed nothing to the budget; they are sent on every request")
	}
	if got := CountToolTokens(schemas); got <= 0 {
		t.Fatalf("CountToolTokens = %d", got)
	}
}

func TestBudgetNeverReservesTheWholeWindow(t *testing.T) {
	// A capability that caps output at or above its own context window would
	// otherwise leave zero room for history.
	l := &Loop{cfg: Config{
		Provider: fixedCapProvider{window: 4096, maxOut: 8192},
		Model:    "m",
	}}
	b := l.Budget()
	if b.ReservedOutput >= b.ContextWindow {
		t.Fatalf("reserved %d of a %d window", b.ReservedOutput, b.ContextWindow)
	}
	if b.Threshold() <= 0 {
		t.Fatalf("threshold %d", b.Threshold())
	}
}

func TestCompactionRoundTripsThroughTheLog(t *testing.T) {
	log := session.NewLog()
	if _, err := log.Append(session.TurnStart, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.UserMessage, session.MessageData{
		Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}},
	}); err != nil {
		t.Fatal(err)
	}
	original := bigOutput(80, "z")
	if _, err := log.Append(session.ToolResult, session.ToolResultData{
		ToolCallID: "c1", Text: original,
	}); err != nil {
		t.Fatal(err)
	}

	plan := CompactionPlan{Steps: []CompactionStep{{
		ToolCallID: "c1", Text: CompactToolResultText(original, 300),
		FromBytes: len(original), ToBytes: len(CompactToolResultText(original, 300)),
	}}}
	if err := plan.Apply(log); err != nil {
		t.Fatal(err)
	}

	derived, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	got := findToolResultText(derived, "c1")
	if got == original {
		t.Fatal("the projection did not apply the logged compaction")
	}
	if !strings.Contains(got, "omitted to fit the context window") {
		t.Fatalf("unexpected projected text: %q", got)
	}

	// Two derivations of the same log must be byte-identical, or the prefix
	// cache has nothing stable to key on.
	again, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	if findToolResultText(again, "c1") != got {
		t.Fatal("two derivations of one log disagreed")
	}

	// And the original is still in the log for an evidence report.
	if !logContainsText(log, original) {
		t.Fatal("compaction destroyed the original tool result in the log")
	}
}

func applyPlan(messages []llm.Message, plan CompactionPlan) []llm.Message {
	replace := map[llm.CallID]string{}
	for _, s := range plan.Steps {
		replace[s.ToolCallID] = s.Text
	}
	out := make([]llm.Message, len(messages))
	for i, msg := range messages {
		blocks := make([]llm.ContentBlock, 0, len(msg.Content))
		for _, b := range msg.Content {
			if tr, ok := b.(llm.ToolResultBlock); ok {
				if text, hit := replace[tr.ToolCallID]; hit {
					blocks = append(blocks, llm.ToolResultBlock{
						ToolCallID: tr.ToolCallID,
						Content:    []llm.ContentBlock{llm.TextBlock{Text: text}},
						IsError:    tr.IsError,
					})
					continue
				}
			}
			blocks = append(blocks, b)
		}
		out[i] = llm.Message{Role: msg.Role, Content: blocks, Provenance: msg.Provenance}
	}
	return out
}

func findToolResultText(messages []llm.Message, id llm.CallID) string {
	for _, msg := range messages {
		for _, b := range msg.Content {
			tr, ok := b.(llm.ToolResultBlock)
			if !ok || tr.ToolCallID != id {
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

// logContainsText looks for the original tool-result text still present in the
// log. Event data is JSON, so the comparison decodes rather than substring
// matching escaped bytes against raw ones.
func logContainsText(log *session.Log, want string) bool {
	for _, e := range log.Events() {
		if e.Type != session.ToolResult {
			continue
		}
		var data session.ToolResultData
		if json.Unmarshal(e.Data, &data) != nil {
			continue
		}
		if data.Text == want {
			return true
		}
	}
	return false
}

type fixedCapProvider struct {
	window int
	maxOut int
}

func (f fixedCapProvider) Name() string { return "fixed" }
func (f fixedCapProvider) Capability(string) (llm.Capability, bool) {
	return llm.Capability{ContextWindow: f.window, MaxOutputTokens: f.maxOut}, true
}
func (f fixedCapProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	return nil, fmt.Errorf("not used")
}

// Compaction must survive the turn boundary, not merely the step boundary.
//
// The face builds a fresh agent.Loop for every user submission and `run
// --resume` builds one against a log restored from disk, while the session log
// outlives both. When the ledger of "already compacted" lived on the Loop it
// was empty at the start of every turn, so results compacted in turn one were
// handed to the planner as untouched text and compacted a second time. That
// changes their bytes, which moves the divergence point to the *first* tool
// result and throws away the whole KV prefix — the precise cost this design
// exists to avoid, paid again at every turn boundary.
//
// TestCompactionIsStickyAcrossSteps cannot catch this: it threads one `already`
// map by hand, which is the very thing that did not survive a new Loop.
func TestCompactionSurvivesTheTurnBoundary(t *testing.T) {
	log := session.NewLog()
	if _, err := log.Append(session.TurnStart, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.UserMessage, session.MessageData{
		Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}},
	}); err != nil {
		t.Fatal(err)
	}
	// A turn's worth of work: the model calls a tool a few times and each call
	// comes back with output. Turns *accumulate* — that is what keeps the
	// history over threshold, and it is why a second turn plans at all.
	addWork := func(turnNo, n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			id := llm.CallID(fmt.Sprintf("c%d_%02d", turnNo, i))
			if _, err := log.Append(session.AssistantMessage, session.MessageData{
				Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
					llm.ToolCallBlock{ID: id, Name: "read", Arguments: json.RawMessage(`{}`)},
				}},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := log.Append(session.ToolResult, session.ToolResultData{
				ToolCallID: id, Text: bigOutput(200, fmt.Sprintf("t%d_%02d", turnNo, i)),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	addWork(1, 40)

	// Each turn gets its own Loop against the one log, exactly as the face does.
	turn := func() []llm.Message {
		t.Helper()
		loop, err := NewLoop(Config{
			Provider: fixedCapProvider{window: 32768, maxOut: 4096},
			Model:    "m",
			MaxSteps: 8,
		}, bus.New(), log, tools.NewRegistry(bus.New()))
		if err != nil {
			t.Fatal(err)
		}
		messages, err := log.DeriveMessages()
		if err != nil {
			t.Fatal(err)
		}
		out, err := loop.compact(messages)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	// compactionCounts reports how many times each result has been compacted so
	// far, read from the log because the log is the ledger.
	compactionCounts := func() map[llm.CallID]int {
		t.Helper()
		counts := map[llm.CallID]int{}
		for _, event := range log.Events() {
			if event.Type != session.ToolResultCompacted {
				continue
			}
			var data session.CompactionData
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatal(err)
			}
			counts[data.ToolCallID]++
		}
		return counts
	}

	first := turn()
	afterFirst := compactionCounts()
	if len(afterFirst) == 0 {
		t.Fatal("the first turn compacted nothing; the fixture no longer exercises the boundary")
	}

	// The next turn brings enough work that the planner must pick a TIGHTER
	// floor than turn one did. That is the condition the bug needs, and it is
	// the ordinary one: a session accumulates. At the same floor the
	// "never grow" guard in planCompaction happens to skip an already-compacted
	// result, so a fixture that keeps the floor steady passes either way and
	// proves nothing.
	addWork(2, 60)
	second := turn()

	final := compactionCounts()
	// No tool result may carry two compaction events: the second pass would
	// re-elide already-elided text and produce different bytes.
	for id, n := range final {
		if n > 1 {
			t.Errorf("tool result %s was compacted %d times across two turns", id, n)
		}
	}

	// The consequence that actually costs money. Only results the FIRST turn
	// compacted are held to this: one compacted for the first time during turn
	// two is expected to differ from its uncompacted turn-one form.
	for id := range afterFirst {
		before, after := findToolResultText(first, id), findToolResultText(second, id)
		if before != after {
			t.Errorf("tool result %s changed across the turn boundary; the prefix cache is invalidated from here on\n before: %.80q\n  after: %.80q",
				id, before, after)
		}
	}
}

// Compaction must respect the floor it is given and must be idempotent.
//
// The elision notice used to be appended on top of a budget already split
// between head and tail, so the result came back over the floor the planner had
// budgeted against, and compacting an already-compacted result produced yet
// another distinct string. Both are prefix-cache faults: the planner's
// arithmetic assumes the floor holds, and any changed byte invalidates the
// server's cache from that point on.
func TestCompactionRespectsItsFloorAndIsIdempotent(t *testing.T) {
	for _, lines := range []int{8, 30, 200, 2000} {
		text := bigOutput(lines, "q")
		for _, floor := range compactFloors {
			out := CompactToolResultText(text, floor)
			if len(out) > floor {
				t.Errorf("%d lines at floor %d: produced %d bytes, over the floor",
					lines, floor, len(out))
			}
			if len(out) > len(text) {
				t.Errorf("%d lines at floor %d: compaction grew the text", lines, floor)
			}
			if again := CompactToolResultText(out, floor); again != out {
				t.Errorf("%d lines at floor %d: not idempotent (%d bytes -> %d bytes on a second pass)",
					lines, floor, len(out), len(again))
			}
		}
	}
}
