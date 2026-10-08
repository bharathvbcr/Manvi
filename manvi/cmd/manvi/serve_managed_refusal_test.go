package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/testsupport"
	"github.com/bharathvbcr/Manvi/manvi/serve"
)

// The provider fixtures are the codingagent package's own protocol children
// (TestClaudeProtocolChild, TestProtocolChild), compiled once into a test
// binary. A second fake written here would drift from the frames those
// fixtures copy from recorded provider traffic.
var (
	providerFixtureOnce sync.Once
	providerFixtureBin  string
	providerFixtureErr  error
)

// managedProviderFixture returns an executable that `manvi serve` can be
// pointed at as the provider. It is a wrapper with the same shape as
// codingagent's fixtureArgs: the fixture selector, then the adapter's real
// launch line unchanged — so the Claude `--version` probe and its stream-json
// flags, or Codex's `app-server`, reach the child exactly as they would reach
// the installed CLI.
func managedProviderFixture(t *testing.T, provider, scenario string) string {
	t.Helper()
	providerFixtureOnce.Do(func() {
		dir, err := os.MkdirTemp("", "manvi-provider-fixture")
		if err != nil {
			providerFixtureErr = err
			return
		}
		bin := filepath.Join(dir, "codingagent.test")
		// #nosec G204 -- every argument is a literal except bin, a path this
		// function just made under os.MkdirTemp.
		cmd := exec.CommandContext(t.Context(), "go", "test", "-c", "-o", bin, "./codingagent")
		cmd.Dir = filepath.Join(testsupport.RepoRoot(t), "manvi")
		if out, err := cmd.CombinedOutput(); err != nil {
			providerFixtureErr = fmt.Errorf("building the provider fixture: %w\n%s", err, out)
			return
		}
		providerFixtureBin = bin
	})
	if providerFixtureErr != nil {
		t.Fatal(providerFixtureErr)
	}
	var line string
	switch provider {
	case "claude":
		line = fmt.Sprintf(`exec '%s' '-test.run=^TestClaudeProtocolChild$' -- 'claude-fixture:%s' "$@"`, providerFixtureBin, scenario)
	case "codex":
		// The Codex child reads its scenario from the last argument.
		line = fmt.Sprintf(`exec '%s' '-test.run=^TestProtocolChild$' -- "$@" 'codex-fixture:%s'`, providerFixtureBin, scenario)
	default:
		t.Fatalf("no fixture for provider %q", provider)
	}
	if strings.Contains(providerFixtureBin+scenario, "'") {
		t.Fatalf("fixture path or scenario cannot be single-quoted: %s %s", providerFixtureBin, scenario)
	}
	wrapper := filepath.Join(t.TempDir(), provider)
	// #nosec G306 -- the wrapper is exec'd as the provider, so it needs its
	// owner execute bit; it lives in this test's own temp directory.
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\n"+line+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return wrapper
}

// TestServeRecordsARefusedProviderCallbackAsTheRunsVisibleReason drives one
// managed run per provider through `manvi serve --workbench-db` — the same
// NDJSON ops GitPulse sends — against a provider that asks for something the
// adapter cannot answer. The refusal has to come back out of the host's own
// read of the run, naming the callback, or the person sees a failed run with
// no idea that the agent was waiting on them.
func TestServeRecordsARefusedProviderCallbackAsTheRunsVisibleReason(t *testing.T) {
	for _, tc := range []struct {
		provider, scenario, want string
	}{
		{"claude", "dialog", `unsupported Claude callback "request_user_dialog"; no response was granted`},
		{"codex", "callback:mcpServer/elicitation/request", `unsupported Codex callback "mcpServer/elicitation/request"; no response was granted`},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			fixture := managedProviderFixture(t, tc.provider, tc.scenario)
			binary := e2eBinary(t)
			storeBinary := testsupport.DCStore(t)
			repo, err := filepath.EvalSymlinks(e2eRepo(t))
			if err != nil {
				t.Fatal(err)
			}
			gitDir := filepath.Join(repo, ".git")
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			t.Cleanup(cancel)
			// #nosec G204 -- binary is the one e2eBinary built into its own temp
			// directory; the remaining arguments are literals and a temp path.
			cmd := exec.CommandContext(ctx, binary, "serve", "--posture", "host", "--workbench-db", filepath.Join(t.TempDir(), "profile.sqlite"))
			cmd.Dir = repo
			cmd.Env = append(os.Environ(), "MANVI_HARNESS_INIT_ENABLED=false", "DEVCOUNCIL_ROOT="+repo,
				"MANVI_STORE_BINARY="+storeBinary, "MANVI_CLAUDE_BINARY="+fixture, "MANVI_CODEX_BINARY="+fixture)
			cmd.WaitDelay = 3 * time.Second
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			t.Cleanup(func() {
				cancel()
				if !waited {
					_ = cmd.Wait()
				}
			})
			encoder, decoder := json.NewEncoder(stdin), json.NewDecoder(stdout)
			sequence := 0
			request := func(op, params string) json.RawMessage {
				t.Helper()
				sequence++
				id := fmt.Sprintf("%s-%d", op, sequence)
				if err := encoder.Encode(serve.Request{ID: id, Op: op, Params: json.RawMessage(params)}); err != nil {
					t.Fatal(err)
				}
				var response serve.Response
				if err := decoder.Decode(&response); err != nil {
					t.Fatalf("%s: %v; stderr: %s", op, err, stderr.String())
				}
				if !response.OK || response.ID != id {
					t.Fatalf("%s failed: %+v; stderr: %s", op, response, stderr.String())
				}
				return response.Result
			}
			request("work.repositories.put", fmt.Sprintf(`{"id":"r","request_id":"r","expected_revision":0,"name":"Repo","identity_key":%q}`, "local:"+gitDir))
			request("work.items.put", `{"id":"t","request_id":"t","expected_revision":0,"title":"Inspect fixture","repository_ids":["r"],"primary_repository_id":"r"}`)
			request("work.runs.prepare", fmt.Sprintf(`{"id":"run","request_id":"prep","expected_revision":0,"kind":"managed","task_id":"t","source_revision":1,"repository_id":"r","repository_revision":1,"provider":%q,"permission_mode":"ask","cwd":%q,"git_dir":%q,"git_common_dir":%q,"head_oid":null}`, tc.provider, repo, gitDir, gitDir))
			var prepared serve.ManagedPrepared
			if err := json.Unmarshal(request("work.runs.managed.prepare", `{"id":"run","request_id":"launch","expected_revision":1}`), &prepared); err != nil {
				t.Fatal(err)
			}
			if prepared.Phase != "awaiting_activation" || prepared.PID <= 0 {
				t.Fatalf("provider was not prepared: %+v", prepared)
			}
			request("work.runs.managed.activate", fmt.Sprintf(`{"id":"run","owner_id":%q,"session_id":%q,"process_id":%d,"process_start":"fixture-birth"}`, prepared.Owner, prepared.Session, prepared.PID))

			var run struct {
				Item struct {
					State         string `json:"state"`
					Reason        string `json:"reason"`
					ProviderState string `json:"provider_state"`
				} `json:"item"`
			}
			var raw json.RawMessage
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			for {
				raw = request("work.runs.get", `{"id":"run"}`)
				if err := json.Unmarshal(raw, &run); err != nil {
					t.Fatal(err)
				}
				if run.Item.State != "running" {
					break
				}
				select {
				case <-ticker.C:
				case <-ctx.Done():
					t.Fatalf("the run never ended after its callback was refused: %s", raw)
				}
			}
			// The provider process exited when the host closed it, so the
			// lifecycle is "exited"; the protocol receipt is the failure, and
			// the reason is the adapter's refusal, unparaphrased.
			if run.Item.State != "exited" || run.Item.ProviderState != "failed" {
				t.Fatalf("refused callback did not end the run as a provider failure: %s", raw)
			}
			if run.Item.Reason != tc.want {
				t.Fatalf("visible reason = %q, want %q; run: %s", run.Item.Reason, tc.want, raw)
			}
			// The callback was refused, never captured: nothing may be left in
			// the person's decision queue for a question nobody can deliver.
			decisions := request("work.decisions.list", `{"run_id":"run","limit":32}`)
			var page struct {
				Items []json.RawMessage `json:"items"`
			}
			if err := json.Unmarshal(decisions, &page); err != nil || len(page.Items) != 0 {
				t.Fatalf("a refused callback reached the decision queue: %s (%v)", decisions, err)
			}
			if err := stdin.Close(); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			waited = true
			if err != nil {
				t.Fatalf("serve shutdown: %v; stderr: %s", err, stderr.String())
			}
		})
	}
}
