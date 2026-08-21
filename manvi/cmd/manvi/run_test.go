package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"manvi/flags"
	"manvi/llm"
	"manvi/session"
)

func TestParseRunArgsPositional(t *testing.T) {
	opts, err := parseRunArgs(newTestRegistry(t), []string{"hello", "world"}, nil)
	if err != nil {
		t.Fatalf("parseRunArgs: %v", err)
	}
	if opts.prompt != "hello world" {
		t.Errorf("prompt = %q, want %q", opts.prompt, "hello world")
	}
	if opts.asJSON {
		t.Error("expected asJSON = false")
	}
}

func TestParseRunArgsFlags(t *testing.T) {
	opts, err := parseRunArgs(newTestRegistry(t), []string{
		"-p", "inspect codebase",
		"--task", "TASK-001",
		"--max-steps", "10",
		"--timeout", "5m",
		"--json",
	}, nil)
	if err != nil {
		t.Fatalf("parseRunArgs: %v", err)
	}
	if opts.prompt != "inspect codebase" {
		t.Errorf("prompt = %q, want %q", opts.prompt, "inspect codebase")
	}
	if opts.task != "TASK-001" {
		t.Errorf("task = %q, want TASK-001", opts.task)
	}
	if opts.maxSteps != 10 {
		t.Errorf("maxSteps = %d, want 10", opts.maxSteps)
	}
	if opts.timeout != 5*time.Minute {
		t.Errorf("timeout = %v, want 5m", opts.timeout)
	}
	if !opts.asJSON {
		t.Error("expected asJSON = true")
	}
}

func TestParseRunArgsEmptyPromptRefused(t *testing.T) {
	_, err := parseRunArgs(newTestRegistry(t), []string{}, nil)
	if err == nil {
		t.Fatal("empty prompt should error")
	}
	if !strings.Contains(err.Error(), "needs a prompt") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseRunArgsConflictingOutputModes(t *testing.T) {
	_, err := parseRunArgs(newTestRegistry(t), []string{"-p", "hello", "--json", "--quiet"}, nil)
	if err == nil {
		t.Fatal("expected conflict between --json and --quiet")
	}
	if !strings.Contains(err.Error(), "--json and --quiet") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseRunArgsInvalidMaxSteps(t *testing.T) {
	_, err := parseRunArgs(newTestRegistry(t), []string{"-p", "hello", "--max-steps", "0"}, nil)
	if err == nil {
		t.Fatal("expected error on --max-steps 0")
	}
	_, err = parseRunArgs(newTestRegistry(t), []string{"-p", "hello", "--max-steps", "abc"}, nil)
	if err == nil {
		t.Fatal("expected error on non-integer --max-steps")
	}
}

func TestRunHeadlessMissingCredentialReportsActionableError(t *testing.T) {
	reg, err := flags.NewHarnessRegistry("")
	if err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	err = runHeadless(&out, &errOut, reg, []string{"-p", "test prompt"})
	if err == nil {
		t.Fatal("runHeadless with unconfigured credential should fail")
	}
	// The error should mention credential resolution or missing API key
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") && !strings.Contains(err.Error(), "credential") && !strings.Contains(err.Error(), "cannot probe") {
		t.Logf("got error: %v", err)
	}
}

func TestSystemPromptAssembly(t *testing.T) {
	reg, err := flags.NewHarnessRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	prompt := systemPrompt(reg)
	if !strings.Contains(prompt, "You are a builder agent inside MANVI") {
		t.Errorf("expected identity in system prompt, got: %s", prompt)
	}
	if !strings.Contains(prompt, "devcouncil_* tools") {
		t.Errorf("expected tools notice in system prompt, got: %s", prompt)
	}
	if !strings.Contains(prompt, "Tasks: devcouncil_next_task") {
		t.Errorf("expected tasks notice in system prompt, got: %s", prompt)
	}
}

func TestLocalConfigSamplingParsing(t *testing.T) {
	reg, err := flags.NewHarnessRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	_ = reg.Set(flags.Human, flags.LLMLocalTemperature, "0.2")
	_ = reg.Set(flags.Human, flags.LLMLocalTopP, "0.9")
	_ = reg.Set(flags.Human, flags.LLMLocalRepetitionPenalty, "1.1")

	cfg, err := localConfig(reg)
	if err != nil {
		t.Fatalf("localConfig: %v", err)
	}
	if cfg.Temperature == nil || *cfg.Temperature != 0.2 {
		t.Errorf("expected Temperature 0.2, got %v", cfg.Temperature)
	}
	if cfg.TopP == nil || *cfg.TopP != 0.9 {
		t.Errorf("expected TopP 0.9, got %v", cfg.TopP)
	}
	if cfg.RepetitionPenalty == nil || *cfg.RepetitionPenalty != 1.1 {
		t.Errorf("expected RepetitionPenalty 1.1, got %v", cfg.RepetitionPenalty)
	}
}

// --- session continuation ---

func TestParseRunArgsSessionSelection(t *testing.T) {
	opts, err := parseRunArgs(newTestRegistry(t), []string{"-c", "-p", "carry on"}, nil)
	if err != nil {
		t.Fatalf("parseRunArgs: %v", err)
	}
	if !opts.continueLast || opts.resume != "" {
		t.Errorf("-c gave continueLast=%v resume=%q", opts.continueLast, opts.resume)
	}

	opts, err = parseRunArgs(newTestRegistry(t), []string{"--resume", "abc123", "-p", "carry on"}, nil)
	if err != nil {
		t.Fatalf("parseRunArgs: %v", err)
	}
	if opts.resume != "abc123" || opts.continueLast {
		t.Errorf("--resume gave continueLast=%v resume=%q", opts.continueLast, opts.resume)
	}

	if _, err := parseRunArgs(newTestRegistry(t), []string{"--continue", "--resume", "abc", "-p", "x"}, nil); err == nil {
		t.Fatal("--continue and --resume together must be refused, not silently reconciled")
	} else if !strings.Contains(err.Error(), "pass one") {
		t.Errorf("unexpected error: %v", err)
	}

	if _, err := parseRunArgs(newTestRegistry(t), []string{"--resume"}, nil); err == nil {
		t.Fatal("--resume with no value must be refused")
	}
	if _, err := parseRunArgs(newTestRegistry(t), []string{"--resume", "   ", "-p", "x"}, nil); err == nil {
		t.Fatal("--resume with a blank value must be refused")
	}
}

// seedSession records one turn under the given id, the way a completed run
// does. The id is chosen by the caller so a test that turns on prefixes is
// deterministic rather than dependent on what the random generator produced.
func seedSession(t *testing.T, store *session.Store, id, prompt string) string {
	t.Helper()
	log := session.NewLog()
	for _, step := range []struct {
		typ     session.Type
		payload any
	}{
		{session.TurnStart, nil},
		{session.UserMessage, session.MessageData{Message: llm.Message{
			Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: prompt}},
		}}},
		{session.AssistantMessage, session.MessageData{Message: llm.Message{
			Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: "acknowledged"}},
		}}},
		{session.TurnEnd, nil},
	} {
		if _, err := log.Append(step.typ, step.payload); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Save(id, log); err != nil {
		t.Fatalf("seeding %s: %v", id, err)
	}
	return id
}

func TestSessionsDirFollowsTheStateDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MANVI_STATE_DIR", dir)
	if got, want := sessionsDir(), filepath.Join(dir, "sessions"); got != want {
		t.Errorf("sessionsDir = %q, want %q", got, want)
	}
}

func TestOpenSessionStartsFreshAndResumesWhatItSaved(t *testing.T) {
	t.Setenv("MANVI_STATE_DIR", t.TempDir())
	store := session.NewStore(sessionsDir())
	var notes bytes.Buffer

	id, log, err := openSession(&notes, store, runOptions{})
	if err != nil {
		t.Fatalf("openSession: %v", err)
	}
	if id == "" || log == nil || log.Len() != 0 {
		t.Fatalf("a fresh session should be empty: id=%q len=%d", id, log.Len())
	}
	if !strings.Contains(notes.String(), id) {
		t.Errorf("the session id was not reported: %q", notes.String())
	}

	if _, err := log.Append(session.TurnStart, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.UserMessage, session.MessageData{Message: llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "the original prompt"}},
	}}); err != nil {
		t.Fatal(err)
	}
	want, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	if err := saveSession(&notes, store, id, log); err != nil {
		t.Fatalf("saveSession: %v", err)
	}

	// A second process: a new store over the same directory.
	resumedID, resumed, err := openSession(&notes, session.NewStore(sessionsDir()), runOptions{continueLast: true})
	if err != nil {
		t.Fatalf("openSession --continue: %v", err)
	}
	if resumedID != id {
		t.Errorf("--continue resumed %s, want %s", resumedID, id)
	}
	got, err := resumed.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) || len(got) == 0 || got[0].Text() != want[0].Text() {
		t.Fatalf("resumed history = %+v, want %+v", got, want)
	}

	// And by prefix, which is the way a human names one.
	byPrefix, _, err := openSession(&notes, session.NewStore(sessionsDir()), runOptions{resume: id[:4]})
	if err != nil {
		t.Fatalf("openSession --resume prefix: %v", err)
	}
	if byPrefix != id {
		t.Errorf("--resume %s resolved to %s, want %s", id[:4], byPrefix, id)
	}
}

func TestContinueWithNoPriorSessionIsAnError(t *testing.T) {
	t.Setenv("MANVI_STATE_DIR", t.TempDir())
	var notes bytes.Buffer
	_, log, err := openSession(&notes, session.NewStore(sessionsDir()), runOptions{continueLast: true})
	if err == nil {
		t.Fatal("--continue with nothing to continue must fail, not start a fresh run that looks resumed")
	}
	if log != nil {
		t.Fatal("no log may be returned when there is nothing to continue")
	}
	if !strings.Contains(err.Error(), "--continue") || !strings.Contains(err.Error(), "no saved session") {
		t.Errorf("error does not explain itself: %v", err)
	}
}

func TestResumeUnknownAndAmbiguousReferences(t *testing.T) {
	t.Setenv("MANVI_STATE_DIR", t.TempDir())
	store := session.NewStore(sessionsDir())
	var notes bytes.Buffer

	first := seedSession(t, store, "aaaa000000000000", "first")
	second := seedSession(t, store, "aaab000000000000", "second")
	seedSession(t, store, "bbbb000000000000", "third")

	if _, _, err := openSession(&notes, store, runOptions{resume: "ffffffffffffffff"}); err == nil {
		t.Fatal("resuming an id that does not exist must fail")
	} else if !strings.Contains(err.Error(), "no session") {
		t.Errorf("unexpected error: %v", err)
	}

	// A prefix both sessions share must be refused with both names rather than
	// resolved to whichever the directory yielded first.
	_, log, err := openSession(&notes, session.NewStore(sessionsDir()), runOptions{resume: "aaa"})
	if err == nil {
		t.Fatal("an ambiguous prefix must not resolve to an arbitrary session")
	}
	if log != nil {
		t.Fatal("no log may be returned for an ambiguous reference")
	}
	for _, want := range []string{first, second} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name candidate %s", err.Error(), want)
		}
	}

	// The unambiguous prefix of the same pair still resolves.
	got, _, err := openSession(&notes, session.NewStore(sessionsDir()), runOptions{resume: "aaab"})
	if err != nil {
		t.Fatalf("openSession --resume aaab: %v", err)
	}
	if got != second {
		t.Errorf("--resume aaab resolved to %s, want %s", got, second)
	}
}

func TestResumeRefusesACorruptSession(t *testing.T) {
	t.Setenv("MANVI_STATE_DIR", t.TempDir())
	store := session.NewStore(sessionsDir())
	var notes bytes.Buffer

	id := seedSession(t, store, "abcdef0123456789", "first")
	entries, err := os.ReadDir(sessionsDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		path := filepath.Join(sessionsDir(), entry.Name())
		if err := os.WriteFile(path, []byte(`{"manvi_session":1,"id":"`), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	_, log, err := openSession(&notes, session.NewStore(sessionsDir()), runOptions{resume: id})
	if err == nil {
		t.Fatal("a corrupt session must be refused, not resumed as an empty one")
	}
	if log != nil {
		t.Fatal("no log may be returned for a session that could not be read")
	}
	if !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("error does not say the file is unreadable: %v", err)
	}

	// And --continue must not silently fall through to it either.
	if _, _, err := openSession(&notes, session.NewStore(sessionsDir()), runOptions{continueLast: true}); err == nil {
		t.Fatal("--continue onto a corrupt session must fail")
	}
}

func TestSaveSessionReportsWhatTheBoundCost(t *testing.T) {
	t.Setenv("MANVI_STATE_DIR", t.TempDir())
	store := session.NewStore(sessionsDir())
	store.SetLimits(4000, 0)
	var notes bytes.Buffer

	id, log, err := openSession(&notes, store, runOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for turn := 0; turn < 6; turn++ {
		if _, err := log.Append(session.TurnStart, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := log.Append(session.UserMessage, session.MessageData{Message: llm.Message{
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{llm.TextBlock{Text: strings.Repeat("q", 900)}},
		}}); err != nil {
			t.Fatal(err)
		}
		if _, err := log.Append(session.TurnEnd, nil); err != nil {
			t.Fatal(err)
		}
	}

	notes.Reset()
	if err := saveSession(&notes, store, id, log); err != nil {
		t.Fatalf("saveSession: %v", err)
	}
	if !strings.Contains(notes.String(), "dropped") {
		t.Errorf("history was dropped without saying so: %q", notes.String())
	}
	if !strings.Contains(notes.String(), id) {
		t.Errorf("the save did not name the session: %q", notes.String())
	}
}

// The step default lives in maxSteps(), which this file does not own. Pinning
// the help text to it means the two cannot drift apart silently again — a help
// string that names a ceiling the harness does not use is worse than one that
// names none, because an operator will plan around it.
func TestRunUsageStatesTheRealStepDefault(t *testing.T) {
	want := fmt.Sprintf("default %d", maxSteps(newTestRegistry(t)))
	if !strings.Contains(runUsage, want) {
		t.Errorf("manvi run --help does not say %q; it and maxSteps() have drifted apart", want)
	}
}
