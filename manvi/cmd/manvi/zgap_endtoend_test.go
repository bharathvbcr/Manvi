package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/testsupport"
)

// The gap this file closes: every other test in this tree drives the loop
// in-process, with the provider seam, the gate, or the tool registry supplied
// as a Go value. That covers the parts and it cannot cover the whole — the
// argument parser, the flag resolution from the environment, the provider
// built from settings rather than handed in, the real HTTP transport, the
// session store, and the exit status a caller branches on are all outside
// every one of those tests. The bench rig exercises them, but only against a
// real model, which costs tokens and answers differently every run.
//
// So: the shipped binary, as a child process, against a scripted model server
// over real HTTP, in a repository made for the test. Deterministic, offline,
// and it fails when any link in that chain breaks rather than when the model
// has a bad day.

// e2eModel is a scripted OpenAI-compatible server: the wire the `local`
// provider speaks, and nothing more of it than these tests need.
type e2eModel struct {
	mu       sync.Mutex
	requests []e2eRequest
	// reply returns the streamed deltas for the nth turn, 1-based. Returning
	// a tool call keeps the loop going; returning content ends it.
	reply func(turn int) (delta map[string]any, finish string)
}

type e2eRequest struct {
	Model    string           `json:"model"`
	Messages []map[string]any `json:"messages"`
	Tools    []map[string]any `json:"tools"`
}

func (m *e2eModel) start(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()

	// Discovery. The adapter also probes runtime-specific paths (Ollama's
	// /api/version, LM Studio's /api/v1/models); this server answers none of
	// them, which is a server the adapter classifies as unidentified — the
	// case with the least behaviour of its own, and so the one that tests the
	// harness rather than a runtime quirk.
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{
			"id": "mock-model", "object": "model", "max_model_len": 32768,
		}}})
	})

	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req e2eRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		m.requests = append(m.requests, req)
		turn := len(m.requests)
		m.mu.Unlock()

		delta, finish := m.reply(turn)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("the test server's ResponseWriter cannot flush; the adapter would read one buffered blob rather than a stream")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, chunk := range []map[string]any{
			{"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": nil}}},
			{"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}},
		} {
			encoded, err := json.Marshal(chunk)
			if err != nil {
				t.Errorf("encoding a scripted chunk: %v", err)
				return
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
			flusher.Flush()
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

// seen returns the requests the model server was sent.
func (m *e2eModel) seen() []e2eRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]e2eRequest(nil), m.requests...)
}

// toolCall is one streamed delta asking for a native tool.
func toolCall(name string, args map[string]any) (map[string]any, string) {
	encoded, err := json.Marshal(args)
	if err != nil {
		panic(fmt.Sprintf("scripting %s: %v", name, err))
	}
	return map[string]any{"tool_calls": []map[string]any{{
		"index": 0, "id": "call_1", "type": "function",
		"function": map[string]any{"name": name, "arguments": string(encoded)},
	}}}, "tool_calls"
}

// e2eBinary builds the shipped binary once for the package.
//
// Built rather than exercised through main() in-process, because the exit
// status is half of what these tests assert and os.Exit is not observable from
// inside the test binary. The status is a documented contract — the bench rig
// branches on it — so it has to come from a real process.
var (
	e2eBinaryOnce sync.Once
	e2eBinaryPath string
	e2eBinaryErr  error
)

func e2eBinary(t *testing.T) string {
	t.Helper()
	e2eBinaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "manvi-e2e-bin")
		if err != nil {
			e2eBinaryErr = err
			return
		}
		bin := filepath.Join(dir, "manvi")
		// #nosec G204 -- every argument is a literal in this file except bin,
		// which is a path this function just made under os.MkdirTemp.
		cmd := exec.CommandContext(t.Context(), "go", "build", "-o", bin, "./cmd/manvi")
		cmd.Dir = filepath.Join(testsupport.RepoRoot(t), "manvi")
		if out, err := cmd.CombinedOutput(); err != nil {
			e2eBinaryErr = fmt.Errorf("building manvi: %w\n%s", err, out)
			return
		}
		e2eBinaryPath = bin
	})
	if e2eBinaryErr != nil {
		t.Fatal(e2eBinaryErr)
	}
	return e2eBinaryPath
}

// e2eRepo makes the repository the run happens in. It is a git repository
// because that is what the harness discovers a project root from, and an empty
// commit because a repository with no HEAD is a different set of code paths
// than the one every real run takes.
func e2eRepo(t *testing.T) string {
	t.Helper()
	git := testsupport.Tool(t, "git")
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=e2e@example.invalid", "-c", "user.name=e2e",
			"commit", "-q", "--allow-empty", "-m", "root"},
	} {
		// #nosec G204 -- git is resolved from PATH by testsupport.Tool and the
		// arguments are the literals in the loop above.
		cmd := exec.CommandContext(t.Context(), git, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return dir
}

// e2eRun runs one headless turn and returns everything a caller sees: the
// combined output and the exit status.
func e2eRun(t *testing.T, repo, baseURL, prompt string, extraEnv ...string) (string, int) {
	t.Helper()
	// #nosec G204 -- the binary is the one this package just built into its
	// own temp directory, and prompt comes from the test that called this.
	cmd := exec.CommandContext(t.Context(), e2eBinary(t), "run", "-p", prompt)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		// Init writes workspace guides into the repository; a run under test
		// should change only what the turn changes.
		"MANVI_HARNESS_INIT_ENABLED=false",
		"MANVI_LLM_PROVIDER_DEFAULT=local",
		"MANVI_LLM_LOCAL_BASE_URL="+baseURL,
		"MANVI_LLM_LOCAL_MODEL=mock-model",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()

	status := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("running the harness: %v\n%s", err, out)
		}
		status = exit.ExitCode()
	}
	return string(out), status
}

// TestHeadlessRunAppliesTheModelsWriteAndFeedsTheResultBack is the whole cycle
// in one assertion set: a prompt goes in, a tool call comes back, the gate
// admits it, the file appears with the bytes the model asked for, the result
// is fed back as a second request, and the process exits 0.
func TestHeadlessRunAppliesTheModelsWriteAndFeedsTheResultBack(t *testing.T) {
	model := &e2eModel{reply: func(turn int) (map[string]any, string) {
		if turn == 1 {
			return toolCall("devcouncil_write_file", map[string]any{
				"path": "hello.txt", "content": "written by the loop\n",
			})
		}
		return map[string]any{"content": "Wrote hello.txt."}, "stop"
	}}
	base := model.start(t)
	repo := e2eRepo(t)

	out, status := e2eRun(t, repo, base, "write hello.txt")
	if status != 0 {
		t.Fatalf("a turn that completed exited %d, want 0\n%s", status, out)
	}

	// #nosec G304 -- repo is this test's own t.TempDir and the name is a literal.
	written, err := os.ReadFile(filepath.Join(repo, "hello.txt"))
	if err != nil {
		t.Fatalf("the tool call reported success and the file is not there: %v\n%s", err, out)
	}
	if got := string(written); got != "written by the loop\n" {
		t.Errorf("file holds %q, want %q", got, "written by the loop\n")
	}

	// Two requests, not one. A loop that applies the tool call and stops has
	// left the model unable to see what its own call did, and the turn's
	// answer would be a guess.
	seen := model.seen()
	if len(seen) != 2 {
		t.Fatalf("the model server saw %d request(s), want 2 (the prompt, then the tool result)", len(seen))
	}
	if len(seen[0].Tools) == 0 {
		t.Error("the first request carried no tool schemas, so the model was asked to work with no tools")
	}
	if !strings.Contains(marshal(t, seen[1].Messages), "hello.txt") {
		t.Errorf("the second request does not mention the file the first turn wrote:\n%s", marshal(t, seen[1].Messages))
	}

	// The non-cheating half. This write is outside any task's plan, so the
	// scope rung would have blocked it and the dev posture demoted that to
	// advisory. An allow reached that way must never read like an allow the
	// rules gave — the run has to say so.
	for _, want := range []string{"task.absent", "policy.file.mode=advisory"} {
		if !strings.Contains(out, want) {
			t.Errorf("the run does not report %q, so a demoted allow is indistinguishable from a clean one:\n%s", want, out)
		}
	}
}

// TestHeadlessRunRefusesAWriteOutsideTheRepository proves the gate is in the
// path the binary actually takes. The dev posture demotes scope rules; it does
// not demote this one, and the file must not appear.
func TestHeadlessRunRefusesAWriteOutsideTheRepository(t *testing.T) {
	model := &e2eModel{reply: func(turn int) (map[string]any, string) {
		if turn == 1 {
			return toolCall("devcouncil_write_file", map[string]any{
				"path": "../escaped.txt", "content": "outside the root\n",
			})
		}
		return map[string]any{"content": "I could not write there."}, "stop"
	}}
	base := model.start(t)
	repo := e2eRepo(t)

	out, _ := e2eRun(t, repo, base, "write ../escaped.txt")

	escaped := filepath.Join(filepath.Dir(repo), "escaped.txt")
	if _, err := os.Stat(escaped); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a write outside the repository root landed at %s (stat error: %v)\n%s", escaped, err, out)
	}
	// Named specifically. "the file is not there" is also true of a run that
	// never reached the model, and the two must not assert the same way.
	for _, want := range []string{
		"path.outside_root",
		"hard rule: no override clears this, by any authority",
		"1 of 1 tool call(s) were refused by the gate",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the run does not report %q, so a blocked write reads like one that never happened:\n%s", want, out)
		}
	}
	// The refusal goes back to the model. A gate that blocks and says nothing
	// upstream leaves the model believing the write landed.
	if seen := model.seen(); len(seen) != 2 {
		t.Fatalf("the model server saw %d request(s), want 2 (the prompt, then the refusal)", len(seen))
	}
}

// TestHeadlessRunExitsTwoWhenTheStepCeilingTruncates pins the exit status a
// caller branches on. A turn that ran and did not finish is neither a success
// nor a failure, and 2 is how the CLI says so.
func TestHeadlessRunExitsTwoWhenTheStepCeilingTruncates(t *testing.T) {
	// Never stops asking for another tool call, so the ceiling is what ends it.
	model := &e2eModel{reply: func(int) (map[string]any, string) {
		return toolCall("devcouncil_list_dir", map[string]any{"path": "."})
	}}
	base := model.start(t)
	repo := e2eRepo(t)

	out, status := e2eRun(t, repo, base, "look around forever", "MANVI_MAX_STEPS=2")
	if status != 2 {
		t.Fatalf("a turn truncated by the step ceiling exited %d, want 2\n%s", status, out)
	}
}

// TestHeadlessRunFailsLoudlyWhenTheModelServerIsUnreachable is the case a
// harness must never round to success: nothing ran, so nothing passed.
func TestHeadlessRunFailsLoudlyWhenTheModelServerIsUnreachable(t *testing.T) {
	repo := e2eRepo(t)
	// Port 1 with nothing on it: a connection refused, not a timeout, so the
	// test does not wait on one.
	out, status := e2eRun(t, repo, "http://127.0.0.1:1/v1", "write hello.txt")
	if status == 0 {
		t.Fatalf("a run that reached no model exited 0\n%s", out)
	}
	if !strings.Contains(out, "local") {
		t.Errorf("the failure does not name the provider that could not be reached:\n%s", out)
	}
	if entries, err := os.ReadDir(repo); err == nil {
		for _, e := range entries {
			if e.Name() == "hello.txt" {
				t.Error("a run that never reached a model still wrote the file")
			}
		}
	}
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	encoded, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshalling for the failure message: %v", err)
	}
	return string(encoded)
}
