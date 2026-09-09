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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/flags"
	"github.com/bharathvbcr/Manvi/manvi/internal/testsupport"
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
	settings, err := workbenchEnhancementConfiguration(reg)
	if err != nil || settings.Model != "saved-model" || settings.ModelSource != "llm.local.model" {
		t.Fatalf("configured local selection: %+v %v", settings, err)
	}
	t.Setenv("MANVI_MODEL", "chosen-model")
	settings, err = workbenchEnhancementConfiguration(reg)
	if err != nil || settings.Model != "chosen-model" || settings.ModelSource != "MANVI_MODEL" {
		t.Fatalf("explicit override: %+v %v", settings, err)
	}
	t.Setenv("MANVI_MODEL", "")
	settings, err = workbenchEnhancementConfiguration(registryWith(t, map[string]string{flags.LLMDefaultProvider: "anthropic", flags.LLMLocalModel: "local-only"}))
	if err != nil || settings.Model != "" || settings.ModelSource != "none" {
		t.Fatalf("cloud inherited an unrelated local model: %+v %v", settings, err)
	}
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
			http.Error(w, "invalid fixture request", 400)
			return
		}
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
	binary := e2eBinary(t)
	storeBinary := testsupport.DCStore(t)
	repo := e2eRepo(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, binary, "serve", "--workbench-db", filepath.Join(t.TempDir(), "profile.sqlite"))
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "MANVI_HARNESS_INIT_ENABLED=false", "DEVCOUNCIL_ROOT="+repo, "MANVI_STORE_BINARY="+storeBinary,
		"MANVI_LLM_LOCAL_BASE_URL="+model.URL+"/v1", "LOCAL_API_KEY=workbench-fixture-credential")
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
			if err := cmd.Wait(); err != nil && ctx.Err() == nil {
				t.Error(err)
			}
		}
	})
	encoder, decoder := json.NewEncoder(stdin), json.NewDecoder(stdout)
	request := func(op, params string) json.RawMessage {
		t.Helper()
		if err := encoder.Encode(serve.Request{ID: op, Op: op, Params: json.RawMessage(params)}); err != nil {
			t.Fatal(err)
		}
		var response serve.Response
		if err := decoder.Decode(&response); err != nil {
			t.Fatal(err)
		}
		if !response.OK || response.ID != op {
			t.Fatalf("host request failed: %+v", response)
		}
		return response.Result
	}
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
	err = cmd.Wait()
	waited = true
	if err != nil {
		t.Fatalf("CLI shutdown: %v; stderr: %s", err, stderr.String())
	}
}
