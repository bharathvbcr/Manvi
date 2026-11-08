package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/session"
)

// trimFiller bloats the saved file without touching what a request replays: a
// provenance record is in the log and the file, and the projection ignores it.
// It lets a store with the production byte budget really trim.
func trimFiller(t *testing.T, log *session.Log) {
	t.Helper()
	if _, err := log.Append(session.ProvenanceDropped, session.DropData{
		Drops: []llm.Drop{{FromProvider: strings.Repeat("x", 2500<<10)}},
	}); err != nil {
		t.Fatal(err)
	}
}

func trimScript(turns int) []string {
	var script []string
	for n := 0; n < turns; n++ {
		script = append(script, thinkingToolStep(n), thinkingAnswer(fmt.Sprintf("ans-%d", n)))
	}
	return script
}

// replaysAny reports the signatures of want that req carries.
func replaysAny(t *testing.T, req wireRequest, want map[string]bool) []string {
	t.Helper()
	var found []string
	for _, blk := range signedBlocks(t, req) {
		if want[blk.signature] {
			found = append(found, blk.signature)
		}
	}
	return found
}

// Retention drops the oldest turns of a saved session. On Anthropic every signed
// block that survives was produced behind messages the file no longer holds, so
// the next request must not carry one (a 400 on an enforced account, shared/
// model-migration.md "Breaking change 3"); the removal is a leading run, oldest
// first, which "What keeps later blocks valid" lists. A block produced after the
// trimmed file was loaded is signed under the trimmed history and is replayed.
// The store is the production one, so Save really trims, and the log is reloaded
// as a resume reloads it.
func TestAdvTrimmedSessionReplaysNoBlockSignedBeforeTheTrim(t *testing.T) {
	c := newConvo(t, trimScript(6), nil)
	if err := c.reg.Register(echoTool("echo", "echo", nil)); err != nil {
		t.Fatal(err)
	}
	store := session.NewStore(t.TempDir())
	id, err := session.NewID()
	if err != nil {
		t.Fatal(err)
	}

	signedSoFar := map[string]bool{}
	run := func(prompt string, fromTurn int) {
		t.Helper()
		if err := c.turn("p", prompt, nil); err != nil {
			t.Fatal(err)
		}
		signedSoFar[fmt.Sprintf("sig-%d", fromTurn)] = true
		signedSoFar[fmt.Sprintf("ans-%d", fromTurn)] = true
		trimFiller(t, c.log)
	}
	for n := 0; n < 4; n++ {
		run(fmt.Sprintf("turn %d", n), n)
	}
	report, err := store.Save(id, c.log)
	if err != nil {
		t.Fatal(err)
	}
	if report.TrimmedTurns == 0 {
		t.Fatal("the save did not trim, so the test says nothing")
	}
	restored, _, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	c.log = restored
	before := len(c.requests())

	// The turn after the resume. Its own thinking is signed under the trimmed
	// history and is replayed on its second request.
	run("turn 4", 4)
	reqs := c.requests()
	post := reqs[before:]
	preTrim := map[string]bool{}
	for n := 0; n < 4; n++ {
		preTrim[fmt.Sprintf("sig-%d", n)] = true
		preTrim[fmt.Sprintf("ans-%d", n)] = true
	}
	for i, req := range post {
		if found := replaysAny(t, req, preTrim); len(found) > 0 {
			t.Errorf("request %d after the trim replays %v, signed before retention dropped %d turn(s)", before+i, found, report.TrimmedTurns)
		}
	}
	if n := assertPrefixesPreserved(t, reqs); n < 1 {
		t.Fatalf("only %d replays were checked; the block signed after the resume was never sent back", n)
	}

	// A second trim makes the first resumed run's blocks stale as well.
	report, err = store.Save(id, c.log)
	if err != nil {
		t.Fatal(err)
	}
	if report.TrimmedTurns == 0 {
		t.Fatal("the second save did not trim, so the test says nothing")
	}
	restored, _, err = store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	c.log = restored
	before = len(c.requests())
	run("turn 5", 5)
	reqs = c.requests()
	delete(signedSoFar, "sig-5")
	delete(signedSoFar, "ans-5")
	for i, req := range reqs[before:] {
		if found := replaysAny(t, req, signedSoFar); len(found) > 0 {
			t.Errorf("request %d after the second trim replays %v, signed before it", before+i, found)
		}
	}
	if n := assertPrefixesPreserved(t, reqs); n < 2 {
		t.Fatalf("only %d replays were checked across both trims", n)
	}

	// The strip is recorded, so the log and --json say reasoning was removed.
	recorded := 0
	for _, e := range c.log.Events() {
		if e.Type == session.ProvenanceDropped && strings.Contains(string(e.Data), dropReasonRetentionTrim) {
			recorded++
		}
	}
	if recorded == 0 {
		t.Errorf("no provenance/dropped record names %q, so nothing says why reasoning was removed", dropReasonRetentionTrim)
	}
}

// dropReasonRetentionTrim is the reason a provenance/dropped record gives for
// reasoning removed because retention trimmed the session.
const dropReasonRetentionTrim = "retention_trim"
