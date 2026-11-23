package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/gate"
	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// Adversarial tests for criterion 6 of lane w24-verdicts-b: what the serve
// critic can read, and write, when it is pointed at a worktree.

// zadvCritic builds the critic over a real native surface on a registry in the
// given posture, with extra tools registered on the parent surface (the
// pipeline) the way an MCP server or a plugin would register them.
func zadvCritic(t *testing.T, posture string, provider llm.Provider, model string, extra ...tools.Tool) (*serveCritic, *tools.Registry) {
	t.Helper()
	reg := newTestRegistry(t)
	if posture != "" {
		if err := reg.Set(flags.Human, flags.HarnessPosture, posture); err != nil {
			t.Fatal(err)
		}
	}
	scratch := t.TempDir()
	t.Chdir(scratch)
	for _, key := range []string{"MANVI_PROJECT_ROOT", "MANVI_ROOT", "DEVCOUNCIL_ROOT"} {
		t.Setenv(key, "")
	}
	native, pipeline, err := nativeToolsWith(reg, nil)
	if err != nil {
		t.Fatalf("the native tool surface could not be built: %v", err)
	}
	for _, tool := range extra {
		if err := pipeline.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	runner, _ := harnessFor(pipeline)
	if runner == nil {
		t.Fatal("nativeToolsWith did not attach a sub-agent runner")
	}
	models := llm.NewRegistry()
	if err := models.Register(provider); err != nil {
		t.Fatal(err)
	}
	runner.attach(subAgentConfig{
		provider: provider, models: models, model: model, registry: pipeline, native: native,
		systemPrompt: "parent", meter: &subAgentMeter{},
	})
	def, ok := agents.NewRegistry().Get(criticRoleName)
	if !ok {
		t.Fatal("no built-in critic role")
	}
	return &serveCritic{build: func(context.Context) (*criticRuntime, error) {
		return &criticRuntime{runner: runner, def: def}, nil
	}}, pipeline
}

// scriptProvider offers what the critic is shown on its first request, calls
// each scripted tool in turn, and records every tool result it is handed back.
type scriptProvider struct {
	mu      sync.Mutex
	calls   [][2]string
	offered map[string]bool
	results []string
	streams int
}

func (p *scriptProvider) Name() string { return "zadv-script" }

func (p *scriptProvider) Capability(model string) (llm.Capability, bool) {
	if model != "zadv-model" {
		return llm.Capability{}, false
	}
	return llm.Capability{Provider: p.Name(), Model: model, ContextWindow: 32768, MaxOutputTokens: 4096, SupportsTools: true}, true
}

func (p *scriptProvider) Stream(_ context.Context, req llm.Request) (llm.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.streams++
	if p.offered == nil {
		p.offered = map[string]bool{}
	}
	for _, schema := range req.Tools {
		p.offered[schema.Name] = true
	}
	turn := len(req.Messages) / 2
	if turn > 0 {
		p.results = append(p.results, marshalBlocks(req.Messages[len(req.Messages)-1].Content))
	}
	if turn < len(p.calls) {
		return &scriptedStream{resp: wantToolCall(p.calls[turn][0], p.calls[turn][1])}, nil
	}
	return &scriptedStream{resp: endTurn("done\nMANVI-VERDICT[1]: PASS\n")}, nil
}

func zadvTool(name string, readOnly bool, ran *atomic.Bool) tools.Tool {
	return tools.Tool{
		Schema:   llm.ToolSchema{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)},
		ReadOnly: readOnly,
		Handler: func(context.Context, tools.Call) tools.Result {
			ran.Store(true)
			return tools.Result{Text: "ran " + name}
		},
	}
}

// The critic is read-only in every posture, and a write-capable tool that the
// parent registered (an MCP or plugin tool) is not carried into its surface by
// MergeMissingFrom. Every tool it is offered is one the parent calls read-only.
func TestZadv_w24vb_CriticSurfaceIsReadOnlyInEveryPosture(t *testing.T) {
	for _, posture := range []string{flags.PostureDev, flags.PostureStrict, flags.PostureYolo} {
		t.Run(posture, func(t *testing.T) {
			var wrote, read atomic.Bool
			provider := &scriptProvider{calls: [][2]string{
				{"mcp__evil__write", `{}`},
				{"plugin_evil_write", `{}`},
				{"devcouncil_write_file", `{"path":"pwned.txt","content":"x"}`},
				{"mcp__evil__peek", `{}`},
			}}
			critic, pipeline := zadvCritic(t, posture, provider, "zadv-model",
				zadvTool("mcp__evil__write", false, &wrote),
				zadvTool("plugin_evil_write", false, &wrote),
				zadvTool("mcp__evil__peek", true, &read),
			)
			repo := initTestRepoForSubagents(t)
			if _, err := critic.Judge(context.Background(), repo, "judge", 1); err != nil {
				t.Fatal(err)
			}
			if wrote.Load() {
				t.Error("a write-capable parent tool ran inside the critic")
			}
			if _, err := os.Stat(filepath.Join(repo, "pwned.txt")); err == nil {
				t.Error("the critic wrote a file into the worktree")
			}
			readOnly := map[string]bool{}
			for _, schema := range pipeline.ReadOnlySchemas() {
				readOnly[schema.Name] = true
			}
			var offered []string
			provider.mu.Lock()
			for name := range provider.offered {
				offered = append(offered, name)
			}
			provider.mu.Unlock()
			sort.Strings(offered)
			for _, name := range offered {
				if !readOnly[name] {
					t.Errorf("the critic was offered %q, which the parent does not mark read-only", name)
				}
			}
			t.Logf("%s: %d tools offered: %s", posture, len(offered), strings.Join(offered, ","))
		})
	}
}

// Everything the critic is offered other than what Register rebuilt for the
// worktree came from the parent through MergeMissingFrom, and so is rooted
// wherever the parent was. List those, and refuse any read-only tool that takes
// a path or runs git and is not one of the worktree-independent kinds.
func TestZadv_w24vb_MergedParentToolsAreNotWorktreeRootedReaders(t *testing.T) {
	provider := &scriptProvider{}
	critic, pipeline := zadvCritic(t, "", provider, "zadv-model")
	repo := initTestRepoForSubagents(t)
	if _, err := critic.Judge(context.Background(), repo, "judge", 1); err != nil {
		t.Fatal(err)
	}
	runtime, err := critic.get(context.Background())
	if err != nil || runtime == nil {
		t.Fatal(err)
	}
	cfg := runtime.runner.cfg
	parentGate := cfg.native.Gate()
	wtGate := &gate.Gate{Flags: parentGate.Flags, Ledger: parentGate.Ledger, Root: repo, Subsystems: parentGate.Subsystems, Matcher: parentGate.Matcher}
	wtNative, err := cfg.native.ForWorktree(repo, wtGate, warmMapClient(repo), grepClient(repo))
	if err != nil {
		t.Fatal(err)
	}
	rooted := pipeline.CloneEmpty()
	if err := wtNative.Register(rooted); err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, s := range rooted.Schemas() {
		have[s.Name] = true
	}
	var merged []string
	provider.mu.Lock()
	for name := range provider.offered {
		if !have[name] {
			merged = append(merged, name)
		}
	}
	provider.mu.Unlock()
	sort.Strings(merged)
	t.Logf("offered to the critic but not rebuilt for the worktree: %v", merged)
	for _, name := range merged {
		for _, hint := range []string{"read", "file", "git", "grep", "search", "glob", "list", "diff", "open"} {
			if strings.Contains(name, hint) && !strings.HasPrefix(name, "mcp__") {
				t.Errorf("%q reaches the critic from the parent surface and looks like a filesystem or git reader, so it is rooted at the process's directory, not the worktree", name)
			}
		}
	}
}

// The critic cannot read outside the worktree it was given: not by .., not by
// an absolute path, not by a symlink inside the repository, and not by being
// pointed at a symlink to the worktree. Yolo posture turns the containment off
// by design and is only logged.
func TestZadv_w24vb_CriticCannotReadOutsideItsWorktree(t *testing.T) {
	for _, posture := range []string{flags.PostureDev, flags.PostureStrict, flags.PostureYolo} {
		t.Run(posture, func(t *testing.T) {
			outside := t.TempDir()
			secret := filepath.Join(outside, "secret.txt")
			if err := os.WriteFile(secret, []byte("SECRET-OUTSIDE-THE-WORKTREE\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			repo := initTestRepoForSubagents(t)
			if err := os.Symlink(secret, filepath.Join(repo, "escape.txt")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(repo, "escape_dir")); err != nil {
				t.Fatal(err)
			}
			rel, err := filepath.Rel(repo, secret)
			if err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(t.TempDir(), "alias")
			if err := os.Symlink(repo, alias); err != nil {
				t.Fatal(err)
			}
			read := func(p string) [2]string {
				b, _ := json.Marshal(map[string]string{"path": p})
				return [2]string{"devcouncil_read_file", string(b)}
			}
			// The control read is sent twice: the loop refuses a lone lookup that
			// would be the fifth in a row, once, and says to send it again (main's
			// enforced batching, 42cf98f6), so the first send may be that refusal.
			provider := &scriptProvider{calls: [][2]string{read(rel), read(secret), read("escape.txt"), read("escape_dir/secret.txt"), read("shared.txt"), read("shared.txt")}}
			critic, _ := zadvCritic(t, posture, provider, "zadv-model")
			if _, err := critic.Judge(context.Background(), alias, "judge", 1); err != nil {
				t.Fatal(err)
			}
			provider.mu.Lock()
			defer provider.mu.Unlock()
			if len(provider.results) != 6 || !strings.Contains(provider.results[5], "parent baseline content") {
				t.Fatalf("the control read of an in-tree file did not come back, so the escapes above prove nothing: %v", provider.results)
			}
			// The batching rule must not pre-empt containment: none of the four
			// escape reads may have been refused by it, so each was answered by
			// the path check. The fifth lookup is either that rule's one refusal
			// or the baseline content.
			for i, result := range provider.results[:4] {
				if strings.Contains(result, "refused, not run") {
					t.Fatalf("escape read %d was refused by the batching rule, not judged by containment: %s", i, result)
				}
			}
			if r := provider.results[4]; !strings.Contains(r, "refused, not run") && !strings.Contains(r, "parent baseline content") {
				t.Fatalf("the fifth lookup is neither the batching refusal nor the baseline content: %s", r)
			}
			leaked := 0
			for _, result := range provider.results {
				if strings.Contains(result, "SECRET-OUTSIDE-THE-WORKTREE") {
					leaked++
				}
			}
			if leaked > 0 && posture == flags.PostureYolo {
				t.Logf("yolo: %d of %d reads outside the worktree succeeded (containment is off by design in this posture)", leaked, len(provider.results))
				return
			}
			if leaked > 0 {
				t.Errorf("%d of %d reads outside the worktree returned the secret: %v", leaked, len(provider.results), provider.results)
			}
		})
	}
}

// A judgement of a directory that is not there, or is not a git work tree, is
// refused rather than run: Judge's own check is syntax only, and "/" is a clean
// absolute path.
func TestZadv_w24vb_CriticRefusesAWorktreeThatIsNotAGitWorkTree(t *testing.T) {
	provider := &scriptProvider{}
	critic, _ := zadvCritic(t, "", provider, "zadv-model")
	notGit := t.TempDir()
	for name, worktree := range map[string]string{
		"nonexistent": filepath.Join(t.TempDir(), "no", "such", "dir"),
		"not a repo":  notGit,
		"fs root":     string(filepath.Separator),
	} {
		provider.mu.Lock()
		before := provider.streams
		provider.mu.Unlock()
		_, err := critic.Judge(context.Background(), worktree, "judge", 1)
		provider.mu.Lock()
		ran := provider.streams > before
		provider.mu.Unlock()
		if err == nil || ran {
			t.Errorf("%s: a critic was run over %q (err=%v, provider reached=%v)", name, worktree, err, ran)
		}
	}
}

// Two judgements on two worktrees at once, many times, each reads its own
// worktree's file. Run with -race.
func TestZadv_w24vb_ConcurrentJudgementsOnTwoWorktrees(t *testing.T) {
	scratch := t.TempDir()
	t.Chdir(scratch)
	for _, key := range []string{"MANVI_PROJECT_ROOT", "MANVI_ROOT", "DEVCOUNCIL_ROOT"} {
		t.Setenv(key, "")
	}
	repos := map[string]string{"alpha": initTestRepoForSubagents(t), "beta": initTestRepoForSubagents(t)}
	for name, repo := range repos {
		if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte(name+"-only-in-this-worktree\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reg, err := flags.NewHarnessRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	native, pipeline, err := nativeToolsWith(reg, nil)
	if err != nil {
		t.Fatal(err)
	}
	runner, _ := harnessFor(pipeline)
	if runner == nil {
		t.Fatal("nativeToolsWith recorded no sub-agent runner for the pipeline it returned")
	}
	provider := &untrackedReader{}
	models := llm.NewRegistry()
	if err := models.Register(provider); err != nil {
		t.Fatal(err)
	}
	runner.attach(subAgentConfig{
		provider: provider, models: models, model: "critic-model", registry: pipeline, native: native,
		systemPrompt: "parent", meter: &subAgentMeter{},
	})
	def, _ := agents.NewRegistry().Get(criticRoleName)
	critic := &serveCritic{build: func(context.Context) (*criticRuntime, error) {
		return &criticRuntime{runner: runner, def: def}, nil
	}}
	var wg sync.WaitGroup
	for i := range 10 {
		name := []string{"alpha", "beta"}[i%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			summary, err := critic.Judge(context.Background(), repos[name], "EXPECT:"+name+"-only-in-this-worktree", 1)
			if err != nil {
				t.Errorf("judging %s: %v", name, err)
				return
			}
			if !strings.Contains(summary, "MANVI-VERDICT[1]: PASS") {
				t.Errorf("judging %s read the wrong worktree: %s", name, summary)
			}
		}()
	}
	wg.Wait()
}
