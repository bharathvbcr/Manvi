package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/harness"
)

// Adversarial tests for dh-3 (store-free). Each one that is expected to fail
// names the defect in its comment.

func specNamed(name, text string) harness.Spec {
	return harness.Spec{Name: name, Sections: []harness.Section{{Name: "house-rules", Order: 70, Text: text}}}
}

func writeRaw(t *testing.T, root, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ATTACK 1a. The agent deletes only the END marker (or duplicates the block).
// Restore must neither destroy the agent's content nor leave a half block, and
// the next materialisation on the checkout must not be wedged by it.
func TestAdversarialRestoreAfterAgentDeletesEndMarkerLeavesNoHalfBlock(t *testing.T) {
	root := scanRepo(t, map[string]string{"AGENTS.md": "rules\n"})
	m, err := materialiseHarness(root, "codex", specNamed("team", "Prefer small diffs.\n"))
	if err != nil {
		t.Fatal(err)
	}
	live := readFile(t, root, "AGENTS.md")
	var kept []string
	for _, l := range strings.Split(live, "\n") {
		if !strings.HasPrefix(l, harnessEndMarker) {
			kept = append(kept, l)
		}
	}
	writeRaw(t, root, "AGENTS.md", strings.Join(kept, "\n")+"agent note after\n")
	rerr := m.Restore()
	got := readFile(t, root, "AGENTS.md")
	if strings.Contains(got, harnessBeginMarker) {
		t.Fatalf("restore (err=%v) left a half block behind:\n%s", rerr, got)
	}
	if !strings.Contains(got, "agent note after") {
		t.Fatalf("the agent's content was destroyed:\n%s", got)
	}
}

func TestAdversarialStaleHalfBlockDoesNotWedgeTheNextMaterialisation(t *testing.T) {
	root := scanRepo(t, map[string]string{"AGENTS.md": "rules\n"})
	writeRaw(t, root, "AGENTS.md", "rules\n"+harnessBeginMarker+" name=x digest=y -->\nresidue with no end\n")
	if _, err := materialiseHarness(root, "codex", specNamed("team", "Prefer small diffs.\n")); err != nil {
		t.Fatalf("a begin marker with no end makes every later run on this checkout abort for good: %v", err)
	}
}

func TestAdversarialDuplicatedBlockIsNotAPermanentWedge(t *testing.T) {
	root := scanRepo(t, map[string]string{"AGENTS.md": "rules\n"})
	m, err := materialiseHarness(root, "codex", specNamed("team", "Prefer small diffs.\n"))
	if err != nil {
		t.Fatal(err)
	}
	live := readFile(t, root, "AGENTS.md")
	writeRaw(t, root, "AGENTS.md", live+"\n"+live[strings.Index(live, harnessBeginMarker):]) // agent copies the block
	rerr := m.Restore()
	if got := readFile(t, root, "AGENTS.md"); strings.Contains(got, harnessBeginMarker) {
		t.Fatalf("restore (err=%v) left block text in the checkout:\n%s", rerr, got)
	}
}

// ATTACK 4. Repo content that merely looks like a block (a doc that quotes the
// markers, as Manvi's own docs do) is deleted by materialisation and never
// restored: the checkout carries a deletion nobody made.
func TestAdversarialMaterialiseDoesNotDeleteRepoContentBetweenLookalikeMarkers(t *testing.T) {
	original := "# Rules\n" + harnessBeginMarker + " example -->\nimportant repo rule\n" + harnessEndMarker + " example -->\nmore\n"
	root := scanRepo(t, map[string]string{"AGENTS.md": original})
	m, err := materialiseHarness(root, "codex", specNamed("team", "Prefer small diffs.\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, root, "AGENTS.md"); got != original {
		t.Fatalf("repo content between lookalike markers was destroyed:\nwant %q\ngot  %q", original, got)
	}
}

// A marker that is only a prefix match ("...HARNESSfoo") is taken as a marker.
func TestAdversarialMarkerIsMatchedAsAWholeToken(t *testing.T) {
	original := "doc\n" + harnessBeginMarker + "FOO is a different comment -->\n"
	root := scanRepo(t, map[string]string{"AGENTS.md": original})
	if _, err := materialiseHarness(root, "codex", specNamed("team", "x\n")); err != nil {
		t.Fatalf("an unrelated line sharing the marker prefix aborts the run: %v", err)
	}
}

// ATTACK 3. Two managed runs on one checkout. The first to end must not take
// the other's live block out.
func TestAdversarialSecondRunsBlockSurvivesFirstRunsRestore(t *testing.T) {
	root := scanRepo(t, map[string]string{"AGENTS.md": "rules\n"})
	m1, err := materialiseHarness(root, "codex", specNamed("one", "from run one\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := materialiseHarness(root, "codex", specNamed("two", "from run two\n")); err != nil {
		t.Fatalf("a second materialisation on a checkout with a live block is not refused or serialised: %v", err)
	}
	if err := m1.Restore(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, root, "AGENTS.md"); !strings.Contains(got, "from run two") {
		t.Fatalf("run one's restore removed run two's live block:\n%s", got)
	}
}

// ATTACK 5. Byte exactness when the agent edited the file (the unchanged path
// is exact). The file had no trailing newline.
func TestAdversarialRestoreIsByteExactWhenTheAgentEditedAFileWithoutTrailingNewline(t *testing.T) {
	root := scanRepo(t, map[string]string{"AGENTS.md": "rules"})
	m, err := materialiseHarness(root, "codex", specNamed("team", "x\n"))
	if err != nil {
		t.Fatal(err)
	}
	live := readFile(t, root, "AGENTS.md")
	writeRaw(t, root, "AGENTS.md", "agent line\n"+live)
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, root, "AGENTS.md"); got != "agent line\nrules" {
		t.Fatalf("restore changed bytes outside the block: %q", got)
	}
}

func TestAdversarialFileStates(t *testing.T) {
	spec := specNamed("team", "x\n")
	cases := map[string]string{
		"crlf":     "a\r\nb\r\n",
		"bom":      "\xef\xbb\xbfrules\n",
		"non-utf8": "rules \xff\xfe\n",
		"empty":    "",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			root := scanRepo(t, map[string]string{"AGENTS.md": body, "keep": "k"})
			m, err := materialiseHarness(root, "codex", spec)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Restore(); err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, root, "AGENTS.md"); got != body {
				t.Fatalf("not byte exact: want %q got %q", body, got)
			}
		})
	}
	t.Run("huge", func(t *testing.T) {
		big := strings.Repeat("a", maxInstructionFileBytes+1)
		root := scanRepo(t, map[string]string{"AGENTS.md": big})
		if _, err := materialiseHarness(root, "codex", spec); err == nil {
			t.Fatal("an over-limit instruction file was not refused")
		}
		if got := readFile(t, root, "AGENTS.md"); got != big {
			t.Fatal("a refused materialisation changed the file")
		}
	})
	t.Run("directory", func(t *testing.T) {
		root := scanRepo(t, map[string]string{"AGENTS.md/x": "k"})
		if _, err := materialiseHarness(root, "codex", spec); err == nil {
			t.Fatal("a directory named AGENTS.md was not refused")
		}
	})
	t.Run("read-only", func(t *testing.T) {
		root := scanRepo(t, map[string]string{"AGENTS.md": "rules\n"})
		if err := os.Chmod(filepath.Join(root, "AGENTS.md"), 0o400); err != nil {
			t.Fatal(err)
		}
		m, err := materialiseHarness(root, "codex", spec)
		if err == nil {
			_ = m.Restore()
		}
		if got := readFile(t, root, "AGENTS.md"); strings.Contains(got, harnessBeginMarker) {
			t.Fatalf("block left in a read-only file (err=%v)", err)
		}
	})
}

// ATTACK 6. harness.resolve inline specs: the 4 KiB bound the managed-plane doc
// cites and the unknown-field and size behaviour of the op.
func TestAdversarialResolveInlineSizeAndStrictness(t *testing.T) {
	h := newTestResolver(t)
	ctx := context.Background()
	call := func(params string) *Error {
		_, e := h.resolve(ctx, json.RawMessage(params))
		return e
	}
	if e := call(`{"session_id":"s","provider":"claude","spec":{"kind":"named","name":"registered"},"tier":"x"}`); e == nil {
		t.Error("unknown top-level param accepted")
	}
	if e := call(`{"session_id":"s","provider":"claude","spec":{"kind":"named","name":"registered","posture":"x"}}`); e == nil {
		t.Error("unknown spec param accepted")
	}
	if e := call(`{"session_id":"s","provider":"claude","spec":{"kind":"named","name":"../x"}}`); e == nil {
		t.Error("traversal name accepted")
	}
	big := strings.Repeat("a", 200*1024)
	inline := `{"session_id":"s","provider":"claude","spec":{"kind":"inline","inline":{"name":"big","sections":[{"name":"house-rules","order":70,"text":"` + big + `"}]}}}`
	if e := call(inline); e == nil {
		t.Error("a 200 KiB inline spec was accepted; the documented request bound is 4 KiB")
	}
	// Root: confinement. A root anywhere on disk is read for repo specs.
	outside := scanRepo(t, map[string]string{".devcouncil/harness/secretspec.json": `{"name":"secretspec","sections":[{"name":"house-rules","order":70,"text":"outside"}]}`})
	if _, e := h.resolve(ctx, json.RawMessage(`{"session_id":"s","provider":"claude","root":"`+outside+`","spec":{"kind":"repo","name":"secretspec"}}`)); e == nil {
		t.Log("root is not confined to any workspace (same as devmap/scan roots; no workspace notion in serve)")
	}
}

var _ = bytes.Equal
