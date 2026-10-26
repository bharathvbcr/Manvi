package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/agent"
	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
	"github.com/bharathvbcr/Manvi/manvi/session"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// benchGitRepo builds a repository of tracked committed files with untracked
// and gitignored content on top — the three things a live worktree holds and
// the baseline capture pays for. It is devcouncil's benchRepo for this
// package: that one is a test helper of another package and cannot be reached
// from here.
func benchGitRepo(tb testing.TB, tracked, untracked, ignored int) string {
	tb.Helper()
	root := tb.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	put := func(rel, body string) {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			tb.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			tb.Fatal(err)
		}
	}
	gitIn(tb, root, "init", "-q")
	gitIn(tb, root, "config", "gc.auto", "0")
	gitIn(tb, root, "config", "maintenance.auto", "false")
	put(".gitignore", "ignored/\n")
	for i := 0; i < tracked; i++ {
		put(fmt.Sprintf("pkg%d/file%d.go", i%40, i), fmt.Sprintf("package p\n\nfunc F%d() int { return %d }\n", i, i))
	}
	gitIn(tb, root, "add", "-A")
	gitIn(tb, root, "commit", "-qm", "seed")
	for i := 0; i < untracked; i++ {
		put(fmt.Sprintf("new/u%d.go", i), fmt.Sprintf("package n\n\nfunc U%d() {}\n", i))
	}
	for i := 0; i < ignored; i++ {
		put(fmt.Sprintf("ignored/i%d.bin", i), strings.Repeat("x", 512))
	}
	return root
}

// timingProvider is a replay provider that records when the loop first asked it
// for a response — the moment a real model request would go on the wire.
type timingProvider struct {
	*replay.Provider
	once  sync.Once
	first chan time.Time
}

func newTimingProvider(turns ...replay.Turn) *timingProvider {
	return &timingProvider{
		Provider: replay.New(replay.Fixture{
			Provider: "replay",
			Capabilities: []llm.Capability{{
				Provider: "replay", Model: "m",
				ContextWindow: 200000, MaxOutputTokens: 8192,
				SupportsTools: true, SupportsStreaming: true,
			}},
			Turns: turns,
		}),
		first: make(chan time.Time, 1),
	}
}

func (p *timingProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	p.once.Do(func() { p.first <- time.Now() })
	return p.Provider.Stream(ctx, req)
}

// BenchmarkTurnTimeToFirstRequest measures how long a turn takes to issue its
// first model request, with and without the end-of-turn sensor attached, on a
// 1,200-file repository with untracked and ignored content. The difference is
// what the sensor puts in front of the model: its turn-start baseline.
//
// The two arms alternate inside one loop and each reports its minimum, so
// machine load lands on both. Run with
//
//	go test ./cmd/manvi -run '^$' -bench TurnTimeToFirstRequest -benchtime=10x -count=5
func BenchmarkTurnTimeToFirstRequest(b *testing.B) {
	root := benchGitRepo(b, 1200, 40, 200)
	verifier, err := devcouncil.NewPathVerifier(root, "", "")
	if err != nil {
		b.Fatal(err)
	}

	turn := func(withSensor bool) time.Duration {
		provider := newTimingProvider(replayText("ok"))
		loopBus := bus.New()
		log := session.NewLog()
		loop, err := agent.NewLoop(agent.Config{
			Provider: provider, Model: "m", SystemPrompt: "you are a builder",
			MaxSteps: 4, MaxTokens: 1024,
		}, loopBus, log, tools.NewRegistry(loopBus))
		if err != nil {
			b.Fatal(err)
		}
		if withSensor {
			if err := attachSensor(loopBus, &sensor{native: verifier, log: log}); err != nil {
				b.Fatal(err)
			}
		}
		start := time.Now()
		if _, err := loop.Run(context.Background(), llm.Message{
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{llm.TextBlock{Text: "hello"}},
		}); err != nil {
			b.Fatal(err)
		}
		return (<-provider.first).Sub(start)
	}

	var without, with time.Duration
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d0 := turn(false)
		d1 := turn(true)
		if without == 0 || d0 < without {
			without = d0
		}
		if with == 0 || d1 < with {
			with = d1
		}
	}
	b.ReportMetric(float64(without)/1e6, "no-sensor-first-request-min-ms")
	b.ReportMetric(float64(with)/1e6, "sensor-first-request-min-ms")
}
