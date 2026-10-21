package devcouncil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/gate"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
)

// promptInputNames are the root files an agent must not be able to write: the
// first three are read into the next turn's system prompt
// (cmd/manvi/prompt.go projectInstructionsSource) and the last two declare MCP
// server commands the harness spawns (mcp/manager.go). A model holding a write
// lease could otherwise rewrite its own next prompt or add a command.
var promptInputNames = []string{"AGENTS.md", "CLAUDE.md", "GEMINI.md", "mcp.json", ".mcp.json"}

// pendingPin is what a failure here means. The rule lives in DevCouncil
// (policy.PromptInputPatterns), not in Manvi: path policy has one owner. This
// test is deliberately left failing on a build whose linked DevCouncil lacks it,
// rather than skipped, because a skip reads as a pass.
const pendingPin = "the linked DevCouncil does not refuse agent writes to prompt inputs: " +
	"dh-4 needs the DevCouncil patch applied and scripts/module-pins.txt bumped to it " +
	"(task ft-d4be683f7753622abb4af08414900e39)"

// TestAgentCannotWritePromptInputs drives devcouncil_write_file under a valid
// write lease that plans every one of the five files, so scope is not what
// refuses them, and under every posture that changes what the gate enforces. The
// refusal must be the restricted-path rule, hard, and must not be satisfiable by
// an override request. Reads of the same files must keep working: they are how
// the agent learns the project's rules.
func TestAgentCannotWritePromptInputs(t *testing.T) {
	postures := []string{flags.PostureStrict, flags.PostureDev, flags.PostureYolo}
	for _, posture := range postures {
		for _, name := range promptInputNames {
			t.Run(posture+"/"+name, func(t *testing.T) {
				f := newFixtureWith(t, map[string]string{flags.HarnessPosture: posture})
				seedPromptInputTask(t, f)
				if out := f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-002"}); out["acquired"] != true {
					t.Fatalf("checkout = %v", out)
				}
				path := filepath.Join(f.root, name)
				const original = "original\n"
				if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
					t.Fatal(err)
				}

				res := f.call("devcouncil_write_file", map[string]string{"path": name, "content": "rewritten\n"})
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != original {
					t.Fatalf("%s was rewritten under a lease (posture %s): %s", name, posture, pendingPin)
				}
				if !res.IsError || !res.Blocked || res.Rule != string(policy.RuleRestrictedPath) || res.Severity != string(policy.Hard) {
					t.Fatalf("write %s = error=%v blocked=%v rule=%q severity=%q text=%s; want a hard %s",
						name, res.IsError, res.Blocked, res.Rule, res.Severity, res.Text, policy.RuleRestrictedPath)
				}

				read := f.call("devcouncil_read_file", map[string]string{"path": name})
				if read.IsError || read.Text != original {
					t.Fatalf("read %s = error=%v %q; prompt inputs must stay readable", name, read.IsError, read.Text)
				}
			})
		}
	}
}

// TestAgentCannotWriteNestedPromptInputs: the instruction file Manvi reads is the
// working directory's, which is a subdirectory when the agent is launched in one,
// and the other clients load nested files too.
func TestAgentCannotWriteNestedPromptInputs(t *testing.T) {
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureYolo})
	seedPromptInputTask(t, f)
	if out := f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-002"}); out["acquired"] != true {
		t.Fatalf("checkout = %v", out)
	}
	res := f.call("devcouncil_write_file", map[string]string{"path": "docs/AGENTS.md", "content": "x\n"})
	if !res.Blocked || res.Rule != string(policy.RuleRestrictedPath) {
		t.Fatalf("write docs/AGENTS.md = blocked=%v rule=%q text=%s: %s", res.Blocked, res.Rule, res.Text, pendingPin)
	}
}

// TestGateRefusesPromptInputsUnderEveryPosture is the store-free form: it asks
// the harness's own gate (the one devcouncil_write_file consults) directly, so it
// needs no Rust binaries. It adds the skill roots, which devcouncil_load_skill
// serves to the model as instructions: under yolo the restricted rung is off, and
// an agent could otherwise write the skill it then loads. Reads stay allowed.
func TestGateRefusesPromptInputsUnderEveryPosture(t *testing.T) {
	names := append(append([]string{}, promptInputNames...),
		"docs/AGENTS.md", ".agents/skills/x/SKILL.md", ".claude/skills/x/SKILL.md", ".cursor/skills/x/SKILL.md")
	for _, posture := range []string{flags.PostureStrict, flags.PostureDev, flags.PostureYolo} {
		reg := flags.New()
		if err := flags.DefineHarnessFlags(reg); err != nil {
			t.Fatal(err)
		}
		if err := reg.LoadConfig(map[string]string{flags.HarnessPosture: posture}); err != nil {
			t.Fatal(err)
		}
		g, err := gate.New(reg, t.TempDir(), nil)
		if err != nil {
			t.Fatal(err)
		}
		task := &dc.Task{ID: "TASK-002", PlannedFiles: []dc.PlannedFile{{Path: "**", AllowedChange: dc.ChangeModify}}}
		for _, name := range names {
			d, err := g.EvaluateWrite(name, task, dc.OpWrite)
			if err != nil {
				t.Fatal(err)
			}
			if !d.Blocked() || d.Rule != policy.RuleRestrictedPath || d.Severity != policy.Hard {
				t.Errorf("%s: write %q = %s/%s/%s; want a hard %s: %s",
					posture, name, d.Action, d.Rule, d.Severity, policy.RuleRestrictedPath, pendingPin)
			}
			if r, err := g.EvaluateRead(name, task); err != nil || r.Blocked() {
				t.Errorf("%s: read %q = %+v (%v); prompt inputs must stay readable", posture, name, r, err)
			}
		}
	}
}

// seedPromptInputTask adds a task whose plan names every prompt input, so a
// refusal can only come from the path rules.
func seedPromptInputTask(t *testing.T, f *fixture) {
	t.Helper()
	planned := `[`
	for i, name := range append(append([]string{}, promptInputNames...), "docs/AGENTS.md") {
		if i > 0 {
			planned += ","
		}
		planned += `{"path":"` + name + `","allowed_change":"modify"}`
	}
	planned += `]`
	insertTask(t, storeBinary(t), f.db, "TASK-002", planned)
}
