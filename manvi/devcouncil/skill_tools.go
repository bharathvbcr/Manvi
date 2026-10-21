package devcouncil

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
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

// skillReader is the reader a single skill is read with: the repository's
// contained reader, behind the full read gate. A skill file is judged the way
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

// listingReader is skillReader for reading a whole directory of skills.
//
// The full read gate lists every directory on the path to find the on-disk
// spelling of each name, and the one that matters here, .agents/skills, is the
// directory with all the skills in it: a read per skill is a listing of every
// skill per skill. Measured on a hostile tree of 10,000 skill directories that
// was 3.3 s for 200 reads, on every prompt assembly. So the full gate judges the
// first read, which settles the shared prefix (.agents and .agents/skills are
// not links to somewhere withheld), and each later read is judged by what can
// still differ between siblings: the spelling of its own path and its link
// count. A skill directory that is itself a symbolic link, or whose state
// cannot be read, gets the full gate again.
func (r *Registry) listingReader(ctx context.Context) harness.ReadFunc {
	prefixJudged := false
	return func(rel string, limit int64) ([]byte, error) {
		fi, err := os.Lstat(filepath.Join(r.deps.Root, filepath.FromSlash(path.Dir(rel))))
		linked := err != nil || fi.Mode()&fs.ModeSymlink != 0
		var why string
		if !prefixJudged || linked {
			prefixJudged = true
			why = r.readRefusal(rel)
		} else {
			why = r.siblingReadRefusal(rel)
		}
		if why != "" {
			return nil, fmt.Errorf("%s is withheld by %s", rel, why)
		}
		return readContained(ctx, r.deps.Root, rel, limit)
	}
}

// LoadSkills reads the repository's skills, at most harness.MaxSkills of them,
// for the prompt's always-on listing. The names the model is shown here and the
// names devcouncil_load_skill accepts come from the same loader.
func (r *Registry) LoadSkills(ctx context.Context) (harness.SkillSet, error) {
	return harness.LoadSkills(r.deps.Root, r.listingReader(ctx))
}

// loadSkill returns the body of one repository skill as a tool result.
//
// It reads and validates only the requested directory, so its cost does not
// grow with the number of skills. Skills are resolved on every call from the
// session root, the way read_file resolves its path: the tool keeps no state of
// its own, so it cannot disagree with the working tree.
//
// SECURITY IMPACT: none widened. A skill body is untrusted repository content
// entering the model's context through a tool result, the same way AGENTS.md
// does through the prompt; it is no more trusted than a file the model could
// read with read_file. Whether an agent can author a skill depends on the
// posture. DevCouncil's write policy denies .agents/** (the ".agents/*" and
// ".agents/**" entries of the protected-path list in policy/file.go), but that
// rung is inside the hard-rule block, which the yolo posture switches off, so
// under yolo an agent can write .agents/skills/** and then load what it wrote.
// Until dh-4 (ft-d4be683f) makes prompt-input protection posture-independent,
// that is the one posture where "a model cannot author its own skills" does
// not hold; TestAgentSkillAuthoringByPosture pins the current behaviour and
// fails when it changes.
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
		return tools.Errorf("%s is not a skill name: it must match [a-z0-9][a-z0-9_-]* and be at most %d characters",
			harness.QuoteName(args.Name), harness.MaxSkillNameLen)
	}
	l, err := harness.LookupSkill(r.deps.Root, r.skillReader(ctx), args.Name)
	if err != nil {
		return unavailable("the skills list", err)
	}
	switch l.Outcome {
	case harness.SkillFound:
		return tools.Result{Text: TruncateRunes(l.Skill.Body, maxSkillBodyRunes)}
	case harness.SkillRejected:
		if l.Dir != args.Name {
			return tools.Errorf("%s exists but was rejected: %s", harness.QuoteName(l.Dir), l.Reason)
		}
		return tools.Errorf("skill %s was rejected: %s", harness.QuoteName(args.Name), l.Reason)
	}
	if l.AvailableTotal == 0 {
		return tools.Errorf("no skill named %s: this repository has no skills under %s", harness.QuoteName(args.Name), harness.SkillsDir)
	}
	more := ""
	if l.AvailableTotal > len(l.Available) {
		more = fmt.Sprintf(" and %d more", l.AvailableTotal-len(l.Available))
	}
	return tools.Errorf("no skill named %s; the skills directories are: %s%s",
		harness.QuoteName(args.Name), strings.Join(l.Available, ", "), more)
}
