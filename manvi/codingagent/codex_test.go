package codingagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProtocolChild(t *testing.T) {
	args := os.Args
	if len(args) < 2 || !strings.HasPrefix(args[len(args)-1], "codex-fixture:") {
		return
	}
	scenario := strings.TrimPrefix(args[len(args)-1], "codex-fixture:")
	scan := bufio.NewScanner(os.Stdin)
	for scan.Scan() {
		var in struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(scan.Bytes(), &in) != nil {
			os.Exit(3)
		}
		switch in.Method {
		case "initialize":
			fmt.Printf("{\"id\":%s,\"result\":{\"userAgent\":\"fixture/1\",\"platformOs\":\"fixture\"}}\n", in.ID)
		case "thread/start":
			var p struct {
				Cwd      string `json:"cwd"`
				Approval string `json:"approvalPolicy"`
				Reviewer string `json:"approvalsReviewer"`
			}
			if json.Unmarshal(in.Params, &p) != nil {
				os.Exit(4)
			}
			network := scenario == "widened-policy"
			fmt.Printf("{\"id\":%s,\"result\":{\"cwd\":%q,\"approvalPolicy\":%q,\"approvalsReviewer\":%q,\"sandbox\":{\"type\":\"readOnly\",\"networkAccess\":%t},\"model\":\"fixture-model\",\"modelProvider\":\"fixture\",\"thread\":{\"id\":\"thread\",\"cwd\":%q,\"ephemeral\":true}}}\n", in.ID, p.Cwd, p.Approval, p.Reviewer, network, p.Cwd)
		case "turn/start":
			if scenario == "flood" {
				fmt.Println(strings.Repeat("x", MaxFrameBytes+1))
				continue
			}
			fmt.Println(`{"method":"turn/started","params":{"threadId":"thread","turn":{"id":"turn","status":"inProgress","items":[]}}}`)
			fmt.Printf("{\"id\":%s,\"result\":{\"turn\":{\"id\":\"turn\",\"status\":\"inProgress\",\"items\":[]}}}\n", in.ID)
			if scenario == "burst" {
				for i := 0; i < 160; i++ {
					fmt.Println(`{"method":"item/commandExecution/outputDelta","params":{"threadId":"thread","turnId":"turn","itemId":"item","delta":"bounded output\n"}}`)
				}
				fmt.Println(`{"method":"turn/completed","params":{"threadId":"thread","turn":{"id":"turn","status":"completed","items":[]}}}`)
				continue
			}
			if scenario == "foreign" {
				fmt.Println(`{"id":7,"method":"item/commandExecution/requestApproval","params":{"threadId":"other","turnId":"turn","itemId":"item","command":"git status","cwd":"/foreign","startedAtMs":1}}`)
				continue
			}
			if scenario == "question" {
				fmt.Println(`{"id":"request","method":"item/tool/requestUserInput","params":{"threadId":"thread","turnId":"turn","itemId":"item","isBlocking":true,"questions":[{"id":"tests","header":"Tests","question":"Which tests?","isOther":true}]}}`)
			} else {
				fmt.Println(`{"method":"item/started","params":{"threadId":"thread","turnId":"turn","item":{"id":"item","type":"fileChange","status":"inProgress","changes":[{"path":"file.txt","kind":{"type":"update"},"diff":"-old\n+new"}]}}}`)
				if scenario == "resolved-first" {
					fmt.Println(`{"method":"serverRequest/resolved","params":{"threadId":"thread","requestId":"request"}}`)
				}
				fmt.Println(`{"id":"request","method":"item/fileChange/requestApproval","params":{"threadId":"thread","turnId":"turn","itemId":"item","startedAtMs":1}}`)
			}
		case "":
			if string(in.ID) != `"request"` {
				continue
			}
			var response struct {
				Decision string `json:"decision"`
				Answers  map[string]struct {
					Answers []string `json:"answers"`
				} `json:"answers"`
			}
			if json.Unmarshal(in.Result, &response) != nil {
				os.Exit(5)
			}
			if scenario == "question" && len(response.Answers["tests"].Answers) != 1 {
				os.Exit(6)
			}
			if scenario != "question" && response.Decision != "accept" && response.Decision != "decline" {
				os.Exit(7)
			}
			fmt.Println(`{"method":"serverRequest/resolved","params":{"threadId":"thread","requestId":"request"}}`)
			fmt.Println(`{"method":"item/completed","params":{"threadId":"thread","turnId":"turn","item":{"id":"answer","type":"agentMessage","text":"Fixture completed"}}}`)
			fmt.Println(`{"method":"turn/completed","params":{"threadId":"thread","turn":{"id":"turn","status":"completed","items":[]}}}`)
		}
	}
	os.Exit(0)
}

func fixture(t *testing.T, scenario string) *Codex {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := openCodex(ctx, Options{Program: bin, Cwd: root, Mode: "ask"}, []string{"-test.run=^TestProtocolChild$", "--", "codex-fixture:" + scenario})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestStartedCodexWaitsForExplicitInitializationAndRetainsFailedProcess(t *testing.T) {
	for _, scenario := range []string{"normal", "widened-policy"} {
		t.Run(scenario, func(t *testing.T) {
			bin, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			s, err := startCodex(t.Context(), Options{Program: bin, Cwd: t.TempDir(), Mode: "ask"}, []string{"-test.run=^TestProtocolChild$", "--", "codex-fixture:" + scenario})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := s.Close(); err != nil {
					t.Error(err)
				}
			})
			if s.PID() <= 0 || s.Effective().Thread.ID != "" {
				t.Fatal("process start fabricated negotiated configuration")
			}
			if err := s.StartTurn(t.Context(), "Must not be sent"); err == nil {
				t.Fatal("uninitialized turn was accepted")
			}
			err = s.Initialize(t.Context())
			if (err != nil) != (scenario == "widened-policy") {
				t.Fatalf("unexpected initialization outcome: %v", err)
			}
			if NoProcessStarted(err) {
				t.Fatal("spawned process classified as unstarted")
			}
			if scenario == "widened-policy" {
				if s.Effective().Thread.ID != "" {
					t.Fatal("unverified configuration exposed")
				}
				if err := s.StartTurn(t.Context(), "Must not be sent"); err == nil {
					t.Fatal("failed initialization permitted work")
				}
			} else if s.Effective().Thread.ID != "thread" {
				t.Fatal("verified configuration missing")
			}
			if err := s.Initialize(t.Context()); err == nil {
				t.Fatal("initialization replayed")
			}
			exit, err := s.Close()
			if err != nil || !exit.Reaped {
				t.Fatalf("owned process not reaped: %+v %v", exit, err)
			}
		})
	}
}

func TestOnlyKnownPreSpawnErrorsCertifyNoProcessStarted(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, options := range []Options{
		{Program: filepath.Join(root, "missing"), Cwd: root, Mode: "ask"},
		{Program: bin, Cwd: filepath.Join(root, "missing"), Mode: "ask"},
		{Program: bin, Cwd: root, Mode: "bypass"},
	} {
		s, err := StartCodex(t.Context(), options)
		if s != nil || err == nil || !NoProcessStarted(fmt.Errorf("wrapped: %w", err)) {
			t.Fatalf("unproven pre-spawn error: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if s, err := StartCodex(ctx, Options{Program: bin, Cwd: root, Mode: "ask"}); s != nil || !NoProcessStarted(err) {
		t.Fatalf("cancelled pre-spawn was uncertain: %v", err)
	}
	if NoProcessStarted(nil) || NoProcessStarted(errors.New("unknown startup outcome")) {
		t.Fatal("uncertainty certified as unstarted")
	}
}

func TestCodexFullFileRequestRequiresOneExactResponseAndProviderResolution(t *testing.T) {
	s := fixture(t, "normal")
	if s.Effective().Sandbox.Type != "readOnly" || s.PID() <= 0 {
		t.Fatal("missing negotiated policy or process")
	}
	if err := s.StartTurn(t.Context(), "Inspect the fixture"); err != nil {
		t.Fatal(err)
	}
	var request Request
	for request.ID == "" {
		e, err := s.Next(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if e.Request != nil {
			request = *e.Request
		}
	}
	if !strings.Contains(request.Payload, `-old\n+new`) || request.Kind != "permission" {
		t.Fatalf("incomplete review payload: %s", request.Payload)
	}
	wrong := request
	wrong.Payload = "{}"
	if err := s.Respond(t.Context(), wrong, Response{Decision: "allow_once"}); err == nil {
		t.Fatal("changed payload was delivered")
	}
	if err := s.Respond(t.Context(), request, Response{Decision: "allow_once"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Respond(t.Context(), request, Response{Decision: "allow_once"}); err == nil {
		t.Fatal("response replayed")
	}
	resolved, completed, output := false, false, false
	for !completed {
		e, err := s.Next(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		resolved = resolved || e.ResolvedID == request.ID
		completed = e.Status == "completed"
		output = output || e.Text == "Fixture completed"
	}
	if !resolved || !output {
		t.Fatal("provider receipts or output missing")
	}
}

func TestCodexQuestionAnswersAreBoundToQuestionIDs(t *testing.T) {
	s := fixture(t, "question")
	if err := s.StartTurn(t.Context(), "Ask"); err != nil {
		t.Fatal(err)
	}
	var req Request
	for req.ID == "" {
		e, err := s.Next(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if e.Request != nil {
			req = *e.Request
		}
	}
	if err := s.Respond(t.Context(), req, Response{Decision: "allow_once"}); err == nil {
		t.Fatal("question became approval")
	}
	if err := s.Respond(t.Context(), req, Response{Decision: "answer", Answers: map[string][]string{"other": {"tests"}}}); err == nil {
		t.Fatal("answer reached another question")
	}
	req.Questions[0].ID = "other"
	if err := s.Respond(t.Context(), req, Response{Decision: "answer", Answers: map[string][]string{"other": {"tests"}}}); err == nil {
		t.Fatal("mutating returned question metadata rewrote retained authority")
	}
	req.Questions[0].ID = "tests"
	if err := s.Respond(t.Context(), req, Response{Decision: "answer", Answers: map[string][]string{"tests": {"workbench"}}}); err != nil {
		t.Fatal(err)
	}
	for {
		e, err := s.Next(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if e.Status == "completed" {
			break
		}
	}
}

func TestCodexRefusesForeignRequestsAndOversizedFrames(t *testing.T) {
	for _, scenario := range []string{"foreign", "flood", "resolved-first"} {
		t.Run(scenario, func(t *testing.T) {
			s := fixture(t, scenario)
			err := s.StartTurn(t.Context(), "Inspect")
			for i := 0; err == nil && i < 8; i++ {
				_, err = s.Next(t.Context())
			}
			if err == nil {
				t.Fatal("invalid provider stream accepted")
			}
		})
	}
}

func TestCodexRejectsWidenedEffectivePolicyBeforeTurn(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s, err := openCodex(t.Context(), Options{Program: bin, Cwd: t.TempDir(), Mode: "ask"}, []string{"-test.run=^TestProtocolChild$", "--", "codex-fixture:widened-policy"})
	if s != nil {
		s.Close()
	}
	if err == nil {
		t.Fatal("provider widened network policy")
	}
}

func TestCodexOutputBurstWaitsForBoundedConsumerWithoutKillingTheRun(t *testing.T) {
	s := fixture(t, "burst")
	if err := s.StartTurn(t.Context(), "Inspect"); err != nil {
		t.Fatal(err)
	}
	// A native store round trip can briefly stop consumption while the provider
	// emits more than the 64-frame buffer. The producer must wait, not be killed.
	time.Sleep(150 * time.Millisecond)
	count := 0
	for {
		e, err := s.Next(t.Context())
		if err != nil {
			t.Fatal("bounded output burst terminated the run", err)
		}
		if e.Text != "" {
			count++
		}
		if e.Status == "completed" {
			break
		}
	}
	if count != 160 {
		t.Fatalf("lost output frames: %d of 160", count)
	}
}

func TestCodexPreflightRetainsAuthorityAndObservesQueuedResolution(t *testing.T) {
	s := fixture(t, "question")
	if err := s.StartTurn(t.Context(), "Ask"); err != nil {
		t.Fatal(err)
	}
	var request Request
	for request.ID == "" {
		e, err := s.Next(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if e.Request != nil {
			request = *e.Request
		}
	}
	answer := Response{Decision: "answer", Answers: map[string][]string{"tests": {"workbench"}}}
	if err := s.ValidateResponse(request, Response{Decision: "allow_once"}); err == nil {
		t.Fatal("invalid response passed preflight")
	}
	if err := s.ValidateResponse(request, answer); err != nil {
		t.Fatal(err)
	}
	// A callback can resolve while the host reads the durable human answer.
	// Queue the exact provider event at that boundary, before the one-use claim.
	s.mu.Lock()
	s.queue = append(s.queue, packet{Method: "serverRequest/resolved", Params: json.RawMessage(`{"threadId":"thread","requestId":"request"}`)})
	s.mu.Unlock()
	if err := s.ValidateResponse(request, answer); err == nil {
		t.Fatal("queued resolution was ignored before consuming a durable claim")
	}
	e, err := s.Next(t.Context())
	if err != nil || e.ResolvedID != request.ID {
		t.Fatal("preflight lost the resolution receipt", err)
	}
}

func TestInstalledCodexHandshakeWithoutModelTurn(t *testing.T) {
	program := os.Getenv("MANVI_TEST_CODEX_BINARY")
	if program == "" {
		t.Skip("set MANVI_TEST_CODEX_BINARY to verify the installed provider handshake")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	root := t.TempDir()
	s, err := OpenCodex(ctx, Options{Program: program, Cwd: root, Mode: "inspect"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e := s.Effective()
	wanted, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := filepath.EvalSymlinks(e.Cwd)
	if err != nil || actual != wanted || e.ProviderUserAgent == "" || e.Sandbox.Type != "readOnly" || e.Sandbox.NetworkAccess || e.ApprovalPolicy != "never" || s.started {
		t.Fatalf("unexpected effective settings: %+v (%v)", e, err)
	}
	t.Logf("Installed provider %s: read-only ephemeral thread verified, no model turn sent", e.ProviderUserAgent)
	exit, err := s.Close()
	if err != nil || !exit.Reaped {
		t.Fatalf("shutdown unconfirmed: %+v %v", exit, err)
	}
}

func TestInstalledCodexReadOnlyTurn(t *testing.T) {
	program := os.Getenv("MANVI_TEST_CODEX_BINARY")
	if program == "" || os.Getenv("MANVI_TEST_CODEX_TURN") != "1" {
		t.Skip("opt-in installed-provider model turn")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	s, err := OpenCodex(ctx, Options{Program: program, Cwd: t.TempDir(), Mode: "inspect"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.StartTurn(ctx, "Reply with MANVI_CODEX_TURN_OK only. Do not call tools or change files."); err != nil {
		t.Fatal(err)
	}
	var text string
	for {
		e, err := s.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if e.Request != nil {
			t.Fatal("read-only marker probe unexpectedly requested user action")
		}
		text += e.Text
		if e.Status != "" {
			if e.Status != "completed" || !strings.Contains(text, "MANVI_CODEX_TURN_OK") {
				t.Fatalf("provider did not complete the marker probe: status %s", e.Status)
			}
			break
		}
	}
	exit, err := s.Close()
	if err != nil || !exit.Reaped {
		t.Fatalf("shutdown unconfirmed: %+v %v", exit, err)
	}
	t.Log("Installed Codex completed one read-only marker turn and its process was reaped")
}
