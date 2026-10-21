package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/bharathvbcr/Manvi/manvi/prompt"
)

func forbiddenRune(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Co, r)
}

func TestControlFormatAndPrivateUseRunesRefusedWithCodepoint(t *testing.T) {
	for want, r := range map[string]string{
		"U+0000": "\x00", "U+001B": "\x1b[2J", "U+0007": "\a", "U+007F": "\x7f",
		"U+202E": "\u202e", "U+200B": "\u200b", "U+E0041": "\U000e0041", "U+E000": "",
	} {
		_, reason := loadSkill("foo", stubRead("---\nname: foo\ndescription: \"a "+r+" b\"\n---\n"))
		if !strings.Contains(reason, want) {
			t.Errorf("%s: reason %q does not name the codepoint", want, reason)
		}
	}
	if _, reason := loadSkill("foo", stubRead("---\nname: foo\ndescription: plain ünïcode 日本\n---\n")); reason != "" {
		t.Errorf("ordinary non-ASCII text refused: %s", reason)
	}
}

// Whatever the budget, the section is within it, and a section that had to be
// cut says so.
func TestSkillsSectionNeverExceedsAnyBudget(t *testing.T) {
	var skills []Skill
	for _, n := range []string{"a", "bb", "ccc", "dddd", "eeeee"} {
		skills = append(skills, Skill{Name: n, Description: strings.Repeat("é", 37)})
	}
	full, _ := SkillsSection(skills, 0)
	fullTokens := prompt.EstimateTokens(full)
	sections := 0
	for budget := 1; budget <= fullTokens+20; budget++ {
		out, ok := SkillsSection(skills, budget)
		if !ok {
			if out != "" {
				t.Fatalf("budget %d: text with ok=false", budget)
			}
			continue
		}
		sections++
		if n := prompt.EstimateTokens(out); n > budget {
			t.Fatalf("budget %d: %d tokens", budget, n)
		}
		if budget < fullTokens && !strings.Contains(out, "was cut here") {
			t.Fatalf("budget %d: cut without saying so", budget)
		}
		if budget >= fullTokens && out != full {
			t.Fatalf("budget %d: a listing that fits was changed", budget)
		}
	}
	if sections == 0 {
		t.Fatal("no budget produced a section")
	}
	if out, ok := SkillsSection(skills, prompt.EstimateTokens(skillsPreamble)); ok || out != "" {
		t.Error("a budget that holds the preamble but not the cut notice must produce no section")
	}
}

func TestLookupSkill(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "good", "---\nname: good\ndescription: d\n---\nB")
	writeSkill(t, root, "nodesc", "---\nname: nodesc\n---\nB")
	writeSkill(t, root, "Foo", "---\nname: Foo\ndescription: d\n---\nB")
	read := rootRead(root)

	l, err := LookupSkill(root, read, "good")
	if err != nil || l.Outcome != SkillFound || l.Skill.Body != "B" {
		t.Fatalf("good: %+v %v", l, err)
	}
	l, _ = LookupSkill(root, read, "nodesc")
	if l.Outcome != SkillRejected || !strings.Contains(l.Reason, "no description") {
		t.Errorf("nodesc: %+v", l)
	}
	l, _ = LookupSkill(root, read, "absent")
	if l.Outcome != SkillMissing || l.AvailableTotal != 2 || strings.Join(l.Available, ",") != "good,nodesc" {
		t.Errorf("absent: %+v", l)
	}
	// "foo" differs from the directory "Foo" only in case. Whether the
	// filesystem folds case or not, the caller must be told Foo exists and why
	// it was refused.
	l, _ = LookupSkill(root, read, "foo")
	if l.Outcome != SkillRejected || l.Dir != "Foo" {
		t.Errorf("case variant: %+v", l)
	}
	if _, err := LookupSkill(root, read, "../x"); err == nil {
		t.Error("an invalid name must be refused before any lookup")
	}
}

func TestLookupSkillReadsAtMostOne(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"sa", "sb", "sc", "sd"} {
		writeSkill(t, root, n, "---\nname: "+n+"\ndescription: d\n---\nb")
	}
	reads := 0
	read := func(rel string, limit int64) ([]byte, error) {
		reads++
		return rootRead(root)(rel, limit)
	}
	if _, err := LookupSkill(root, read, "sa"); err != nil {
		t.Fatal(err)
	}
	if _, err := LookupSkill(root, read, "nope"); err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Errorf("two lookups made %d reads; want 1 (found) + 0 (missing)", reads)
	}
}

func TestRejectionsAreBoundedAndCounted(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < MaxRejected+30; i++ {
		if err := os.MkdirAll(filepath.Join(root, SkillsDir, "Bad"+strings.Repeat("x", i)), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	reads := 0
	set, err := LoadSkills(root, func(string, int64) ([]byte, error) { reads++; return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Rejected) != MaxRejected || set.RejectedOmitted != 30 || reads != 0 {
		t.Errorf("rejected=%d omitted=%d reads=%d", len(set.Rejected), set.RejectedOmitted, reads)
	}
}

func TestQuoteNameBoundsAndEscapes(t *testing.T) {
	q := QuoteName(strings.Repeat("A", 1<<20))
	if len(q) > 200 || !strings.Contains(q, "1048576 runes") {
		t.Errorf("QuoteName of 1 MiB = %d bytes: %q", len(q), q[:60])
	}
	if got := QuoteName("a\x1b[2Jb"); strings.ContainsRune(got, 0x1b) {
		t.Errorf("escape byte survived: %q", got)
	}
	if ValidSkillName(strings.Repeat("a", MaxSkillNameLen+1)) || !ValidSkillName(strings.Repeat("a", MaxSkillNameLen)) {
		t.Error("name length bound is off")
	}
}

func TestQuotedScalarClosingRules(t *testing.T) {
	for in, want := range map[string]string{
		`"a \"q\" b"`: `a "q" b`, `'it''s'`: "it's", `"x"  `: "x", `"a\\"`: `a\`,
	} {
		got, err := scalar(strings.TrimSpace(in))
		if err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{`"a" "b"`, `'a'b'`, `"a"b"`, `"abc\"`, `"a" # c`, `"`, `'`} {
		if got, err := scalar(in); err == nil {
			t.Errorf("%s accepted as %q", in, got)
		}
	}
}
