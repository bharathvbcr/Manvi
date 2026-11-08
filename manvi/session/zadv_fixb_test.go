package session

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// Adversarial review of fix-b. Wire rules cited are from the claude-api skill's
// shared/ directory: prompt-caching.md "Mid-conversation system messages"
// (a system message follows a user message, is last or followed by an assistant
// turn, is never messages[0]) and preserved-thinking-migration.md ("Remove
// thinking only as a contiguous run").

// ---------------------------------------------------------------------------
// Exhaustive histories: placement on the sent form, and nothing but notices lost.

type advTok int

const (
	advU  advTok = iota // user message
	advA                // assistant text
	advH1               // assistant, whitespace text
	advH2               // assistant, unsigned thinking
	advH3               // assistant, no blocks
	advN                // notice
	advT                // assistant tool call, then its result
)

func (k advTok) String() string { return [...]string{"U", "A", "H1", "H2", "H3", "N", "T"}[k] }

func advHistory(toks []advTok) string {
	parts := make([]string, len(toks))
	for i, k := range toks {
		parts[i] = k.String()
	}
	return strings.Join(parts, " ")
}

func advHollow(k advTok) llm.Message {
	switch k {
	case advH1:
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: " \n"}}}
	case advH2:
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ReasoningBlock{Text: "cut off"}}}
	}
	return llm.Message{Role: llm.RoleAssistant}
}

func advAppendTok(t *testing.T, log *Log, i int, k advTok) {
	t.Helper()
	switch k {
	case advU:
		mustAppend(t, log, UserMessage, MessageData{Message: user(fmt.Sprintf("u%d", i))})
	case advA:
		mustAppend(t, log, AssistantMessage, MessageData{Message: assistant(fmt.Sprintf("a%d", i))})
	case advH1, advH2, advH3:
		mustAppend(t, log, AssistantMessage, MessageData{Message: advHollow(k)})
	case advN:
		appendNotice(t, log, NoticeData{Text: fmt.Sprintf("n%d", i), Prompt: fmt.Sprintf("p%d", i)})
	case advT:
		id := llm.CallID(fmt.Sprintf("c%d", i))
		mustAppend(t, log, AssistantMessage, MessageData{Message: llm.Message{Role: llm.RoleAssistant,
			Content: []llm.ContentBlock{llm.ToolCallBlock{ID: id, Name: "read", Arguments: json.RawMessage(`{}`)}}}})
		mustAppend(t, log, ToolResult, ToolResultData{ToolCallID: id, Text: "out"})
	}
}

// oldProjection is the projection of 6b0c915 for a token sequence: a user-role
// message removes the notices directly before it, and nothing else is ever
// removed.
func oldProjection(toks []advTok) []string {
	var out []string
	appendOld := func(role llm.Role, sig string) {
		if role == llm.RoleUser {
			for len(out) > 0 && strings.HasPrefix(out[len(out)-1], "system:") {
				out = out[:len(out)-1]
			}
		}
		out = append(out, string(role)+":"+sig)
	}
	for i, k := range toks {
		switch k {
		case advU:
			appendOld(llm.RoleUser, fmt.Sprintf("text=u%d", i))
		case advA:
			appendOld(llm.RoleAssistant, fmt.Sprintf("text=a%d", i))
		case advH1, advH2:
			appendOld(llm.RoleAssistant, advSig(advHollow(k)))
		case advH3:
			// A message with no blocks was never projected, before or after.
		case advN:
			appendOld(llm.RoleSystem, "notice")
		case advT:
			appendOld(llm.RoleAssistant, "toolcall")
			appendOld(llm.RoleUser, "toolresult")
		}
	}
	return out
}

func advSig(m llm.Message) string {
	var parts []string
	for _, b := range m.Content {
		switch b := b.(type) {
		case llm.TextBlock:
			parts = append(parts, "text="+b.Text)
		case llm.ReasoningBlock:
			parts = append(parts, "think")
		case llm.ToolCallBlock:
			parts = append(parts, "toolcall")
		case llm.ToolResultBlock:
			parts = append(parts, "toolresult")
		}
	}
	return strings.Join(parts, "+")
}

func advNewSigs(msgs []llm.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		sig := advSig(m)
		if m.Role == llm.RoleSystem {
			sig = "notice"
		}
		out[i] = string(m.Role) + ":" + sig
	}
	return out
}

func advWithoutSystem(sigs []string) []string {
	return slices.DeleteFunc(slices.Clone(sigs), func(s string) bool { return strings.HasPrefix(s, "system:") })
}

// advWirePlacement judges a projection in the form the encoder sends it (an
// assistant message with nothing to send omitted) against the documented
// placement of a system message.
func advWirePlacement(msgs []llm.Message) []string {
	sent := slices.DeleteFunc(slices.Clone(msgs), func(m llm.Message) bool {
		return m.Role == llm.RoleAssistant && !m.HasSendableContent()
	})
	var problems []string
	for i, m := range sent {
		if m.Role != llm.RoleSystem {
			continue
		}
		if i == 0 {
			problems = append(problems, "system is messages[0]")
		} else if sent[i-1].Role != llm.RoleUser {
			problems = append(problems, fmt.Sprintf("system %d follows %s", i, sent[i-1].Role))
		}
		if i+1 < len(sent) && sent[i+1].Role != llm.RoleAssistant {
			problems = append(problems, fmt.Sprintf("system %d is followed by %s", i, sent[i+1].Role))
		}
	}
	return problems
}

// Every history a Loop can build (opens with a user message; a notice is minted
// right after a user-role message, as assembleRequest does) of up to six items.
// For each: the warm projection equals the restored one; the sent form obeys the
// placement rules; the user, assistant and tool-result messages are exactly the
// ones 6b0c915 projected, so a provider that never sees notices (Gemini,
// OpenAI-compatible: agent.noticesFor strips them) is sent what it always was.
func TestAdvFixBEveryReachableHistoryKeepsPlacementAndOnlyLosesNotices(t *testing.T) {
	const maxLen = 6
	checked, bad := 0, 0
	report := func(format string, args ...any) {
		bad++
		if bad <= 8 {
			t.Errorf(format, args...)
		}
	}
	var walk func(toks []advTok)
	walk = func(toks []advTok) {
		if len(toks) > 0 {
			checked++
			warm := NewLog()
			var warmErr error
			for i, k := range toks {
				advAppendTok(t, warm, i, k)
				if _, err := warm.DeriveMessages(); err != nil {
					warmErr = err
				}
			}
			if warmErr != nil {
				t.Fatalf("%s: %v", advHistory(toks), warmErr)
			}
			restored, err := RestoreLog(warm.Events())
			if err != nil {
				t.Fatal(err)
			}
			wm, err := warm.DeriveMessages()
			if err != nil {
				t.Fatal(err)
			}
			rm, err := restored.DeriveMessages()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(advNewSigs(wm), advNewSigs(rm)) {
				report("%s: warm %v != restored %v", advHistory(toks), advNewSigs(wm), advNewSigs(rm))
			}
			if p := advWirePlacement(wm); len(p) != 0 {
				report("%s: sent form %v breaks placement: %v", advHistory(toks), advNewSigs(wm), p)
			}
			got, want := advWithoutSystem(advNewSigs(wm)), advWithoutSystem(oldProjection(toks))
			if !reflect.DeepEqual(got, want) {
				report("%s: non-notice messages %v differ from 6b0c915's %v", advHistory(toks), got, want)
			}
			stale, err := staleMessages(warm.snapshot())
			if err != nil || len(stale) != len(wm) {
				report("%s: staleMessages covers %d (err %v), projection %d", advHistory(toks), len(stale), err, len(wm))
			}
		}
		if len(toks) == maxLen {
			return
		}
		for k := advU; k <= advT; k++ {
			switch {
			case len(toks) == 0 && k != advU:
				continue
			case k == advN && toks[len(toks)-1] != advU && toks[len(toks)-1] != advT:
				continue
			}
			walk(append(slices.Clone(toks), k))
		}
	}
	walk(nil)
	if checked < 1000 {
		t.Fatalf("only %d histories enumerated", checked)
	}
	t.Logf("%d histories checked, %d violations", checked, bad)
}

// ---------------------------------------------------------------------------
// Drop records.

// recordedHistoryEdit is every message index any history_edit record names.
func recordedIndexes(t *testing.T, log *Log, reason string) map[int]bool {
	t.Helper()
	out := map[int]bool{}
	for _, e := range log.Events() {
		if e.Type != ProvenanceDropped {
			continue
		}
		var d DropData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatal(err)
		}
		if d.Reason != reason {
			continue
		}
		for _, drop := range d.Drops {
			out[drop.MessageIndex] = true
		}
	}
	return out
}

func recordCount(log *Log, reason string) int {
	n := 0
	for _, e := range log.Events() {
		var d DropData
		if e.Type == ProvenanceDropped && json.Unmarshal(e.Data, &d) == nil && d.Reason == reason {
			n++
		}
	}
	return n
}

// advRecordStep is what a Loop does on a request: derive, strip, record. It
// fails when a reasoning block the strip removed is named by no record, which is
// what "model-visible means logged" asks of provenance: the log and --json must
// say which messages lost reasoning.
func advRecordStep(t *testing.T, log *Log, label string, suffix bool) []StaleDrop {
	t.Helper()
	msgs, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	_, drops, err := log.StripStale(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.RecordStale(drops, "anthropic", "claude-opus-5-5"); err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{DropReasonHistoryEdit, DropReasonRetentionTrim} {
		recorded := recordedIndexes(t, log, reason)
		first := -1
		for _, d := range drops {
			if d.Reason == reason && (first < 0 || d.MessageIndex < first) {
				first = d.MessageIndex
			}
		}
		for _, d := range drops {
			// A compaction's strip is "from the first edited position on", so a
			// record that names the first position says the rest; the dedupe is
			// built on that. Anything else (a trim, a tool boundary) names its set.
			if suffix && reason == DropReasonHistoryEdit && d.MessageIndex != first {
				continue
			}
			if d.Reason == reason && !recorded[d.MessageIndex] {
				t.Errorf("%s: message %d lost reasoning (%s) and no record names it; recorded %v", label, d.MessageIndex, reason, slices.Sorted(maps_keys(recorded)))
			}
		}
	}
	return drops
}

func maps_keys(m map[int]bool) func(yield func(int) bool) {
	return func(yield func(int) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func toolsAdded(t *testing.T, log *Log, name string) {
	t.Helper()
	mustAppend(t, log, WireFrame, FrameData{System: "p", Why: WhyToolsAdded, Added: []string{name}})
}

// A tool-set boundary strips a leading run (everything before the frame), so its
// stale set grows at the END of that run when a second boundary lands later. The
// second strip lists the first's indexes and more; "only extends the recorded
// list" is true, yet it is a different fact (messages 5, 7, 9 now lost reasoning
// they carried on the request before) and must be recorded. The dedupe comment
// speaks of "from here on", which is a property of a compaction's suffix, not of
// a boundary's prefix.
func TestAdvFixBASecondToolsBoundaryIsRecorded(t *testing.T) {
	log := NewLog()
	mustAppend(t, log, WireFrame, FrameData{System: "p", Why: WhyOpened})
	staleFixture(t, log, 2) // messages 0..4
	toolsAdded(t, log, "x")
	staleRound(t, log, 2)
	staleRound(t, log, 3)
	d1 := advRecordStep(t, log, "after the first boundary", false)
	toolsAdded(t, log, "y")
	staleRound(t, log, 4)
	d2 := advRecordStep(t, log, "after the second boundary", false)
	if len(d2) <= len(d1) {
		t.Fatalf("the second boundary stripped %d messages, the first %d; the fixture says nothing", len(d2), len(d1))
	}
}

// A compaction later in the log than the recorded one, then an earlier one: each
// boundary move must be recorded and the number of records must stay small.
func TestAdvFixBBoundaryMovingEarlierIsRecordedAndGrowthIsBounded(t *testing.T) {
	log := NewLog()
	staleFixture(t, log, 6)
	compact(t, log, "c4", false)
	advRecordStep(t, log, "compaction at round 4", true)
	for i := 6; i < 20; i++ {
		staleRound(t, log, i)
		advRecordStep(t, log, fmt.Sprintf("growth %d", i), true)
	}
	compact(t, log, "c1", false)
	advRecordStep(t, log, "compaction at round 1", true)
	for i := 20; i < 40; i++ {
		staleRound(t, log, i)
		advRecordStep(t, log, fmt.Sprintf("growth %d", i), true)
	}
	compact(t, log, "c0", false)
	advRecordStep(t, log, "compaction at round 0", true)
	if n := recordCount(log, DropReasonHistoryEdit); n > 8 {
		t.Errorf("%d history_edit records for three compactions and 34 appended rounds", n)
	}
	// The earliest message that lost reasoning is named by some record.
	if got := recordedIndexes(t, log, DropReasonHistoryEdit); !got[3] {
		t.Errorf("message 3 (first assistant after the earliest edit) is in no record: %v", slices.Sorted(maps_keys(got)))
	}
}

// A retention trim and a compaction in one log: two reasons, each kept exact for
// its own cause. The trim's record never grows with the turns after it and the
// compaction's is bounded; neither reason hides the other.
func TestAdvFixBTrimAndCompactionInOneLog(t *testing.T) {
	store := NewStore(t.TempDir())
	store.setLimits(6000, 0)
	id := newID(t)
	log := NewLog()
	for i := 0; i < 6; i++ {
		mustAppend(t, log, TurnStart, nil)
		if i == 0 {
			mustAppend(t, log, WireFrame, FrameData{System: "p", Why: WhyOpened})
		}
		mustAppend(t, log, UserMessage, MessageData{Message: user(fmt.Sprintf("u%d", i) + strings.Repeat("y", 1500))})
		staleRound(t, log, i)
		mustAppend(t, log, AssistantMessage, MessageData{Message: llm.Message{Role: llm.RoleAssistant,
			Content: []llm.ContentBlock{llm.ReasoningBlock{Text: "t", Signature: fmt.Sprintf("end-%d", i)}, llm.TextBlock{Text: "done"}}}})
		mustAppend(t, log, TurnEnd, nil)
	}
	report, err := store.Save(id, log)
	if err != nil || report.TrimmedTurns == 0 {
		t.Fatalf("save: %v, trimmed %d", err, report.TrimmedTurns)
	}
	loaded, _, err := NewStore(store.Dir()).Load(id)
	if err != nil {
		t.Fatal(err)
	}
	advRecordStep(t, loaded, "after load", true)
	mustAppend(t, loaded, UserMessage, MessageData{Message: user("after")})
	staleRound(t, loaded, 50)
	staleRound(t, loaded, 51)
	// A compaction of a result the trim did not remove.
	compact(t, loaded, "c50", false)
	advRecordStep(t, loaded, "compaction after trim", true)
	for i := 52; i < 70; i++ {
		staleRound(t, loaded, i)
		advRecordStep(t, loaded, fmt.Sprintf("growth %d", i), true)
	}
	if n := recordCount(loaded, DropReasonRetentionTrim); n != 1 {
		t.Errorf("%d retention_trim records, want 1", n)
	}
	if n := recordCount(loaded, DropReasonHistoryEdit); n == 0 || n > 4 {
		t.Errorf("%d history_edit records after a compaction and 18 rounds, want a small number", n)
	}
}

// ---------------------------------------------------------------------------
// A file written by 6b0c915: the carried frame holds delivered and told.

func oldCarriedFrame(through int) map[string]any {
	return map[string]any{
		"system": "prompt A", "why": WhyCarried, "delivered": "prompt B", "told": []string{"echo"},
		"through": through,
		"tools": []llm.ToolSchema{{Name: "echo", Description: "d",
			InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}
}

func TestAdvFixBOldCarriedFrameLoadsSavesAndReloadsWithoutLoss(t *testing.T) {
	store := NewStore(t.TempDir())
	id := newID(t)
	log := NewLog()
	mustAppend(t, log, TurnStart, nil)
	mustAppend(t, log, WireFrame, oldCarriedFrame(1))
	mustAppend(t, log, UserMessage, MessageData{Message: user("kept user")})
	mustAppend(t, log, AssistantMessage, MessageData{Message: llm.Message{Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{llm.ReasoningBlock{Text: "t", Signature: "post-trim"}, llm.TextBlock{Text: "kept"}}}})
	mustAppend(t, log, TurnEnd, nil)
	before := log.Wire()
	if _, err := store.Save(id, log); err != nil {
		t.Fatal(err)
	}
	var last *Log
	for round := 0; round < 3; round++ {
		loaded, _, err := NewStore(store.Dir()).Load(id)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		w := loaded.Wire()
		if w.Frame == nil || w.Frame.System != "prompt A" || w.Frame.Through != 1 || w.Frame.Why != WhyCarried ||
			len(w.Frame.Tools) != 1 || w.Frame.Tools[0].Name != "echo" || string(w.Frame.Tools[0].InputSchema) != `{"type":"object"}` {
			t.Fatalf("round %d: frame = %+v", round, w.Frame)
		}
		if w.Delivered != "prompt A" || w.Told != nil {
			t.Errorf("round %d: delivered/told = %q %v: the notices the old build carried are ignored, the frame is what the model holds", round, w.Delivered, w.Told)
		}
		if before.Frame.System != w.Frame.System {
			t.Errorf("round %d: frame system changed", round)
		}
		msgs, err := loaded.DeriveMessages()
		if err != nil || len(msgs) != 2 {
			t.Fatalf("round %d: %d messages (err %v)", round, len(msgs), err)
		}
		// The block produced after the old build's trim stays valid: Through = 1
		// covers only the event the trim ended on.
		out, drops, err := loaded.StripStale(msgs)
		if err != nil {
			t.Fatal(err)
		}
		if len(drops) != 0 || len(signaturesOf(out)) != 1 {
			t.Errorf("round %d: strip dropped %v, signatures %v", round, drops, signaturesOf(out))
		}
		if _, err := store.Save(id, loaded); err != nil {
			t.Fatal(err)
		}
		last = loaded
	}
	if last == nil {
		t.Fatal("no round ran")
	}
}

// A trim by the new build of a file the old build wrote restates the frame
// without the fields, and the events it kept are intact.
func TestAdvFixBTrimOfAFileThatHoldsOldFramesDropsOnlyTheOldFields(t *testing.T) {
	store := NewStore(t.TempDir())
	store.setLimits(5000, 0)
	id := newID(t)
	log := NewLog()
	for i := 0; i < 5; i++ {
		mustAppend(t, log, TurnStart, nil)
		if i == 0 {
			frame := oldCarriedFrame(0)
			frame["why"] = WhyOpened
			delete(frame, "through")
			mustAppend(t, log, WireFrame, frame)
		}
		mustAppend(t, log, UserMessage, MessageData{Message: user(fmt.Sprintf("u%d", i) + strings.Repeat("y", 1500))})
		mustAppend(t, log, AssistantMessage, MessageData{Message: assistant("done")})
		mustAppend(t, log, TurnEnd, nil)
	}
	report, err := store.Save(id, log)
	if err != nil || report.TrimmedTurns == 0 {
		t.Fatalf("save: %v, trimmed %d", err, report.TrimmedTurns)
	}
	loaded, _, err := NewStore(store.Dir()).Load(id)
	if err != nil {
		t.Fatal(err)
	}
	w := loaded.Wire()
	if w.Frame == nil || w.Frame.System != "prompt A" || len(w.Frame.Tools) != 1 || w.Frame.Why != WhyCarried || w.Frame.Through == 0 {
		t.Fatalf("carried frame = %+v", w.Frame)
	}
	if w.Delivered != "prompt A" {
		t.Errorf("Delivered = %q", w.Delivered)
	}
	for _, e := range loaded.Events() {
		if e.Type == WireFrame && (strings.Contains(string(e.Data), "delivered") || strings.Contains(string(e.Data), "told")) {
			t.Errorf("carried frame still holds the retired fields: %s", e.Data)
		}
	}
}

// The log's reckoning of what the model holds (Wire().Delivered) and the
// request StripStale returns must agree. A notice answered only by signed
// reasoning is dropped from the request once the strip empties that reply, while
// the log still counts it delivered; no later Loop says it again, so the model
// is left on the frame's prompt. Both edit kinds that strip reach it.
func TestAdvFixBDeliveredAgreesWithTheStrippedRequest(t *testing.T) {
	edits := map[string]func(t *testing.T, log *Log){
		"tool boundary": func(t *testing.T, log *Log) { toolsAdded(t, log, "x") },
		"compaction":    func(t *testing.T, log *Log) { compact(t, log, "c0", false) },
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			log := NewLog()
			mustAppend(t, log, WireFrame, FrameData{System: "first", Why: WhyOpened})
			staleFixture(t, log, 1) // user, assistant+call, result
			mustAppend(t, log, AssistantMessage, MessageData{Message: assistant("a")})
			mustAppend(t, log, UserMessage, MessageData{Message: user("two")})
			appendNotice(t, log, NoticeData{Text: "now second", Prompt: "second"})
			mustAppend(t, log, AssistantMessage, MessageData{Message: llm.Message{Role: llm.RoleAssistant,
				Content: []llm.ContentBlock{llm.ReasoningBlock{Text: "t", Signature: "sig-only"}}}})
			mustAppend(t, log, UserMessage, MessageData{Message: user("three")})
			edit(t, log)
			msgs, err := log.DeriveMessages()
			if err != nil {
				t.Fatal(err)
			}
			out, drops, err := log.StripStale(msgs)
			if err != nil {
				t.Fatal(err)
			}
			if len(drops) == 0 {
				t.Fatal("nothing was stripped; the fixture says nothing")
			}
			holdsNotice := false
			for _, m := range out {
				if m.Role == llm.RoleSystem {
					holdsNotice = true
				}
			}
			if delivered := log.Wire().Delivered; delivered == "second" && !holdsNotice {
				t.Errorf("the log says %q was delivered but the request carries no notice: roles %v; no Loop will say it again", delivered, roleList(out))
			}
		})
	}
}
