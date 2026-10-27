package serve

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A restore whose read of the instruction file is refused keeps its record, and
// says what an operator needs: that it was a read, which block and where, and a
// remedy that works for that cause. The same facts come from the record on disk
// after a restart, when nothing is remembered.

func damageByLink(t *testing.T, root string) {
	t.Helper()
	if err := os.Remove(filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("README.md", filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
}

func TestARefusedRestoreReadSaysWhatHappenedAndHowToRecover(t *testing.T) {
	cases := []struct {
		name   string
		damage func(t *testing.T, root string)
		cause  string // what the cause-specific remedy says
	}{
		{"symbolic link", damageByLink, "is a symbolic link"},
		{"hard link", func(t *testing.T, root string) {
			if err := os.Link(filepath.Join(root, "CLAUDE.md"), filepath.Join(root, "second-name.md")); err != nil {
				t.Skipf("no hard links here: %v", err)
			}
		}, "has more than one name"},
		{"grown past the bound", func(t *testing.T, root string) {
			writeRaw(t, root, "CLAUDE.md", readFile(t, root, "CLAUDE.md")+strings.Repeat("y", maxInstructionFileBytes))
		}, "larger than the " + strconv.Itoa(maxInstructionFileBytes) + "-byte limit"},
		{"a directory", func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "CLAUDE.md")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "CLAUDE.md"), 0o750); err != nil {
				t.Fatal(err)
			}
		}, "regular file with one name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := scanRepo(t, map[string]string{"CLAUDE.md": "rules\n", "README.md": "readme\n"})
			m, err := materialiseHarness(root, "claude", specWithSections(t, houseRules))
			if err != nil {
				t.Fatal(err)
			}
			c.damage(t, root)
			rerr := m.Restore()
			if rerr == nil {
				t.Fatal("a restore that could not read the file succeeded")
			}
			text := rerr.Error()
			for _, want := range []string{
				ErrHarnessMaterialise, "CLAUDE.md", "This was a read", "nothing was written",
				"nonce=" + m.pending.Nonce, "byte offset " + strconv.Itoa(m.pending.Offset),
				harnessPendingFile, "by hand", c.cause,
			} {
				if !strings.Contains(text, want) {
					t.Errorf("the refusal lacks %q:\n%s", want, text)
				}
			}
			if strings.Contains(text, "write through") || strings.Contains(text, "writing through") {
				t.Errorf("a refused read is described as a write:\n%s", text)
			}

			// After a restart nothing is in memory: the refusal comes from the record
			// and the file, and is the same refusal.
			busy := (&ManagedRunner{jobs: map[string]*managedJob{}}).HarnessBusy(root)
			if busy == nil || busy.Code != ErrHarnessBusy {
				t.Fatalf("HarnessBusy = %v, want busy", busy)
			}
			for _, want := range []string{"taking it out is refused", "CLAUDE.md", "nonce=" + m.pending.Nonce, harnessPendingFile, c.cause} {
				if !strings.Contains(busy.Message, want) {
					t.Errorf("the busy refusal lacks %q:\n%s", want, busy.Message)
				}
			}
			if strings.Contains(busy.Message, "the next work.runs.managed.prepare on this checkout takes it out and reports it") {
				t.Errorf("the busy refusal promises a recovery that would be refused:\n%s", busy.Message)
			}
		})
	}
}

// The remedies named work: the link replaced by a copy lets the next recovery
// take the block out; and the manual one, the block's lines out and the record
// deleted, leaves a checkout nothing refuses.
func TestTheNamedRemediesWork(t *testing.T) {
	t.Run("copy replacing the link", func(t *testing.T) {
		root := scanRepo(t, map[string]string{"CLAUDE.md": "rules\n", "README.md": "readme\n"})
		if _, err := materialiseHarness(root, "claude", specWithSections(t, houseRules)); err != nil {
			t.Fatal(err)
		}
		live := readFile(t, root, "CLAUDE.md")
		// The agent replaced the file with a link to a file that holds the same bytes.
		writeRaw(t, root, "README.md", live)
		damageByLink(t, root)
		if _, err := recoverPendingHarness(root); err == nil {
			t.Fatal("recovery read through a link")
		}
		if err := os.Remove(filepath.Join(root, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		writeRaw(t, root, "CLAUDE.md", readFile(t, root, "README.md"))
		if note, err := recoverPendingHarness(root); err != nil || note == "" {
			t.Fatalf("recovery after the named remedy = %q, %v", note, err)
		}
		if got := readFile(t, root, "CLAUDE.md"); got != "rules\n" {
			t.Fatalf("the block was not taken out: %q", got)
		}
	})
	t.Run("remove the lines and the record", func(t *testing.T) {
		root := scanRepo(t, map[string]string{"CLAUDE.md": "rules\n"})
		m, err := materialiseHarness(root, "claude", specWithSections(t, houseRules))
		if err != nil {
			t.Fatal(err)
		}
		writeRaw(t, root, "CLAUDE.md", readFile(t, root, "CLAUDE.md")+strings.Repeat("y", maxInstructionFileBytes))
		if m.Restore() == nil {
			t.Fatal("the grown file was read")
		}
		// By hand: the block's lines out, the agent's growth left, the record deleted.
		writeRaw(t, root, "CLAUDE.md", "rules\n")
		if err := os.Remove(filepath.Join(root, harnessPendingFile)); err != nil {
			t.Fatal(err)
		}
		if e := (&ManagedRunner{jobs: map[string]*managedJob{}}).HarnessBusy(root); e != nil {
			t.Fatalf("HarnessBusy = %v after the manual remedy", e)
		}
	})
}
