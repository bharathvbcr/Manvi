package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// The sidecar is launched in a scratch directory, and one sidecar verifies many
// worktrees. The critic's read surface has to be the worktree it was asked to
// judge, rebuilt for each verification through Registry.ForWorktree, and never
// the directory the process happened to start in.

// untrackedReader is a critic model that reads untracked.txt through the
// harness's read tool and passes only if what came back is what the prompt says
// that worktree holds.
type untrackedReader struct {
	mu   sync.Mutex
	seen []string
	// offered is every tool name the critic was shown on a first request.
	offered map[string]bool
}

func (p *untrackedReader) Name() string { return "untracked-reader" }

func (p *untrackedReader) Capability(model string) (llm.Capability, bool) {
	if model != "critic-model" {
		return llm.Capability{}, false
	}
	return llm.Capability{Provider: p.Name(), Model: model, ContextWindow: 32768, MaxOutputTokens: 4096, SupportsTools: true}, true
}

func (p *untrackedReader) Stream(_ context.Context, req llm.Request) (llm.Stream, error) {
	if len(req.Messages) == 1 {
		p.mu.Lock()
		if p.offered == nil {
			p.offered = map[string]bool{}
		}
		for _, schema := range req.Tools {
			p.offered[schema.Name] = true
		}
		p.mu.Unlock()
		return &scriptedStream{resp: wantToolCall("devcouncil_read_file", `{"path":"untracked.txt"}`)}, nil
	}
	want := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(req.Messages[0].Text(), "\n", 2)[0], "EXPECT:"))
	payload := marshalBlocks(req.Messages[len(req.Messages)-1].Content)
	p.mu.Lock()
	p.seen = append(p.seen, payload)
	p.mu.Unlock()
	if strings.Contains(payload, want) {
		return &scriptedStream{resp: endTurn("I read it.\nMANVI-VERDICT[1]: PASS\n")}, nil
	}
	return &scriptedStream{resp: endTurn("MANVI-VERDICT[1]: FAIL could not read untracked.txt; saw " + payload + "\n")}, nil
}

// Criterion 6: the failing-before test. Two worktrees, one critic, a sidecar
// whose own directory holds neither: each judgement reads its own worktree's
// untracked file.
func TestServeCriticReadsTheWorktreeItWasGiven(t *testing.T) {
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
		t.Fatalf("the native tool surface could not be built: %v", err)
	}
	runner, _ := harnessFor(pipeline)
	if runner == nil {
		t.Fatal("nativeToolsWith did not attach a sub-agent runner")
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
	def, ok := agents.NewRegistry().Get(criticRoleName)
	if !ok {
		t.Fatal("no built-in critic role")
	}
	critic := &serveCritic{build: func(context.Context) (*criticRuntime, error) {
		return &criticRuntime{runner: runner, def: def}, nil
	}}

	readOnly := map[string]bool{}
	for _, schema := range pipeline.ReadOnlySchemas() {
		readOnly[schema.Name] = true
	}
	defer func() {
		provider.mu.Lock()
		defer provider.mu.Unlock()
		if !provider.offered["devcouncil_read_file"] {
			t.Errorf("the critic was not offered the read tool: %v", provider.offered)
		}
		for name := range provider.offered {
			if !readOnly[name] {
				t.Errorf("the critic on the re-rooted surface was offered %q, which is not a read-only tool", name)
			}
		}
	}()
	for _, name := range []string{"alpha", "beta", "alpha"} {
		summary, err := critic.Judge(context.Background(), repos[name], "EXPECT:"+name+"-only-in-this-worktree", 1)
		if err != nil {
			t.Fatalf("judging %s: %v", name, err)
		}
		if !strings.Contains(summary, "MANVI-VERDICT[1]: PASS") {
			t.Errorf("judging %s: the critic could not read the worktree's untracked file: %s", name, summary)
		}
	}
}

// A critic with no worktree to read is refused, not run against whatever
// directory the process is in.
func TestServeCriticRefusesAJudgementWithNoWorktree(t *testing.T) {
	var wrote atomic.Bool
	critic, provider := criticFixture(t, &wrote)
	for _, worktree := range []string{"", "relative/path"} {
		if _, err := critic.Judge(context.Background(), worktree, "judge", 1); err == nil {
			t.Errorf("a judgement of worktree %q was run", worktree)
		}
	}
	if n := len(provider.Requests()); n != 0 {
		t.Fatalf("the provider saw %d request(s) for a judgement that has no worktree", n)
	}
}
