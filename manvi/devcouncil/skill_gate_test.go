package devcouncil

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A skill directory named for a secret is refused by the real read gate
// (policy.ClassifyRead matches SecretPathPatterns such as **/secrets/**), both
// when listing and when loading. There is no lighter path for either.
func TestSecretNamedSkillDirectoriesAreRefusedByTheReadGate(t *testing.T) {
	f := newSkillFixture(t)
	good := "---\nname: %s\ndescription: d\n---\nBODY\n"
	for _, n := range []string{"credentials", "secrets", "ok-one", "ok-two"} {
		writeSkillFile(t, f.root, n, fmt.Sprintf(good, n))
	}

	set, err := f.reg.LoadSkills(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	refused := map[string]string{}
	for _, name := range []string{"credentials", "secrets"} {
		reason, ok := set.Rejection(name)
		if !ok || !strings.Contains(reason, "withheld by") {
			t.Errorf("listing: %s was not rejected by the read gate: %q (accepted=%v)", name, reason, set.Skills)
		}
		refused[name] = reason
		if _, ok := set.Find(name); ok {
			t.Errorf("listing: %s was accepted", name)
		}
		res := f.call("devcouncil_load_skill", map[string]string{"name": name})
		if !res.IsError || strings.Contains(res.Text, "BODY") || !strings.Contains(res.Text, "withheld by") {
			t.Errorf("tool: %s: %v %q", name, res.IsError, res.Text)
		}
	}
	for _, name := range []string{"ok-one", "ok-two"} {
		if _, ok := set.Find(name); !ok {
			t.Errorf("%s was not accepted: %v", name, set.Rejected)
		}
	}
	t.Logf("gate reasons: %v", refused)
}

// TestLoadSkillsTimingWithFullGate reports LoadSkills at 25 and 200 skills, min
// of 6, with every read behind the full gate. It asserts only a generous bound.
func TestLoadSkillsTimingWithFullGate(t *testing.T) {
	for _, n := range []int{25, 200} {
		f := newSkillFixture(t)
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("s%03d", i)
			writeSkillFile(t, f.root, name, fmt.Sprintf(advGoodSkill, name))
		}
		best := time.Hour
		for run := 0; run < 6; run++ {
			start := time.Now()
			set, err := f.reg.LoadSkills(context.Background())
			el := time.Since(start)
			if err != nil || len(set.Skills) != n {
				t.Fatalf("n=%d: %d skills, err %v", n, len(set.Skills), err)
			}
			if el < best {
				best = el
			}
		}
		t.Logf("LoadSkills over %d skills, full gate, min of 6: %v (%v per skill)", n, best, best/time.Duration(n))
		if best > 5*time.Second {
			t.Errorf("%d skills took %v", n, best)
		}
	}
}
