package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// devcouncil_load_skill is registered when a session opens, and only when the
// repository the session resolves has skills. These tests open the session the
// way every face does, through nativeToolsWith, and read the tool surface and
// the prompt listing off it.

const skillToolName = "devcouncil_load_skill"

func plantSkill(t *testing.T, root, name string) {
	t.Helper()
	dir := filepath.Join(root, ".agents", "skills", name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: reviews " + name + "\n---\nsteps\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// markedRepo is a directory the project-root search stops at.
func markedRepo(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	return root
}

func openSkillSession(t *testing.T, wd string) (*devcouncil.Registry, *tools.Registry) {
	t.Helper()
	t.Chdir(wd)
	native, pipeline, err := nativeToolsWith(newTestRegistry(t), nil)
	if err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}
	return native, pipeline
}

func surfaceHasSkillTool(p *tools.Registry) bool {
	for _, s := range p.Schemas() {
		if s.Name == skillToolName {
			return true
		}
	}
	return p.Has(skillToolName)
}

func TestSessionWithoutSkillsHasNeitherTheToolNorTheSection(t *testing.T) {
	root := markedRepo(t)
	native, pipeline := openSkillSession(t, root)
	if surfaceHasSkillTool(pipeline) {
		t.Fatalf("a repository with no skills was offered %s", skillToolName)
	}
	if res := pipeline.Run(context.Background(), tools.Call{ID: "c", Name: skillToolName, Arguments: []byte(`{"name":"x"}`)}); !res.IsError || !strings.Contains(res.Text, "unknown tool") {
		t.Fatalf("calling the absent tool = %v %q, want an unknown-tool error", res.IsError, res.Text)
	}

	src, notices := skillsForPrompt(context.Background(), native, 4096)
	if src != nil || len(notices) != 0 {
		t.Fatalf("skillsForPrompt = %v, %v; want no section and no notice", src, notices)
	}
	text, _, _ := assembleWithSkills(t, newTestRegistry(t), "anthropic", native)
	if strings.Contains(text, skillToolName) {
		t.Fatal("the prompt names a tool this session does not have")
	}

	// A child inherits the parent's surface.
	child := pipeline.Subset(func(tools.Tool) bool { return true })
	if surfaceHasSkillTool(child) {
		t.Fatal("a child of a no-skills session got the tool")
	}
}

func TestSessionWithSkillsHasTheToolAndTheSection(t *testing.T) {
	root := markedRepo(t)
	plantSkill(t, root, "go-review")
	native, pipeline := openSkillSession(t, root)
	if !surfaceHasSkillTool(pipeline) {
		t.Fatalf("a repository with skills was not offered %s", skillToolName)
	}
	text, _, _ := assembleWithSkills(t, newTestRegistry(t), "anthropic", native)
	if !strings.Contains(text, skillToolName) || !strings.Contains(text, "go-review") {
		t.Fatalf("the prompt does not list the skill and name its tool:\n%s", text)
	}
	if child := pipeline.Subset(func(tools.Tool) bool { return true }); !surfaceHasSkillTool(child) {
		t.Fatal("a child of a skills session lost the tool")
	}
}

// A session opened in a subdirectory resolves the repository's root, so the
// skills at the root decide it.
func TestSessionOpenedInASubdirectoryDecidesFromTheRepositoryRoot(t *testing.T) {
	root := markedRepo(t)
	plantSkill(t, root, "go-review")
	sub := filepath.Join(root, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	_, pipeline := openSkillSession(t, sub)
	if !surfaceHasSkillTool(pipeline) {
		t.Fatal("a session in a subdirectory ignored the repository's skills")
	}

	// And the converse: skills only in the subdirectory are not the repository's.
	other := markedRepo(t)
	nested := filepath.Join(other, "pkg")
	plantSkill(t, nested, "nested-only")
	_, pipeline = openSkillSession(t, nested)
	if surfaceHasSkillTool(pipeline) {
		t.Fatal("skills under a subdirectory, not the repository root, offered the tool")
	}
}

// DEVCOUNCIL_ROOT names the repository, so it decides, not the working directory.
func TestSessionRootOverrideDecidesTheSkillTool(t *testing.T) {
	withSkills := markedRepo(t)
	plantSkill(t, withSkills, "go-review")
	without := markedRepo(t)

	t.Setenv("MANVI_PROJECT_ROOT", "")
	t.Setenv("MANVI_ROOT", "")
	t.Setenv("DEVCOUNCIL_ROOT", withSkills)
	if _, pipeline := openSkillSession(t, without); !surfaceHasSkillTool(pipeline) {
		t.Fatal("DEVCOUNCIL_ROOT at a repository with skills did not offer the tool")
	}

	t.Setenv("DEVCOUNCIL_ROOT", without)
	if _, pipeline := openSkillSession(t, withSkills); surfaceHasSkillTool(pipeline) {
		t.Fatal("DEVCOUNCIL_ROOT at a repository without skills still offered the tool because the working directory has some")
	}
}
