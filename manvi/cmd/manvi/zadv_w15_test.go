package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// Adversarial review of lane w15-workwire: the work_* board tools reach the
// top-level agent of `manvi run` and the TUI and nothing else. Each test names
// the invariant it attacks.

// seenToolNames is every tool name any request of p offered, across all steps.
func seenToolNames(p *scriptedProvider) map[string]bool {
	out := map[string]bool{}
	for _, req := range p.seen {
		for _, s := range req.Tools {
			out[s.Name] = true
		}
	}
	return out
}

// boardAttempts is a child model that tries every route to the board: activate
// by name, then call each tool by name.
func boardAttempts() []llm.Response {
	return []llm.Response{
		wantToolCall("devcouncil_activate_tools", `{"tools":["work_create_item","work_list_items","work_update_own_item"]}`),
		wantToolCall("work_create_item", `{"title":"smuggled"}`),
		wantToolCall("work_list_items", `{}`),
		wantToolCall("work_update_own_item", `{"status":"review"}`),
		endTurn("done"),
	}
}

func assertChildHeldNoBoard(t *testing.T, p *scriptedProvider) {
	t.Helper()
	if held := workToolsIn(seenToolNames(p)); len(held) != 0 {
		t.Errorf("a child was offered %v on some step", held)
	}
	got := toolResultText(t, p)
	// Each board call must have been refused as "not offered" or "unknown",
	// never answered by the adapter (its answers carry a typed "error" code or
	// an items list).
	for _, marker := range []string{"repository_unmapped", "inherited_session", "own_task_unmapped", `"items"`, "lease_required"} {
		if strings.Contains(got, marker) {
			t.Errorf("a board adapter answered a child (%s):\n%s", marker, got)
		}
	}
}

// 1a. Dynamic activation inside a child. The child's registry is cut from the
// parent's by name, so activating by name must not resurrect a tool, and the
// child's calls must never reach the adapter.
func TestAdvW15ChildDynamicActivationCannotReachTheBoard(t *testing.T) {
	_, pipeline, err := agentTools(newTestRegistry(t), nil, "0123456789abcdef")
	if err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}
	pipeline.SetDynamicMode(true)
	for _, ro := range []bool{false, true} {
		p := &scriptedProvider{name: "p", serves: []string{"m"}, script: boardAttempts()}
		r := runnerOver(t, pipeline, p)
		if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{Label: "c", Prompt: "go", ReadOnly: ro}); err != nil {
			t.Fatalf("RunSubAgent(readOnly=%v): %v", ro, err)
		}
		assertChildHeldNoBoard(t, p)
	}
}

// 1a'. A child's devcouncil_activate_tools is a closure over the PARENT's tool
// registry. A child naming a work tool must not change what the parent offers:
// the parent's wire frame is frozen at its own first request, and a board write
// tool appearing in the parent's offered set is the parent's decision.
func TestAdvW15ChildActivationLeavesTheParentsOfferedSetAlone(t *testing.T) {
	_, pipeline, err := agentTools(newTestRegistry(t), nil, "0123456789abcdef")
	if err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}
	pipeline.SetDynamicMode(true)
	before := workToolsIn(activeNames(pipeline))
	if len(before) != 0 {
		t.Fatalf("the dynamic profile starts with %v active; the board tools are Extended", before)
	}
	p := &scriptedProvider{name: "p", serves: []string{"m"}, script: boardAttempts()}
	r := runnerOver(t, pipeline, p)
	if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{Label: "c", Prompt: "go"}); err != nil {
		t.Fatal(err)
	}
	if after := workToolsIn(activeNames(pipeline)); len(after) != 0 {
		t.Errorf("a child's activation changed the parent's offered set: %v are now active", after)
	}
}

func activeNames(p *tools.Registry) map[string]bool {
	out := map[string]bool{}
	for _, s := range p.ActiveSchemas() {
		out[s.Name] = true
	}
	return out
}

// 1b. An isolated-worktree child. ForWorktree copies Deps.Work and registers a
// fresh set of board tools on the re-rooted registry; the floor must still cut
// them (and the merged-in parent ones).
func TestAdvW15IsolatedWorktreeChildHoldsNoBoardTool(t *testing.T) {
	repo := initTestRepoForSubagents(t)
	t.Setenv("MANVI_PROJECT_ROOT", repo)
	t.Setenv("MANVI_POLICY_FILE_MODE", "advisory")
	native, pipeline, err := agentTools(newTestRegistry(t), nil, "0123456789abcdef")
	if err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}
	requireWorkSurface(t, "the parent", pipeline)
	wt := filepath.Join(t.TempDir(), "wt")
	git := func(args ...string) {
		t.Helper()
		// Background, not t.Context(): the cleanup below runs after the test's
		// context is cancelled.
		cmd := exec.CommandContext(context.Background(), "git", args...) //nolint:gosec // G204: literal git; args are this test's own fixture commands
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("worktree", "add", "-q", "-b", "w15-child", wt)
	t.Cleanup(func() { git("worktree", "remove", "--force", wt) })

	for _, ro := range []bool{false, true} {
		p := &scriptedProvider{name: "p", serves: []string{"m"}, script: boardAttempts()}
		models := llm.NewRegistry()
		if err := models.Register(p); err != nil {
			t.Fatal(err)
		}
		r := &subAgentRunner{}
		r.attach(subAgentConfig{
			provider: p, models: models, model: "m", registry: pipeline, native: native,
			systemPrompt: "PARENT", meter: &subAgentMeter{},
		})
		if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
			Label: "c", Prompt: "go", ReadOnly: ro, WorktreePath: wt,
		}); err != nil {
			t.Fatalf("RunSubAgent in a worktree (readOnly=%v): %v", ro, err)
		}
		assertChildHeldNoBoard(t, p)
	}
}

// 1c. A role that names the board tools, with the activation tools reachable.
func TestAdvW15RoleNamingBoardToolsStillHoldsNone(t *testing.T) {
	_, pipeline, err := agentTools(newTestRegistry(t), nil, "0123456789abcdef")
	if err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}
	pipeline.SetDynamicMode(true)
	surface := agents.Definition{
		Name: "namer", EnableWriteTools: true,
		AllowedTools: []string{
			"work_create_item", "work_update_own_item", "work_list_items", "work_get_item",
			"devcouncil_activate_tools", "devcouncil_search_tools",
		},
	}.Surface()
	p := &scriptedProvider{name: "p", serves: []string{"m"}, script: boardAttempts()}
	r := runnerOver(t, pipeline, p)
	if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{Label: "c", Prompt: "go", Surface: surface}); err != nil {
		t.Fatalf("RunSubAgent: %v", err)
	}
	assertChildHeldNoBoard(t, p)
}

// 1d. An escalation re-run builds a second child on a higher tier from the same
// request; it must be cut by the same floor.
func TestAdvW15EscalationRerunHoldsNoBoardTool(t *testing.T) {
	_, pipeline, err := agentTools(newTestRegistry(t), nil, "0123456789abcdef")
	if err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}
	p := &modelProvider{reasons: allModels()}
	p.answer = func(_ context.Context, req llm.Request, n int) (llm.Response, error) {
		if n == 1 {
			return wantToolCall("work_create_item", `{"title":"smuggled"}`), nil
		}
		if req.Model == "lite-model" {
			return endTurn(""), nil
		}
		return endTurn("the real answer"), nil
	}
	models := llm.NewRegistry()
	if err := models.Register(p); err != nil {
		t.Fatal(err)
	}
	r := &subAgentRunner{}
	r.attach(subAgentConfig{
		provider: p, models: models, model: "heavy-model", tiers: ladderProfile,
		registry: pipeline, systemPrompt: "PARENT", meter: &subAgentMeter{},
	})
	out, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label: "scan", Prompt: "find", ReadOnly: true, Tier: agents.TierLite,
		Escalation: devcouncil.NewEscalationBudget(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Escalations) != 1 {
		t.Fatalf("the test did not exercise an escalation: %+v", out.Escalations)
	}
	seen := p.requests()
	if len(seen) < 3 {
		t.Fatalf("expected both attempts to run, saw %d requests", len(seen))
	}
	for i, req := range seen {
		for _, s := range req.Tools {
			if devcouncil.IsWorkTool(s.Name) {
				t.Errorf("request %d (%s) offered %s", i, req.Model, s.Name)
			}
		}
	}
}

// 1e. Nested child: the dispatch tools are absent from a child, so there is no
// grandchild. Checked against the real agent surface with the board bound.
func TestAdvW15NoGrandchildPathExistsOnTheBoardSurface(t *testing.T) {
	_, pipeline, err := agentTools(newTestRegistry(t), nil, "0123456789abcdef")
	if err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}
	pipeline.SetDynamicMode(true)
	p := &scriptedProvider{name: "p", serves: []string{"m"}, script: []llm.Response{
		wantToolCall("devcouncil_spawn_subagents", `{"tasks":[{"label":"g","prompt":"x"}]}`),
		wantToolCall("devcouncil_invoke_subagent", `{"role":"self","prompt":"x"}`),
		endTurn("done"),
	}}
	r := runnerOver(t, pipeline, p)
	if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{Label: "c", Prompt: "go"}); err != nil {
		t.Fatal(err)
	}
	for name := range seenToolNames(p) {
		if strings.Contains(name, "subagent") || devcouncil.IsWorkTool(name) {
			t.Errorf("a child was offered %s", name)
		}
	}
	if got := toolResultText(t, p); strings.Count(got, "was not offered") < 2 {
		t.Errorf("the child's dispatch attempts were not both refused as not offered:\n%s", got)
	}
}

// 3. `manvi tool` (the CLI's single-call command) builds through nativeTools.
// It must hold no board tool, so it can neither be the agent's identity nor a
// way around the caps.
func TestAdvW15CLIToolCommandHasNoBoard(t *testing.T) {
	_, pipeline, err := nativeTools(newTestRegistry(t))
	if err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}
	for _, name := range workToolNames {
		var out bytes.Buffer
		err := callTool(&out, &out, nil, pipeline, []string{name, "--json", `{"title":"x"}`})
		if err == nil || !strings.Contains(err.Error(), "no native tool") {
			t.Errorf("manvi tool %s: err=%v out=%q, want an unknown-tool refusal", name, err, out.String())
		}
	}
}

// 3b. The TUI's /tool dispatches through the session's own pipeline, which is
// the agent's. It must therefore carry the agent's identity: the session's
// storeID as RunID, and the same refusals. With no lease the own-task write
// must refuse, so /tool cannot reach an own task by scope the agent lacks.
func TestAdvW15TUIToolIsTheSessionsPipelineUnderTheSameRefusals(t *testing.T) {
	repo := e2eRepo(t)
	t.Setenv("MANVI_PROJECT_ROOT", repo)
	s, err := newSessionState(newTestRegistry(t), "S1", "0123456789abcdef", nil)
	if err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}
	requireWorkSurface(t, "a TUI session", s.pipeline)
	var out bytes.Buffer
	err = callTool(&out, &out, nil, s.pipeline, []string{"work_update_own_item", "--json", `{"expected_revision":1,"status":"review"}`})
	if err == nil {
		t.Fatalf("/tool work_update_own_item with no lease succeeded: %s", out.String())
	}
	if !strings.Contains(out.String(), `"error"`) {
		t.Errorf("the refusal is not a typed board refusal: %q", out.String())
	}
	// A forged identity in the arguments is not an identity.
	out.Reset()
	_ = callTool(&out, &out, nil, s.pipeline, []string{"work_create_item", "--json", `{"title":"x","run_id":"forged","request_id":"agent_forged_s1_0","owner":"me"}`})
	if strings.Contains(out.String(), "forged") && !strings.Contains(out.String(), "unknown") && !strings.Contains(out.String(), "repository_unmapped") {
		t.Errorf("a forged identity field was not refused: %q", out.String())
	}
}

// repositoryRows counts rows in every table of the repo-local store whose name
// mentions "repositor", with the sqlite3 CLI. No database file at all is zero
// rows, verified. ok is false when sqlite3 is missing or the database has no
// such table, in which case the caller cannot conclude "none".
func repositoryRows(t *testing.T, repo string) (rows int, ok bool) {
	t.Helper()
	db := filepath.Join(repo, ".devcouncil", "state.sqlite")
	if _, err := os.Stat(db); err != nil {
		return 0, true
	}
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		return 0, false
	}
	run := func(sql string) string {
		cmd := exec.CommandContext(t.Context(), sqlite, "-readonly", db, sql) //nolint:gosec // G204: the sqlite3 CLI with this test's own fixed SQL
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("sqlite3 %q: %v\n%s", sql, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	tables := strings.Fields(run("select name from sqlite_master where type='table' and name like '%repositor%'"))
	if len(tables) == 0 {
		return 0, false
	}
	for _, tbl := range tables {
		var n int
		if err := json.Unmarshal([]byte(run("select count(*) from \""+tbl+"\"")), &n); err != nil {
			t.Fatal(err)
		}
		rows += n
	}
	return rows, true
}

// 5. The shipped binary, wired, against a repository whose store has no
// `this-repo` row: every board call refuses and the run must not leave a
// repository row behind (the "never created" rule).
func TestAdvW15HeadlessRunNeverCreatesTheLocalRepositoryRow(t *testing.T) {
	repo := e2eRepo(t)
	model := &e2eModel{reply: func(turn int) (map[string]any, string) {
		switch turn {
		case 1:
			return toolCall("work_list_items", map[string]any{})
		case 2:
			return toolCall("work_create_item", map[string]any{"title": "proposal"})
		case 3:
			return toolCall("work_get_item", map[string]any{"id": "this-repo"})
		}
		return map[string]any{"content": "ok"}, "stop"
	}}
	out, status := e2eRun(t, repo, model.start(t), "use the board", "MANVI_LLM_LOCAL_DYNAMIC_TOOLS=false")
	if status != 0 {
		t.Fatalf("exit %d\n%s", status, out)
	}
	seen := model.seen()
	if len(seen) < 4 {
		t.Fatalf("the board calls did not all run: %d requests\n%s", len(seen), out)
	}
	answers := marshal(t, seen[len(seen)-1].Messages)
	if !strings.Contains(answers, "repository_unmapped") {
		t.Errorf("a board call on a store with no this-repo row was not refused as repository_unmapped:\n%s", answers)
	}
	if rows, ok := repositoryRows(t, repo); ok && rows != 0 {
		t.Errorf("the run left %d repository row(s) in the repo-local store", rows)
	} else if !ok {
		t.Log("no repo-local store or repository table to inspect: the zero-row claim is not verified by this run")
	}
}

// 6. `manvi tools` in a repository with no skills reports the documented count.
func TestAdvW15ManviToolsCountMatchesTheDocs(t *testing.T) {
	t.Setenv("MANVI_PROJECT_ROOT", e2eRepo(t))
	var out bytes.Buffer
	if err := listTools(&out, newTestRegistry(t)); err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "\n50 tools, all native") {
		t.Errorf("manvi tools in a skill-less repository does not end with 50 tools:\n%s", text[max(0, len(text)-200):])
	}
	board := 0
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && devcouncil.IsWorkTool(fields[0]) {
			board++
			wantAccess := "read"
			if fields[0] == "work_create_item" || fields[0] == "work_update_own_item" {
				wantAccess = "write"
			}
			if fields[1] != wantAccess {
				t.Errorf("%s is listed as %s, want %s", fields[0], fields[1], wantAccess)
			}
		}
	}
	if board != 6 {
		t.Errorf("manvi tools lists %d board tools, want 6", board)
	}
}

// 7. The board's schemas. The wire frame freezes the tool list at a session's
// first request, so the schemas must be byte-stable across two hosts of the same
// repository: a run id or session id leaking into a description would change the
// frozen prefix on every process start and break resume. The size is measured
// and bounded so an unbounded growth of the six descriptions is caught.
func TestAdvW15BoardSchemasAreByteStableAndBounded(t *testing.T) {
	flagReg := newTestRegistry(t)
	schemas := func(id string) map[string]string {
		_, p, err := agentTools(flagReg, nil, id)
		if err != nil {
			t.Skipf("the native tool surface could not be built here: %v", err)
		}
		out := map[string]string{}
		for _, s := range p.Schemas() {
			if devcouncil.IsWorkTool(s.Name) {
				b, _ := json.Marshal(s)
				out[s.Name] = string(b)
			}
		}
		return out
	}
	a, b, c := schemas("0123456789abcdef"), schemas(""), schemas(strings.Repeat("z", 64))
	if len(a) != 6 {
		t.Fatalf("%d board schemas, want 6", len(a))
	}
	total := 0
	for name, sa := range a {
		total += len(sa)
		if sa != b[name] || sa != c[name] {
			t.Errorf("%s's schema depends on the run id", name)
		}
	}
	// ~4 bytes per token. Measured at review: see the report; the bound is a
	// generous ceiling, not a target.
	t.Logf("six board schemas: %d bytes (~%d tokens at 4 bytes/token)", total, total/4)
	if total > 24<<10 {
		t.Errorf("the six board schemas weigh %d bytes (~%d tokens): every non-dynamic first request carries them", total, total/4)
	}
}

// 7b. In a non-dynamic profile the first request carries all six schemas; in
// the dynamic profile it carries none. Measured on the real registry, offered
// set versus total, so the cost is a number and not an assumption.
func TestAdvW15FirstRequestCostOfTheBoardPerProfile(t *testing.T) {
	_, p, err := agentTools(newTestRegistry(t), nil, "0123456789abcdef")
	if err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}
	weigh := func(ss []llm.ToolSchema) (n, bytes int) {
		for _, s := range ss {
			if devcouncil.IsWorkTool(s.Name) {
				b, _ := json.Marshal(s)
				n++
				bytes += len(b)
			}
		}
		return n, bytes
	}
	n, bytesAll := weigh(p.ActiveSchemas())
	t.Logf("non-dynamic first request: %d board schemas, %d bytes (~%d tokens)", n, bytesAll, bytesAll/4)
	if n != 6 {
		t.Errorf("non-dynamic profile offers %d board schemas, want 6", n)
	}
	p.SetDynamicMode(true)
	if n, _ := weigh(p.ActiveSchemas()); n != 0 {
		t.Errorf("dynamic profile offers %d board schemas up front, want 0", n)
	}
}
