package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"manvi/llm"
	"manvi/prompt"
	"manvi/session"
)

// chdir moves into dir for the duration of the test. The instructions file is
// read from the working directory, so the only way to exercise that read is to
// have one.
func chdir(t *testing.T, dir string) {
	t.Helper()
	t.Chdir(dir)
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Prompt assembly is a single greedy pass in section order under a budget that
// is a fraction of the model's window. Two things follow from that and neither
// was visible before: what gets dropped is decided by section order, and the
// account of what got dropped went to stderr — invisible under the alternate
// screen and absent from every session log.

// TestOversizedProjectInstructionsDoNotDisplaceTheGuidance is the defect.
//
// project-instructions sits at order 35, ahead of problem-deconstruction at 42,
// hardening at 43, inquiry at 44 and working-method at 50. An instructions file
// large enough to fit and large enough to exhaust the rest of the budget
// therefore consumed exactly the four sections that carry hypotheses, hardening
// and method — the guidance most likely to be missed and least likely to be
// noticed missing.
func TestOversizedProjectInstructionsDoNotDisplaceTheGuidance(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	// Far larger than any budget: the point is that it cannot take everything
	// with it, however big it is.
	var huge strings.Builder
	for range 4000 {
		huge.WriteString("- always run the full suite before claiming a fix is complete\n")
	}
	writeFile(t, dir, "AGENTS.md", huge.String())

	const budget = 2000
	src, ok := projectInstructionsSource(budget)
	if !ok {
		t.Fatal("the instructions file was not picked up at all")
	}
	sections, err := src.Sections()
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 1 {
		t.Fatalf("sections = %d, want 1", len(sections))
	}
	got := prompt.EstimateTokens(sections[0].Text)
	if limit := budget / projectInstructionShare; got > limit+64 {
		t.Fatalf("the instructions section is %d tokens against a %d-token share; it can still "+
			"displace the guidance behind it", got, limit)
	}
	if !strings.Contains(sections[0].Text, "cut here") {
		t.Fatal("the file was shortened without saying so, so an operator whose rules stop being " +
			"followed has no way to learn why")
	}
	// Cut on a line boundary: half a rule reads as a whole rule that happens to
	// be wrong.
	body := sections[0].Text[:strings.Index(sections[0].Text, "[this instructions file")]
	if strings.HasSuffix(strings.TrimRight(body, "\n"), "before claiming a fix is comp") {
		t.Fatal("the cut landed mid-line")
	}
}

// An unbounded budget is what every non-local provider gets, and nothing is
// scarce there, so nothing is cut.
func TestProjectInstructionsAreNotCutWithoutABudget(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	writeFile(t, dir, "AGENTS.md", strings.Repeat("- a rule\n", 5000))

	src, ok := projectInstructionsSource(0)
	if !ok {
		t.Fatal("not picked up")
	}
	sections, _ := src.Sections()
	if strings.Contains(sections[0].Text, "cut here") {
		t.Fatal("an unbounded prompt cut the instructions file anyway")
	}
}

// The four guidance sections survive an oversized instructions file. This is
// the property the bound exists for, asserted end to end rather than through
// the token arithmetic above.
func TestGuidanceSurvivesAnOversizedInstructionsFile(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	writeFile(t, dir, "AGENTS.md", strings.Repeat("- always run the full suite\n", 4000))

	reg := routerRegistry(t, nil)
	text, _, _ := assemblePromptReported(reg, PromptOptions{
		Provider: "local", TaskToolsOffered: true,
	})
	for _, want := range []string{
		"Problem Deconstruction & Hypotheses",
		"Hardening & Adversarial Stress-Testing",
		"Inquiry & Decision Impact",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("an oversized AGENTS.md displaced %q", want)
		}
	}
}

// The account of the assembly reaches the session log, which is the only place
// a resumed session or an evidence report can read it.
func TestPromptAssemblyIsRecorded(t *testing.T) {
	log := session.NewLog()
	report := prompt.Report{
		Included: []prompt.Included{{Name: "identity", Source: "identity"}},
		Omitted:  []prompt.Omitted{{Name: "working-method", Reason: "dropped: over budget"}},
		Tokens:   1234,
	}
	if err := recordPromptAssembly(log, report, 4096); err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, e := range log.Events() {
		if e.Type != session.PromptAssembled {
			continue
		}
		found = true
		var d session.PromptAssembledData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatal(err)
		}
		if d.Tokens != 1234 || d.Budget != 4096 {
			t.Fatalf("data = %+v, want the measured tokens and the budget", d)
		}
		if len(d.Included) != 1 || d.Included[0] != "identity" {
			t.Fatalf("included = %v", d.Included)
		}
		if len(d.Dropped) != 1 || !strings.Contains(d.Dropped[0], "working-method") {
			t.Fatalf("dropped = %v, want the section and its reason", d.Dropped)
		}
	}
	if !found {
		t.Fatal("the assembly left no trace in the log")
	}
}

// A nil log is the case where nothing is recording, and it must not be an
// error: the assembly still happened and the turn still runs.
func TestPromptAssemblyRecordingToleratesNoLog(t *testing.T) {
	if err := recordPromptAssembly(nil, prompt.Report{}, 0); err != nil {
		t.Fatalf("recording to no log returned %v", err)
	}
}

// TestTUISessionsPersist closes the gap root.go's own comment already claimed
// was closed: "sessionsDir is where headless and TUI sessions persist", while
// only the headless path ever called Save. Closing the window discarded the
// conversation, and the resume the CLI advertises had no counterpart here.
func TestTUISessionsPersist(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MANVI_STATE_DIR", dir)

	log := session.NewLog()
	if _, err := log.Append(session.UserMessage, session.MessageData{
		Message: llm.Message{
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{llm.TextBlock{Text: "hello"}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	h := &harnessHost{}
	s := &tuiSession{id: "S1", storeID: "abc123", log: log}
	if err := h.persist(s); err != nil {
		t.Fatalf("persist: %v", err)
	}

	store := session.NewStore(sessionsDir())
	loaded, _, err := store.Load("abc123")
	if err != nil {
		t.Fatalf("the session did not come back: %v", err)
	}
	if len(loaded.Events()) != len(log.Events()) {
		t.Fatalf("loaded %d events, saved %d", len(loaded.Events()), len(log.Events()))
	}
}

// A session with no log is not an error. It is a tab nobody has typed into, and
// refusing to "save" it would turn an empty tab into a failed turn.
func TestPersistToleratesAnEmptySession(t *testing.T) {
	h := &harnessHost{}
	if err := h.persist(nil); err != nil {
		t.Fatalf("persist(nil) = %v", err)
	}
	if err := h.persist(&tuiSession{id: "x"}); err != nil {
		t.Fatalf("persist with no log = %v", err)
	}
	// A session that has a log and no identifier is a real fault and must be
	// reported, not quietly skipped: quietly skipping is what this replaced.
	if err := h.persist(&tuiSession{id: "x", log: session.NewLog()}); err == nil {
		t.Fatal("a session with no durable identifier was silently not saved")
	}
}
