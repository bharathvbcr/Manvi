//go:build unix

package devcouncil

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestAdvPerSkillReadCost separates the listing cost from the per-skill read
// cost: LoadSkills runs on every devcouncil_load_skill call and (per the card)
// every prompt assembly, and re-reads every skill each time.
func TestAdvPerSkillReadCost(t *testing.T) {
	f := newSkillFixture(t)
	const n = 200
	for i := range n {
		name := fmt.Sprintf("s%03d", i)
		writeSkillFile(t, f.root, name, fmt.Sprintf(advGoodSkill, name))
	}
	start := time.Now()
	set, err := f.reg.LoadSkills(context.Background())
	el := time.Since(start)
	advMust(t, err)
	per := el / time.Duration(len(set.Skills))
	t.Logf("%d small skills: %v total, %v per skill", len(set.Skills), el, per)
	if el > 250*time.Millisecond {
		t.Errorf("one LoadSkills over %d tiny skills took %v (%v per skill); it runs on every load_skill call", n, el, per)
	}
	// And the tool pays the full scan to return one skill.
	start = time.Now()
	res := advLoad(f, "s000")
	t.Logf("devcouncil_load_skill for one skill with %d siblings: %v (isError=%v)", n, time.Since(start), res.IsError)
}
