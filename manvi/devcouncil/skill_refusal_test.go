package devcouncil

import (
	"strings"
	"testing"
)

func TestLoadSkillRefusalOffersSkillsTheListingLacks(t *testing.T) {
	f := newSkillFixture(t)
	var listed []string
	for i := 0; i < 30; i++ {
		n := "s" + string(rune('a'+i/10)) + string(rune('0'+i%10))
		writeSkillFile(t, f.root, n, "---\nname: "+n+"\ndescription: d\n---\nB")
		if i < 25 {
			listed = append(listed, n)
		}
	}
	f.reg.NoteListedSkills(listed)
	res := f.call("devcouncil_load_skill", map[string]string{"name": "nope"})
	if !res.IsError || !strings.Contains(res.Text, "sc5") || strings.Contains(res.Text, "sc4") {
		t.Fatalf("the refusal must offer only unlisted skills: %q", res.Text)
	}
	f.reg.NoteListedSkills(append(listed, "sc5", "sc6", "sc7", "sc8", "sc9"))
	res = f.call("devcouncil_load_skill", map[string]string{"name": "nope"})
	if !res.IsError || !strings.Contains(res.Text, "already named in the system prompt") {
		t.Fatalf("every skill listed must say so, not claim there are no skills: %q", res.Text)
	}
}
