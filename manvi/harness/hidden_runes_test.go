package harness

import (
	"fmt"
	"strings"
	"testing"
)

// hiddenRuneProblem is the one rune policy for a skill's description and body
// and a spec's text: an enumerated refusal (C0/C1 controls except tab, newline
// and carriage return; bidi embedding, override and isolate; the tag block), and
// everything else allowed. These tests spell the runes as numbers so no hidden
// rune is ever a literal in this file.

func runesOf(codes ...rune) string { return string(codes) }

// The corpus real text in real languages carries. Each was refused when the
// policy was a Unicode category (Cf and Co), which swept all of it in.
func TestHiddenRunePolicyAllowsRealText(t *testing.T) {
	for name, text := range map[string]string{
		"zwj family emoji":       runesOf(0x1F468, 0x200D, 0x1F469, 0x200D, 0x1F467),
		"zwj technologist":       runesOf(0x1F468, 0x200D, 0x1F4BB),
		"persian zwnj":           runesOf(0x645, 0x6CC, 0x200C, 0x62E, 0x648, 0x627, 0x647, 0x645),
		"soft hyphen":            "co" + runesOf(0xAD) + "operate",
		"zero width space":       "a" + runesOf(0x200B) + "b",
		"word joiner":            "a" + runesOf(0x2060) + "b",
		"byte order mark":        "start " + runesOf(0xFEFF) + " middle",
		"bom first":              runesOf(0xFEFF) + "text",
		"left to right mark":     "a" + runesOf(0x200E) + "b",
		"right to left mark":     "a" + runesOf(0x200F) + "b",
		"arabic letter mark":     "a" + runesOf(0x61C) + "b",
		"variation selector":     runesOf(0x2764, 0xFE0F),
		"variation selector 17":  runesOf(0x4E00, 0xE0100),
		"private use":            "icon " + runesOf(0xE000) + " glyph",
		"private use plane 15":   runesOf(0xF0000),
		"private use plane 16":   runesOf(0x10FFFD),
		"line separator":         "a" + runesOf(0x2028) + "b",
		"tab newline cr":         "a\tb\r\nc\n",
		"accented and cjk":       "caf" + runesOf(0xE9) + " " + runesOf(0x65E5, 0x672C, 0x8A9E),
		"one past the tag block": runesOf(0xE0080),
	} {
		t.Run(name, func(t *testing.T) {
			if why := hiddenRuneProblem(text); why != "" {
				t.Fatalf("refused: %s", why)
			}
			if why := textProblem(text); why != "" {
				t.Fatalf("a spec's text refused it: %s", why)
			}
		})
	}
}

func TestHiddenRunePolicyRefusesWhatHidesTextAndNamesIt(t *testing.T) {
	for _, c := range []struct {
		name, text, want string
	}{
		{"escape", "ab\n\x1bc", "U+001B ESCAPE at line 2, column 1"},
		{"nul", "a\x00", "U+0000 NULL at line 1, column 2"},
		{"c0 other", "a\x01", "U+0001 C0 CONTROL"},
		{"del", "ab\x7f", "U+007F DELETE at line 1, column 3"},
		{"c1", "x" + runesOf(0x85), "U+0085 NEXT LINE"},
		{"c1 other", runesOf(0x99), "U+0099 C1 CONTROL at line 1, column 1"},
		{"bidi override", "line one\nline " + runesOf(0x202E) + "two", "U+202E RIGHT-TO-LEFT OVERRIDE at line 2, column 6"},
		{"bidi embedding", runesOf(0x202A), "U+202A LEFT-TO-RIGHT EMBEDDING"},
		{"pop formatting", runesOf(0x202C), "U+202C POP DIRECTIONAL FORMATTING"},
		{"bidi isolate", "a" + runesOf(0x2066) + "b", "U+2066 LEFT-TO-RIGHT ISOLATE at line 1, column 2"},
		{"pop isolate", runesOf(0x2069), "U+2069 POP DIRECTIONAL ISOLATE"},
		{"tag char", "x" + runesOf(0xE0041, 0xE0042), "U+E0041 TAG CHARACTER at line 1, column 2"},
		{"tag block start", runesOf(0xE0000), "U+E0000 TAG CHARACTER"},
		{"tag block end", runesOf(0xE007F), "U+E007F TAG CHARACTER"},
	} {
		t.Run(c.name, func(t *testing.T) {
			why := hiddenRuneProblem(c.text)
			if !strings.Contains(why, c.want) {
				t.Fatalf("got %q, want it to contain %q", why, c.want)
			}
			if why != "" && strings.Count(why, ":") < 1 {
				t.Fatalf("no reason given: %q", why)
			}
			if textProblem(c.text) == "" {
				t.Fatal("a spec's text was not refused by the same policy")
			}
		})
	}
}

// The three inputs share it: a description, a body and a spec's text refuse
// the same runes, and the allowed corpus loads in a real skill.
func TestHiddenRunePolicyIsSharedByDescriptionBodyAndSpec(t *testing.T) {
	allowed := runesOf(0x1F468, 0x200D, 0x1F4BB) + " " + runesOf(0x645, 0x6CC, 0x200C, 0x62E) + " mid" + runesOf(0xFEFF) + "bom" + runesOf(0xE000)
	root := t.TempDir()
	writeSkill(t, root, "ok", "---\nname: ok\ndescription: \""+allowed+"\"\n---\nsteps "+allowed+"\n")
	set := load(t, root)
	if len(set.Skills) != 1 || len(set.Rejected) != 0 {
		t.Fatalf("a skill carrying the allowed corpus was refused: %+v", set.Rejected)
	}

	for _, hidden := range []string{"\x1b[2J", runesOf(0x202E), runesOf(0x2066), runesOf(0xE0041)} {
		root := t.TempDir()
		writeSkill(t, root, "d", "---\nname: d\ndescription: \"a "+hidden+" b\"\n---\nbody\n")
		writeSkill(t, root, "b", "---\nname: b\ndescription: fine\n---\nbody "+hidden+"\n")
		set := load(t, root)
		if len(set.Skills) != 0 || len(set.Rejected) != 2 {
			t.Fatalf("%q: skills=%d rejected=%v", hidden, len(set.Skills), set.Rejected)
		}
		for _, r := range set.Rejected {
			if !strings.Contains(r.Reason, fmt.Sprintf("U+%04X", []rune(hidden)[0])) {
				t.Fatalf("%s: the reason does not name the rune: %q", r.Name, r.Reason)
			}
		}
	}
}
