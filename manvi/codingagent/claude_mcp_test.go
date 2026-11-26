package codingagent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestClaudeMcpConfigJSONAndRejectionCases(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := claudeMcpConfigJSON([]McpServer{
		{
			Name:    "waylode",
			Command: bin,
			Args:    []string{"--stdio"},
			Env:     []McpEnvVar{{Name: "WAYLODE_HOST", Value: "claude"}},
		},
		{
			Name:    "devmap",
			Command: bin,
			Args:    []string{"mcp"},
		},
	})
	if err != nil {
		t.Fatalf("claudeMcpConfigJSON: %v", err)
	}
	var parsed struct {
		McpServers map[string]struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("unmarshal config JSON: %v", err)
	}
	wl, ok := parsed.McpServers["waylode"]
	if !ok || wl.Type != "stdio" || wl.Command != bin || !slices.Equal(wl.Args, []string{"--stdio"}) || wl.Env["WAYLODE_HOST"] != "claude" {
		t.Fatalf("unexpected waylode entry: %+v", wl)
	}
	dm, ok := parsed.McpServers["devmap"]
	if !ok || dm.Type != "stdio" || dm.Command != bin || !slices.Equal(dm.Args, []string{"mcp"}) || len(dm.Env) != 0 {
		t.Fatalf("unexpected devmap entry: %+v", dm)
	}

	for name, servers := range map[string][]McpServer{
		"relative-command": {{Name: "waylode", Command: "waylode-mcp"}},
		"duplicate-server": {{Name: "wl", Command: bin}, {Name: "wl", Command: bin}},
		"duplicate-env":    {{Name: "wl", Command: bin, Env: []McpEnvVar{{Name: "A", Value: "1"}, {Name: "A", Value: "2"}}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := claudeMcpConfigJSON(servers); err == nil {
				t.Fatalf("expected claudeMcpConfigJSON to reject %s", name)
			}
		})
	}
}

func TestClaudeMcpStartPassesConfigFlagAndKeepsSettingSourcesUser(t *testing.T) {
	argsOut := filepath.Join(t.TempDir(), "claude-args.json")
	t.Setenv("CLAUDE_ARGS_CAPTURE_OUT", argsOut)
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	s, err := startClaude(t.Context(), Options{
		Program: bin,
		Cwd:     root,
		Mode:    "ask",
		McpServers: []McpServer{{
			Name:    "waylode",
			Command: bin,
			Args:    []string{},
			Env:     []McpEnvVar{{Name: "WL_TEST", Value: "1"}},
		}},
	}, func(mode, session string) []string {
		return append([]string{"-test.run=^TestClaudeMcpArgsChild$", "--", "claude-args-fixture"}, claudeArgs(mode, session)...)
	})
	if err != nil {
		t.Fatalf("startClaude with McpServers: %v", err)
	}
	defer func() { _, _ = s.Close() }()

	<-s.Done()
	raw, err := os.ReadFile(argsOut) //nolint:gosec // G304: reads path created by this test
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	var argv []string
	if err := json.Unmarshal(raw, &argv); err != nil {
		t.Fatalf("decode captured argv (%q): %v", string(raw), err)
	}
	srcIdx := slices.Index(argv, "--setting-sources")
	if srcIdx < 0 || srcIdx+1 >= len(argv) || argv[srcIdx+1] != "user" {
		t.Fatalf("expected --setting-sources user in %v", argv)
	}
	mcpIdx := slices.Index(argv, "--mcp-config")
	if mcpIdx < 0 || mcpIdx+1 >= len(argv) || !strings.Contains(argv[mcpIdx+1], `"waylode"`) {
		t.Fatalf("expected --mcp-config with waylode entry in %v", argv)
	}
	if !slices.Contains(argv, "--strict-mcp-config") {
		t.Fatalf("expected --strict-mcp-config, so the operator's own MCP servers do not load beside Manvi's, in %v", argv)
	}
}

// With no server attached (every opt-out set, or none resolved), the session
// still ignores the operator's user-level MCP servers and plugins.
func TestClaudeStartWithoutServersStillIgnoresUserMcpServers(t *testing.T) {
	argsOut := filepath.Join(t.TempDir(), "claude-args.json")
	t.Setenv("CLAUDE_ARGS_CAPTURE_OUT", argsOut)
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := startClaude(t.Context(), Options{Program: bin, Cwd: root, Mode: "inspect"}, func(mode, session string) []string {
		return append([]string{"-test.run=^TestClaudeMcpArgsChild$", "--", "claude-args-fixture"}, claudeArgs(mode, session)...)
	})
	if err != nil {
		t.Fatalf("startClaude without McpServers: %v", err)
	}
	defer func() { _, _ = s.Close() }()
	<-s.Done()
	raw, err := os.ReadFile(argsOut) //nolint:gosec // G304: reads path created by this test
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	var argv []string
	if err := json.Unmarshal(raw, &argv); err != nil {
		t.Fatalf("decode captured argv (%q): %v", string(raw), err)
	}
	if !slices.Contains(argv, "--strict-mcp-config") || slices.Contains(argv, "--mcp-config") {
		t.Fatalf("want --strict-mcp-config and no --mcp-config, got %v", argv)
	}
}

func TestClaudeMcpArgsChild(t *testing.T) {
	if !slices.Contains(os.Args, "claude-args-fixture") {
		return
	}
	out := os.Getenv("CLAUDE_ARGS_CAPTURE_OUT")
	if out == "" {
		return
	}
	raw, err := json.Marshal(os.Args)
	if err != nil {
		os.Exit(2)
	}
	tmp := out + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil { //nolint:gosec // G703: a test helper child writing to the capture path its own test passed in the environment
		os.Exit(3)
	}
	if err := os.Rename(tmp, out); err != nil { //nolint:gosec // G703: the same test-owned capture path
		os.Exit(4)
	}
	os.Exit(0)
}

func TestClaudeAskUserQuestionAndElicitationHandling(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	openInteractive := func(t *testing.T, scenario string) *Claude {
		t.Helper()
		s, err := startClaude(t.Context(), Options{
			Program: bin,
			Cwd:     root,
			Mode:    "ask",
		}, func(mode, session string) []string {
			return append([]string{"-test.run=^TestClaudeInteractiveCallbacksChild$", "--", "claude-interactive:" + scenario}, claudeArgs(mode, session)...)
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = s.Close() })
		if err := s.Initialize(t.Context()); err != nil {
			t.Fatalf("Initialize(%s): %v", scenario, err)
		}
		return s
	}

	t.Run("ask_user_question allow and deny", func(t *testing.T) {
		s := openInteractive(t, "ask-allow")
		if err := s.StartTurn(t.Context(), "ask user"); err != nil {
			t.Fatal(err)
		}
		var req *Request
		var texts []string
		for {
			ev, err := s.Next(t.Context())
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if ev.Text != "" {
				texts = append(texts, ev.Text)
			}
			if ev.Request != nil {
				req = ev.Request
				break
			}
		}
		if req.Kind != "question" || len(req.Questions) != 2 {
			t.Fatalf("unexpected AskUserQuestion request: %+v", req)
		}
		if req.Questions[0].ID != "q_0" || req.Questions[1].ID != "q_1" {
			t.Fatalf("unexpected question IDs: %+v", req.Questions)
		}
		// Reject invalid decisions or missing questions beforeRespond.
		if err := s.ValidateResponse(*req, Response{Decision: "allow_once"}); err == nil {
			t.Fatal("expected allow_once on AskUserQuestion to fail")
		}
		if err := s.ValidateResponse(*req, Response{Decision: "answer", Answers: map[string][]string{"q_0": {"SQLite"}}}); err == nil {
			t.Fatal("expected incomplete answers to fail")
		}
		ans := Response{
			Decision: "answer",
			Answers: map[string][]string{
				"q_0": {"SQLite"},
				"q_1": {"FastAPI"},
			},
		}
		if err := s.ValidateResponse(*req, ans); err != nil {
			t.Fatalf("ValidateResponse: %v", err)
		}
		if err := s.Respond(t.Context(), *req, ans); err != nil {
			t.Fatalf("Respond: %v", err)
		}
		for {
			ev, err := s.Next(t.Context())
			if err != nil {
				t.Fatalf("Next after answer: %v", err)
			}
			if ev.Text != "" {
				texts = append(texts, ev.Text)
			}
			if ev.Status != "" {
				if ev.Status != "completed" || !strings.Contains(strings.Join(texts, ""), "answered:SQLite|FastAPI") {
					t.Fatalf("unexpected turn finish: status=%q texts=%v", ev.Status, texts)
				}
				break
			}
		}
		_, _ = s.Close()

		// Now verify deny on AskUserQuestion.
		sDeny := openInteractive(t, "ask-deny")
		if err := sDeny.StartTurn(t.Context(), "ask user deny"); err != nil {
			t.Fatal(err)
		}
		var denyReq *Request
		for {
			ev, err := sDeny.Next(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if ev.Request != nil {
				denyReq = ev.Request
				break
			}
		}
		if err := sDeny.Respond(t.Context(), *denyReq, Response{Decision: "deny"}); err != nil {
			t.Fatalf("Respond deny: %v", err)
		}
		var denyTexts []string
		for {
			ev, err := sDeny.Next(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if ev.Text != "" {
				denyTexts = append(denyTexts, ev.Text)
			}
			if ev.Status != "" {
				if ev.Status != "completed" || !strings.Contains(strings.Join(denyTexts, ""), "declined") {
					t.Fatalf("unexpected deny completion: status=%q texts=%v", ev.Status, denyTexts)
				}
				break
			}
		}
	})

	t.Run("elicitation form coercion and url auto-cancel", func(t *testing.T) {
		s := openInteractive(t, "elicit-url-and-form")
		if err := s.StartTurn(t.Context(), "run mcp flow"); err != nil {
			t.Fatal(err)
		}
		var req *Request
		var saw strings.Builder
		for {
			ev, err := s.Next(t.Context())
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			saw.WriteString(ev.Text)
			if ev.Request != nil {
				req = ev.Request
				break
			}
		}
		// URL elicitation was auto-cancelled silently before the form request arrived.
		if req.Kind != "question" || len(req.Questions) != 3 {
			t.Fatalf("unexpected form request: %+v", req)
		}
		// Sorted keys: confirm (boolean), retries (integer), scale (number).
		if err := s.ValidateResponse(*req, Response{
			Decision: "answer",
			Answers: map[string][]string{
				"q_0": {"not-bool"},
				"q_1": {"3"},
				"q_2": {"1.5"},
			},
		}); err == nil {
			t.Fatal("expected invalid boolean answer to fail validation")
		}
		if err := s.ValidateResponse(*req, Response{
			Decision: "answer",
			Answers: map[string][]string{
				"q_0": {"true"},
				"q_1": {"1.5"},
				"q_2": {"1.5"},
			},
		}); err == nil {
			t.Fatal("expected non-integer answer for integer field to fail")
		}
		if err := s.Respond(t.Context(), *req, Response{
			Decision: "answer",
			Answers: map[string][]string{
				"q_0": {"true"},
				"q_1": {"3"},
				"q_2": {"2.5"},
			},
		}); err != nil {
			t.Fatalf("Respond form: %v", err)
		}
		for {
			ev, err := s.Next(t.Context())
			if err != nil {
				t.Fatalf("Next after form: %v", err)
			}
			saw.WriteString(ev.Text)
			if ev.Status != "" {
				if ev.Status != "completed" || !strings.Contains(saw.String(), "form:true|3|2.5") || !strings.Contains(saw.String(), "url:cancel") {
					t.Fatalf("unexpected form/url outcome: status=%q text=%q", ev.Status, saw.String())
				}
				break
			}
		}
	})

	t.Run("schemaless elicitation still refuses", func(t *testing.T) {
		s := &Claude{
			started:   true,
			sessionID: "s1",
			turnID:    "msg_1",
			pending:   make(map[string]*claudePending),
			seen:      make(map[string]bool),
		}
		rawReq := json.RawMessage(`{"subtype":"elicitation","mode":"form","message":"fill"}`)
		_, _, err := s.capture(claudeFrame{Type: "control_request", RequestID: "r1", Request: rawReq})
		if err == nil || !strings.Contains(err.Error(), `unsupported Claude callback "elicitation"`) {
			t.Fatalf("expected schemaless elicitation refusal, got %v", err)
		}

		// Verify form decline -> {"action": "decline"} and multiSelect AskUserQuestion stays permission.
		formReq := json.RawMessage(`{"subtype":"elicitation","mode":"form","message":"fill","requested_schema":{"type":"object","properties":{"tok":{"type":"string"}}}}`)
		evForm, ok, err := s.capture(claudeFrame{Type: "control_request", RequestID: "r-form", Request: formReq})
		if err != nil || !ok || evForm.Request == nil || evForm.Request.Kind != "question" {
			t.Fatalf("capture form: ev=%+v ok=%v err=%v", evForm, ok, err)
		}
		dec, err := s.response(*evForm.Request, Response{Decision: "deny"})
		if err != nil {
			t.Fatalf("response decline: %v", err)
		}
		decMap, _ := dec.(map[string]any)
		if decMap["action"] != "decline" {
			t.Fatalf("expected action=decline, got %#v", decMap)
		}

		multiReq := json.RawMessage(`{"subtype":"can_use_tool","tool_name":"AskUserQuestion","display_name":"AskUserQuestion","input":{"questions":[{"question":"Pick tags","header":"Tags","multiSelect":true,"options":[{"label":"a"}]}]}}`)
		evMulti, ok, err := s.capture(claudeFrame{Type: "control_request", RequestID: "r-multi", Request: multiReq})
		if err != nil || !ok || evMulti.Request == nil || evMulti.Request.Kind != "permission" {
			t.Fatalf("expected multiSelect AskUserQuestion to stay permission, got ev=%+v ok=%v err=%v", evMulti, ok, err)
		}
		_ = time.Second
	})
}

func TestClaudeInteractiveCallbacksChild(t *testing.T) {
	scenario := ""
	for _, a := range os.Args {
		if v, ok := strings.CutPrefix(a, "claude-interactive:"); ok {
			scenario = v
		}
	}
	if scenario == "" {
		return
	}
	session := ""
	for i := 0; i < len(os.Args)-1; i++ {
		if os.Args[i] == "--session-id" {
			session = os.Args[i+1]
		}
	}
	emit := func(format string, a ...any) {
		fmt.Printf(format+"\n", a...)
		_ = os.Stdout.Sync()
	}
	scan := bufio.NewScanner(os.Stdin)
	scan.Buffer(make([]byte, 4096), MaxFrameBytes+1)
	for scan.Scan() {
		var f map[string]any
		if json.Unmarshal(scan.Bytes(), &f) != nil {
			continue
		}
		switch f["type"] {
		case "control_request":
			reqID, _ := f["request_id"].(string)
			emit(`{"type":"control_response","response":{"subtype":"success","request_id":%q,"response":{"pid":%d,"current_permission_mode":"default","account":{"apiProvider":"firstParty"}}}}`, reqID, os.Getpid())
		case "control_response":
			resp, _ := f["response"].(map[string]any)
			inner, _ := resp["response"].(map[string]any)
			reqID, _ := resp["request_id"].(string)
			switch reqID {
			case "req-url":
				action, _ := inner["action"].(string)
				emit(`{"type":"assistant","session_id":%q,"message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"url:%s"}]},"uuid":"u-url"}`, session, action)
				emit(`{"type":"control_request","request_id":"req-form","request":{"subtype":"elicitation","mode":"form","message":"Need inputs","requested_schema":{"type":"object","properties":{"confirm":{"type":"boolean","title":"Confirm"},"retries":{"type":"integer"},"scale":{"type":"number"}}}}}`)
			case "req-form":
				content, _ := inner["content"].(map[string]any)
				emit(`{"type":"assistant","session_id":%q,"message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"form:%v|%v|%v"}]},"uuid":"u-form"}`, session, content["confirm"], content["retries"], content["scale"])
				emit(`{"type":"result","subtype":"success","is_error":false,"session_id":%q,"result":"ok","terminal_reason":"completed","uuid":"u-done"}`, session)
			case "req-ask":
				if inner["behavior"] == "deny" {
					emit(`{"type":"assistant","session_id":%q,"message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"declined"}]},"uuid":"u-dec"}`, session)
				} else {
					upd, _ := inner["updatedInput"].(map[string]any)
					ans, _ := upd["answers"].(map[string]any)
					emit(`{"type":"assistant","session_id":%q,"message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"answered:%v|%v"}]},"uuid":"u-ans"}`, session, ans["Which DB?"], ans["Which web framework?"])
				}
				emit(`{"type":"result","subtype":"success","is_error":false,"session_id":%q,"result":"ok","terminal_reason":"completed","uuid":"u-done"}`, session)
			}
		case "user":
			wd, _ := os.Getwd()
			emit(`{"type":"system","subtype":"init","session_id":%q,"cwd":%q,"tools":["AskUserQuestion"],"model":"claude-sonnet","permissionMode":"default","claude_code_version":%q,"uuid":"u-init"}`, session, wd, fixtureBuild)
			emit(`{"type":"assistant","session_id":%q,"message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"starting"}]},"uuid":"u1"}`, session)
			switch scenario {
			case "ask-allow", "ask-deny":
				emit(`{"type":"control_request","request_id":"req-ask","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","display_name":"AskUserQuestion","input":{"questions":[{"question":"Which DB?","header":"DB","multiSelect":false,"options":[{"label":"SQLite"},{"label":"Postgres"}]},{"question":"Which web framework?","header":"Web","multiSelect":false,"options":[{"label":"FastAPI"}]}],"metadata":{"count":2}},"tool_use_id":"toolu_1"}}`)
			case "elicit-url-and-form":
				emit(`{"type":"control_request","request_id":"req-url","request":{"subtype":"elicitation","mode":"url","url":"https://example.com/auth","message":"Auth required"}}`)
			}
		}
	}
	os.Exit(0)
}
