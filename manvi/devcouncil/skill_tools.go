package devcouncil

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/harness"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// maxSkillBodyRunes bounds what one devcouncil_load_skill call puts in the
// model's context. harness.MaxSkillBytes bounds the file; this bounds the
// answer, so the two can be tuned apart. 32,000 runes is about eight thousand
// tokens: enough for a long procedure, small enough that loading one skill
// cannot displace the task.
const maxSkillBodyRunes = 32_000

// skillReader is the one reader every skill is read with, listing and single
// lookup alike: the repository's contained reader behind the full read gate, with
// no lighter path for any caller. A skill file is judged the way read_file
// judges any path, so a skill whose directory is named for a secret, or that
// is a hard link to something the gate withholds, is refused here exactly as it
// would be there.
func (r *Registry) skillReader(ctx context.Context) harness.ReadFunc {
	return RepoReaderAt(ctx, r.deps.Root)
}

// RepoReaderAt is that reader for a root with no Registry: what a host op uses
// to read a repository spec, judged by the same gate and read through the same
// pinned path, never by a second, lighter one.
func RepoReaderAt(ctx context.Context, root string) harness.ReadFunc {
	return func(rel string, limit int64) ([]byte, error) {
		if why := readRefusalAt(root, rel); why != "" {
			return nil, fmt.Errorf("%s is withheld by %s", rel, why)
		}
		return readContained(ctx, root, rel, limit)
	}
}

// RepoReader is the same contained, gated reader, handed out for the other
// repository text that reaches a prompt: harness specs under
// .devcouncil/harness. There is one reader for repository bytes; a second one
// for specs would be a second place the read gate could be forgotten.
func (r *Registry) RepoReader(ctx context.Context) harness.ReadFunc { return r.skillReader(ctx) }

// Root is the repository root RepoReader reads under, for a caller that has to
// place a path (the working directory) beneath it.
func (r *Registry) Root() string { return r.deps.Root }

// LoadSkills reads the repository's skills, at most harness.MaxSkills of them,
// for the prompt's always-on listing. The names the model is shown here and the
// names devcouncil_load_skill accepts come from the same loader.
//
// A session that opened without the skill tool lists no skills for its whole
// life: a listing that tells the model to call a tool it does not have would be
// worse than none, and a skill added mid-session is picked up by the next
// session, with the rest of its tool surface, so the prompt-cache prefix does
// not change under a running one.
func (r *Registry) LoadSkills(ctx context.Context) (harness.SkillSet, error) {
	if r.skillSurface.Load() == skillToolWithheld {
		return harness.SkillSet{}, nil
	}
	return r.readSkills(ctx)
}

func (r *Registry) readSkills(ctx context.Context) (harness.SkillSet, error) {
	return harness.LoadSkills(ctx, r.deps.Root, r.skillReader(ctx))
}

// loadSkillToolName is the one tool whose registration depends on the
// repository.
const loadSkillToolName = "devcouncil_load_skill"

const (
	skillToolUndecided int32 = iota
	skillToolOffered
	skillToolWithheld
)

// skillOpenTimeout bounds the one skills read a session's open makes, so a
// repository whose skills directory blocks cannot stall opening the session.
const skillOpenTimeout = 10 * time.Second

// decideSkillTool takes the session's decision and records it.
func (r *Registry) decideSkillTool() bool {
	ctx, cancel := context.WithTimeout(context.Background(), skillOpenTimeout)
	defer cancel()
	offer := r.skillToolOffered(ctx)
	if offer {
		r.skillSurface.Store(skillToolOffered)
	} else {
		r.skillSurface.Store(skillToolWithheld)
	}
	return offer
}

// skillToolOffered reports whether this session's surface carries
// devcouncil_load_skill: when the repository has at least one skill, or at
// least one skill directory the loader rejected, so a model told a skill was
// refused can still ask why. A repository with neither never pays the schema's
// tokens.
//
// It is decided once, at session open, and never per turn: the tool list is
// part of the prompt-cache prefix, and a surface that changed when someone
// added a skill would invalidate the prefix mid-session. The cost is that a
// skill added mid-session is invisible until the next session.
//
// A read that fails, a cancelled or timed-out one included, offers the tool.
// The failure is not evidence that there are no skills, and the tool is how
// the model sees the fault: it answers with the reason instead of the surface
// silently lacking a tool the repository may well have.
func (r *Registry) skillToolOffered(ctx context.Context) bool {
	set, err := r.readSkills(ctx)
	if err != nil {
		return true
	}
	return len(set.Skills) > 0 || len(set.Rejected) > 0 || set.RejectedOmitted > 0 || set.NotExamined > 0 || set.ListingCut
}

// NoteListedSkills records the skill names the system prompt just carried, in
// full or by name alone. The refusal of an unknown name in devcouncil_load_skill
// then offers the skills that are not among them. An empty list clears it.
func (r *Registry) NoteListedSkills(names []string) {
	if len(names) == 0 {
		r.listedSkills.Store(nil)
		return
	}
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	r.listedSkills.Store(&set)
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
	var listed map[string]bool
	if p := r.listedSkills.Load(); p != nil {
		listed = *p
	}
	l, err := harness.LookupSkillExcluding(ctx, r.deps.Root, r.skillReader(ctx), args.Name, listed)
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
	if l.ListingCut {
		// A name that is not among the entries read may be among the ones that
		// were not, so "no such skill" would be a claim the listing cannot make.
		return tools.Errorf("no skill named %s among the first %d entries of %s, which holds more than that and was not listed past them",
			harness.QuoteName(args.Name), harness.MaxSkillDirEntries, harness.SkillsDir)
	}
	if l.Directories == 0 {
		return tools.Errorf("no skill named %s: this repository has no skills under %s", harness.QuoteName(args.Name), harness.SkillsDir)
	}
	if l.AvailableTotal == 0 {
		return tools.Errorf("no skill named %s: every skill this repository has is already named in the system prompt", harness.QuoteName(args.Name))
	}
	more := ""
	if l.AvailableTotal > len(l.Available) {
		more = fmt.Sprintf(" and %d more", l.AvailableTotal-len(l.Available))
	}
	shown := "the skills directories are"
	if listed != nil {
		shown = "skills not named in the system prompt are"
	}
	return tools.Errorf("no skill named %s; %s: %s%s",
		harness.QuoteName(args.Name), shown, strings.Join(l.Available, ", "), more)
}
