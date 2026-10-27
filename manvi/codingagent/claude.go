package codingagent

// Claude Code's managed adapter.
//
// The protocol is Claude Code's `stream-json` transport, verified against the
// installed CLI (2.1.278) rather than transcribed from documentation. Three
// facts about it shape everything below, and each one is a trap:
//
//   - **The host must write first.** The CLI emits nothing — not even its
//     `system/init` frame — until it has read a line from stdin. A driver that
//     waits for init before speaking deadlocks with no error on either side.
//
//   - **`--permission-prompt-tool stdio` is what creates an approval surface.**
//     `--permission-prompts host` alone does not: without the prompt tool every
//     `ask` decision silently becomes an automatic *deny*, reported only as a
//     `system/permission_denied` frame. A managed run that looked like it was
//     enforcing approvals would in fact be refusing everything.
//
//   - **`system/init` only arrives once a turn has begun**, which is after the
//     host has to publish the run's effective configuration. So the settings
//     that decide what this run may do are taken from the `initialize`
//     control_response, which is available before the turn, and `system/init`
//     is re-checked against them when it does arrive.
//
// Unlike Codex, Claude Code has no sandbox: `Effective.Sandbox` reports that
// plainly rather than describing a confinement that does not exist.
import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/proc"
	"github.com/bharathvbcr/Manvi/manvi/childenv"
)

// Budgets mirror the Codex adapter: the same run is being protected, and a
// second set of numbers for the same risk would only drift.
const (
	claudeHandshake   = 30 * time.Second
	claudeMaxPending  = 32
	claudeMaxRequests = 2048
	claudeMaxQueue    = 64

	// The build probe is bounded by its own deadline and then by how long a
	// descendant may keep the stdout pipe open after the process is gone.
	claudeProbeTimeout   = 10 * time.Second
	claudeProbeWaitDelay = 2 * time.Second
)

type claudePending struct {
	source    Request
	requestID string
	sent      bool
}

// Claude owns one session and one turn. Operations serialize; Close can
// interrupt a blocked read. Persist a human decision and consume its durable
// host claim before Respond; the one-use guard here supplements that
// transaction, it does not replace it.
type Claude struct {
	mu           sync.Mutex
	options      Options
	initializing bool
	ready        bool
	wire         *connection
	effective    Effective
	sessionID    string
	turnID       string
	build        string
	reported     string
	started      bool
	finished     bool
	sawText      bool
	sawInit      bool
	queue        [][]byte
	deferred     []Event
	pending      map[string]*claudePending
	withdrawn    map[string]bool
	seen         map[string]bool
	requestCount int
}

// StartClaude starts only the stdio process. The owner records its OS process
// identity before Initialize negotiates the session or StartTurn sends work.
func StartClaude(ctx context.Context, options Options) (*Claude, error) {
	return startClaude(ctx, options, claudeArgs)
}

// claudePolicy maps a managed permission mode onto the CLI's own vocabulary,
// and onto what the CLI then *reports* being in — which is not always the same
// word. `manual` is reported as `default`; verified against 2.1.278 for all six.
func claudePolicy(o Options) (flag string, reported string, err error) {
	if (o.Mode == "bypass") != o.AcknowledgeBypass {
		return "", "", errors.New("bypass requires acknowledgment for this managed attempt only")
	}
	switch o.Mode {
	case "inspect":
		return "plan", "plan", nil
	case "ask":
		return "manual", "default", nil
	case "edit":
		return "acceptEdits", "acceptEdits", nil
	case "preapproved":
		return "dontAsk", "dontAsk", nil
	case "auto_review":
		return "auto", "auto", nil
	case "bypass":
		return "bypassPermissions", "bypassPermissions", nil
	default:
		return "", "", errors.New("unsupported managed Claude permission mode")
	}
}

// claudeBuild identifies the executable about to be started.
//
// Needed before the turn because the host records a run's effective
// configuration once and the store then holds it immutable, while the CLI only
// publishes its build on `system/init` — a frame that does not exist until a
// turn is running. So the build is read from the binary itself, and the live
// session's own `system/init` is checked against it later: if the process that
// answered is not the build that was recorded, the run fails rather than
// carrying a configuration describing something else.
func claudeBuild(ctx context.Context, program string) (string, error) {
	probe, stop := context.WithTimeout(ctx, claudeProbeTimeout)
	defer stop()
	cmd := exec.CommandContext(probe, program, "--version")
	// The deadline has to reach what the executable started, not only the
	// executable: Output waits for the pipe to close, and a descendant holding
	// it kept a 10 s probe running for 25. The group is killed with the
	// deadline, and WaitDelay stops the wait on a pipe nobody will close.
	proc.ConfigureGroup(cmd)
	cmd.WaitDelay = claudeProbeWaitDelay
	// The probe is the same executable the session will be; it gets the same
	// withholding.
	cmd.Env = childenv.Inherit()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("read managed Claude build identity: %w", err)
	}
	// `claude --version` prints `<semver> (Claude Code)`. The suffix is the
	// identity check — a binary named `claude` that says something else is not
	// the provider this adapter speaks to.
	text := strings.TrimSpace(string(out))
	if len(text) > 256 || !strings.Contains(text, "(Claude Code)") {
		return "", errors.New("managed Claude executable did not identify itself as Claude Code")
	}
	version, _, _ := strings.Cut(text, " ")
	if !opaque(version) {
		return "", errors.New("managed Claude executable reported no version")
	}
	return version, nil
}

func claudeSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate managed Claude session identity: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// startClaude spawns the CLI. `argv` builds the launch line from the negotiated
// mode and the session identity this host chose, so a test can wrap the real
// arguments rather than reinventing them and drifting from what ships.
func startClaude(ctx context.Context, options Options, argv func(mode, session string) []string) (*Claude, error) {
	program, err := exec.LookPath(options.Program)
	if err != nil {
		return nil, beforeStart(fmt.Errorf("resolve managed Claude executable: %w", err))
	}
	options.Program = program
	if !filepath.IsAbs(program) || !filepath.IsAbs(options.Cwd) || strings.ContainsAny(program+options.Cwd, "\x00\r\n") {
		return nil, beforeStart(errors.New("managed Claude requires an absolute executable and checkout path"))
	}
	// Before anything is run, the probe included: a wrapper that starts the CLI
	// as a child would otherwise fail the process-identity check only after the
	// run began, without saying why. A script the kernel runs in place (an
	// npm install's node launcher) keeps its pid and is accepted.
	if err := requireLaunchableClaude(program); err != nil {
		return nil, beforeStart(err)
	}
	cwd, err := filepath.EvalSymlinks(options.Cwd)
	if err != nil {
		return nil, beforeStart(fmt.Errorf("resolve managed checkout: %w", err))
	}
	options.Cwd = cwd
	mode, reported, err := claudePolicy(options)
	if err != nil {
		return nil, beforeStart(err)
	}
	session := options.Resume
	if !opaque(session) {
		var sessionErr error
		session, sessionErr = claudeSessionID()
		if sessionErr != nil {
			return nil, beforeStart(sessionErr)
		}
	}
	build, err := claudeBuild(ctx, program)
	if err != nil {
		return nil, beforeStart(err)
	}
	args := argv(mode, session)
	if opaque(options.Resume) {
		// argv is claudeArgs for a real launch. Swap its session flag here so
		// claudeArgs itself stays the new-session line every other test pins.
		args, err = claudeResumeArgs(args)
		if err != nil {
			return nil, beforeStart(err)
		}
	}
	wire, err := startConnection(ctx, program, args, cwd, validateClaudeFrame)
	if err != nil {
		return nil, err
	}
	return &Claude{
		wire:      wire,
		options:   options,
		sessionID: session,
		build:     build,
		reported:  reported,
		pending:   make(map[string]*claudePending),
		withdrawn: make(map[string]bool),
		seen:      make(map[string]bool),
	}, nil
}

// claudeArgs is the launch line, and every flag on it is load-bearing.
//
// `--setting-sources user` deliberately omits `project` and `local`: those
// settings files live *inside the checkout this run is about to edit*, so
// honouring them would let a repository widen the permissions of the run
// inspecting it — and in `ask` mode, empty the approval surface the managed
// lane exists to provide. The operator's own user settings still apply, which
// is their choice to have made. The flag scopes more than settings files:
// project CLAUDE.md, rules, skills and `.mcp.json` follow it too (measured on
// Claude Code 2.1.289, the project CLAUDE.md is absent from such a session's
// context). serve.managedTurn therefore tells the agent to read the
// repository's instruction files itself; they are content, not settings, so
// reading them widens nothing.
func claudeArgs(mode, session string) []string {
	return []string{
		"--print",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--permission-mode", mode,
		// Without this, `ask` silently becomes auto-deny. See the file comment.
		"--permission-prompt-tool", "stdio",
		"--setting-sources", "user",
		"--session-id", session,
	}
}

// claudeResumeArgs is claudeArgs with `--session-id` replaced by `--resume`.
// The two flags are not combined: a resume that also passed `--session-id`
// would mint a second session. The input slice is not modified.
func claudeResumeArgs(args []string) ([]string, error) {
	out := append([]string(nil), args...)
	swapped := false
	for i := 0; i < len(out)-1; i++ {
		if out[i] == "--session-id" {
			out[i] = "--resume"
			swapped = true
		}
	}
	if !swapped {
		return nil, errors.New("managed Claude resume requires the session-id launch flag")
	}
	for _, arg := range out {
		if arg == "--session-id" {
			return nil, errors.New("managed Claude resume must not also pass --session-id")
		}
	}
	return out, nil
}

func (s *Claude) PID() int { return s.wire.pid }

func (s *Claude) Effective() Effective {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return Effective{}
	}
	return s.effective.clone()
}

func (s *Claude) Close() (Exit, error) { return s.wire.close() }

// claudeFrame is the envelope plus the fields this adapter reads. Bodies that
// vary per type (`message`, `request`) stay raw and are decoded where used.
type claudeFrame struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	UUID      string          `json:"uuid"`
	Cwd       string          `json:"cwd"`
	Model     string          `json:"model"`
	Mode      string          `json:"permissionMode"`
	ToolName  string          `json:"tool_name"`
	Version   string          `json:"claude_code_version"`
	Message   json.RawMessage `json:"message"`
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`
	Response  json.RawMessage `json:"response"`
	IsError   bool            `json:"is_error"`
	Result    string          `json:"result"`
	Terminal  string          `json:"terminal_reason"`
}

func decodeClaude(raw []byte) (claudeFrame, error) {
	var f claudeFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		return claudeFrame{}, errors.New("invalid Claude stream frame")
	}
	return f, nil
}

func (s *Claude) send(ctx context.Context, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(encoded) > MaxFrameBytes {
		return errors.New("managed request exceeds its wire budget")
	}
	return s.wire.sendRaw(ctx, encoded)
}

// Initialize negotiates the control channel and fixes the run's configuration.
//
// What can be verified here is the permission mode, because the CLI reports it
// on the `initialize` reply. What cannot is anything carried only by
// `system/init` — that frame does not exist until a turn is running, and the
// host publishes this configuration before starting one. Those fields are
// filled and cross-checked in Next when init arrives; a disagreement there
// fails the run rather than being overwritten quietly.
func (s *Claude) Initialize(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initializing {
		return errors.New("provider initialization is already consumed")
	}
	s.initializing = true
	ready, stop := context.WithTimeout(ctx, claudeHandshake)
	defer stop()
	if err := s.send(ready, map[string]any{
		"type":       "control_request",
		"request_id": "manvi_initialize",
		"request":    map[string]any{"subtype": "initialize", "hooks": map[string]any{}},
	}); err != nil {
		return err
	}
	reply, err := s.await(ready, "manvi_initialize")
	if err != nil {
		return err
	}
	var identity struct {
		PID     int    `json:"pid"`
		Mode    string `json:"current_permission_mode"`
		Account struct {
			Provider     string `json:"apiProvider"`
			Subscription string `json:"subscriptionType"`
		} `json:"account"`
	}
	if json.Unmarshal(reply, &identity) != nil {
		return errors.New("invalid Claude initialization reply")
	}
	if identity.PID != s.wire.pid {
		// Still refused, exactly as before. The usual cause is a wrapper that
		// started the CLI as a child, which the launch-time check cannot see
		// when the wrapper is not a script it recognises, so say so.
		return errors.New("Claude reported a process identity other than the one this host started; " +
			"the usual cause is a wrapper that runs the Claude CLI as a child process, so set " +
			ClaudeBinaryVariable + " to the Claude Code executable itself")
	}
	if identity.Mode != s.reported {
		return fmt.Errorf("Claude negotiated permission mode %q for a run that requested %q", identity.Mode, s.reported)
	}
	if !opaque(identity.Account.Provider) {
		return errors.New("Claude omitted its model provider identity")
	}
	s.effective = Effective{
		ProviderUserAgent: "claude-code/" + s.build,
		Cwd:               s.options.Cwd,
		ApprovalPolicy:    identity.Mode,
		ApprovalsReviewer: "user",
		Sandbox: Sandbox{
			// Claude Code confines nothing at the OS level, so there is no
			// sandbox to describe. Saying "none" and admitting network access
			// is the truth; naming a confinement class here would make an
			// unsandboxed run read like a sandboxed one.
			Type:          "none",
			NetworkAccess: true,
		},
		ModelProvider: identity.Account.Provider,
	}
	if s.options.Mode == "auto_review" {
		s.effective.ApprovalsReviewer = "auto_review"
	}
	s.effective.Thread.ID = s.sessionID
	s.effective.Thread.Cwd = s.options.Cwd
	// Persisted: the session id above resumes this run in a terminal later.
	s.effective.Thread.Ephemeral = false
	s.ready = true
	return nil
}

// await reads until the control_response for `id`, buffering everything else.
func (s *Claude) await(ctx context.Context, id string) (json.RawMessage, error) {
	for {
		_, raw, err := s.wire.receiveFrame(ctx)
		if err != nil {
			return nil, err
		}
		f, err := decodeClaude(raw)
		if err != nil {
			return nil, err
		}
		if f.Type == "control_response" {
			// No `error` field: a non-success subtype is refused on the
			// subtype alone, and the provider's error text is deliberately not
			// copied anywhere — decoding it would advertise a detail this
			// adapter does not carry.
			var response struct {
				Subtype   string          `json:"subtype"`
				RequestID string          `json:"request_id"`
				Response  json.RawMessage `json:"response"`
			}
			if json.Unmarshal(f.Response, &response) != nil {
				return nil, errors.New("invalid Claude control response")
			}
			if response.RequestID != id {
				return nil, errors.New("Claude answered an unrecognized control request")
			}
			if response.Subtype != "success" {
				return nil, errors.New("Claude rejected the control request; provider error details are not copied into logs")
			}
			return response.Response, nil
		}
		if len(s.queue) >= claudeMaxQueue {
			return nil, errors.New("Claude exceeded the handshake event queue")
		}
		s.queue = append(s.queue, raw)
	}
}

func (s *Claude) StartTurn(ctx context.Context, brief string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready || s.started || strings.TrimSpace(brief) == "" || len(brief) > 128*1024 {
		return errors.New("a managed connection accepts one bounded, nonblank task brief")
	}
	s.started = true // Uncertain send can never become a second model turn.
	return s.send(ctx, map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]string{{"type": "text", "text": brief}},
		},
	})
}

func (s *Claude) Next(ctx context.Context) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.deferred) > 0 {
		e := s.deferred[0]
		s.deferred = s.deferred[1:]
		return e, nil
	}
	for {
		var raw []byte
		if len(s.queue) > 0 {
			raw = s.queue[0]
			s.queue = s.queue[1:]
		} else {
			var err error
			_, raw, err = s.wire.receiveFrame(ctx)
			if err != nil {
				return Event{}, err
			}
		}
		e, meaningful, err := s.event(raw)
		if err != nil {
			return Event{}, err
		}
		if meaningful {
			return e, nil
		}
		if err := ctx.Err(); err != nil {
			return Event{}, err
		}
	}
}

func (s *Claude) event(raw []byte) (Event, bool, error) {
	f, err := decodeClaude(raw)
	if err != nil {
		return Event{}, false, err
	}
	// Every frame that names a session must name this one. A frame from
	// another session is not noise to skip past; it means the stream is not
	// the one this run negotiated.
	if f.SessionID != "" && f.SessionID != s.sessionID {
		return Event{}, false, errors.New("Claude event belongs to another session")
	}
	switch f.Type {
	case "system":
		return s.system(f)
	case "assistant":
		return s.assistant(f)
	case "control_request":
		return s.capture(f)
	case "result":
		return s.result(f)
	case "control_cancel_request":
		return s.withdraw(f.RequestID)
	case "user":
		return s.userResults(f)
	case "stream_event", "rate_limit_event", "control_response":
		// Partial deltas, rate-limit notices and answers to our own control
		// requests carry nothing the managed run records. Tool results arrive
		// on user frames and are kept; a user frame with no tool_use_id is not.
		return Event{}, false, nil
	default:
		// Additive protocol: an unknown frame type is not an error, because a
		// newer CLI adding one must not fail a run that does not need it.
		return Event{}, false, nil
	}
}

// withdraw handles the CLI cancelling a permission request it offered. A
// request still awaiting an answer is forgotten here and reported, so the host
// can resolve its decision row instead of leaving it live; one already answered
// (or never captured) has nothing left to withdraw.
func (s *Claude) withdraw(id string) (Event, bool, error) {
	p := s.pending[id]
	if p == nil || p.sent {
		return Event{}, false, nil
	}
	delete(s.pending, id)
	s.withdrawn[id] = true
	return Event{CancelledID: id}, true, nil
}

func (s *Claude) system(f claudeFrame) (Event, bool, error) {
	switch f.Subtype {
	case "init":
		if s.sawInit {
			return Event{}, false, errors.New("Claude re-initialized an active managed session")
		}
		s.sawInit = true
		actual, err := filepath.EvalSymlinks(f.Cwd)
		if err != nil || actual != s.options.Cwd {
			return Event{}, false, errors.New("Claude started in a checkout other than the one this run selected")
		}
		if f.Mode != s.reported {
			return Event{}, false, fmt.Errorf("Claude is running in permission mode %q for a run that requested %q", f.Mode, s.reported)
		}
		if !opaque(f.Model) || !opaque(f.Version) {
			return Event{}, false, errors.New("Claude omitted its model or build identity")
		}
		if f.Version != s.build {
			return Event{}, false, fmt.Errorf("Claude session reports build %q for a run recorded against %q", f.Version, s.build)
		}
		// The model is learned here and not before: the CLI names it only once
		// a turn is running, which is after the host has already written this
		// run's effective configuration and the store has made it immutable.
		// It is therefore absent from that record by construction — the record
		// carries what could be verified before the turn, and nothing it could
		// not.
		s.effective.Model = f.Model
		return Event{}, false, nil
	case "permission_denied":
		// The CLI denied a tool without asking. In a managed run that is
		// evidence the operator should see, not something to swallow: it is
		// exactly how a missing approval surface presents.
		//
		// `tool_name` and `message` are siblings of `subtype` on this frame,
		// and `message` is a plain string here while it is an object on an
		// assistant frame — which is why it is decoded per frame type rather
		// than once in the envelope.
		tool := f.ToolName
		if !opaque(tool) {
			tool = "a tool"
		}
		reason := ""
		if json.Unmarshal(f.Message, &reason) != nil || !opaque(reason) {
			reason = "no reason given"
		}
		return Event{Text: fmt.Sprintf("\n[permission denied: %s — %s]\n", tool, reason)}, true, nil
	}
	return Event{}, false, nil
}

// claudeContent is one block of an assistant or user message. A block that is
// neither text nor a tool call is ignored: an unknown block is not a failed turn.
type claudeContent struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	ToolUse string          `json:"tool_use_id"`
	Parent  string          `json:"parent_tool_use_id"`
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

func claudeRaw(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return string(raw)
}

func (s *Claude) assistant(f claudeFrame) (Event, bool, error) {
	var message struct {
		ID      string          `json:"id"`
		Content []claudeContent `json:"content"`
	}
	if json.Unmarshal(f.Message, &message) != nil {
		return Event{}, false, errors.New("invalid Claude assistant message")
	}
	// Claude Code's stream has no turn identity. The first model response of
	// the run is the closest thing to one that the provider actually issues,
	// so it is used as such rather than inventing a local label and recording
	// it as provider evidence.
	turn := ""
	if s.turnID == "" && opaque(message.ID) {
		s.turnID = message.ID
		turn = message.ID
	}
	text := strings.Builder{}
	var tools []Tool
	for _, block := range message.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			if tool, ok := BoundTool(Tool{
				ID: block.ID, Name: block.Name, Status: "started",
				ParentID: block.Parent, Input: claudeRaw(block.Input),
			}); ok {
				tools = append(tools, tool)
			}
		}
	}
	if text.Len() > 0 {
		s.sawText = true
	}
	// A tool-only message still has to be delivered. Dropping it would leave
	// the host with no row for the call, and a reader that stops only on a
	// request or a status would never see the request that follows it.
	if turn == "" && text.Len() == 0 && len(tools) == 0 {
		return Event{}, false, nil
	}
	return Event{TurnID: turn, Text: text.String(), Tools: tools}, true, nil
}

// userResults keeps tool_result blocks that name the call they finish. A user
// frame that carries no result array is skipped, and that skip is not a failed
// turn: the message may be absent, or its content a plain string. claudeArgs
// does not ask for --replay-user-messages, so the stream does not echo our own
// turn back by default, but nothing guarantees the CLI never sends a string
// user frame, so one is still tolerated. A results array that does not decode
// is different: it is a real tool_result that would be lost while the run
// reports completed, so it fails like an undecodable assistant message does.
func (s *Claude) userResults(f claudeFrame) (Event, bool, error) {
	if len(f.Message) == 0 || string(f.Message) == "null" {
		return Event{}, false, nil
	}
	var message struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(f.Message, &message) != nil {
		return Event{}, false, errors.New("invalid Claude user message")
	}
	content := bytes.TrimSpace(message.Content)
	if len(content) == 0 || string(content) == "null" || content[0] == '"' {
		// Absent, null or a string: no result array to read or to lose.
		return Event{}, false, nil
	}
	var blocks []claudeContent
	if json.Unmarshal(content, &blocks) != nil {
		return Event{}, false, errors.New("invalid Claude user message")
	}
	var tools []Tool
	for _, block := range blocks {
		if block.Type != "tool_result" || !opaque(block.ToolUse) {
			continue
		}
		status := "completed"
		if block.IsError {
			status = "error"
		}
		if tool, ok := BoundTool(Tool{ID: block.ToolUse, Status: status, Output: claudeRaw(block.Content)}); ok {
			tools = append(tools, tool)
		}
	}
	if len(tools) == 0 {
		return Event{}, false, nil
	}
	return Event{Tools: tools}, true, nil
}

func (s *Claude) result(f claudeFrame) (Event, bool, error) {
	if s.finished || !s.started {
		return Event{}, false, errors.New("invalid Claude turn completion")
	}
	s.finished = true
	status := "failed"
	switch {
	case f.Terminal == "aborted_streaming" || f.Terminal == "aborted_tools":
		status = "interrupted"
	case f.Subtype == "success" && !f.IsError:
		status = "completed"
	}
	// The result text repeats the last assistant message, so it is carried
	// only when it would otherwise be the run's only explanation.
	text := ""
	if status != "completed" || !s.sawText {
		text = f.Result
	}
	return Event{Status: status, Text: text}, true, nil
}

// capture turns an approval request into a durable, one-use callback.
func (s *Claude) capture(f claudeFrame) (Event, bool, error) {
	var request struct {
		Subtype   string          `json:"subtype"`
		ToolName  string          `json:"tool_name"`
		Display   string          `json:"display_name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Reason    string          `json:"decision_reason"`
		Kind      string          `json:"decision_reason_type"`
	}
	if json.Unmarshal(f.Request, &request) != nil {
		return Event{}, false, errors.New("invalid Claude control request")
	}
	if request.Subtype != "can_use_tool" {
		// `request_user_dialog` and `elicitation` need a free-form answer whose
		// shape this adapter does not model. Refusing is the honest outcome:
		// granting a response it cannot construct would be worse than failing.
		return Event{}, false, fmt.Errorf("unsupported Claude callback %q; no response was granted", request.Subtype)
	}
	// A `can_use_tool` always follows the `tool_use` block that provoked it, so
	// by the time one arrives the turn identity is known. Without it there is
	// nothing to bind the approval to, and the host would durably record an
	// empty turn as the provider's own — so this is refused rather than
	// captured with a blank.
	if !s.started || s.finished || s.turnID == "" {
		return Event{}, false, errors.New("Claude callback has no matching active turn")
	}
	if !opaque(f.RequestID) || !opaque(request.ToolName) || len(request.Input) == 0 {
		return Event{}, false, errors.New("Claude approval omits its reviewable action")
	}
	if s.seen[f.RequestID] || len(s.pending) >= claudeMaxPending || s.requestCount >= claudeMaxRequests {
		return Event{}, false, errors.New("Claude callback is duplicated or exceeds the run budget")
	}
	payload, err := json.Marshal(struct {
		Tool      string          `json:"tool_name"`
		Display   string          `json:"display_name,omitempty"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id,omitempty"`
		Reason    string          `json:"decision_reason,omitempty"`
		Kind      string          `json:"decision_reason_type,omitempty"`
	}{request.ToolName, request.Display, request.Input, request.ToolUseID, request.Reason, request.Kind})
	if err != nil || len(payload) > 65536 {
		return Event{}, false, errors.New("complete Claude approval payload exceeds 64 KiB")
	}
	r := Request{
		ID:        f.RequestID,
		ThreadID:  s.sessionID,
		TurnID:    s.turnID,
		Kind:      "permission",
		Payload:   string(payload),
		Digest:    payloadDigest(string(payload)),
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	s.pending[f.RequestID] = &claudePending{source: r, requestID: f.RequestID}
	s.seen[f.RequestID] = true
	s.requestCount++
	return Event{Request: &r}, true, nil
}

// ValidateResponse checks the complete response before the host consumes its
// durable delivery claim. It observes already-buffered control events; remote
// resolution can still race the eventual write, so Respond repeats the check.
func (s *Claude) ValidateResponse(request Request, response Response) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.drain(); err != nil {
		return err
	}
	_, err := s.response(request, response)
	return err
}

func (s *Claude) drain() error {
	return drainBacklog("Claude", s.wire, &s.queue, &s.deferred,
		func(next wireRead) []byte { return next.raw }, s.event)
}

func (s *Claude) response(request Request, response Response) (any, error) {
	if s.withdrawn[request.ID] {
		return nil, ErrCallbackWithdrawn
	}
	p := s.pending[request.ID]
	if p == nil || callbackStale(request, p.source, p.sent, s.finished) {
		return nil, errors.New("Claude callback is stale, changed or already consumed")
	}
	if len(response.Answers) > 0 || (response.Decision != "allow_once" && response.Decision != "deny") {
		return nil, errors.New("choose a one-time approval or denial")
	}
	if response.Decision == "allow_once" {
		// No updatedInput and no updatedPermissions: this approves exactly the
		// call that was reviewed, and grants nothing beyond it. A rule added
		// here would outlive the decision the human actually made.
		return map[string]any{"behavior": "allow"}, nil
	}
	return map[string]any{
		"behavior": "deny",
		"message":  "The operator denied this action in GitPulse.",
	}, nil
}

func (s *Claude) Respond(ctx context.Context, request Request, response Response) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.drain(); err != nil {
		return err
	}
	decision, err := s.response(request, response)
	if err != nil {
		return err
	}
	p := s.pending[request.ID]
	if p == nil {
		return errors.New("Claude callback is stale, changed or already consumed")
	}
	p.sent = true // Consume before writing; even a failed/partial write must not retry.
	return s.send(ctx, map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": p.requestID,
			"response":   decision,
		},
	})
}
