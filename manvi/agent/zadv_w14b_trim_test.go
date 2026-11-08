package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/session"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// Second adversarial pass over w14-trim (card ft-d8db16de), at the wire. Doc
// citations are to the claude-api skill's shared/ directory: model-migration.md
// "Breaking change 3" and preserved-thinking-migration/causes.md (rows
// rolling_truncation, tail_kept, and the keep list).

// trimmedConvo runs `turns` turns with filler so the production store really
// trims, saves, reloads, and returns the conversation resumed on the reloaded
// log together with the store and the session id.
func trimmedConvo(t *testing.T, turns int, script []string) (*convo, *session.Store, string) {
	t.Helper()
	c := newConvo(t, script, nil)
	if err := c.reg.Register(echoTool("echo", "echo", nil)); err != nil {
		t.Fatal(err)
	}
	store := session.NewStore(t.TempDir())
	id, err := session.NewID()
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < turns; n++ {
		if err := c.turn("p", fmt.Sprintf("turn %d", n), nil); err != nil {
			t.Fatal(err)
		}
		trimFiller(t, c.log)
	}
	report, err := store.Save(id, c.log)
	if err != nil || report.TrimmedTurns == 0 {
		t.Fatalf("save: %v, trimmed %d: the test says nothing", err, report.TrimmedTurns)
	}
	restored, _, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	c.log = restored
	return c, store, id
}

func countDrops(log *session.Log, reason string) int {
	n := 0
	for _, e := range log.Events() {
		if e.Type != session.ProvenanceDropped {
			continue
		}
		var d session.DropData
		if err := json.Unmarshal(e.Data, &d); err == nil && d.Reason == reason {
			n++
		}
	}
	return n
}

// Item 5. The provenance record says reasoning was removed and why, once per
// change of what is removed. A step repeats the same strip, and so does the next
// turn's Loop (the TUI and serve build a Loop per turn): the record must not grow
// by one per step or one per turn while nothing changed.
func TestAdvBRetentionTrimDropIsRecordedOncePerChangeNotPerStepOrTurn(t *testing.T) {
	c, _, _ := trimmedConvo(t, 4, trimScript(24))
	base := countDrops(c.log, session.DropReasonRetentionTrim)
	if base != 0 {
		t.Fatalf("a freshly loaded log already holds %d retention_trim records", base)
	}
	const turns = 5
	for n := 0; n < turns; n++ {
		if err := c.turn("p", fmt.Sprintf("after %d", n), nil); err != nil {
			t.Fatal(err)
		}
		if got := countDrops(c.log, session.DropReasonRetentionTrim); got > 1 {
			t.Errorf("after %d turn(s) (%d requests each) the log holds %d retention_trim records for one unchanged trim",
				n+1, 2, got)
		}
	}
	if got := countDrops(c.log, session.DropReasonRetentionTrim); got != 1 {
		t.Fatalf("%d retention_trim records after %d turns, want exactly 1 (recorded once, on the first request that stripped)", got, turns)
	}
}

// Item 5, within one Loop: many steps of one turn record the strip once.
func TestAdvBRetentionTrimDropIsRecordedOncePerLoopAcrossManySteps(t *testing.T) {
	c, _, _ := trimmedConvo(t, 4, trimScript(24))
	script := []string{}
	for n := 0; n < 6; n++ {
		script = append(script, thinkingToolStep(100+n))
	}
	script = append(script, thinkingAnswer("ans-many"))
	c2 := newConvo(t, script, c.log)
	if err := c2.reg.Register(echoTool("echo", "echo", nil)); err != nil {
		t.Fatal(err)
	}
	if err := c2.turn("p", "many steps", nil); err != nil {
		t.Fatal(err)
	}
	if got := len(c2.requests()); got < 7 {
		t.Fatalf("only %d requests, the turn did not run several steps", got)
	}
	if got := countDrops(c2.log, session.DropReasonRetentionTrim); got != 1 {
		t.Fatalf("%d retention_trim records across %d steps, want 1", got, len(c2.requests()))
	}
}

// Item 6. Model-visible means logged, with the check on, through a trim, a
// resume, a second trim and a second resume: every request is accepted by
// AssertInvariant (convo.turn sets it) and every drop the strip made is in the
// log as provenance/dropped, naming the reason.
func TestAdvBInvariantHoldsThroughTrimAndResumeAndTheStripIsLogged(t *testing.T) {
	c, store, id := trimmedConvo(t, 4, trimScript(30))
	if err := c.turn("p", "resume 1", nil); err != nil {
		t.Fatalf("model-visible-means-logged failed after a trim: %v", err)
	}
	trimFiller(t, c.log)
	if r, err := store.Save(id, c.log); err != nil || r.TrimmedTurns == 0 {
		t.Fatalf("second save: %v, trimmed %d", err, r.TrimmedTurns)
	}
	restored, _, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	c.log = restored
	if err := c.turn("p", "resume 2", nil); err != nil {
		t.Fatalf("model-visible-means-logged failed after the second trim: %v", err)
	}
	msgs, err := c.log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AssertModelVisible(c.log, msgs); err != nil {
		t.Fatal(err)
	}
	stripped, _, err := c.log.StripStaleReasoning(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AssertModelVisible(c.log, stripped); err != nil {
		t.Fatalf("the stripped history is not reconstructable from the log: %v", err)
	}
	if countDrops(c.log, session.DropReasonRetentionTrim) == 0 {
		t.Fatal("the strip is not in the log")
	}
}

// Item 2 at the wire: three resumes, the middle one trimming. After each reload
// no request carries a block signed before the latest trim, and every replayed
// block still sits on the prefix it was produced under.
func TestAdvBThreeResumesNeverReplayABlockFromBeforeTheLatestTrim(t *testing.T) {
	c, store, id := trimmedConvo(t, 4, trimScript(40))
	stale := map[string]bool{}
	mark := func(from, to int) {
		for n := from; n < to; n++ {
			stale[fmt.Sprintf("sig-%d", n)] = true
			stale[fmt.Sprintf("ans-%d", n)] = true
		}
	}
	mark(0, 4)
	next := 4 // the loop numbers its scripted responses by request; turn k uses 2k and 2k+1
	run := func(label string, filler bool) {
		t.Helper()
		before := len(c.requests())
		if err := c.turn("p", label, nil); err != nil {
			t.Fatal(err)
		}
		for i, req := range c.requests()[before:] {
			if found := replaysAny(t, req, stale); len(found) > 0 {
				t.Errorf("%s: request %d replays %v, signed before the latest trim", label, before+i, found)
			}
		}
		if filler {
			trimFiller(t, c.log)
		}
		next++
	}
	resume := func(trim bool) {
		t.Helper()
		r, err := store.Save(id, c.log)
		if err != nil {
			t.Fatal(err)
		}
		if trim && r.TrimmedTurns == 0 {
			t.Fatal("expected this save to trim")
		}
		restored, _, err := store.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		c.log = restored
	}
	// Resume 1 (no further trim): its own block is valid and replayed.
	run("r1", false)
	resume(false)
	// The block of r1 is signed under the trimmed prefix and stays valid after a
	// reload that trimmed nothing: it may be replayed and must be.
	run("r2", true)
	if n := assertPrefixesPreserved(t, c.requests()); n < 2 {
		t.Fatalf("only %d replays checked", n)
	}
	// Resume 2 trims: r1 and r2 are stale from here.
	mark(3, next)
	resume(true)
	run("r3", false)
	resume(false)
	run("r4", false)
	if n := assertPrefixesPreserved(t, c.requests()); n < 3 {
		t.Fatalf("only %d replays checked across the resumes", n)
	}
}

// Item 3 at the wire. A frame-less trimmed file (a log written before the
// carried frame carried Through, or one a non-binding provider wrote) is fully
// stale on every load, so the reasoning the resumed run produced is gone at the
// next reload even though nothing trimmed since. The cost is real and the safe
// side: no block signed before the reload is sent, and none of the legacy ones.
func TestAdvBFramelessTrimCostsTheReasoningProducedAfterResumeAtEachReload(t *testing.T) {
	c := newConvo(t, trimScript(10), nil)
	if err := c.reg.Register(echoTool("echo", "echo", nil)); err != nil {
		t.Fatal(err)
	}
	legacy := session.NewLog()
	for n := 0; n < 4; n++ {
		mustLegacyAppend(t, legacy, session.TurnStart, nil)
		mustLegacyAppend(t, legacy, session.UserMessage, session.MessageData{Message: llm.Message{
			Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: fmt.Sprintf("old %d", n)}}}})
		mustLegacyAppend(t, legacy, session.AssistantMessage, session.MessageData{Message: llm.Message{
			Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				llm.ReasoningBlock{Text: "t", Signature: fmt.Sprintf("legacy-%d", n)}, llm.TextBlock{Text: "ok"}}}})
		mustLegacyAppend(t, legacy, session.TurnEnd, nil)
		trimFiller(t, legacy)
	}
	store := session.NewStore(t.TempDir())
	id, err := session.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if r, err := store.Save(id, legacy); err != nil || r.TrimmedTurns == 0 {
		t.Fatalf("save: %v, trimmed %d", err, r.TrimmedTurns)
	}
	reload := func() {
		t.Helper()
		restored, _, err := store.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		c.log = restored
	}
	legacyIDs := map[string]bool{"legacy-0": true, "legacy-1": true, "legacy-2": true, "legacy-3": true}

	reload()
	if err := c.turn("p", "a", nil); err != nil { // requests 0 (sig-0) and 1 (ans-0)
		t.Fatal(err)
	}
	if err := c.turn("p", "b", nil); err != nil { // same process: sig-0 and ans-0 are replayed
		t.Fatal(err)
	}
	inProcess := 0
	for _, req := range c.requests()[2:] {
		inProcess += len(replaysAny(t, req, map[string]bool{"sig-0": true, "ans-0": true}))
	}
	if inProcess == 0 {
		t.Fatal("in the process that produced it, the post-resume reasoning was not replayed; the baseline is wrong")
	}
	// The trimmed file is saved and read again without any new trim.
	if _, err := store.Save(id, c.log); err != nil {
		t.Fatal(err)
	}
	reload()
	before := len(c.requests())
	if err := c.turn("p", "c", nil); err != nil {
		t.Fatal(err)
	}
	lost := 0
	for _, req := range c.requests()[before:] {
		lost += len(replaysAny(t, req, map[string]bool{"sig-0": true, "ans-0": true, "sig-1": true, "ans-1": true}))
		if found := replaysAny(t, req, legacyIDs); len(found) > 0 {
			t.Errorf("a legacy block %v is replayed behind a prefix that is gone", found)
		}
	}
	if lost != 0 {
		t.Fatalf("%d block(s) produced after the frame-less trim were replayed after a reload; the fallback treats them as stale", lost)
	}
	t.Logf("cost: %d block(s) replayed in-process before the reload, 0 after it, and no new trim has happened", inProcess)
}

func mustLegacyAppend(t *testing.T, l *session.Log, typ session.Type, payload any) {
	t.Helper()
	if _, err := l.Append(typ, payload); err != nil {
		t.Fatal(err)
	}
}

// Item 1 at the wire. Save runs while a tool round is open: the handler of the
// call saves the log with the production store and the loop's own log is left
// alone, so the round continues with its thinking intact (the keep list:
// "an assistant turn whose tool_use still awaits its tool_result should keep its
// thinking"). Only a log read back from that file is stripped, and what it sends
// must carry none of the thinking it could not have signed under the prefix it
// has now.
func TestAdvBSaveDuringAToolRoundDoesNotStripTheLiveRound(t *testing.T) {
	c := newConvo(t, []string{thinkingToolStep(0), thinkingAnswer("ans-0")}, nil)
	store := session.NewStore(t.TempDir())
	id, err := session.NewID()
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 4; n++ {
		trimFiller(t, c.log)
		if _, err := c.log.Append(session.UserMessage, session.MessageData{Message: llm.Message{
			Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: fmt.Sprintf("old %d", n)}}}}); err != nil {
			t.Fatal(err)
		}
	}
	var saveErr error
	var saved session.SaveReport
	saving := tools.Tool{
		Schema: llm.ToolSchema{Name: "echo", Description: "echo",
			InputSchema: []byte(`{"type":"object","properties":{"v":{"type":"integer"}}}`)},
		Handler: func(context.Context, tools.Call) tools.Result {
			saved, saveErr = store.Save(id, c.log)
			return tools.Result{Text: "done"}
		},
	}
	if err := c.reg.Register(saving); err != nil {
		t.Fatal(err)
	}
	if err := c.turn("p", "go", func(cfg *Config, _ *bus.Bus) {}); err != nil {
		t.Fatal(err)
	}
	if saveErr != nil {
		t.Fatalf("save during the round: %v", saveErr)
	}
	reqs := c.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want the tool request and its continuation", len(reqs))
	}
	// The live round: the continuation replays the thinking of the call it answers.
	if got := replaysAny(t, reqs[1], map[string]bool{"sig-0": true}); len(got) != 1 {
		t.Fatalf("the continuation of the live round replayed %v, want sig-0 kept with its tool_use", got)
	}
	assertPrefixesPreserved(t, reqs)
	t.Logf("mid-round save report: %+v", saved)
	if got := countDrops(c.log, session.DropReasonRetentionTrim); got != 0 {
		t.Fatalf("the live log recorded %d retention_trim drops, it was never reloaded", got)
	}
}
