package devcouncil

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/harness"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// maxSkillBodyRunes bounds what one devcouncil_load_skill call puts in the
// model's context. harness.MaxSkillBytes bounds the file; this bounds the
// answer, so the two can be tuned apart. 32,000 runes is about eight thousand
// tokens: enough for a long procedure, small enough that loading one skill
// cannot displace the task.
const maxSkillBodyRunes = 32_000

// skillReader is the one reader skills are read with: the repository's
// contained reader, behind the read gate. A skill file is judged the way
// read_file judges any path, so a skill that is a hard link to something the
// gate withholds is refused here exactly as it would be there.
func (r *Registry) skillReader(ctx context.Context) harness.ReadFunc {
	return func(rel string, limit int64) ([]byte, error) {
		if why := r.readRefusal(rel); why != "" {
			return nil, fmt.Errorf("%s is withheld by %s", rel, why)
		}
		return readContained(ctx, r.deps.Root, rel, limit)
	}
}

// LoadSkills lists the repository's skills through the same reader the
// devcouncil_load_skill tool uses. The prompt's always-on listing calls this,
// so the names the model is shown and the names the tool accepts are one set.
func (r *Registry) LoadSkills(ctx context.Context) (harness.SkillSet, error) {
	return harness.LoadSkills(r.deps.Root, r.skillReader(ctx))
}

// loadSkill returns the body of one repository skill as a tool result.
//
// The skills are resolved on every call from the session root, the way
// read_file resolves its path: the tool keeps no state of its own, so it
// cannot disagree with the working tree, and the cost is a directory listing
// and at most harness.MaxSkills bounded reads.
//
// SECURITY IMPACT: none widened. A skill body is untrusted repository content
// entering the model's context through a tool result, the same way AGENTS.md
// does through the prompt; it is no more trusted than a file the model could
// read with read_file. Agents cannot author skills: DevCouncil's write policy
// hard-denies .agents/** (the ".agents/*" and ".agents/**" entries of the
// protected-path list in policy/file.go).
func (r *Registry) loadSkill(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Name string `json:"name"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	// Judged before the filesystem is touched: a name that cannot be a skill
	// directory is refused without being joined to any path.
	if !harness.ValidSkillName(args.Name) {
		return tools.Errorf("%q is not a skill name: it must match [a-z0-9][a-z0-9_-]*", args.Name)
	}
	set, err := r.LoadSkills(ctx)
	if err != nil {
		return unavailable("the skills list", err)
	}
	if s, found := set.Find(args.Name); found {
		return tools.Result{Text: TruncateRunes(s.Body, maxSkillBodyRunes)}
	}
	if reason, rejected := set.Rejection(args.Name); rejected {
		return tools.Errorf("skill %q was rejected: %s", args.Name, reason)
	}
	if len(set.Skills) == 0 {
		return tools.Errorf("no skill named %q: this repository has no skills under %s", args.Name, harness.SkillsDir)
	}
	names := make([]string, 0, len(set.Skills))
	for _, s := range set.Skills {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	return tools.Errorf("no skill named %q; the skills are: %s", args.Name, strings.Join(names, ", "))
}
