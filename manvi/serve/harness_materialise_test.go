package serve

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dc "github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc"
	"github.com/bharathvbcr/Manvi/manvi/harness"
)

func specWithSections(t *testing.T, sections ...harness.Section) harness.Spec {
	t.Helper()
	return harness.Spec{Name: "team", Sections: sections}
}

var houseRules = harness.Section{Name: "house-rules", Order: 70, Text: "Prefer small diffs.\n"}

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel)) // #nosec G304 -- a file under this test's own t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInstructionFileIsTheOneEachCLIReads(t *testing.T) {
	for provider, want := range map[string]string{"claude": "CLAUDE.md", "codex": "AGENTS.md"} {
		if got, ok := harnessInstructionFile(provider); !ok || got != want {
			t.Errorf("%s: instruction file = %q, %v, want %q", provider, got, ok, want)
		}
	}
	if _, ok := harnessInstructionFile("gemini"); ok {
		t.Error("a provider with no adapter was given an instruction file")
	}
	if _, err := materialiseHarness(t.TempDir(), "gemini", specWithSections(t, houseRules)); err == nil {
		t.Error("materialising for a provider with no instruction file was not refused")
	}
}

// The block is the only thing written: everything before it is the file as it
// was, byte for byte, and the file is the provider's own.
func TestMaterialiseWritesOnlyTheMarkedTailBlock(t *testing.T) {
	original := "# Project rules\n\nUse tabs.\n"
	root := scanRepo(t, map[string]string{"AGENTS.md": original, "CLAUDE.md": "claude's own\n"})
	m, err := materialiseHarness(root, "codex", specWithSections(t, houseRules))
	if err != nil {
		t.Fatal(err)
	}
	got := readFile(t, root, "AGENTS.md")
	if !strings.HasPrefix(got, original+"\n"+harnessBeginMarker) {
		t.Fatalf("the block is not a tail after the original content:\n%s", got)
	}
	spec := specWithSections(t, houseRules)
	for _, want := range []string{"name=team", "digest=" + spec.Digest(), "## house-rules", "Prefer small diffs.", harnessEndMarker} {
		if !strings.Contains(got, want) {
			t.Errorf("the block lacks %q:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, "-->\n") {
		t.Errorf("the block does not end on its end marker line:\n%s", got)
	}
	if other := readFile(t, root, "CLAUDE.md"); other != "claude's own\n" {
		t.Errorf("the other provider's file was touched: %q", other)
	}
	if m.File != "AGENTS.md" {
		t.Errorf("materialisation names %q", m.File)
	}
}

func TestMaterialiseIsIdempotentAndReplacesAStaleBlock(t *testing.T) {
	root := scanRepo(t, map[string]string{"AGENTS.md": "rules\n"})
	if _, err := materialiseHarness(root, "codex", specWithSections(t, houseRules)); err != nil {
		t.Fatal(err)
	}
	// A second run finds the first's block: the residue of a run that never
	// restored. It is replaced, not stacked, and the new block is the spec now.
	newer := specWithSections(t, harness.Section{Name: "house-rules", Order: 70, Text: "Changed text.\n"})
	m, err := materialiseHarness(root, "codex", newer)
	if err != nil {
		t.Fatal(err)
	}
	got := readFile(t, root, "AGENTS.md")
	if strings.Count(got, harnessBeginMarker) != 1 || strings.Contains(got, "Prefer small diffs.") || !strings.Contains(got, "Changed text.") {
		t.Fatalf("a stale block was stacked or kept:\n%s", got)
	}
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, root, "AGENTS.md"); got != "rules\n" {
		t.Fatalf("restore did not return the file to its content outside any block: %q", got)
	}
}

func TestMaterialiseCreatesTheFileAndRestoreRemovesIt(t *testing.T) {
	root := scanRepo(t, map[string]string{"README.md": "x"})
	m, err := materialiseHarness(root, "claude", specWithSections(t, houseRules))
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, root, "CLAUDE.md"); !strings.HasPrefix(got, harnessBeginMarker) {
		t.Fatalf("a created file is not just the block:\n%s", got)
	}
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "CLAUDE.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file Manvi created was left behind: %v", err)
	}
}

// After the run the checkout's own diff is the agent's: a tracked instruction
// file is back to its committed bytes, so a verifier diffing the work tree
// never sees the block and cannot blame it on the agent.
func TestRestoreLeavesNoDiffInTheCheckout(t *testing.T) {
	root := scanRepo(t, map[string]string{"AGENTS.md": "rules without a trailing newline"})
	m, err := materialiseHarness(root, "codex", specWithSections(t, houseRules))
	if err != nil {
		t.Fatal(err)
	}
	if during := gitIn(t, root, "status", "--porcelain"); !strings.Contains(during, "AGENTS.md") {
		t.Fatalf("the fixture is wrong: the block does not show in the diff while the run is live: %q", during)
	}
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	if after := gitIn(t, root, "status", "--porcelain"); after != "" {
		t.Fatalf("the harness block outlived the run in the checkout's diff:\n%s", after)
	}
}

// What the agent changed is the agent's: only the block is cut, never the rest
// of the file, and a block the agent removed is not an error.
func TestRestoreKeepsTheAgentsOwnEditsOutsideTheBlock(t *testing.T) {
	root := scanRepo(t, map[string]string{"AGENTS.md": "rules\n"})
	m, err := materialiseHarness(root, "codex", specWithSections(t, houseRules))
	if err != nil {
		t.Fatal(err)
	}
	live := readFile(t, root, "AGENTS.md")
	edited := "agent added a rule\n" + live + "agent appended after the block\n"
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, root, "AGENTS.md")
	if strings.Contains(got, harnessBeginMarker) || strings.Contains(got, harnessEndMarker) {
		t.Fatalf("the block survived the restore:\n%s", got)
	}
	if !strings.Contains(got, "agent added a rule") || !strings.Contains(got, "agent appended after the block") || !strings.Contains(got, "rules\n") {
		t.Fatalf("the agent's own edits were lost:\n%s", got)
	}

	// The agent deleting the block itself, or the whole file, is nothing to undo.
	m2, err := materialiseHarness(root, "codex", specWithSections(t, houseRules))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if err := m2.Restore(); err != nil {
		t.Fatalf("restoring after the agent deleted the file: %v", err)
	}
}

func TestMaterialiseRefusesWhatWouldMakeTheBlockAmbiguous(t *testing.T) {
	root := scanRepo(t, map[string]string{"AGENTS.md": "rules\n"})
	for name, text := range map[string]string{
		"begin marker in a section": harnessBeginMarker + " name=x -->",
		"end marker in a section":   "text\n" + harnessEndMarker + " -->",
	} {
		_, err := materialiseHarness(root, "codex", specWithSections(t, harness.Section{Name: "s", Order: 70, Text: text}))
		if err == nil {
			t.Errorf("%s was written", name)
		}
	}
	if got := readFile(t, root, "AGENTS.md"); got != "rules\n" {
		t.Errorf("a refused materialisation changed the file: %q", got)
	}
	// A file whose own markers cannot be paired is not guessed about.
	bad := "x\n" + harnessBeginMarker + " name=old -->\nno end\n"
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := materialiseHarness(root, "codex", specWithSections(t, houseRules)); err == nil {
		t.Error("a file with an unmatched begin marker was written to")
	}
	if got := readFile(t, root, "AGENTS.md"); got != bad {
		t.Errorf("the unmatched file was changed: %q", got)
	}
}

func TestMaterialiseWritesNothingForASpecWithNoSections(t *testing.T) {
	root := scanRepo(t, map[string]string{"README.md": "x"})
	m, err := materialiseHarness(root, "codex", harness.Spec{Name: "roles-only"})
	if err != nil || m != nil {
		t.Fatalf("materialise = %v, %v, want nothing to write", m, err)
	}
	if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file was created for a spec with no text: %v", err)
	}
	if err := m.Restore(); err != nil {
		t.Fatalf("restoring nothing failed: %v", err)
	}
}

// Never through a link: a CLAUDE.md that is a symbolic link out of the checkout
// is refused, the target untouched, and nothing is left half-written.
func TestMaterialiseRefusesALinkedInstructionFile(t *testing.T) {
	root := scanRepo(t, map[string]string{"README.md": "x"})
	outside := filepath.Join(t.TempDir(), "elsewhere.md")
	if err := os.WriteFile(outside, []byte("not yours\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	_, err := materialiseHarness(root, "claude", specWithSections(t, houseRules))
	if err == nil {
		t.Fatal("a write through a symbolic link was accepted")
	}
	var typed *harnessMaterialiseError
	if !errors.As(err, &typed) {
		t.Fatalf("the failure is not the typed materialise error: %T %v", err, err)
	}
	// #nosec G304 -- a file under this test's own t.TempDir.
	if got, _ := os.ReadFile(outside); string(got) != "not yours\n" {
		t.Fatalf("the link's target was written: %q", got)
	}
}

// The write is judged by the file gate as Manvi's own, with hard rules on: a
// path the hard rules protect is refused even though internal skips the
// restricted rung, and an ordinary instruction file is allowed.
func TestHarnessWriterIsJudgedByTheFileGate(t *testing.T) {
	root := scanRepo(t, map[string]string{"README.md": "x"})
	for _, rel := range []string{".env", "../escape.md", "secrets/key.md"} {
		w := harnessWriter{root: root, rel: rel}
		err := w.write(dc.OpCreate, []byte("x"))
		var typed *harnessMaterialiseError
		if !errors.As(err, &typed) {
			t.Errorf("%s: write error = %v, want the gate's typed refusal", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".env")); !errors.Is(err, os.ErrNotExist) {
		t.Error(".env was written")
	}
	if err := (harnessWriter{root: root, rel: "AGENTS.md"}).write(dc.OpCreate, []byte("ok\n")); err != nil {
		t.Errorf("an ordinary instruction file was refused: %v", err)
	}
}

func TestOnlyRepoAndNamedHarnessesAreAcceptedOverTheManagedPlane(t *testing.T) {
	r := &ManagedRunner{}
	if err := r.SetHarness(newTestResolver(t)); err != nil {
		t.Fatal(err)
	}
	ok, e := r.harnessRef([]byte(`{"kind":"named","name":"registered"}`))
	if e != nil || ok != (ManagedHarness{Kind: "named", Name: "registered"}) {
		t.Fatalf("a named harness was refused: %v", e)
	}
	if none, e := r.harnessRef(nil); e != nil || none != (ManagedHarness{}) {
		t.Fatalf("absence is no harness: %v %v", none, e)
	}
	if none, e := r.harnessRef([]byte("null")); e != nil || none != (ManagedHarness{}) {
		t.Fatalf("null is no harness: %v %v", none, e)
	}
	for name, raw := range map[string]string{
		"inline":         `{"kind":"inline","name":"x"}`,
		"unknown kind":   `{"kind":"remote","name":"x"}`,
		"bad name":       `{"kind":"repo","name":"../x"}`,
		"unknown member": `{"kind":"repo","name":"x","write_exceptions":["**"]}`,
		"not an object":  `"repo"`,
	} {
		if _, e := r.harnessRef([]byte(raw)); e == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, e := (&ManagedRunner{}).harnessRef([]byte(`{"kind":"repo","name":"x"}`)); e == nil || e.Code != ErrHarnessRefused {
		t.Errorf("a harness named on a build that serves none must be refused, not ignored: %v", e)
	}
}
