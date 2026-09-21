package codingagent

// The fixture child below speaks Claude Code's stream-json protocol back at the
// adapter. Its frames are copies of traffic recorded from the installed CLI
// (2.1.278) — the initialize reply, `system/init`, an assistant message with a
// tool_use, a `can_use_tool` control_request and a `result` — so a scenario
// that passes here is a scenario the real transport produces, not one invented
// to match the parser.

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

// The build the fixture claims to be. The adapter reads this from the
// executable before spawning a session and then holds the live session to it,
// so the fixture has to answer the probe and report the same string on init.
const fixtureBuild = "9.9.9-fixture"

// TestMain intercepts the adapter's `--version` probe.
//
// Without it the probe reaches Go's flag parser, which exits 2 on an unknown
// flag — so every fixture session would fail before starting, for a reason
// that looks nothing like its cause.
func TestMain(m *testing.M) {
	for _, arg := range os.Args[1:] {
		if arg == "--version" {
			fmt.Println(fixtureBuild + " (Claude Code)")
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

// sessionArg reads the session identity the adapter chose out of the child's
// own argv, which is how the fixture can echo the id it is required to use.
func sessionArg() string {
	for i, a := range os.Args {
		if a == "--session-id" && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return ""
}

func TestClaudeProtocolChild(t *testing.T) {
	args := os.Args
	marker := ""
	for _, a := range args {
		if strings.HasPrefix(a, "claude-fixture:") {
			marker = strings.TrimPrefix(a, "claude-fixture:")
		}
	}
	if marker == "" {
		return
	}
	scenario := marker
	session := sessionArg()
	cwd, _ := os.Getwd()
	emit := func(format string, a ...any) { fmt.Printf(format+"\n", a...) }
	build := fixtureBuild
	if scenario == "drifted-build" {
		build = "0.0.1-other"
	}
	initFrame := func(mode string) {
		emit(`{"type":"system","subtype":"init","session_id":%q,"cwd":%q,"model":"claude-sonnet-5","permissionMode":%q,"claude_code_version":%q,"tools":["Bash"],"uuid":"u-init"}`, session, cwd, mode, build)
	}
	scan := bufio.NewScanner(os.Stdin)
	scan.Buffer(make([]byte, 4096), MaxFrameBytes+1)
	for scan.Scan() {
		var in struct {
			Type      string          `json:"type"`
			RequestID string          `json:"request_id"`
			Request   json.RawMessage `json:"request"`
			Response  json.RawMessage `json:"response"`
		}
		if json.Unmarshal(scan.Bytes(), &in) != nil {
			os.Exit(3)
		}
		switch in.Type {
		case "control_request":
			pid := os.Getpid()
			if scenario == "foreign-pid" {
				pid = 1
			}
			mode := "default"
			if scenario == "widened-mode" {
				mode = "bypassPermissions"
			}
			emit(`{"type":"control_response","response":{"subtype":"success","request_id":%q,"response":{"pid":%d,"current_permission_mode":%q,"account":{"apiProvider":"firstParty","subscriptionType":"Claude Max"},"session_state":"idle"}}}`, in.RequestID, pid, mode)
		case "control_response":
			// The adapter answered a permission request. Close the turn so the
			// test can observe the decision reaching the wire.
			var decision struct {
				Response struct {
					Response struct {
						Behavior string `json:"behavior"`
					} `json:"response"`
				} `json:"response"`
			}
			_ = json.Unmarshal(scan.Bytes(), &decision)
			// Reported as an assistant message, not in the result text: a
			// completed turn's result repeats the last assistant message, and
			// the adapter deliberately does not carry that twice.
			emit(`{"type":"assistant","session_id":%q,"message":{"id":"msg_3","role":"assistant","content":[{"type":"text","text":"decision:%s"}]},"uuid":"u3"}`, session, decision.Response.Response.Behavior)
			emit(`{"type":"result","subtype":"success","is_error":false,"session_id":%q,"result":"decision:%s","terminal_reason":"completed","uuid":"u-result"}`, session, decision.Response.Response.Behavior)
		case "user":
			switch scenario {
			case "flood":
				emit("%s", strings.Repeat("x", MaxFrameBytes+1))
				continue
			case "foreign-session":
				initFrame("default")
				emit(`{"type":"assistant","session_id":"someone-else","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"hello"}]},"uuid":"u1"}`)
				continue
			case "foreign-cwd":
				emit(`{"type":"system","subtype":"init","session_id":%q,"cwd":"/somewhere/else","model":"claude-sonnet-5","permissionMode":"default","claude_code_version":"2.1.278","uuid":"u-init"}`, session)
				continue
			case "drifted-mode":
				initFrame("bypassPermissions")
				continue
			case "drifted-build":
				// The session answering is not the build that was recorded.
				initFrame("default")
				continue
			case "reinit":
				initFrame("default")
				initFrame("default")
				continue
			case "burst":
				initFrame("default")
				for i := 0; i < 160; i++ {
					emit(`{"type":"assistant","session_id":%q,"message":{"id":"msg_%d","role":"assistant","content":[{"type":"text","text":"bounded output\n"}]},"uuid":"u%d"}`, session, i, i)
				}
				emit(`{"type":"result","subtype":"success","is_error":false,"session_id":%q,"result":"done","terminal_reason":"completed","uuid":"u-result"}`, session)
				continue
			case "denied":
				initFrame("default")
				emit(`{"type":"assistant","session_id":%q,"message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"trying"}]},"uuid":"u1"}`, session)
				// Copied from a real 2.1.278 frame: tool_name and message are
				// siblings of subtype, and message is a plain string.
				emit(`{"type":"system","subtype":"permission_denied","tool_name":"Bash","tool_use_id":"toolu_9","decision_reason_type":"rule","message":"Claude requested permissions to use Bash, but you haven't granted it yet.","uuid":"u2","session_id":%q}`, session)
				emit(`{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":%q,"result":"blocked","terminal_reason":"completed","uuid":"u-result"}`, session)
				continue
			case "interrupted":
				initFrame("default")
				emit(`{"type":"result","subtype":"success","is_error":false,"session_id":%q,"result":"stopped","terminal_reason":"aborted_tools","uuid":"u-result"}`, session)
				continue
			case "dialog":
				initFrame("default")
				emit(`{"type":"assistant","session_id":%q,"message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"asking"}]},"uuid":"u1"}`, session)
				emit(`{"type":"control_request","request_id":"req-1","request":{"subtype":"request_user_dialog","dialog_kind":"refusal_fallback_prompt","payload":{}}}`)
				continue
			case "duplicate":
				initFrame("default")
				emit(`{"type":"assistant","session_id":%q,"message":{"id":"msg_1","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]},"uuid":"u1"}`, session)
				emit(`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Bash","display_name":"Bash","input":{"command":"ls"},"tool_use_id":"toolu_1","decision_reason_type":"rule"}}`)
				emit(`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Bash","display_name":"Bash","input":{"command":"ls"},"tool_use_id":"toolu_1","decision_reason_type":"rule"}}`)
				continue
			case "orphan-callback":
				// A permission request with no assistant message before it:
				// there is no turn identity yet, and none may be invented.
				initFrame("default")
				emit(`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Bash","display_name":"Bash","input":{"command":"ls"},"tool_use_id":"toolu_1"}}`)
				continue
			case "permission":
				initFrame("default")
				emit(`{"type":"assistant","session_id":%q,"message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"about to run"}]},"uuid":"u1"}`, session)
				emit(`{"type":"assistant","session_id":%q,"message":{"id":"msg_2","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"echo hi"}}]},"uuid":"u2"}`, session)
				emit(`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Bash","display_name":"Bash","input":{"command":"echo hi"},"tool_use_id":"toolu_1","decision_reason_type":"rule"}}`)
				continue
			default:
				initFrame("default")
				emit(`{"type":"rate_limit_event","session_id":%q,"rate_limit_info":{"status":"allowed"},"uuid":"u0"}`, session)
				emit(`{"type":"assistant","session_id":%q,"message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"MANAGED_OK"}]},"uuid":"u1"}`, session)
				emit(`{"type":"user","session_id":%q,"message":{"role":"user","content":[{"type":"tool_result","content":"ignored"}]},"uuid":"u2"}`, session)
				emit(`{"type":"unknown_future_frame","session_id":%q,"uuid":"u3"}`, session)
				emit(`{"type":"result","subtype":"success","is_error":false,"session_id":%q,"result":"MANAGED_OK","terminal_reason":"completed","uuid":"u-result"}`, session)
				continue
			}
		}
	}
	os.Exit(0)
}

func fixtureArgs(scenario string) func(mode, session string) []string {
	return func(mode, session string) []string {
		return append([]string{"-test.run=^TestClaudeProtocolChild$", "--", "claude-fixture:" + scenario}, claudeArgs(mode, session)...)
	}
}

func openFixture(t *testing.T, scenario, mode string) *Claude {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := startClaude(t.Context(), Options{Program: bin, Cwd: root, Mode: mode}, fixtureArgs(scenario))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := s.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return s
}

// collect drains the turn, returning the concatenated text, the final status
// and any permission request seen.
func collect(t *testing.T, s *Claude) (string, string, *Request) {
	t.Helper()
	text := strings.Builder{}
	var request *Request
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
		e, err := s.Next(ctx)
		stop()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		text.WriteString(e.Text)
		if e.Request != nil {
			request = e.Request
		}
		if e.Status != "" {
			return text.String(), e.Status, request
		}
		if request != nil {
			return text.String(), "", request
		}
	}
	t.Fatal("turn did not settle")
	return "", "", nil
}

func TestClaudeStartedSessionWaitsForInitializationAndVerifiesTheNegotiatedMode(t *testing.T) {
	for _, scenario := range []string{"normal", "widened-mode", "foreign-pid"} {
		t.Run(scenario, func(t *testing.T) {
			bin, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			s, err := startClaude(t.Context(), Options{Program: bin, Cwd: root, Mode: "ask"}, fixtureArgs(scenario))
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
			if (err != nil) != (scenario != "normal") {
				t.Fatalf("unexpected initialization outcome: %v", err)
			}
			if NoProcessStarted(err) {
				t.Fatal("spawned process classified as unstarted")
			}
			if scenario != "normal" {
				if s.Effective().Thread.ID != "" {
					t.Fatal("unverified configuration exposed")
				}
				if err := s.StartTurn(t.Context(), "Must not be sent"); err == nil {
					t.Fatal("failed initialization permitted work")
				}
				return
			}
			e := s.Effective()
			if e.Thread.ID == "" || e.Thread.Cwd != root || e.Cwd != root {
				t.Fatalf("verified configuration missing: %+v", e)
			}
			if e.ApprovalPolicy != "default" || e.ApprovalsReviewer != "user" {
				t.Fatalf("approval settings not reported: %+v", e)
			}
			// The build identity has to be in the record *before* the turn,
			// because the host writes this once and the store then holds it
			// immutable.
			if e.ProviderUserAgent != "claude-code/"+fixtureBuild {
				t.Fatalf("build identity not recorded before the turn: %q", e.ProviderUserAgent)
			}
			if e.Sandbox.Type != "none" || !e.Sandbox.NetworkAccess {
				t.Fatalf("a sandbox Claude Code does not have was described: %+v", e.Sandbox)
			}
			if err := s.Initialize(t.Context()); err == nil {
				t.Fatal("initialization replayed")
			}
		})
	}
}

func TestClaudeTurnReportsTextTurnIdentityAndCompletion(t *testing.T) {
	s := openFixture(t, "normal", "ask")
	if err := s.StartTurn(t.Context(), "Do the thing"); err != nil {
		t.Fatal(err)
	}
	if err := s.StartTurn(t.Context(), "Second turn"); err == nil {
		t.Fatal("a managed connection accepted a second turn")
	}
	text, status, request := collect(t, s)
	if status != "completed" || request != nil {
		t.Fatalf("status %q request %+v", status, request)
	}
	if !strings.Contains(text, "MANAGED_OK") {
		t.Fatalf("assistant text missing: %q", text)
	}
	if strings.Count(text, "MANAGED_OK") != 1 {
		t.Fatalf("the result frame duplicated the assistant text: %q", text)
	}
	if s.turnID != "msg_1" {
		t.Fatalf("turn identity not taken from the provider: %q", s.turnID)
	}
	// system/init arrived mid-turn and named the model, which is the one field
	// the CLI does not offer before a turn exists.
	if e := s.Effective(); e.Model != "claude-sonnet-5" || e.ProviderUserAgent != "claude-code/"+fixtureBuild {
		t.Fatalf("init frame did not complete the configuration: %+v", e)
	}
}

func TestClaudeApprovalRoundTripsAndIsSingleUse(t *testing.T) {
	for _, decision := range []string{"allow_once", "deny"} {
		t.Run(decision, func(t *testing.T) {
			s := openFixture(t, "permission", "ask")
			if err := s.StartTurn(t.Context(), "Run something"); err != nil {
				t.Fatal(err)
			}
			_, _, request := collect(t, s)
			if request == nil {
				t.Fatal("no approval was captured")
			}
			if request.Kind != "permission" || request.TurnID != "msg_1" || request.ThreadID != s.sessionID {
				t.Fatalf("approval not bound to its turn: %+v", request)
			}
			if !strings.Contains(request.Payload, "echo hi") || request.Digest != payloadDigest(request.Payload) {
				t.Fatalf("approval payload is not reviewable: %+v", request)
			}
			// A changed payload must not be answerable.
			tampered := *request
			tampered.Payload = strings.Replace(tampered.Payload, "echo hi", "rm -rf /", 1)
			if err := s.ValidateResponse(tampered, Response{Decision: decision}); err == nil {
				t.Fatal("a rewritten approval payload was accepted")
			}
			if err := s.ValidateResponse(*request, Response{Decision: "answer"}); err == nil {
				t.Fatal("a question answer was accepted for a permission request")
			}
			if err := s.ValidateResponse(*request, Response{Decision: decision}); err != nil {
				t.Fatalf("validate: %v", err)
			}
			if err := s.Respond(t.Context(), *request, Response{Decision: decision}); err != nil {
				t.Fatalf("respond: %v", err)
			}
			if err := s.Respond(t.Context(), *request, Response{Decision: decision}); err == nil {
				t.Fatal("an approval was answered twice")
			}
			expected := "allow"
			if decision == "deny" {
				expected = "deny"
			}
			text, status, _ := collect(t, s)
			if status != "completed" {
				t.Fatalf("turn did not complete: %q", status)
			}
			if !strings.Contains(text, "decision:"+expected) {
				t.Fatalf("the decision that reached the provider was %q", text)
			}
		})
	}
}

func TestClaudeRefusesFramesThatDoNotBelongToThisRun(t *testing.T) {
	for _, scenario := range []string{"foreign-session", "foreign-cwd", "drifted-mode", "drifted-build", "reinit", "flood", "dialog", "duplicate", "orphan-callback"} {
		t.Run(scenario, func(t *testing.T) {
			s := openFixture(t, scenario, "ask")
			if err := s.StartTurn(t.Context(), "Do the thing"); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(20 * time.Second)
			var err error
			for time.Now().Before(deadline) {
				ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
				var e Event
				e, err = s.Next(ctx)
				stop()
				if err != nil {
					break
				}
				if e.Status != "" {
					t.Fatalf("%s completed instead of failing", scenario)
				}
			}
			if err == nil {
				t.Fatalf("%s produced no error", scenario)
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s hung rather than refusing: %v", scenario, err)
			}
		})
	}
}

func TestClaudeSurfacesASilentDenialRatherThanSwallowingIt(t *testing.T) {
	s := openFixture(t, "denied", "ask")
	if err := s.StartTurn(t.Context(), "Do the thing"); err != nil {
		t.Fatal(err)
	}
	text, status, _ := collect(t, s)
	if status != "failed" {
		t.Fatalf("an errored result reported %q", status)
	}
	if !strings.Contains(text, "permission denied: Bash") {
		t.Fatalf("a denial the operator never saw: %q", text)
	}
	// The reason, not a placeholder: reading it needs the real frame shape.
	if !strings.Contains(text, "you haven't granted it yet") {
		t.Fatalf("the denial reason was lost: %q", text)
	}
	if !strings.Contains(text, "blocked") {
		t.Fatalf("the failure explanation was dropped: %q", text)
	}
}

func TestClaudeReportsAnAbortedTurnAsInterrupted(t *testing.T) {
	s := openFixture(t, "interrupted", "ask")
	if err := s.StartTurn(t.Context(), "Do the thing"); err != nil {
		t.Fatal(err)
	}
	if _, status, _ := collect(t, s); status != "interrupted" {
		t.Fatalf("aborted_tools reported as %q", status)
	}
}

func TestClaudeBurstStaysBoundedAndOrdered(t *testing.T) {
	s := openFixture(t, "burst", "ask")
	if err := s.StartTurn(t.Context(), "Do the thing"); err != nil {
		t.Fatal(err)
	}
	text, status, _ := collect(t, s)
	if status != "completed" {
		t.Fatalf("burst ended %q", status)
	}
	if strings.Count(text, "bounded output") != 160 {
		t.Fatalf("burst lost or duplicated frames: %d", strings.Count(text, "bounded output"))
	}
}

func TestClaudeModeMappingCoversEveryManagedModeAndRefusesUnacknowledgedBypass(t *testing.T) {
	// Every mode the workbench can store must map to a real CLI mode, and to
	// the word the CLI reports back — which for `ask` is not the word passed.
	expected := map[string][2]string{
		"inspect":     {"plan", "plan"},
		"ask":         {"manual", "default"},
		"edit":        {"acceptEdits", "acceptEdits"},
		"preapproved": {"dontAsk", "dontAsk"},
		"auto_review": {"auto", "auto"},
	}
	for mode, want := range expected {
		flag, reported, err := claudePolicy(Options{Mode: mode})
		if err != nil || flag != want[0] || reported != want[1] {
			t.Fatalf("%s mapped to %q/%q (%v)", mode, flag, reported, err)
		}
	}
	if _, _, err := claudePolicy(Options{Mode: "bypass"}); err == nil {
		t.Fatal("bypass without acknowledgment was accepted")
	}
	flag, reported, err := claudePolicy(Options{Mode: "bypass", AcknowledgeBypass: true})
	if err != nil || flag != "bypassPermissions" || reported != "bypassPermissions" {
		t.Fatalf("acknowledged bypass mapped to %q/%q (%v)", flag, reported, err)
	}
	if _, _, err := claudePolicy(Options{Mode: "ask", AcknowledgeBypass: true}); err == nil {
		t.Fatal("an acknowledgment without bypass was accepted")
	}
	if _, _, err := claudePolicy(Options{Mode: "nonsense"}); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
}

func TestClaudeLaunchLineKeepsItsApprovalSurfaceAndScopesRepositorySettings(t *testing.T) {
	args := claudeArgs("manual", "session-1")
	joined := strings.Join(args, " ")
	// Each of these is load-bearing; the comments on claudeArgs say why.
	for _, required := range []string{
		"--permission-prompt-tool stdio",
		"--permission-mode manual",
		"--input-format stream-json",
		"--output-format stream-json",
		"--setting-sources user",
		"--session-id session-1",
		"--print",
		"--verbose",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("launch line lost %q: %s", required, joined)
		}
	}
	if strings.Contains(joined, "project") || strings.Contains(joined, "local") {
		t.Fatalf("repository-controlled settings were re-enabled: %s", joined)
	}
}

func TestClaudeFrameValidationRejectsAnotherProtocolsEnvelope(t *testing.T) {
	for _, bad := range []string{
		`{"method":"turn/started","params":{}}`,
		`{"id":1,"result":{}}`,
		`{"session_id":"x"}`,
		`{"type":"system","type":"system"}`,
		`{"type":"system"} {"type":"system"}`,
		`[]`,
	} {
		if err := validateClaudeFrame([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	if err := validateClaudeFrame([]byte(`{"type":"system","subtype":"init"}`)); err != nil {
		t.Fatalf("rejected a real frame: %v", err)
	}
	// The Codex validator must still reject Claude's frames, so a misrouted
	// provider fails loudly instead of being read as silence.
	if err := validateCodexFrame([]byte(`{"type":"system","subtype":"init"}`)); err == nil {
		t.Fatal("the Codex validator accepted a Claude frame")
	}
}

func TestOnlyKnownPreSpawnClaudeErrorsCertifyNoProcessStarted(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, options := range []Options{
		{Program: filepath.Join(root, "missing"), Cwd: root, Mode: "ask"},
		{Program: bin, Cwd: filepath.Join(root, "missing"), Mode: "ask"},
		{Program: bin, Cwd: root, Mode: "bypass"},
		{Program: bin, Cwd: root, Mode: "nonsense"},
	} {
		s, err := StartClaude(t.Context(), options)
		if s != nil || err == nil || !NoProcessStarted(fmt.Errorf("wrapped: %w", err)) {
			t.Fatalf("unproven pre-spawn error: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if s, err := StartClaude(ctx, Options{Program: bin, Cwd: root, Mode: "ask"}); s != nil || !NoProcessStarted(err) {
		t.Fatalf("cancelled pre-spawn was uncertain: %v", err)
	}
}

func TestManagedProviderListMatchesTheAdaptersThatExist(t *testing.T) {
	if !IsManagedProvider("codex") || !IsManagedProvider("claude") {
		t.Fatal("a provider with an adapter is not listed as managed")
	}
	for _, absent := range []string{"grok", "agy", "shell", "", "CLAUDE"} {
		if IsManagedProvider(absent) {
			t.Fatalf("%q has no managed adapter but is listed as managed", absent)
		}
	}
}

func TestClaudeSessionIdentityIsAUniqueUUID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id, err := claudeSessionID()
		if err != nil {
			t.Fatal(err)
		}
		// --session-id requires a UUID; a malformed one makes the CLI exit
		// before any frame, which presents as an unexplained startup failure.
		if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[14] != '4' || id[18] != '-' || id[23] != '-' {
			t.Fatalf("not a v4 UUID: %q", id)
		}
		if !strings.ContainsAny(string(id[19]), "89ab") {
			t.Fatalf("variant bits not set: %q", id)
		}
		if seen[id] {
			t.Fatalf("session identity repeated: %q", id)
		}
		seen[id] = true
	}
}
