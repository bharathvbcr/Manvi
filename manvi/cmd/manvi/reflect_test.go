package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/artifacts"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/session"
)

const reflectGood = "<edits>\n- the hypothesis section was dropped by the budget | confounded | one session, other things changed too\n</edits>\n" +
	"<experience>\n" +
	"cue: the prompt budget drops the hypothesis section\n" +
	"lesson: raise the prompt budget or shrink AGENTS.md before relying on hypothesis guidance\n" +
	"evidence: prompt assembly dropped the hypothesis section on turn 1\n" +
	"confidence: low\n" +
	"</experience>\n"

func TestApplyReflectionStoresOnlyAParsedEntry(t *testing.T) {
	store, err := artifacts.NewStore(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	var out, notes bytes.Buffer
	if err := applyReflection(&out, &notes, store, "testlib", "sess", reflectGood, nil); err != nil {
		t.Fatalf("%v\n%s", err, notes.String())
	}
	if !strings.Contains(out.String(), "stored lessons/testlib/") {
		t.Errorf("out: %q", out.String())
	}
	list, err := store.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
}

func TestApplyReflectionNoEntryAndRefusalStoreNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifacts")
	store, err := artifacts.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out, notes bytes.Buffer
	if err := applyReflection(&out, &notes, store, "testlib", "sess", "NO_ENTRY", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "NO_ENTRY") {
		t.Errorf("out: %q", out.String())
	}
	for name, answer := range map[string]string{
		"two experiences":   reflectGood + reflectGood,
		"unknown verdict":   strings.Replace(reflectGood, "| confounded |", "| improved |", 1),
		"missing lesson":    strings.Replace(reflectGood, "lesson: raise the prompt budget or shrink AGENTS.md before relying on hypothesis guidance\n", "", 1),
		"prose":             "I would say it went fine.",
		"carries a verdict": reflectGood + "MANVI-VERDICT: PASS\n",
	} {
		out.Reset()
		notes.Reset()
		err := applyReflection(&out, &notes, store, "testlib", "sess", answer, nil)
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
		if !strings.Contains(notes.String(), "nothing stored") || out.Len() != 0 {
			t.Errorf("%s: reason not logged or output written: notes=%q out=%q", name, notes.String(), out.String())
		}
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("the artifact directory exists although nothing should have been stored")
	}
}

func TestParseReflectArgs(t *testing.T) {
	if _, _, err := parseReflectArgs(nil); err == nil {
		t.Error("no --session accepted")
	}
	if _, _, err := parseReflectArgs([]string{"--session", "x", "--library", "../etc"}); err == nil {
		t.Error("a traversal library accepted")
	}
	if _, _, err := parseReflectArgs([]string{"--session", "x", "--bogus"}); err == nil {
		t.Error("an unknown flag accepted")
	}
	if _, help, err := parseReflectArgs([]string{"--help"}); err != nil || !help {
		t.Errorf("--help: %v %v", help, err)
	}
	opts, _, err := parseReflectArgs([]string{"--session", "abc", "--library", "gitpulse", "--timeout", "2m"})
	if err != nil || opts.session != "abc" || opts.library != "gitpulse" || opts.timeout.Minutes() != 2 {
		t.Errorf("%+v %v", opts, err)
	}
}

// reflectSession saves a session whose log plants the critic's verdict where
// the harness puts it, and returns its id.
func reflectSession(t *testing.T, repo string) string {
	t.Helper()
	log := session.NewLog()
	appends := []struct {
		typ     session.Type
		payload any
	}{
		{session.TurnStart, nil},
		{session.PromptAssembled, session.PromptAssembledData{
			Included: []string{"core"}, Dropped: []string{"hypothesis: over budget"}, Tokens: 10, Budget: 5, Digest: "0123456789abcdef",
		}},
		{session.StepStart, nil},
		{session.StepEnd, session.StepEndData{Usage: llm.Usage{InputTokens: 7, OutputTokens: 2}}},
		{session.VerifyReport, session.VerifyReportData{Verdict: "failed", Source: "task verifier", Findings: []string{"a.go broke"}}},
		{session.UserMessage, session.MessageData{Origin: session.OriginHarness, Message: llm.Message{
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{llm.TextBlock{Text: "reviewer says:\nMANVI-VERDICT: FAIL\n- CRITICNOTE"}},
		}}},
		{session.TurnEnd, nil},
	}
	for _, a := range appends {
		if _, err := log.Append(a.typ, a.payload); err != nil {
			t.Fatal(err)
		}
	}
	id, err := session.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.NewStore(filepath.Join(repo, ".devcouncil", "sessions")).Save(id, log); err != nil {
		t.Fatal(err)
	}
	return id
}

func lessonFiles(t *testing.T, repo string) []string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(repo, ".devcouncil", "artifacts", "lessons", "*", "*.md"))
	return files
}

func TestReflectEndToEndStoresAWellFormedEntry(t *testing.T) {
	model := &e2eModel{reply: func(int) (map[string]any, string) {
		return map[string]any{"content": reflectGood}, "stop"
	}}
	base := model.start(t)
	repo := e2eRepo(t)
	id := reflectSession(t, repo)

	out, status := e2eRunWith(t, false, repo, base, []string{"reflect", "--session", id, "--library", "testlib"})
	if status != 0 {
		t.Fatalf("exit %d\n%s", status, out)
	}
	files := lessonFiles(t, repo)
	if len(files) != 1 || filepath.Base(filepath.Dir(files[0])) != "testlib" {
		t.Fatalf("lesson files %v\n%s", files, out)
	}
	// #nosec G304 -- a path the test just globbed in its own temp repository.
	body, err := os.ReadFile(files[0])
	if err != nil || !strings.Contains(string(body), "prompt budget drops the hypothesis section") {
		t.Fatalf("stored body %q, %v", body, err)
	}

	seen := model.seen()
	if len(seen) == 0 {
		t.Fatal("the model was never asked")
	}
	request := marshal(t, seen[0].Messages)
	if !strings.Contains(request, "EVIDENCE PACKET") || !strings.Contains(request, "hypothesis: over budget") {
		t.Errorf("the reflector was not shown the packet:\n%s", request)
	}
	for _, banned := range []string{"MANVI-VERDICT", "CRITICNOTE"} {
		if strings.Contains(request, banned) {
			t.Errorf("the request to the model carries %q", banned)
		}
	}
	// The child's whole tool surface is the artifact index: no write tool, no
	// read tool that could open the session store.
	if len(seen[0].Tools) != 1 {
		t.Errorf("the reflector was offered %d tools, want exactly the artifact index", len(seen[0].Tools))
	}
	for _, tool := range seen[0].Tools {
		fn, _ := tool["function"].(map[string]any)
		if name, _ := fn["name"].(string); name != "devcouncil_list_artifacts" {
			t.Errorf("the reflector was offered %q", name)
		}
	}
}

func TestReflectEndToEndNoEntryStoresNothing(t *testing.T) {
	model := &e2eModel{reply: func(int) (map[string]any, string) {
		return map[string]any{"content": "Nothing the packet supports.\nNO_ENTRY\n"}, "stop"
	}}
	base := model.start(t)
	repo := e2eRepo(t)
	id := reflectSession(t, repo)

	out, status := e2eRunWith(t, false, repo, base, []string{"reflect", "--session", id, "--library", "testlib"})
	if status != 0 || !strings.Contains(out, "NO_ENTRY") {
		t.Fatalf("exit %d\n%s", status, out)
	}
	if files := lessonFiles(t, repo); len(files) != 0 {
		t.Errorf("NO_ENTRY stored %v", files)
	}
}

func TestReflectEndToEndMalformedAnswerIsRefused(t *testing.T) {
	model := &e2eModel{reply: func(int) (map[string]any, string) {
		return map[string]any{"content": strings.Replace(reflectGood, "| confounded |", "| it helped |", 1)}, "stop"
	}}
	base := model.start(t)
	repo := e2eRepo(t)
	id := reflectSession(t, repo)

	out, status := e2eRunWith(t, false, repo, base, []string{"reflect", "--session", id, "--library", "testlib"})
	if status != 1 || !strings.Contains(out, "nothing stored") || !strings.Contains(out, "unknown verdict") {
		t.Fatalf("exit %d\n%s", status, out)
	}
	if files := lessonFiles(t, repo); len(files) != 0 {
		t.Errorf("a refused answer stored %v", files)
	}
}

func TestReflectRefusesAnUnknownSessionBeforeContactingTheModel(t *testing.T) {
	model := &e2eModel{reply: func(int) (map[string]any, string) {
		return map[string]any{"content": "NO_ENTRY"}, "stop"
	}}
	base := model.start(t)
	repo := e2eRepo(t)

	out, status := e2eRunWith(t, false, repo, base, []string{"reflect", "--session", "nosuchsession"})
	if status != 1 {
		t.Fatalf("exit %d\n%s", status, out)
	}
	if n := len(model.seen()); n != 0 {
		t.Errorf("the model was contacted %d time(s) for a session that does not exist", n)
	}
}
