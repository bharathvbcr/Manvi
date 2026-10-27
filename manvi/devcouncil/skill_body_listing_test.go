//go:build unix

package devcouncil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/harness"
)

// AUDIT 2026-10-26 F2: a skill body with ESC, a bidi override and Unicode tag
// characters reached the model through devcouncil_load_skill, which hands over
// the body whole. The tool now refuses the skill, naming the reason.
func TestLoadSkillRefusesAHiddenRuneBody(t *testing.T) {
	f := newSkillFixture(t)
	writeSkillFile(t, f.root, "x", "---\nname: x\ndescription: fine\n---\nreal step\x1b[2J\u202eevil\U000e0041\U000e0042 hidden tag chars\n")
	res := advLoad(f, "x")
	if !res.IsError {
		t.Fatalf("a body carrying ESC, a bidi override and tag characters was handed to the model: %q", res.Text)
	}
	if strings.ContainsAny(res.Text, "\x1b\u202e\U000e0041") {
		t.Fatalf("the refusal echoes the hidden runes: %q", res.Text)
	}
	if !strings.Contains(res.Text, "U+001B") {
		t.Fatalf("the refusal does not say why: %q", res.Text)
	}
}

// AUDIT 2026-10-26 S3: every turn listed the whole skills directory (ReadDir(-1))
// to find at most MaxSkills skills, and every devcouncil_load_skill call listed
// it again: 100,000 entries cost 5.5 s a turn. The listing reads at most
// harness.MaxSkillDirEntries entries and says when it stopped.
func TestSkillsDirectoryListingIsBounded(t *testing.T) {
	f := newSkillFixture(t)
	dir := filepath.Join(f.root, ".agents", "skills")
	advMust(t, os.MkdirAll(dir, 0o750))
	const extra = 2_000
	n := harness.MaxSkillDirEntries + extra
	for i := range n {
		advMust(t, os.Mkdir(filepath.Join(dir, fmt.Sprintf("d%06d", i)), 0o750))
	}

	set, err := f.reg.LoadSkills(context.Background())
	advMust(t, err)
	considered := len(set.Skills) + len(set.Rejected) + set.RejectedOmitted + set.NotExamined
	if considered > harness.MaxSkillDirEntries {
		t.Fatalf("%d entries were considered from a directory of %d: the listing is not bounded by %d",
			considered, n, harness.MaxSkillDirEntries)
	}
	if !set.ListingCut {
		t.Fatalf("the listing stopped at the bound and did not say so: %+v", set.NotExamined)
	}

	// The tool reports the same cut for a name it cannot place, rather than
	// calling it absent.
	res := advLoad(f, "zzz-not-listed")
	if !res.IsError || !strings.Contains(res.Text, fmt.Sprint(harness.MaxSkillDirEntries)) {
		t.Fatalf("a lookup over a cut listing did not say the listing was cut: %q", res.Text)
	}
}

// At the bound exactly, nothing is cut and nothing is reported as cut.
func TestSkillsDirectoryAtTheBoundIsNotCut(t *testing.T) {
	f := newSkillFixture(t)
	dir := filepath.Join(f.root, ".agents", "skills")
	advMust(t, os.MkdirAll(dir, 0o750))
	for i := range harness.MaxSkillDirEntries {
		advMust(t, os.Mkdir(filepath.Join(dir, fmt.Sprintf("d%06d", i)), 0o750))
	}
	set, err := f.reg.LoadSkills(context.Background())
	advMust(t, err)
	if set.ListingCut {
		t.Fatal("a directory of exactly the bound was reported as cut")
	}
	if got := len(set.Rejected) + set.RejectedOmitted + set.NotExamined; got != harness.MaxSkillDirEntries {
		t.Fatalf("considered %d entries, want %d", got, harness.MaxSkillDirEntries)
	}
}
