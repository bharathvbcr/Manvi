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

// A block Manvi did not write in this run, whatever it looks like and whatever
// it carries, is the file's own content: it is not replaced, not stripped and
// not restored away. Each materialisation takes out its own bytes only.
func TestEachMaterialisationRemovesOnlyItsOwnBytes(t *testing.T) {
	root := scanRepo(t, map[string]string{"AGENTS.md": "rules\n"})
	m1, err := materialiseHarness(root, "codex", specWithSections(t, houseRules))
	if err != nil {
		t.Fatal(err)
	}
	newer := specWithSections(t, harness.Section{Name: "house-rules", Order: 70, Text: "Changed text.\n"})
	m2, err := materialiseHarness(root, "codex", newer)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, root, "AGENTS.md"); strings.Count(got, harnessBeginMarker) != 2 {
		t.Fatalf("the second block replaced the first or was not appended:\n%s", got)
	}
	if err := m1.Restore(); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, root, "AGENTS.md")
	if strings.Contains(got, "Prefer small diffs.") || !strings.Contains(got, "Changed text.") {
		t.Fatalf("restoring the first run removed the second's block or kept its own:\n%s", got)
	}
	if err := m2.Restore(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, root, "AGENTS.md"); got != "rules\n" {
		t.Fatalf("the file is not as it was: %q", got)
	}
}

func fixedNonce(t *testing.T, nonces ...string) {
	t.Helper()
	saved := newHarnessNonce
	t.Cleanup(func() { newHarnessNonce = saved })
	i := 0
	newHarnessNonce = func() (string, error) {
		n := nonces[min(i, len(nonces)-1)]
		i++
		return n, nil
	}
}

func TestEveryBlockCarriesAFreshRandomNonce(t *testing.T) {
	root := scanRepo(t, map[string]string{"AGENTS.md": "rules\n"})
	seen := map[string]bool{}
	for range 4 {
		m, err := materialiseHarness(root, "codex", specWithSections(t, houseRules))
		if err != nil {
			t.Fatal(err)
		}
		n := m.pending.Nonce
		if !nonceForm.MatchString(n) || seen[n] {
			t.Fatalf("nonce %q is not a fresh 128-bit hex value", n)
		}
		seen[n] = true
		line := harnessBeginMarker + " nonce=" + n + " "
		end := harnessEndMarker + " nonce=" + n + " "
		if got := readFile(t, root, "AGENTS.md"); !strings.Contains(got, "\n"+line) || !strings.Contains(got, "\n"+end) {
			t.Fatalf("the BEGIN and END lines do not both carry the nonce:\n%s", got)
		}
		if err := m.Restore(); err != nil {
			t.Fatal(err)
		}
	}
}

// Repo content that quotes the markers has no nonce Manvi issued: it is never
// matched, so it is never removed and never refuses a run.
func TestUnnoncedLookalikesAreOrdinaryContent(t *testing.T) {
	original := "# Rules\n" + harnessBeginMarker + " example -->\nimportant repo rule\n" + harnessEndMarker + " example -->\n" +
		harnessBeginMarker + "FOO is another comment -->\nno end here\n" +
		harnessBeginMarker + " nonce=" + strings.Repeat("0", 32) + " name=old digest=x -->\nresidue of nobody's run\n"
	root := scanRepo(t, map[string]string{"AGENTS.md": original})
	m, err := materialiseHarness(root, "codex", specWithSections(t, houseRules))
	if err != nil {
		t.Fatalf("lookalikes refused a run: %v", err)
	}
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, root, "AGENTS.md"); got != original {
		t.Fatalf("lookalike content was changed:\nwant %q\ngot  %q", original, got)
	}
}

// What the agent changed inside the block is not Manvi's bytes: restore refuses,
// typed, leaves the file as it is and keeps the record, and the next Prepare
// reports it once and is not wedged by it.
func TestAnEditedBlockIsRefusedNotGuessedAt(t *testing.T) {
	for name, edit := range map[string]func(live string) string{
		"END deleted": func(l string) string {
			return strings.Replace(l, harnessEndMarker, "<!-- gone", 1) + "agent note after\n"
		},
		"text edited":       func(l string) string { return strings.Replace(l, "Prefer small diffs.", "Prefer huge diffs.", 1) },
		"BEGIN line edited": func(l string) string { return strings.Replace(l, "name=team", "name=other", 1) },
	} {
		t.Run(name, func(t *testing.T) {
			root := scanRepo(t, map[string]string{"AGENTS.md": "rules\n"})
			m, err := materialiseHarness(root, "codex", specWithSections(t, houseRules))
			if err != nil {
				t.Fatal(err)
			}
			edited := edit(readFile(t, root, "AGENTS.md"))
			writeRaw(t, root, "AGENTS.md", edited)
			rerr := m.Restore()
			var typed *harnessMaterialiseError
			if !errors.As(rerr, &typed) || !strings.Contains(rerr.Error(), "modified harness block") {
				t.Fatalf("restore = %v, want the typed modified-block refusal", rerr)
			}
			if got := readFile(t, root, "AGENTS.md"); got != edited {
				t.Fatalf("restore touched a file it refused:\n%s", got)
			}
			if _, err := os.Stat(filepath.Join(root, harnessPendingFile)); err != nil {
				t.Fatalf("the record of the modified block was not kept: %v", err)
			}
			// The next attempt on the checkout reports it once, typed, and clears it.
			note, err := recoverPendingHarness(root)
			if !errors.As(err, &typed) || note != "" {
				t.Fatalf("recovery = %q, %v, want the typed refusal", note, err)
			}
			if _, err := os.Stat(filepath.Join(root, harnessPendingFile)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("the refusal was not cleared, so it would be made on every run")
			}
			if _, err := materialiseHarness(root, "codex", specWithSections(t, houseRules)); err != nil {
				t.Fatalf("the modified block wedged the next materialisation: %v", err)
			}
			if note, err := recoverPendingHarness(root); err != nil || note == "" {
				t.Fatalf("a clean record was not applied: %q %v", note, err)
			}
		})
	}
}

// An agent that copies the block leaves several spans byte-identical to what
// Manvi wrote, nonce included: all of them are Manvi's bytes and all go.
func TestACopiedBlockIsRemovedWithTheOriginal(t *testing.T) {
	root := scanRepo(t, map[string]string{"AGENTS.md": "rules\n"})
	m, err := materialiseHarness(root, "codex", specWithSections(t, houseRules))
	if err != nil {
		t.Fatal(err)
	}
	live := readFile(t, root, "AGENTS.md")
	writeRaw(t, root, "AGENTS.md", live+"\n"+live[strings.Index(live, harnessBeginMarker):])
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, root, "AGENTS.md"); got != "rules\n" {
		t.Fatalf("a copy of the block survived or the file changed: %q", got)
	}
}

// Restore is exact on every shape of file because it removes a recorded span and
// never strips a newline: no trailing newline, CRLF, a BOM, bytes that are not
// UTF-8, an empty file, with the agent's own edit before or after the block.
func TestRestoreIsByteExact(t *testing.T) {
	bodies := map[string]string{
		"no trailing newline": "rules",
		"crlf":                "a\r\nb\r\n",
		"crlf no final":       "a\r\nb",
		"bom":                 "\xef\xbb\xbfrules\n",
		"non-utf8":            "rules \xff\xfe\n",
		"empty":               "",
		"blank lines":         "rules\n\n\n",
	}
	for name, body := range bodies {
		for edit, shape := range map[string]func(live, body string) (string, string){
			"untouched":            func(live, body string) (string, string) { return live, body },
			"agent edit before":    func(live, body string) (string, string) { return "agent\r\n" + live, "agent\r\n" + body },
			"agent edit after":     func(live, body string) (string, string) { return live + "tail", body + "tail" },
			"agent edit both ends": func(live, body string) (string, string) { return "x" + live + "y", "x" + body + "y" },
		} {
			t.Run(name+"/"+edit, func(t *testing.T) {
				root := scanRepo(t, map[string]string{"AGENTS.md": body, "keep": "k"})
				m, err := materialiseHarness(root, "codex", specWithSections(t, houseRules))
				if err != nil {
					t.Fatal(err)
				}
				live, want := shape(readFile(t, root, "AGENTS.md"), body)
				writeRaw(t, root, "AGENTS.md", live)
				if err := m.Restore(); err != nil {
					t.Fatal(err)
				}
				got, rerr := os.ReadFile(filepath.Join(root, "AGENTS.md")) // #nosec G304 -- a file under this test's own t.TempDir
				if edit == "untouched" && body == "" {
					// A file that existed empty is restored empty, not removed.
					if rerr != nil || string(got) != "" {
						t.Fatalf("an empty file that existed was not restored empty: %q %v", got, rerr)
					}
					return
				}
				if rerr != nil || string(got) != want {
					t.Fatalf("not byte exact: want %q got %q (%v)", want, got, rerr)
				}
			})
		}
	}
}

// A record that could be forged into an instruction to delete is not acted on:
// it can name two files and one nonce'd block, nothing else.
func TestAForgedRecordCannotNameAnythingButABlock(t *testing.T) {
	good := func() pendingRestore {
		nonce := strings.Repeat("a", 32)
		tag := "nonce=" + nonce + " name=x digest=y -->\n"
		return pendingRestore{Version: 1, Nonce: nonce, File: "AGENTS.md", Offset: 0,
			Appended: []byte(harnessBeginMarker + " " + tag + "text\n" + harnessEndMarker + " " + tag)}
	}
	if err := good().valid(); err != nil {
		t.Fatalf("a well-formed record was refused: %v", err)
	}
	for name, mutate := range map[string]func(*pendingRestore){
		"path":          func(p *pendingRestore) { p.File = ".env" },
		"traversal":     func(p *pendingRestore) { p.File = "../AGENTS.md" },
		"short nonce":   func(p *pendingRestore) { p.Nonce = "abc" },
		"other nonce":   func(p *pendingRestore) { p.Nonce = strings.Repeat("b", 32) },
		"arbitrary":     func(p *pendingRestore) { p.Appended = []byte("the whole file") },
		"empty":         func(p *pendingRestore) { p.Appended = nil },
		"version":       func(p *pendingRestore) { p.Version = 2 },
		"offset":        func(p *pendingRestore) { p.Offset = -1 },
		"leading text":  func(p *pendingRestore) { p.Appended = append([]byte("secret\n"), p.Appended...) },
		"trailing text": func(p *pendingRestore) { p.Appended = append(p.Appended, "more\n"...) },
	} {
		p := good()
		mutate(&p)
		if err := p.valid(); err == nil {
			t.Errorf("%s: a record that names more than one block was accepted", name)
		}
	}
}

func TestRecoveryAppliesARecordOfADeadRunAndReportsIt(t *testing.T) {
	root := scanRepo(t, map[string]string{"AGENTS.md": "rules"})
	if _, err := materialiseHarness(root, "codex", specWithSections(t, houseRules)); err != nil {
		t.Fatal(err)
	}
	// Manvi dies here: no Restore. The next attempt on the checkout recovers.
	note, err := recoverPendingHarness(root)
	if err != nil || !strings.Contains(note, "AGENTS.md") {
		t.Fatalf("recovery = %q, %v", note, err)
	}
	if got := readFile(t, root, "AGENTS.md"); got != "rules" {
		t.Fatalf("the dead run's block was not taken out exactly: %q", got)
	}
	if note, err := recoverPendingHarness(root); err != nil || note != "" {
		t.Fatalf("a second recovery found something: %q %v", note, err)
	}
	if status := gitIn(t, root, "status", "--porcelain"); status != "" {
		t.Fatalf("recovery left the checkout dirty: %q", status)
	}
}

func TestRecoveryOfAnUnreadableRecordIsRefusedOnceAndCleared(t *testing.T) {
	root := scanRepo(t, map[string]string{"AGENTS.md": "rules\n"})
	if err := os.MkdirAll(filepath.Join(root, ".devcouncil"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeRaw(t, root, harnessPendingFile, `{"version":1,"unknown":true}`)
	if _, err := recoverPendingHarness(root); err == nil {
		t.Fatal("an unreadable record was accepted")
	}
	if _, err := os.Stat(filepath.Join(root, harnessPendingFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the unreadable record was kept, so every run would be refused for it")
	}
	if got := readFile(t, root, "AGENTS.md"); got != "rules\n" {
		t.Fatalf("a bad record changed the instruction file: %q", got)
	}
}

// Materialising writes the record before the block, so a failed block write
// leaves neither.
func TestAFailedBlockWriteLeavesNoRecord(t *testing.T) {
	root := scanRepo(t, map[string]string{"README.md": "x"})
	outside := filepath.Join(t.TempDir(), "elsewhere.md")
	writeRaw(t, filepath.Dir(outside), filepath.Base(outside), "x")
	if err := os.Symlink(outside, filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := materialiseHarness(root, "claude", specWithSections(t, houseRules)); err == nil {
		t.Fatal("a link was written through")
	}
	if _, err := os.Stat(filepath.Join(root, harnessPendingFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a record of a block that was never written was left behind")
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
	// The nonce is injectable, so a test can name the lines a block will carry.
	fixedNonce(t, strings.Repeat("c", 32))
	m, err := materialiseHarness(root, "codex", specWithSections(t, houseRules))
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, root, "AGENTS.md"); !strings.Contains(got, "nonce="+strings.Repeat("c", 32)+" ") {
		t.Errorf("the injected nonce is not in the block:\n%s", got)
	}
	if err := m.Restore(); err != nil {
		t.Fatal(err)
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
		err := harnessWriter{root: root}.write(rel, dc.OpCreate, []byte("x"))
		var typed *harnessMaterialiseError
		if !errors.As(err, &typed) {
			t.Errorf("%s: write error = %v, want the gate's typed refusal", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".env")); !errors.Is(err, os.ErrNotExist) {
		t.Error(".env was written")
	}
	if err := (harnessWriter{root: root}).write("AGENTS.md", dc.OpCreate, []byte("ok\n")); err != nil {
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
