package serve

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

// A block is written only if the file with the block in it is a file Manvi can
// read back to take the block out again: one bound, shared by the write and the
// restore. Until it was, a CLAUDE.md a few hundred bytes under the 4 MiB cap was
// accepted, grew past it, could not be read by the restore, and its record was
// then cleared by the next Prepare, leaving Manvi's bytes in the user's file.

const boundNonce = "0123456789abcdef0123456789abcdef"

// sizedClaude is a CLAUDE.md of exactly n bytes that ends in a newline, so the
// separator is the single newline.
func sizedClaude(n int) string { return strings.Repeat("x", n-1) + "\n" }

func TestMaterialiseRefusesABlockThatItsRestoreCouldNotRead(t *testing.T) {
	root := scanRepo(t, map[string]string{"CLAUDE.md": sizedClaude(maxInstructionFileBytes - 50)})
	_, err := materialiseHarness(root, "claude", specWithSections(t, houseRules))
	var typed *harnessMaterialiseError
	if !errors.As(err, &typed) {
		t.Fatalf("materialise = %v, want the typed refusal: the file would grow past what a restore reads", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(maxInstructionFileBytes)) {
		t.Fatalf("the refusal does not name the bound: %v", err)
	}
	if got := readFile(t, root, "CLAUDE.md"); got != sizedClaude(maxInstructionFileBytes-50) {
		t.Fatalf("a refused materialisation changed the file (%d bytes)", len(got))
	}
	if _, err := os.Stat(filepath.Join(root, harnessPendingFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused materialisation left a record")
	}
}

// The bound is on before+block, exactly: the file that ends at the bound is
// written and restored byte for byte, the one a byte over is refused.
func TestMaterialiseBoundIsExactlyTheRestoreLimit(t *testing.T) {
	prev := newHarnessNonce
	newHarnessNonce = func() (string, error) { return boundNonce, nil }
	t.Cleanup(func() { newHarnessNonce = prev })
	block, err := renderHarnessBlock(specWithSections(t, houseRules), boundNonce)
	if err != nil {
		t.Fatal(err)
	}
	fits := maxInstructionFileBytes - len(block) - 1 // the file's bytes, then one separator newline, then the block

	root := scanRepo(t, map[string]string{"CLAUDE.md": sizedClaude(fits)})
	m, err := materialiseHarness(root, "claude", specWithSections(t, houseRules))
	if err != nil {
		t.Fatalf("a file that ends at the bound was refused: %v", err)
	}
	if got := readFile(t, root, "CLAUDE.md"); len(got) != maxInstructionFileBytes {
		t.Fatalf("the file is %d bytes, the test meant it to end at the bound %d", len(got), maxInstructionFileBytes)
	}
	if err := m.Restore(); err != nil {
		t.Fatalf("the file that ends at the bound could not be restored: %v", err)
	}
	if got := readFile(t, root, "CLAUDE.md"); got != sizedClaude(fits) {
		t.Fatal("the restore of the file at the bound is not byte exact")
	}

	over := scanRepo(t, map[string]string{"CLAUDE.md": sizedClaude(fits + 1)})
	if _, err := materialiseHarness(over, "claude", specWithSections(t, houseRules)); err == nil {
		t.Fatal("a file one byte past the bound was written")
	}
	if got := readFile(t, over, "CLAUDE.md"); got != sizedClaude(fits+1) {
		t.Fatal("the refused file was changed")
	}
}

// A restore that cannot do its work for any reason but the agent having changed
// or removed the block keeps the record: the block may still be in the file, and
// the record is the only thing that can take it out.
func TestARestoreThatCannotReadTheFileKeepsItsRecord(t *testing.T) {
	cases := map[string]func(t *testing.T, root string){
		"grown past the bound by the agent": func(t *testing.T, root string) {
			writeRaw(t, root, "AGENTS.md", readFile(t, root, "AGENTS.md")+strings.Repeat("y", maxInstructionFileBytes))
		},
		"replaced by a link": func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "AGENTS.md")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("README.md", filepath.Join(root, "AGENTS.md")); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			root := scanRepo(t, map[string]string{"AGENTS.md": "rules\n", "README.md": "readme\n"})
			m, err := materialiseHarness(root, "codex", specWithSections(t, houseRules))
			if err != nil {
				t.Fatal(err)
			}
			live := readFile(t, root, "AGENTS.md")
			damage(t, root)
			if err := m.Restore(); err == nil {
				t.Fatal("a restore that could not read the file reported success")
			}
			if _, err := os.Stat(filepath.Join(root, harnessPendingFile)); err != nil {
				t.Fatalf("the restore lost the record: %v", err)
			}
			if e := (&ManagedRunner{jobs: map[string]*managedJob{}}).HarnessBusy(root); e == nil {
				t.Fatal("verification would diff a checkout that may carry the block")
			}
			note, err := recoverPendingHarness(root)
			if err == nil || note != "" {
				t.Fatalf("recovery = %q, %v, want the failure", note, err)
			}
			if _, err := os.Stat(filepath.Join(root, harnessPendingFile)); err != nil {
				t.Fatalf("the next Prepare's recovery cleared a record whose block it did not take out: %v", err)
			}
			// What was wrong is put right; the record, kept, still takes the block out.
			if err := os.Remove(filepath.Join(root, "AGENTS.md")); err != nil {
				t.Fatal(err)
			}
			writeRaw(t, root, "AGENTS.md", live)
			if note, err := recoverPendingHarness(root); err != nil || note == "" {
				t.Fatalf("the kept record did not restore once the file was readable: %q %v", note, err)
			}
			if got := readFile(t, root, "AGENTS.md"); got != "rules\n" {
				t.Fatalf("the block was not taken out exactly: %q", got)
			}
			if _, err := os.Stat(filepath.Join(root, harnessPendingFile)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("the record outlived the restore that used it")
			}
		})
	}
}

// What a verifier is told: the checkout is busy, and why.
func TestHarnessBusyNamesWhyABlockCouldNotBeTakenOut(t *testing.T) {
	root := scanRepo(t, map[string]string{"AGENTS.md": "rules\n"})
	if _, err := materialiseHarness(root, "codex", specWithSections(t, houseRules)); err != nil {
		t.Fatal(err)
	}
	writeRaw(t, root, "AGENTS.md", readFile(t, root, "AGENTS.md")+strings.Repeat("y", maxInstructionFileBytes))
	r := &ManagedRunner{jobs: map[string]*managedJob{}}
	for attempt := 1; attempt <= 2; attempt++ {
		job := &managedJob{done: make(chan struct{})}
		r.jobs["j"+strconv.Itoa(attempt)] = job
		if err := r.enterCheckout(job, managedRun{Cwd: root}); err == nil {
			t.Fatalf("attempt %d: a checkout whose block could not be taken out was admitted", attempt)
		}
	}
	e := r.HarnessBusy(root)
	if e == nil || e.Code != ErrHarnessBusy || !strings.Contains(e.Message, strconv.Itoa(maxInstructionFileBytes)+"-byte limit") {
		t.Fatalf("HarnessBusy = %+v, want busy naming the restore error", e)
	}
}

// The same, from the end of a run: finish cannot take the block out, so the run
// is recorded with the reason, the record stays, and the worktree stays busy.
func TestManagedFinishKeepsTheRecordWhenTheBlockCannotBeTakenOut(t *testing.T) {
	root := scanRepo(t, map[string]string{"CLAUDE.md": "project rules\n"})
	var atStart atomic.Value
	var starts atomic.Int32
	m := harnessLifecycle(t, root, "claude", &atStart, &starts)
	build := m.runner.factory
	m.runner.factory = func(ctx context.Context, o codingagent.Options) (ManagedSession, error) {
		// The agent's first act: the file grows past what Manvi can read back.
		writeRaw(t, root, "CLAUDE.md", readFile(t, root, "CLAUDE.md")+strings.Repeat("y", maxInstructionFileBytes))
		return build(ctx, o)
	}
	prepared, err := m.runner.Prepare(t.Context(), preparing("launch", `{"kind":"named","name":"registered"}`))
	if err != nil {
		t.Fatal(err)
	}
	activate(t, m, prepared)
	run, _ := m.finished()
	if !strings.Contains(run.Configuration, "harness_digest") {
		t.Fatalf("the run did not carry the harness: %s", run.Configuration)
	}
	if _, err := os.Stat(filepath.Join(root, harnessPendingFile)); err != nil {
		t.Fatalf("finish lost the record of a block it could not take out: %v", err)
	}
	if e := m.runner.HarnessBusy(root); e == nil || !bytes.Contains([]byte(e.Message), []byte("-byte limit")) {
		t.Fatalf("HarnessBusy = %+v, want busy naming why", e)
	}
}

// A record an earlier build wrote under the looser rule (each part bounded on
// its own) is still a record: valid() reads it, so recovery keeps it when it
// cannot act on it and never clears it as "not usable".
func TestARecordWrittenUnderTheLooserBoundIsStillReadable(t *testing.T) {
	block := harnessBeginMarker + " nonce=" + boundNonce + " name=x digest=y -->\n" + harnessEndMarker + " nonce=" + boundNonce + " name=x digest=y -->\n"
	p := pendingRestore{Version: 1, Nonce: boundNonce, File: "CLAUDE.md", Existed: true, Offset: maxInstructionFileBytes - 10, Appended: []byte("\n" + block)}
	if err := p.valid(); err != nil {
		t.Fatalf("a record that bounds its parts one at a time was refused: %v", err)
	}
	if err := p.fitsRestoreBound(); err == nil || !strings.Contains(err.Error(), strconv.Itoa(maxInstructionFileBytes)) {
		t.Fatalf("fitsRestoreBound = %v, want the refusal naming the bound", err)
	}
	p.Offset = 0
	if err := p.fitsRestoreBound(); err != nil {
		t.Fatalf("a block that fits was refused: %v", err)
	}
}
