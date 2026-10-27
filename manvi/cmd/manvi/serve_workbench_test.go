package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/testsupport"
	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/serve"
)

func TestServeRejectsAmbiguousWorkbenchDatabaseFlagsBeforeStarting(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "profile.sqlite")
	for _, args := range [][]string{
		{"--workbench-db"}, {"--workbench-db", "relative.sqlite"},
		{"--workbench-db", abs, "--workbench-db", abs},
		{"--workbench-db", abs + "\x00"},
	} {
		err := serveCommand(io.Discard, nil, args)
		if err == nil || !strings.Contains(err.Error(), "--workbench-db") {
			t.Fatalf("invalid path flags = %v", err)
		}
	}
}

func TestWorkbenchConfigurationUsesExplicitSelectionWithoutDiscovery(t *testing.T) {
	t.Setenv("MANVI_MODEL", "")
	reg := registryWith(t, map[string]string{flags.LLMDefaultProvider: "local", flags.LLMLocalModel: "saved-model", flags.LLMLocalBaseURL: "http://127.0.0.1:1/v1"})
	settings, err := workbenchEnhancementConfiguration(context.Background(), io.Discard, reg)
	if err != nil || settings.Model != "saved-model" || settings.ModelSource != "llm.local.model" {
		t.Fatalf("configured local selection: %+v %v", settings, err)
	}
	t.Setenv("MANVI_MODEL", "chosen-model")
	settings, err = workbenchEnhancementConfiguration(context.Background(), io.Discard, reg)
	if err != nil || settings.Model != "chosen-model" || settings.ModelSource != "MANVI_MODEL" {
		t.Fatalf("explicit override: %+v %v", settings, err)
	}
	t.Setenv("MANVI_MODEL", "")
	t.Setenv(flags.EnvKey(flags.LLMLocalModel), "local-only")
	settings, err = workbenchEnhancementConfiguration(context.Background(), io.Discard, serveTierRegistry(t, nil))
	runModel, runSource, runErr := resolveModelFor(context.Background(), anthropic.Name, nil, serveTierRegistry(t, nil))
	if err != nil || runErr != nil || settings.Model == "local-only" || settings.Model != runModel || settings.ModelSource != string(runSource) {
		t.Fatalf("cloud inherited an unrelated local model, or disagreed with run: serve %+v %v; run %q %q %v", settings, err, runModel, runSource, runErr)
	}
}

// serveTierRegistry is the registry `manvi serve` is handed: newRegistry, with
// the provider on anthropic and the operator's tier settings as given. The user
// config is pointed at a file that does not exist, so what the test reads is
// what it set and not what the machine running it has configured.
func serveTierRegistry(t *testing.T, tiers map[string]string) *flags.Registry {
	t.Helper()
	t.Setenv(envUserConfig, filepath.Join(t.TempDir(), "absent.yaml"))
	t.Setenv("MANVI_MODEL", "")
	t.Setenv(flags.EnvKey(flags.LLMDefaultProvider), anthropic.Name)
	for _, tier := range agents.Tiers() {
		for _, key := range []string{tierModelKey(tier), tierEffortKey(tier)} {
			t.Setenv(flags.EnvKey(key), "")
			if err := os.Unsetenv(flags.EnvKey(key)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for key, value := range tiers {
		t.Setenv(flags.EnvKey(key), value)
	}
	reg, err := newRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// TestServeResolvesTheAnthropicModelAsRunDoes: serve and run/TUI answer the one
// question — which model does this process send — and may not answer it
// differently. With MANVI_MODEL unset, serve reported "" and "none" and the
// workbench refused on a machine where `manvi run` used the operator's main tier.
func TestServeResolvesTheAnthropicModelAsRunDoes(t *testing.T) {
	mainKey := tierModelKey(agents.TierMain)
	for _, tc := range []struct {
		name   string
		tiers  map[string]string
		model  string // MANVI_MODEL
		want   string
		source string
	}{
		{name: "the operator's main tier", tiers: map[string]string{mainKey: "claude-sonnet-5-5"}, want: "claude-sonnet-5-5", source: "llm.tier.main"},
		{name: "MANVI_MODEL beats the main tier", tiers: map[string]string{mainKey: "claude-sonnet-5-5"}, model: "claude-haiku-5-5", want: "claude-haiku-5-5", source: "MANVI_MODEL"},
		{name: "neither is the default run gives", want: "claude-opus-5-5", source: "llm.tier.main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := serveTierRegistry(t, tc.tiers)
			t.Setenv("MANVI_MODEL", tc.model)
			settings, err := workbenchEnhancementConfiguration(context.Background(), io.Discard, reg)
			if err != nil || !settings.OK || settings.Model != tc.want || settings.ModelSource != tc.source {
				t.Fatalf("serve = %+v, %v; want %q from %s", settings, err, tc.want, tc.source)
			}
			runModel, runSource, err := resolveModelFor(context.Background(), anthropic.Name, nil, reg)
			if err != nil || runModel != settings.Model || string(runSource) != settings.ModelSource {
				t.Fatalf("run = %q from %q, %v; serve = %+v", runModel, runSource, err, settings)
			}
		})
	}
}

// TestServeStartsOnATierItCannotUse: a tier profile this binary cannot run
// degrades the op that needs the model, as newServeCritic does, and never stops
// serve — the host serves the board, the code graph and scans besides.
func TestServeStartsOnATierItCannotUse(t *testing.T) {
	mainKey := tierModelKey(agents.TierMain)
	t.Run("a main tier that resolves to nothing", func(t *testing.T) {
		reg := serveTierRegistry(t, map[string]string{mainKey: " "})
		var notes bytes.Buffer
		settings, err := workbenchEnhancementConfiguration(context.Background(), &notes, reg)
		if err != nil {
			t.Fatalf("an unusable main tier refused serve: %v", err)
		}
		if !settings.OK || settings.Provider != anthropic.Name || settings.Model != "" || settings.ModelSource != "none" {
			t.Fatalf("degraded selection = %+v; want anthropic with no model from none", settings)
		}
		if !strings.Contains(notes.String(), "MANVI_LLM_TIER_MAIN") {
			t.Fatalf("serve degraded without naming the setting to change: %q", notes.String())
		}
	})
	t.Run("a main tier the catalogue does not hold", func(t *testing.T) {
		reg := serveTierRegistry(t, map[string]string{mainKey: "claude-opus-9"})
		settings, err := workbenchEnhancementConfiguration(context.Background(), io.Discard, reg)
		if err != nil || settings.Model != "claude-opus-9" || settings.ModelSource != "llm.tier.main" {
			t.Fatalf("selection = %+v, %v; want the operator's tier named as it was typed", settings, err)
		}
	})
}

func TestServeGeneratesThroughConfiguredLocalHTTPProvider(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `{"data":[{"id":"workbench-model","object":"model","max_model_len":32768}]}`); err != nil {
			t.Error(err)
		}
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model     string            `json:"model"`
			MaxTokens int               `json:"max_tokens"`
			Tools     []json.RawMessage `json:"tools"`
			Messages  []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		// A decoder stops at the end of the object; read to EOF so the server
		// notices the client hanging up, which the wait below depends on.
		_, _ = io.Copy(io.Discard, r.Body)
		if request.Model != "workbench-model" || request.MaxTokens != 8192 || len(request.Tools) != 0 || len(request.Messages) != 2 || !strings.Contains(request.Messages[1].Content, "fix E42") {
			t.Errorf("generation did not use the configured bounded text request: %+v", request)
		}
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"{\\\"title\\\":\\\"Resolve E42\\\"}\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"); err != nil {
			t.Error(err)
		}
	})
	model := httptest.NewServer(mux)
	t.Cleanup(model.Close)
	host := startServeHost(t, "MANVI_LLM_LOCAL_BASE_URL="+model.URL+"/v1", "LOCAL_API_KEY=workbench-fixture-credential")
	ctx, stdin, request := host.ctx, host.stdin, host.request
	request("work.repositories.put", `{"request_id":"r","id":"r","expected_revision":0,"name":"repo","identity_key":"r"}`)
	request("work.items.put", `{"request_id":"t","id":"t","expected_revision":0,"title":"fix E42","repository_ids":["r"],"primary_repository_id":"r"}`)
	request("work.enhancements.create", `{"request_id":"e","id":"e","expected_revision":0,"task_id":"t","source_revision":1,"fields":["title"],"provider":"local","model":"workbench-model"}`)
	if calls.Load() != 0 {
		t.Fatal("CRUD started model inference")
	}
	request("work.enhancements.generate", `{"id":"e","request_id":"generate","expected_revision":1}`)
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("configured model endpoint was never called")
	}
	// The actual CLI must answer while its HTTP provider is still blocked.
	task := request("work.items.get", `{"id":"t"}`)
	if !strings.Contains(string(task), `"title":"fix E42"`) {
		t.Fatalf("generation changed the task: %s", task)
	}
	request("work.enhancements.generate", `{"id":"e","request_id":"generate","expected_revision":1}`)
	close(release)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		result := request("work.enhancements.get", `{"id":"e"}`)
		var proposal struct {
			Item struct {
				State string `json:"state"`
			} `json:"item"`
		}
		if err := json.Unmarshal(result, &proposal); err != nil {
			t.Fatal(err)
		}
		if proposal.Item.State == "ready" {
			if !strings.Contains(string(result), "Resolve E42") {
				t.Fatalf("generated title missing: %s", result)
			}
			break
		}
		if proposal.Item.State != "running" {
			t.Fatalf("generation failed: %s", result)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("generation did not settle")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("made %d model calls for one attempt", calls.Load())
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := host.wait(); err != nil {
		t.Fatalf("CLI shutdown: %v; stderr: %s", err, host.stderr.String())
	}
}

// serveHost is a `manvi serve --workbench-db` process spoken to over stdio.
type serveHost struct {
	t      *testing.T
	repo   string
	ctx    context.Context
	stdin  io.WriteCloser
	stderr *bytes.Buffer
	call   func(op, params string) serve.Response
	wait   func() error
}

// request is a call the host must answer ok, and returns its result.
func (h *serveHost) request(op, params string) json.RawMessage {
	h.t.Helper()
	response := h.call(op, params)
	if !response.OK {
		h.t.Fatalf("host request failed: %+v", response)
	}
	return response.Result
}

// startServeHost runs the built binary against a fresh profile database inside
// a fixture repository. extraEnv is added to the environment.
func startServeHost(t *testing.T, extraEnv ...string) *serveHost {
	t.Helper()
	binary := e2eBinary(t)
	storeBinary := testsupport.DCStore(t)
	repo := e2eRepo(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	// #nosec G204 -- the binary is the one this package built into its own temp
	// directory; every argument is a literal except the temp profile path.
	cmd := exec.CommandContext(ctx, binary, "serve", "--workbench-db", filepath.Join(t.TempDir(), "profile.sqlite"))
	cmd.Dir = repo
	cmd.Env = append(append(os.Environ(), "MANVI_HARNESS_INIT_ENABLED=false", "DEVCOUNCIL_ROOT="+repo, "MANVI_STORE_BINARY="+storeBinary), extraEnv...)
	cmd.WaitDelay = 3 * time.Second
	host := &serveHost{t: t, repo: repo, ctx: ctx, stderr: &bytes.Buffer{}}
	cmd.Stderr = host.stderr
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
	host.stdin = stdin
	waited := false
	host.wait = func() error {
		waited = true
		return cmd.Wait()
	}
	t.Cleanup(func() {
		cancel()
		if !waited {
			if err := cmd.Wait(); err != nil && ctx.Err() == nil {
				t.Error(err)
			}
		}
	})
	encoder, decoder := json.NewEncoder(stdin), json.NewDecoder(stdout)
	host.call = func(op, params string) serve.Response {
		t.Helper()
		if err := encoder.Encode(serve.Request{ID: op, Op: op, Params: json.RawMessage(params)}); err != nil {
			t.Fatal(err)
		}
		var response serve.Response
		if err := decoder.Decode(&response); err != nil {
			t.Fatal(err)
		}
		if response.ID != op {
			t.Fatalf("response %q answers a request for %q", response.ID, op)
		}
		return response
	}
	return host
}

// anthropicRefinementTurn is one completed Messages stream whose text is a
// refinement with nothing to propose.
const anthropicRefinementTurn = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"{\"acceptance_criteria\":[],\"split_into\":[],\"duplicate_of\":[]}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}

event: message_stop
data: {"type":"message_stop"}

`

// TestServeRefusesOnlyTheTierAnOpUses is the shape the host sees. serve starts
// whatever the tier profile holds, and the op that builds a provider is refused
// only for the tier it uses: the main tier is what an enhancement runs on, so a
// main tier the catalogue does not hold is reported before any request, naming
// the setting, while a bad heavy tier is none of that op's business and it is
// not refused for it: the refinement completes against the provider, and the
// request that reaches it carries no temperature, which anthropic models refuse.
func TestServeRefusesOnlyTheTierAnOpUses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tier    string
		refused bool
	}{
		{name: "main tier the catalogue does not hold", tier: "MANVI_LLM_TIER_MAIN=claude-opus-9", refused: true},
		{name: "heavy tier the catalogue does not hold", tier: "MANVI_LLM_TIER_HEAVY=claude-opus-9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reached atomic.Int32
			var wire atomic.Value
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached.Add(1)
				body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
				wire.Store(string(body))
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, anthropicRefinementTurn)
			}))
			t.Cleanup(provider.Close)
			host := startServeHost(t, "MANVI_MODEL=", "MANVI_LLM_DEFAULT_PROVIDER=anthropic", "ANTHROPIC_API_KEY=sk-ant-fixture-serve-tier",
				"ANTHROPIC_BASE_URL="+provider.URL, "MANVI_USER_CONFIG="+filepath.Join(t.TempDir(), "absent.yaml"), tc.tier)
			configuration := host.request("work.enhancements.configuration", `{}`)
			wantModel := `"model":"claude-opus-5-5"`
			if tc.refused {
				wantModel = `"model":"claude-opus-9"`
			}
			if !strings.Contains(string(configuration), wantModel) || !strings.Contains(string(configuration), `"model_source":"llm.tier.main"`) {
				t.Fatalf("serve did not offer the main tier (%s): %s", wantModel, configuration)
			}
			host.request("work.repositories.put", `{"request_id":"r","id":"r","expected_revision":0,"name":"repo","identity_key":"r"}`)
			host.request("work.items.put", `{"request_id":"t","id":"t","expected_revision":0,"title":"fix E42","repository_ids":["r"],"primary_repository_id":"r"}`)
			refined := host.call("work.enhancements.refine", `{"id":"t","request_id":"w","expected_revision":1,"repo_root":`+strconv.Quote(host.repo)+`}`)
			if tc.refused {
				named := refined.Error != nil && strings.Contains(refined.Error.Message, "tier main") && strings.Contains(refined.Error.Message, "llm.tier.main")
				if refined.OK || !named || reached.Load() != 0 {
					t.Fatalf("refinement = %+v after %d requests; want the main tier named before any request", refined, reached.Load())
				}
			} else {
				body, _ := wire.Load().(string)
				if !refined.OK || reached.Load() == 0 || body == "" || strings.Contains(body, "temperature") {
					t.Fatalf("refinement = %+v after %d requests with body %q; an unrelated tier must not refuse it and no temperature may reach the provider", refined, reached.Load(), body)
				}
			}
			if err := host.stdin.Close(); err != nil {
				t.Fatal(err)
			}
			if err := host.wait(); err != nil {
				t.Fatalf("CLI shutdown: %v; stderr: %s", err, host.stderr.String())
			}
		})
	}
}
