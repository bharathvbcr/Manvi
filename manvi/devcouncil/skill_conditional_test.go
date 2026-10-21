package devcouncil

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/agent"
	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/harness"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// The skill tool is registered when a session opens, and only when the
// repository has skills. These tests hold that rule from every side the
// registry can be asked: its schemas, its core profile, its dispatch, a child
// surface, and the prompt listing that must agree with all of them.

const goodSkill = "---\nname: %s\ndescription: d\n---\nbody of %s\n"

func addSkill(t *testing.T, root, name string) {
	t.Helper()
	writeSkillFile(t, root, name, strings.ReplaceAll(goodSkill, "%s", name))
}

// openSession is what a session open does to the tool surface.
func (f *skillFixture) openSession(t *testing.T) *tools.Registry {
	t.Helper()
	pipe := tools.NewRegistry(bus.New())
	if err := f.reg.Register(pipe); err != nil {
		t.Fatalf("register: %v", err)
	}
	return pipe
}

func hasSchema(schemas []llm.ToolSchema, name string) bool {
	for _, s := range schemas {
		if s.Name == name {
			return true
		}
	}
	return false
}

func runSkill(pipe *tools.Registry, name string) tools.Result {
	return pipe.Run(context.Background(), tools.Call{
		ID: "c1", Name: loadSkillToolName, Arguments: json.RawMessage(`{"name":"` + name + `"}`),
	})
}

func requireAbsent(t *testing.T, pipe *tools.Registry) {
	t.Helper()
	if hasSchema(pipe.Schemas(), loadSkillToolName) || hasSchema(pipe.CoreSchemas(), loadSkillToolName) || pipe.Has(loadSkillToolName) {
		t.Fatalf("%s is registered, want it absent", loadSkillToolName)
	}
	res := runSkill(pipe, "anything")
	if !res.IsError || !strings.Contains(res.Text, "unknown tool") {
		t.Fatalf("calling the absent tool = %v %q, want an unknown-tool error", res.IsError, res.Text)
	}
}

func requirePresent(t *testing.T, pipe *tools.Registry) {
	t.Helper()
	if !hasSchema(pipe.Schemas(), loadSkillToolName) || !hasSchema(pipe.CoreSchemas(), loadSkillToolName) || !pipe.Has(loadSkillToolName) {
		t.Fatalf("%s is not registered in the full and core surfaces, want it present", loadSkillToolName)
	}
}

func TestNoSkillsDirectoryWithholdsTheSkillTool(t *testing.T) {
	f := newSkillFixture(t)
	pipe := f.openSession(t)
	requireAbsent(t, pipe)

	// The prompt listing agrees: no skills section, so nothing tells the model
	// to call a tool it does not have.
	set, err := f.reg.LoadSkills(context.Background())
	if err != nil || len(set.Skills) != 0 || len(set.Rejected) != 0 {
		t.Fatalf("LoadSkills = %+v, %v; want an empty set", set, err)
	}
}

func TestEmptySkillsDirectoryWithholdsTheSkillTool(t *testing.T) {
	f := newSkillFixture(t)
	if err := os.MkdirAll(filepath.Join(f.root, harness.SkillsDir), 0o750); err != nil {
		t.Fatal(err)
	}
	requireAbsent(t, f.openSession(t))
}

func TestARepositoryWithSkillsKeepsTheSkillTool(t *testing.T) {
	f := newSkillFixture(t)
	addSkill(t, f.root, "go-review")
	pipe := f.openSession(t)
	requirePresent(t, pipe)
	if res := runSkill(pipe, "go-review"); res.IsError || res.Text != "body of go-review\n" {
		t.Fatalf("load = %v %q", res.IsError, res.Text)
	}
}

// A repository whose only skill was rejected still gets the tool: the prompt
// names the rejection, and the model's way to ask why is this tool.
func TestOnlyRejectedSkillsStillOfferTheSkillTool(t *testing.T) {
	f := newSkillFixture(t)
	writeSkillFile(t, f.root, "broken", "no front matter at all")
	pipe := f.openSession(t)
	requirePresent(t, pipe)
	res := runSkill(pipe, "broken")
	if !res.IsError || !strings.Contains(res.Text, "was rejected") {
		t.Fatalf("load of a rejected skill = %v %q, want the rejection reason", res.IsError, res.Text)
	}
}

func TestAnInvalidlyNamedSkillDirectoryIsARejectionAndOffersTheTool(t *testing.T) {
	f := newSkillFixture(t)
	if err := os.MkdirAll(filepath.Join(f.root, harness.SkillsDir, "Bad Name"), 0o750); err != nil {
		t.Fatal(err)
	}
	requirePresent(t, f.openSession(t))
}

// A sub-agent's surface is a subset of its parent's, so it inherits the
// parent's decision and cannot reintroduce the tool.
func TestChildSurfaceInheritsTheSkillDecision(t *testing.T) {
	keepAll := func(tools.Tool) bool { return true }

	bare := newSkillFixture(t)
	requireAbsent(t, bare.openSession(t).Subset(keepAll))

	withSkills := newSkillFixture(t)
	addSkill(t, withSkills.root, "go-review")
	requirePresent(t, withSkills.openSession(t).Subset(keepAll))
}

// The decision is the session's. A skill added after open leaves the tool
// surface, and the listing, as they were; the next session sees it.
func TestSkillAddedMidSessionAppearsNextSession(t *testing.T) {
	f := newSkillFixture(t)
	pipe := f.openSession(t)
	requireAbsent(t, pipe)

	addSkill(t, f.root, "late")
	requireAbsent(t, pipe)
	set, err := f.reg.LoadSkills(context.Background())
	if err != nil || len(set.Skills) != 0 {
		t.Fatalf("a withheld session listed %+v, %v: the listing must not name a skill whose tool is absent", set.Skills, err)
	}

	next := &skillFixture{root: f.root, reg: &Registry{deps: Deps{Root: f.root}, session: &Session{}}}
	nextPipe := next.openSession(t)
	requirePresent(t, nextPipe)
	set, err = next.reg.LoadSkills(context.Background())
	if err != nil || len(set.Skills) != 1 {
		t.Fatalf("next session listed %+v, %v; want the new skill", set.Skills, err)
	}
}

// And the other way: a skill removed mid-session leaves the tool registered,
// so the surface does not shift under a running prefix. The tool answers
// truthfully that the repository has no skills.
func TestSkillRemovedMidSessionKeepsTheToolRegistered(t *testing.T) {
	f := newSkillFixture(t)
	addSkill(t, f.root, "going")
	pipe := f.openSession(t)
	requirePresent(t, pipe)
	if err := os.RemoveAll(filepath.Join(f.root, harness.SkillsDir)); err != nil {
		t.Fatal(err)
	}
	requirePresent(t, pipe)
	if res := runSkill(pipe, "going"); !res.IsError || !strings.Contains(res.Text, "no skills") {
		t.Fatalf("load after removal = %v %q", res.IsError, res.Text)
	}
}

// A skills directory that cannot be read is not "no skills": the tool is
// offered, and answering with the fault is how the model learns of it.
func TestUnreadableSkillsDirectoryOffersTheToolAndReportsTheFault(t *testing.T) {
	f := newSkillFixture(t)
	addSkill(t, f.root, "hidden")
	dir := filepath.Join(f.root, harness.SkillsDir)
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	// A directory needs its search bit back for TempDir's cleanup to remove it.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // G302: a directory, not a file; 0600 would make it unremovable
	if _, err := os.ReadDir(dir); err == nil {
		t.Skip("directory permissions are not enforced here (running as root?)")
	}
	pipe := f.openSession(t)
	requirePresent(t, pipe)
	res := runSkill(pipe, "hidden")
	if !res.IsError {
		t.Fatalf("an unreadable skills directory answered %q, want the fault", res.Text)
	}
}

func TestSkillsPathThatIsAFileOffersTheTool(t *testing.T) {
	f := newSkillFixture(t)
	writeRepoFile(t, f.root, harness.SkillsDir, "not a directory")
	requirePresent(t, f.openSession(t))
}

// A cancelled open cannot read the skills, and a failed read says nothing about
// whether there are any: the tool is offered rather than silently dropped.
func TestCancelledOpenWithSkillsOffersTheTool(t *testing.T) {
	f := newSkillFixture(t)
	addSkill(t, f.root, "go-review")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !f.reg.skillToolOffered(ctx) {
		t.Fatal("a cancelled read withheld the tool from a repository that has skills")
	}
}

// With no skills directory there is nothing to read, so cancellation has
// nothing to interrupt and the answer is still the true one.
func TestCancelledOpenWithoutSkillsStillWithholds(t *testing.T) {
	f := newSkillFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if f.reg.skillToolOffered(ctx) {
		t.Fatal("a repository with no skills was offered the tool")
	}
}

// TestSkillToolSavingOnTheLeanLocalProfile measures what withholding costs and
// saves, with the repo's own estimator, on the core (lean local) tool set.
func TestSkillToolSavingOnTheLeanLocalProfile(t *testing.T) {
	bare := newSkillFixture(t)
	lean := agent.CountToolTokens(bare.openSession(t).CoreSchemas())

	full := newSkillFixture(t)
	addSkill(t, full.root, "go-review")
	with := agent.CountToolTokens(full.openSession(t).CoreSchemas())

	if lean >= with {
		t.Fatalf("withholding saved nothing: lean core %d tokens, with the tool %d", lean, with)
	}
	t.Logf("lean local core set: %d tokens without devcouncil_load_skill, %d with it; saving %d tokens (%.2f%% of the set, %.2f%% of a 4096-token budget)",
		lean, with, with-lean, 100*float64(with-lean)/float64(with), 100*float64(with-lean)/4096)
}
