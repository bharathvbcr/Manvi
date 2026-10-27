package harness

import (
	"fmt"
	"strings"
	"testing"
)

// A skill's body is handed to the model whole by devcouncil_load_skill and may
// be printed by a terminal, so it is refused on the same runes the description,
// a spec's text and a lesson's answer are refused on: control (except tab,
// newline and carriage return), format (bidi, zero-width) and private-use,
// which includes the Unicode tag block. AUDIT 2026-10-26 F2: it was checked for
// UTF-8 alone.
func TestSkillBodyRefusesHiddenRunes(t *testing.T) {
	for name, body := range map[string]string{
		"escape":        "real step\x1b[2J then more\n",
		"nul":           "real step\x00hidden\n",
		"bidi override": "real step \u202eevil\n",
		"bidi isolate":  "real \u2066step\n",
		"tag chars":     "real step\U000e0041\U000e0042 hidden tag chars\n",
		"del":           "real\x7fstep\n",
		"c1 control":    "real\u0085step\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeSkill(t, root, "x", "---\nname: x\ndescription: fine\n---\n"+body)
			set := load(t, root)
			if len(set.Skills) != 0 {
				t.Fatalf("a body with a hidden rune was accepted: %q", set.Skills[0].Body)
			}
			reason := reasonFor(t, set, "x")
			if !strings.Contains(reason, "body") || !strings.Contains(reason, "U+") {
				t.Fatalf("the refusal does not name the body and the codepoint: %q", reason)
			}

			l, err := LookupSkill(t.Context(), root, rootRead(root), "x")
			if err != nil {
				t.Fatal(err)
			}
			if l.Outcome != SkillRejected || l.Dir != "x" || !strings.Contains(l.Reason, "U+") {
				t.Fatalf("the single lookup does not refuse the same body: %+v", l)
			}
		})
	}
}

// What a real skill's body contains stays legal: tab, newline and carriage
// return, ordinary Unicode text, emoji (a symbol, not a format character), and
// the template-looking text a shell or CI snippet carries, which is data here
// and is only refused in a spec.
func TestSkillBodyKeepsOrdinaryText(t *testing.T) {
	body := "# Steps\r\n\t1. run `echo ${HOME}` and {{ .Values }}\n2. caf\u00e9 \u65e5\u672c\u8a9e \U0001f680 \u2192 done\n"
	root := t.TempDir()
	writeSkill(t, root, "x", "---\nname: x\ndescription: fine\n---\n"+body)
	set := load(t, root)
	if len(set.Skills) != 1 || len(set.Rejected) != 0 {
		t.Fatalf("an ordinary body was refused: skills=%d rejected=%v", len(set.Skills), set.Rejected)
	}
	if !strings.Contains(set.Skills[0].Body, "${HOME}") || !strings.Contains(set.Skills[0].Body, "\t1.") {
		t.Fatalf("the body was altered: %q", set.Skills[0].Body)
	}
}

// The refusal names the skill, so a listing of many reports which one.
func TestSkillBodyRefusalNamesTheSkill(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "good", "---\nname: good\ndescription: fine\n---\nsteps\n")
	writeSkill(t, root, "bad", "---\nname: bad\ndescription: fine\n---\nsteps\x1b[2J\n")
	set := load(t, root)
	if len(set.Skills) != 1 || set.Skills[0].Name != "good" {
		t.Fatalf("skills = %+v", set.Skills)
	}
	if len(set.Rejected) != 1 || set.Rejected[0].Name != "bad" {
		t.Fatalf("rejected = %+v", set.Rejected)
	}
	if got := fmt.Sprint(set.Rejected[0].Reason); !strings.Contains(got, "U+001B") {
		t.Fatalf("the reason does not give the codepoint: %q", got)
	}
}

// A spec that narrows the listing keeps the account of how the listing was
// read: the cut is about the directory, not about which skills were selected.
func TestFilterSkillsKeepsTheListingCut(t *testing.T) {
	set := SkillSet{Skills: []Skill{{Name: "a"}, {Name: "b"}}, ListingCut: true}
	if got := FilterSkills(set, []string{"a"}); !got.ListingCut || len(got.Skills) != 1 {
		t.Fatalf("filtered = %+v", got)
	}
}
