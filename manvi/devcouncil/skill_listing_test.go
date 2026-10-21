package devcouncil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/harness"
)

// The listing judges its first read with the full read gate and the rest by
// spelling and link count. These cases are the ones the cheaper judgement has
// to keep refusing on the second and later reads.
func TestListingGateStillRefusesLaterSiblings(t *testing.T) {
	f := newSkillFixture(t)
	good := "---\nname: %s\ndescription: d\n---\nBODY\n"
	writeSkillFile(t, f.root, "a-first", fmt.Sprintf(good, "a-first"))
	writeRepoFile(t, f.root, ".env", fmt.Sprintf(good, "b-linked")+"SECRET\n")
	if err := os.MkdirAll(filepath.Join(f.root, harness.SkillsDir, "b-linked"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(f.root, ".env"), filepath.Join(f.root, harness.SkillsDir, "b-linked", harness.SkillFile)); err != nil {
		t.Skip("hard links unavailable:", err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, harness.SkillFile), []byte(fmt.Sprintf(good, "c-escape")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.root, harness.SkillsDir, "c-escape")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	writeSkillFile(t, f.root, "d-last", fmt.Sprintf(good, "d-last"))

	set, err := f.reg.LoadSkills(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range set.Skills {
		got = append(got, s.Name)
	}
	if strings.Join(got, ",") != "a-first,d-last" {
		t.Errorf("accepted %v, want a-first and d-last", got)
	}
	if r, _ := set.Rejection("b-linked"); !strings.Contains(r, "path.aliased_read") {
		t.Errorf("hard-linked SKILL.md: %q", r)
	}
	if r, _ := set.Rejection("c-escape"); r == "" {
		t.Error("symlinked skill directory was not rejected")
	}
}

// One load_skill call reads one skill however many directories sit beside it.
func TestLoadSkillCostDoesNotGrowWithSiblings(t *testing.T) {
	f := newSkillFixture(t)
	for i := 0; i < 3000; i++ {
		if err := os.MkdirAll(filepath.Join(f.root, harness.SkillsDir, fmt.Sprintf("s%05d", i)), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	writeSkillFile(t, f.root, "target", "---\nname: target\ndescription: d\n---\nBODY\n")
	start := time.Now()
	res := f.call("devcouncil_load_skill", map[string]string{"name": "target"})
	if res.IsError || res.Text != "BODY\n" {
		t.Fatalf("got %v %q", res.IsError, res.Text)
	}
	if el := time.Since(start); el > 250*time.Millisecond {
		t.Errorf("one skill among 3000 directories took %v", el)
	}
}

func TestRefusalsQuoteAndBoundTheName(t *testing.T) {
	f := newSkillFixture(t)
	writeSkillFile(t, f.root, "foo", "---\nname: foo\ndescription: d\n---\nB")
	for _, name := range []string{strings.Repeat("a", 1<<20), strings.Repeat("A", 1<<20), "bad\x1b[2Jname"} {
		res := f.call("devcouncil_load_skill", map[string]string{"name": name})
		if !res.IsError || len(res.Text) > 512 || strings.ContainsRune(res.Text, 0x1b) {
			t.Errorf("name of %d bytes: %d-byte reply %q", len(name), len(res.Text), res.Text)
		}
	}
}

func TestMoreThanTheListedNamesAreCounted(t *testing.T) {
	f := newSkillFixture(t)
	for i := 0; i < harness.MaxListedNames+5; i++ {
		n := fmt.Sprintf("s%02d", i)
		writeSkillFile(t, f.root, n, fmt.Sprintf("---\nname: %s\ndescription: d\n---\nB", n))
	}
	res := f.call("devcouncil_load_skill", map[string]string{"name": "zzz"})
	if !res.IsError || !strings.Contains(res.Text, "and 5 more") {
		t.Errorf("got %q", res.Text)
	}
}
