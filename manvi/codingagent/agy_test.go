package codingagent

// The fixture child below speaks Antigravity's ACP v1 JSON-RPC 2.0 protocol
// back at the adapter. Its frames mirror recorded traffic from `agy acp` 1.5.3:
// `initialize`, `session/new` / `session/resume`, `session/set_mode`,
// `session/prompt`, `session/update` notifications (`agent_message_chunk`,
// `tool_call`, `tool_call_update`, `available_commands_update`), and
// `session/request_permission` server-to-client requests.

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

func TestAgyProtocolChild(t *testing.T) {
	marker := ""
	for _, a := range os.Args {
		if strings.HasPrefix(a, "agy-fixture:") {
			marker = strings.TrimPrefix(a, "agy-fixture:")
		}
	}
	if marker == "" {
		return
	}
	scenario := marker
	emit := func(format string, a ...any) { fmt.Printf(format+"\n", a...) }

	sessionID := "agy-sess-1111"
	currentMode := "default"
	promptTurns := 0

	scan := bufio.NewScanner(os.Stdin)
	scan.Buffer(make([]byte, 4096), MaxFrameBytes+1)
	for scan.Scan() {
		var in struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
			Result  json.RawMessage `json:"result"`
		}
		if json.Unmarshal(scan.Bytes(), &in) != nil || in.JSONRPC != "2.0" {
			os.Exit(3)
		}

		// Client response to a server-initiated permission request.
		if in.Method == "" {
			var decision struct {
				Outcome struct {
					Outcome  string `json:"outcome"`
					OptionID string `json:"optionId"`
				} `json:"outcome"`
			}
			_ = json.Unmarshal(in.Result, &decision)
			tag := decision.Outcome.OptionID
			if tag == "" {
				tag = decision.Outcome.Outcome
			}
			emit(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":%q,"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"decision:%s"}}}}`, sessionID, tag)
			emit(`{"jsonrpc":"2.0","id":"prompt-active","result":{"stopReason":"end_turn"}}`)
			continue
		}

		switch in.Method {
		case "initialize":
			switch scenario {
			case "impostor":
				emit(`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentInfo":{"name":"Other Server","version":"1.0.0"}}}`, in.ID)
			case "wrong-protocol":
				emit(`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":2,"agentInfo":{"name":"Antigravity ACP Server","version":"1.5.3"}}}`, in.ID)
			case "non-jsonrpc2":
				emit(`{"jsonrpc":"1.0","id":%s,"result":{"protocolVersion":1}}`, in.ID)
			case "duplicate-key":
				emit(`{"jsonrpc":"2.0","jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1}}`, in.ID)
			default:
				emit(`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentInfo":{"name":"Antigravity ACP Server","version":"1.5.3"},"agentCapabilities":{"loadSession":true}}}`, in.ID)
			}

		case "session/new":
			if scenario == "resume-expected" {
				os.Exit(9)
			}
			if scenario == "pre-turn-callback" {
				emit(`{"jsonrpc":"2.0","id":"cb-early","method":"session/request_permission","params":{"sessionId":%q,"toolCall":{"toolCallId":"tc-0","name":"run_command"},"options":[{"optionId":"allow","kind":"allow_once"}]}}`, sessionID)
			}
			// Real `agy acp` emits `available_commands_update` asynchronously around session creation.
			if scenario == "large-commands" {
				bigDesc := strings.Repeat("d", 320*1024)
				emit(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":%q,"update":{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"skills-pack","description":%q}]}}}`, sessionID, bigDesc)
			} else {
				emit(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":%q,"update":{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"clear"}]}}}`, sessionID)
			}
			modelField := `"models":{"currentModelId":"gemini-4-argon-medium"}`
			switch scenario {
			case "missing-model":
				modelField = `"models":{}`
			case "config-option-model":
				modelField = `"models":{},"configOptions":[{"category":"model","currentValue":"gemini-4-config"}]`
			}
			modes := `[{"id":"default"},{"id":"accept-edits"},{"id":"plan"}]`
			if scenario == "missing-plan-mode" {
				modes = `[{"id":"default"},{"id":"accept-edits"}]`
			}
			emit(`{"jsonrpc":"2.0","id":%s,"result":{"sessionId":%q,%s,"modes":{"currentModeId":%q,"availableModes":%s}}}`,
				in.ID, sessionID, modelField, currentMode, modes)

		case "session/resume":
			var req struct {
				SessionID string `json:"sessionId"`
			}
			_ = json.Unmarshal(in.Params, &req)
			sessionID = req.SessionID
			returned := sessionID
			if scenario == "resume-mismatch" {
				returned = "different-session-id"
			}
			emit(`{"jsonrpc":"2.0","id":%s,"result":{"sessionId":%q,"models":{"currentModelId":"gemini-4-argon-medium"},"modes":{"currentModeId":%q,"availableModes":[{"id":"default"},{"id":"accept-edits"},{"id":"plan"}]}}}`,
				in.ID, returned, currentMode)

		case "session/set_mode":
			var req struct {
				ModeID string `json:"modeId"`
			}
			_ = json.Unmarshal(in.Params, &req)
			if req.ModeID != "default" && req.ModeID != "accept-edits" && req.ModeID != "plan" {
				emit(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32602,"message":"Unknown mode"}}`, in.ID)
				continue
			}
			currentMode = req.ModeID
			emit(`{"jsonrpc":"2.0","id":%s,"result":{}}`, in.ID)

		case "session/prompt":
			promptTurns++
			switch scenario {
			case "permission", "permission-allow-only":
				// Rewrite the stored prompt ID expectation by responding to this exact ID on completion;
				// capture it by echoing in.ID when answering the permission response if needed.
				options := `[{"optionId":"allow","name":"Allow","kind":"allow_once"},{"optionId":"deny","name":"Deny","kind":"reject_once"}]`
				if scenario == "permission-allow-only" {
					options = `[{"optionId":"allow","name":"Allow","kind":"allow_once"}]`
				}
				emit(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":%q,"update":{"sessionUpdate":"tool_call","toolCallId":"tc-1","name":"run_command","status":"in_progress","rawInput":{"command":"echo hi"}}}}`, sessionID)
				emit(`{"jsonrpc":"2.0","id":"perm-1","method":"session/request_permission","params":{"sessionId":%q,"toolCall":{"toolCallId":"tc-1","name":"run_command","rawInput":{"command":"echo hi"}},"options":%s}}`, sessionID, options)
				// Read the adapter's response to perm-1 so we can complete with the exact prompt ID `in.ID`.
				if scan.Scan() {
					var resp struct {
						Result struct {
							Outcome struct {
								Outcome  string `json:"outcome"`
								OptionID string `json:"optionId"`
							} `json:"outcome"`
						} `json:"result"`
					}
					_ = json.Unmarshal(scan.Bytes(), &resp)
					tag := resp.Result.Outcome.OptionID
					if tag == "" {
						tag = resp.Result.Outcome.Outcome
					}
					emit(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":%q,"update":{"sessionUpdate":"tool_call_update","toolCallId":"tc-1","name":"run_command","status":"completed","rawOutput":{"output":"hi"}}}}`, sessionID)
					emit(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":%q,"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"decision:%s"}}}}`, sessionID, tag)
					emit(`{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}`, in.ID)
				}
			case "elicitation":
				emit(`{"jsonrpc":"2.0","id":"elic-1","method":"mcpServer/elicitation/request","params":{"message":"Which env?"}}`)
			case "duplicate-callback":
				emit(`{"jsonrpc":"2.0","id":"perm-dup","method":"session/request_permission","params":{"sessionId":%q,"toolCall":{"toolCallId":"tc-1","name":"run_command"},"options":[{"optionId":"allow","kind":"allow_once"}]}}`, sessionID)
				emit(`{"jsonrpc":"2.0","id":"perm-dup","method":"session/request_permission","params":{"sessionId":%q,"toolCall":{"toolCallId":"tc-1","name":"run_command"},"options":[{"optionId":"allow","kind":"allow_once"}]}}`, sessionID)
			case "foreign-session":
				emit(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"foreign-sess","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"sneak"}}}}`)
			case "oversized":
				emit("%s", strings.Repeat("x", MaxFrameBytes+1))
			case "cancelled":
				emit(`{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"cancelled"}}`, in.ID)
			case "rpc-error":
				emit(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"provider boom"}}`, in.ID)
			case "silent":
				// Never answers: stays in flight.
			case "multiturn":
				emit(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":%q,"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"AGY_TURN_%d mode=%s"}}}}`, sessionID, promptTurns, currentMode)
				emit(`{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}`, in.ID)
			case "multiturn-permission":
				if promptTurns == 1 {
					emit(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":%q,"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"AGY_TURN_1"}}}}`, sessionID)
					emit(`{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}`, in.ID)
				} else {
					emit(`{"jsonrpc":"2.0","id":"perm-t2","method":"session/request_permission","params":{"sessionId":%q,"toolCall":{"toolCallId":"tc-2","name":"run_command"},"options":[{"optionId":"allow","kind":"allow_once"}]}}`, sessionID)
				}
			default:
				emit(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":%q,"update":{"sessionUpdate":"tool_call","toolCallId":"tc-1","name":"run_command","status":"in_progress","rawInput":{"command":"ls"}}}}`, sessionID)
				emit(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":%q,"update":{"sessionUpdate":"tool_call_update","toolCallId":"tc-1","name":"run_command","status":"completed","rawOutput":{"output":"README.md"}}}}`, sessionID)
				emit(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":%q,"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"AGY_OK mode=%s"}}}}`, sessionID, currentMode)
				emit(`{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}`, in.ID)
			}
		}
	}
	os.Exit(0)
}

func agyFixtureArgs(scenario string) func(mode string) []string {
	return func(mode string) []string {
		return append([]string{"-test.run=^TestAgyProtocolChild$", "--", "agy-fixture:" + scenario}, agyArgs(mode)...)
	}
}

func openAgyFixture(t *testing.T, scenario, mode string) *Agy {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	s, err := openAgy(t.Context(), Options{
		Program:           bin,
		Cwd:               root,
		Mode:              mode,
		AcknowledgeBypass: mode == "bypass",
	}, agyFixtureArgs(scenario))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.Close() })
	return s
}

func collectAgy(t *testing.T, s *Agy) (text, status string, tools []Tool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		ev, err := s.Next(ctx)
		if err != nil {
			t.Fatalf("Next failed: %v", err)
		}
		text += ev.Text
		tools = append(tools, ev.Tools...)
		if ev.Status != "" {
			return text, ev.Status, tools
		}
	}
}

func TestAgyPreSpawnRefusalsGuaranteeNoProcessStarted(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	wrapper := filepath.Join(t.TempDir(), "agy")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec true\n"), 0o700); err != nil { //nolint:gosec // an executable stand-in under t.TempDir
		t.Fatal(err)
	}

	// Wrapper script must surface *AgyWrapperError AND NoProcessStarted.
	if s, err := StartAgy(t.Context(), Options{Program: wrapper, Cwd: root, Mode: "ask"}); s != nil || err == nil {
		t.Fatalf("wrapper script was allowed: %v", err)
	} else {
		var werr *AgyWrapperError
		if !errors.As(err, &werr) || !NoProcessStarted(err) {
			t.Fatalf("expected AgyWrapperError + NoProcessStarted, got %T: %v", err, err)
		}
	}

	for name, opts := range map[string]Options{
		"missing-binary":   {Program: filepath.Join(root, "does-not-exist"), Cwd: root, Mode: "ask"},
		"relative-cwd":     {Program: bin, Cwd: "relative/path", Mode: "ask"},
		"newline-in-cwd":   {Program: bin, Cwd: root + "\nbad", Mode: "ask"},
		"unacked-bypass":   {Program: bin, Cwd: root, Mode: "bypass", AcknowledgeBypass: false},
		"acked-non-bypass": {Program: bin, Cwd: root, Mode: "ask", AcknowledgeBypass: true},
		"unsupported-mode": {Program: bin, Cwd: root, Mode: "yolo"},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := StartAgy(t.Context(), opts)
			if s != nil || err == nil || !NoProcessStarted(fmt.Errorf("wrapped: %w", err)) {
				t.Fatalf("expected proven pre-spawn refusal, got s=%v err=%v", s, err)
			}
		})
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if s, err := StartAgy(canceled, Options{Program: bin, Cwd: root, Mode: "ask"}); s != nil || !NoProcessStarted(err) {
		t.Fatalf("canceled context was not a proven pre-spawn refusal: %v", err)
	}
}

func TestAgyNegotiatesAllSixModesAndStreamsToolsAndText(t *testing.T) {
	cases := []struct {
		mode         string
		wantACPMode  string
		wantApproval string
		wantReviewer string
		wantSandbox  string
	}{
		{"inspect", "plan", "plan", "user", "readOnly"},
		{"ask", "default", "default", "user", "workspaceWrite"},
		{"edit", "accept-edits", "accept-edits", "user", "workspaceWrite"},
		{"auto_review", "default", "default", "auto_review", "workspaceWrite"},
		{"preapproved", "accept-edits", "never", "user", "workspaceWrite"},
		{"bypass", "accept-edits", "bypass", "user", "dangerFullAccess"},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			args := agyArgs(tc.mode)
			if !strings.Contains(strings.Join(args, " "), "--include-default-customizations=false") {
				t.Fatalf("agyArgs(%q) omitted customization isolation: %v", tc.mode, args)
			}
			if (tc.mode == "bypass") != strings.Contains(strings.Join(args, " "), "--dangerously-skip-permissions") {
				t.Fatalf("agyArgs(%q) bypass flag mismatch: %v", tc.mode, args)
			}

			s := openAgyFixture(t, "ok", tc.mode)
			eff := s.Effective()
			if eff.ApprovalPolicy != tc.wantApproval || eff.ApprovalsReviewer != tc.wantReviewer || eff.Sandbox.Type != tc.wantSandbox {
				t.Fatalf("effective policy = %+v", eff)
			}
			if eff.Model != "gemini-4-argon-medium" || eff.ModelProvider != "antigravity" || eff.Thread.ID != "agy-sess-1111" {
				t.Fatalf("effective session = %+v", eff)
			}
			if err := s.StartTurn(t.Context(), "verify repo"); err != nil {
				t.Fatal(err)
			}
			text, status, tools := collectAgy(t, s)
			if status != "completed" || !strings.Contains(text, "AGY_OK mode="+tc.wantACPMode) {
				t.Fatalf("turn output = %q status = %q", text, status)
			}
			if len(tools) != 2 || tools[0].Name != "run_command" || tools[1].Output != "README.md" {
				t.Fatalf("tools = %+v", tools)
			}
		})
	}
}

func TestAgyResumeVerifiesReturnedSessionID(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	s, err := openAgy(t.Context(), Options{
		Program: bin,
		Cwd:     root,
		Mode:    "ask",
		Resume:  "resumed-uuid-42",
	}, agyFixtureArgs("resume-expected"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = s.Close() }()
	if s.Effective().Thread.ID != "resumed-uuid-42" {
		t.Fatalf("resumed thread ID = %q", s.Effective().Thread.ID)
	}

	if bad, err := openAgy(t.Context(), Options{
		Program: bin,
		Cwd:     root,
		Mode:    "ask",
		Resume:  "resumed-uuid-42",
	}, agyFixtureArgs("resume-mismatch")); err == nil {
		_, _ = bad.Close()
		t.Fatal("mismatched resumed session ID was accepted")
	}
}

func TestAgyHandshakeAndFrameValidationRejectProtocolViolations(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, scenario := range []string{
		"impostor",
		"wrong-protocol",
		"non-jsonrpc2",
		"duplicate-key",
		"pre-turn-callback",
		"missing-model",
		"missing-plan-mode",
	} {
		t.Run(scenario, func(t *testing.T) {
			mode := "ask"
			if scenario == "missing-plan-mode" {
				mode = "inspect"
			}
			s, err := openAgy(t.Context(), Options{Program: bin, Cwd: root, Mode: mode}, agyFixtureArgs(scenario))
			if err == nil {
				_, _ = s.Close()
				t.Fatalf("scenario %q succeeded unexpectedly", scenario)
			}
		})
	}

	// ConfigOptions model fallback succeeds cleanly.
	s := openAgyFixture(t, "config-option-model", "ask")
	if s.Effective().Model != "gemini-4-config" {
		t.Fatalf("expected config-option model fallback, got %q", s.Effective().Model)
	}
}

func TestAgyPermissionApprovalAndDenialSingleUseContract(t *testing.T) {
	for _, tc := range []struct {
		name     string
		scenario string
		decision string
		want     string
	}{
		{"allow_once", "permission", "allow_once", "decision:allow"},
		{"deny_with_reject_option", "permission", "deny", "decision:deny"},
		{"deny_falls_back_to_cancelled", "permission-allow-only", "deny", "decision:cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openAgyFixture(t, tc.scenario, "ask")
			if err := s.StartTurn(t.Context(), "run echo"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			var req *Request
			for req == nil {
				ev, err := s.Next(ctx)
				if err != nil {
					t.Fatal(err)
				}
				req = ev.Request
			}

			// Reject invalid decisions or tampered digest before consuming.
			if err := s.ValidateResponse(*req, Response{Decision: "allow_always"}); err == nil {
				t.Fatal("allow_always must be rejected")
			}
			tampered := *req
			tampered.Digest = "sha256:0000"
			if err := s.ValidateResponse(tampered, Response{Decision: tc.decision}); err == nil {
				t.Fatal("tampered digest must be rejected")
			}

			if err := s.Respond(ctx, *req, Response{Decision: tc.decision}); err != nil {
				t.Fatal(err)
			}
			// Second response to the same callback must fail as already consumed.
			if err := s.Respond(ctx, *req, Response{Decision: tc.decision}); err == nil {
				t.Fatal("replayed callback response was accepted")
			}

			text, status, _ := collectAgy(t, s)
			if status != "completed" || !strings.Contains(text, tc.want) {
				t.Fatalf("got text=%q status=%q, want %q", text, status, tc.want)
			}
		})
	}
}

func TestAgyMidTurnAdversarialFramesAndStatuses(t *testing.T) {
	for _, scenario := range []string{"elicitation", "duplicate-callback", "foreign-session", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			s := openAgyFixture(t, scenario, "ask")
			if err := s.StartTurn(t.Context(), "go"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			for {
				ev, err := s.Next(ctx)
				if err != nil {
					return // expected protocol / callback error
				}
				if ev.Status != "" {
					t.Fatalf("scenario %q finished cleanly with %q instead of failing", scenario, ev.Status)
				}
			}
		})
	}

	sCancel := openAgyFixture(t, "cancelled", "ask")
	if err := sCancel.StartTurn(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	if _, status, _ := collectAgy(t, sCancel); status != "interrupted" {
		t.Fatalf("cancelled turn mapped to %q, want interrupted", status)
	}

	sErr := openAgyFixture(t, "rpc-error", "ask")
	if err := sErr.StartTurn(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	if _, status, _ := collectAgy(t, sErr); status != "failed" {
		t.Fatalf("rpc error turn mapped to %q, want failed", status)
	}
}

func TestAgyContinueTurnLifecycleAndUnresolvedWriteHalt(t *testing.T) {
	s := openAgyFixture(t, "multiturn", "ask")
	requireRefused(t, s.ContinueTurn(t.Context(), "before first"), ErrNoCompletedTurn)

	if err := s.StartTurn(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	requireRefused(t, s.ContinueTurn(t.Context(), "in flight"), ErrTurnInFlight)

	text1, status1, _ := collectAgy(t, s)
	if status1 != "completed" || !strings.Contains(text1, "AGY_TURN_1") {
		t.Fatalf("turn 1: %q %q", text1, status1)
	}

	requireRefused(t, s.ContinueTurn(t.Context(), "   "), ErrContinueMessage)

	if err := s.ContinueTurn(t.Context(), "second"); err != nil {
		t.Fatal(err)
	}
	text2, status2, _ := collectAgy(t, s)
	if status2 != "completed" || !strings.Contains(text2, "AGY_TURN_2") || s.ledger.turns != 2 {
		t.Fatalf("turn 2: %q %q turns=%d", text2, status2, s.ledger.turns)
	}

	for i := 2; i <= MaxContinuations; i++ {
		if err := s.ContinueTurn(t.Context(), fmt.Sprintf("follow-up %d", i)); err != nil {
			t.Fatalf("follow-up %d: %v", i, err)
		}
		if text, status, _ := collectAgy(t, s); status != "completed" || !strings.Contains(text, fmt.Sprintf("AGY_TURN_%d", i+1)) {
			t.Fatalf("follow-up %d: %q %q", i, text, status)
		}
	}
	requireRefused(t, s.ContinueTurn(t.Context(), "one too many"), ErrContinueLimit)

	// Live halted session refuses with ErrContinuationHalted without writing.
	sHalted := openAgyFixture(t, "multiturn", "ask")
	if err := sHalted.StartTurn(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	collectAgy(t, sHalted)
	sHalted.ledger.halted = true
	requireRefused(t, sHalted.ContinueTurn(t.Context(), "after unresolved"), ErrContinuationHalted)

	// Broken pipe on stdin during ContinueTurn marks DeliveryUnresolved, sets
	// ledger.halted, cancels the wire, and never writes a second time.
	sBroken := openAgyFixture(t, "multiturn", "ask")
	if err := sBroken.StartTurn(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	collectAgy(t, sBroken)
	fw := &failingWriter{}
	sBroken.wire.stdin = fw
	err := sBroken.ContinueTurn(t.Context(), "maybe delivered")
	if err == nil || !DeliveryUnresolved(err) || ContinueRefused(err) {
		t.Fatalf("broken pipe must be DeliveryUnresolved, got %v", err)
	}
	requireRefused(t, sBroken.ContinueTurn(t.Context(), "maybe delivered"), ErrSessionClosed)
	if fw.writes.Load() != 1 || !sBroken.ledger.halted {
		t.Fatalf("writes=%d halted=%t", fw.writes.Load(), sBroken.ledger.halted)
	}
}

func TestAgyPreapprovedAutoDeniesPermissionRequestsWithoutBlocking(t *testing.T) {
	for _, tc := range []struct {
		name     string
		scenario string
		want     string
	}{
		{"reject_option", "permission", "decision:deny"},
		{"cancelled_fallback", "permission-allow-only", "decision:cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openAgyFixture(t, tc.scenario, "preapproved")
			if s.Effective().ApprovalPolicy != "never" {
				t.Fatalf("preapproved ApprovalPolicy = %q, want never", s.Effective().ApprovalPolicy)
			}
			if err := s.StartTurn(t.Context(), "run unapproved command"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var text strings.Builder
			for {
				ev, err := s.Next(ctx)
				if err != nil {
					t.Fatalf("unexpected error in preapproved mode: %v", err)
				}
				if ev.Request != nil {
					t.Fatalf("preapproved mode must never surface blocking human Request, got %+v", ev.Request)
				}
				text.WriteString(ev.Text)
				if ev.Status != "" {
					if ev.Status != "completed" || !strings.Contains(text.String(), tc.want) {
						t.Fatalf("got status=%q text=%q, want completed containing %q", ev.Status, text.String(), tc.want)
					}
					break
				}
			}
		})
	}
}

func TestAgyAcceptsLargeAvailableCommandsFrameAbove256KiB(t *testing.T) {
	s := openAgyFixture(t, "large-commands", "ask")
	if err := s.StartTurn(t.Context(), "verify large commands frame"); err != nil {
		t.Fatal(err)
	}
	text, status, _ := collectAgy(t, s)
	if status != "completed" || !strings.Contains(text, "AGY_OK") {
		t.Fatalf("large-commands turn failed: status=%q text=%q", status, text)
	}
}

func TestAgyRealBinaryACPHandshakeAndCrossProcessResume(t *testing.T) {
	var realAgy string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		cand := filepath.Join(home, ".local", "bin", "agy")
		if CheckLaunchableAgy(cand) == nil {
			realAgy = cand
		}
	}
	if realAgy == "" {
		t.Skip("standalone launchable ~/.local/bin/agy not installed on this host")
	}
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	s1, err := StartAgy(ctx, Options{Program: realAgy, Cwd: root, Mode: "edit"})
	if err != nil {
		t.Fatalf("StartAgy real binary: %v", err)
	}
	if err := s1.Initialize(ctx); err != nil {
		_, _ = s1.Close()
		t.Fatalf("Initialize real binary: %v", err)
	}
	eff1 := s1.Effective()
	if eff1.Thread.ID == "" || eff1.Model == "" || eff1.ApprovalPolicy != "accept-edits" {
		_, _ = s1.Close()
		t.Fatalf("unexpected real agy effective config: %+v", eff1)
	}
	if _, err := s1.Close(); err != nil {
		t.Fatalf("Close s1: %v", err)
	}

	s2, err := StartAgy(ctx, Options{Program: realAgy, Cwd: root, Mode: "preapproved", Resume: eff1.Thread.ID})
	if err != nil {
		t.Fatalf("StartAgy resume real binary: %v", err)
	}
	defer func() { _, _ = s2.Close() }()
	if err := s2.Initialize(ctx); err != nil {
		t.Fatalf("Initialize resumed real binary: %v", err)
	}
	eff2 := s2.Effective()
	if eff2.Thread.ID != eff1.Thread.ID || eff2.ApprovalPolicy != "never" {
		t.Fatalf("resumed effective = %+v, want thread %q and approval never", eff2, eff1.Thread.ID)
	}
}
