package policy

import (
	"os"
	"path/filepath"
	"testing"

	"manvi/dc"
)

// forbidden_changes is a prohibition on a file, written down as a name — and a
// filesystem hands out more than one name for the same file. These tests hold
// the two shapes that actually occur: a hard link, which works everywhere, and
// the two Unicode spellings of one name, which are one file on a
// normalisation-insensitive filesystem and two files elsewhere.

func forbiddenTask(planned, forbidden string) *dc.Task {
	return &dc.Task{
		ID:               "TASK-1",
		PlannedFiles:     []dc.PlannedFile{modify(planned)},
		ForbiddenChanges: []string{forbidden},
	}
}

func TestForbiddenChangesSeesThroughAHardLink(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("prod: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "config.yaml"), filepath.Join(root, "copy.yaml")); err != nil {
		t.Skipf("this filesystem does not support hard links: %v", err)
	}
	g := FileGate{Root: root, HardRules: true}

	// The plan authorises copy.yaml, and copy.yaml is config.yaml.
	d := g.EvaluateFileChange("copy.yaml", forbiddenTask("copy.yaml", "config.yaml"), dc.OpWrite, false)
	if d.Rule != RuleForbiddenChange {
		t.Fatalf("writing the forbidden file under its second name was judged %s/%q: %s",
			d.Action, d.Rule, d.Reason)
	}

	// And the prohibition still applies to nothing else: a file that is not the
	// forbidden one stays writable, or the rung becomes noise.
	if err := os.WriteFile(filepath.Join(root, "other.yaml"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if d := g.EvaluateFileChange("other.yaml", forbiddenTask("other.yaml", "config.yaml"),
		dc.OpWrite, false); d.Action != Allow {
		t.Fatalf("an unrelated file was refused: %s/%q %s", d.Action, d.Rule, d.Reason)
	}
}

// TestForbiddenChangesSeesThroughUnicodeNormalisation is the same defect in the
// spelling git can actually deliver. On APFS "café.txt" composed (NFC) and
// decomposed (NFD) are two strings and one file; the matcher saw a name it had
// never been told about while the kernel opened the file it had. On a
// normalisation-sensitive filesystem they are genuinely two files, and allowing
// the second is the right answer — so the test asks the filesystem first
// instead of asserting one platform's behaviour everywhere.
func TestForbiddenChangesSeesThroughUnicodeNormalisation(t *testing.T) {
	const (
		nfc = "caf\u00e9.txt"  // é as one composed rune
		nfd = "cafe\u0301.txt" // e followed by a combining acute
	)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, nfc), []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, nfd)); err != nil {
		t.Skip("this filesystem distinguishes NFC from NFD, so the two names are two files " +
			"and there is no alias to close")
	}

	g := FileGate{Root: root, HardRules: true}
	if d := g.EvaluateFileChange(nfc, forbiddenTask(nfc, nfc), dc.OpWrite, false); d.Rule != RuleForbiddenChange {
		t.Fatalf("the plain spelling was not refused: %s/%q", d.Action, d.Rule)
	}
	d := g.EvaluateFileChange(nfd, forbiddenTask(nfd, nfc), dc.OpWrite, false)
	if d.Rule != RuleForbiddenChange {
		t.Fatalf("the decomposed spelling of a forbidden file was judged %s/%q: it opens the "+
			"same file on this filesystem", d.Action, d.Rule)
	}
}

// A glob entry names a set rather than a file, so it is left to the pattern
// rung — which still answers, and answers case-insensitively.
func TestForbiddenChangesStillMatchesPatterns(t *testing.T) {
	root := t.TempDir()
	g := FileGate{Root: root, HardRules: true}
	task := &dc.Task{
		ID:               "TASK-1",
		PlannedFiles:     []dc.PlannedFile{modify("deploy/PROD.yaml")},
		ForbiddenChanges: []string{"deploy/*.yaml"},
	}
	if d := g.EvaluateFileChange("deploy/PROD.yaml", task, dc.OpWrite, false); d.Rule != RuleForbiddenChange {
		t.Fatalf("a glob prohibition stopped matching: %s/%q", d.Action, d.Rule)
	}
}
