package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

type isolatedWorktreeProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *isolatedWorktreeProvider) Name() string { return "isolated-test" }

func (p *isolatedWorktreeProvider) Capability(model string) (llm.Capability, bool) {
	if model != "worker-model" {
		return llm.Capability{}, false
	}
	return llm.Capability{
		Provider:          p.Name(),
		Model:             model,
		ContextWindow:     32768,
		MaxOutputTokens:   4096,
		SupportsTools:     true,
		SupportsStreaming: false,
	}, true
}

func (p *isolatedWorktreeProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()

	prompt := ""
	if len(req.Messages) > 0 {
		prompt = req.Messages[0].Text()
	}
	step := len(req.Messages)

	var resp llm.Response

	switch {
	case strings.HasPrefix(prompt, "ALPHA:"):
		switch step {
		case 1:
			resp = wantToolCall("devcouncil_write_file", `{"path":"shared.txt","content":"alpha isolated content\n"}`)
		case 3:
			resp = wantToolCall("devcouncil_read_file", `{"path":"shared.txt"}`)
		default:
			lastTool := req.Messages[len(req.Messages)-1]
			payload := marshalBlocks(lastTool.Content)
			if !strings.Contains(payload, "alpha isolated content") {
				return nil, fmt.Errorf("alpha read_file saw %s, want alpha isolated content", payload)
			}
			resp = endTurn("alpha wrote and verified shared.txt in isolated worktree")
		}

	case strings.HasPrefix(prompt, "BETA:"):
		switch step {
		case 1:
			resp = wantToolCall("devcouncil_write_file", `{"path":"shared.txt","content":"beta isolated content\n"}`)
		case 3:
			resp = wantToolCall("devcouncil_read_file", `{"path":"shared.txt"}`)
		default:
			lastTool := req.Messages[len(req.Messages)-1]
			payload := marshalBlocks(lastTool.Content)
			if !strings.Contains(payload, "beta isolated content") {
				return nil, fmt.Errorf("beta read_file saw %s, want beta isolated content", payload)
			}
			resp = endTurn("beta wrote and verified shared.txt in isolated worktree")
		}

	case strings.HasPrefix(prompt, "READER:"):
		if step == 1 {
			resp = wantToolCall("devcouncil_read_file", `{"path":"shared.txt"}`)
		} else {
			lastTool := req.Messages[len(req.Messages)-1]
			payload := marshalBlocks(lastTool.Content)
			if !strings.Contains(payload, "parent baseline content") {
				return nil, fmt.Errorf("reader saw %s, want parent baseline content", payload)
			}
			resp = endTurn("reader verified parent baseline without mutating")
		}

	default:
		resp = endTurn("done")
	}

	resp.Usage = llm.Usage{InputTokens: 20, OutputTokens: 10}
	return &scriptedStream{resp: resp}, nil
}

func marshalBlocks(blocks []llm.ContentBlock) string {
	b, _ := json.Marshal(blocks)
	return string(b)
}

func initTestRepoForSubagents(t *testing.T) string {
	t.Helper()
	raw := t.TempDir()
	repo, err := filepath.EvalSymlinks(raw)
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // G204: literal git; args are this test's own fixture commands
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".devcouncil/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("parent baseline content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-m", "baseline")
	return repo
}

func TestParallelWorktreeSubagentsMutateSameFileWithoutTouchingParentCheckout(t *testing.T) {
	repo := initTestRepoForSubagents(t)
	t.Setenv("MANVI_PROJECT_ROOT", repo)
	t.Setenv("MANVI_POLICY_FILE_MODE", "advisory")

	flagReg, err := flags.NewHarnessRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	native, pipeline, err := nativeTools(flagReg)
	if err != nil {
		t.Fatal(err)
	}
	runner, _ := harnessFor(pipeline)
	if runner == nil {
		t.Fatal("nativeTools did not attach subAgentRunner")
	}

	provider := &isolatedWorktreeProvider{}
	models := llm.NewRegistry()
	if err := models.Register(provider); err != nil {
		t.Fatal(err)
	}
	runner.attach(subAgentConfig{
		provider:     provider,
		models:       models,
		model:        "worker-model",
		registry:     pipeline,
		native:       native,
		systemPrompt: "You are an isolated worker.",
		meter:        &subAgentMeter{},
	})

	args, err := json.Marshal(map[string]any{
		"tasks": []map[string]any{
			{"label": "alpha", "prompt": "ALPHA: update shared.txt", "workspace": "worktree"},
			{"label": "beta", "prompt": "BETA: update shared.txt", "workspace": "worktree"},
			{"label": "reader", "prompt": "READER: inspect shared.txt", "workspace": "worktree", "read_only": true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	res := pipeline.Run(context.Background(), tools.Call{
		ID:        "fanout-1",
		Name:      "devcouncil_spawn_subagents",
		Arguments: args,
	})
	if res.IsError {
		t.Fatalf("devcouncil_spawn_subagents failed: %s", res.Text)
	}
	if len(res.Wrote) != 0 {
		t.Fatalf("parent res.Wrote = %v, want empty because all mutations stayed in isolated child worktrees", res.Wrote)
	}

	// Parent checkout's shared.txt must be completely untouched.
	parentBytes, err := os.ReadFile(filepath.Clean(filepath.Join(repo, "shared.txt")))
	if err != nil {
		t.Fatal(err)
	}
	if string(parentBytes) != "parent baseline content\n" {
		t.Fatalf("parent shared.txt was clobbered: %q", string(parentBytes))
	}

	var payload struct {
		Clean   bool             `json:"clean"`
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal([]byte(res.Text), &payload); err != nil {
		t.Fatalf("unmarshal spawn result: %v\n%s", err, res.Text)
	}
	if !payload.Clean || len(payload.Results) != 3 {
		t.Fatalf("unexpected fanout payload: %+v", payload)
	}

	coord := &devcouncil.GitWorktreeCoordinator{Root: repo}
	byLabel := map[string]map[string]any{}
	for _, r := range payload.Results {
		lbl, _ := r["label"].(string)
		byLabel[lbl] = r
	}

	for _, tc := range []struct {
		label string
		want  string
	}{
		{"alpha", "alpha isolated content\n"},
		{"beta", "beta isolated content\n"},
	} {
		out := byLabel[tc.label]
		if out == nil {
			t.Fatalf("missing child %s in %+v", tc.label, payload.Results)
		}
		wtPath, _ := out["worktree_path"].(string)
		retained, _ := out["worktree_retained"].(bool)
		t.Cleanup(func() {
			if wtPath != "" {
				_, _ = coord.ReleaseWorktree(context.Background(), &devcouncil.WorkspaceLease{
					Path:         wtPath,
					ContainerDir: filepath.Dir(wtPath),
				}, false)
			}
		})
		if !retained || wtPath == "" || wtPath == repo {
			t.Fatalf("%s: retained=%v worktree_path=%q", tc.label, retained, wtPath)
		}
		gotBytes, err := os.ReadFile(filepath.Clean(filepath.Join(wtPath, "shared.txt")))
		if err != nil {
			t.Fatalf("%s worktree shared.txt missing: %v", tc.label, err)
		}
		if string(gotBytes) != tc.want {
			t.Fatalf("%s worktree shared.txt = %q, want %q", tc.label, string(gotBytes), tc.want)
		}
		wtWrote, _ := out["worktree_wrote"].([]any)
		if len(wtWrote) != 1 || wtWrote[0] != "shared.txt" {
			t.Fatalf("%s worktree_wrote = %v, want [shared.txt]", tc.label, wtWrote)
		}
		if _, leaked := out["wrote"]; leaked {
			t.Fatalf("%s leaked parent 'wrote' field on isolated worktree outcome: %+v", tc.label, out)
		}
	}

	readerOut := byLabel["reader"]
	if readerOut == nil {
		t.Fatalf("missing reader result: %+v", payload.Results)
	}
	readerWT, _ := readerOut["worktree_path"].(string)
	readerRetained, _ := readerOut["worktree_retained"].(bool)
	if readerRetained {
		t.Fatalf("clean reader worktree was retained: %+v", readerOut)
	}
	if readerWT == "" {
		t.Fatalf("reader did not report worktree_path: %+v", readerOut)
	}
	if _, err := os.Stat(readerWT); !os.IsNotExist(err) {
		t.Fatalf("clean reader worktree %s still exists on disk after release: %v", readerWT, err)
	}
}
