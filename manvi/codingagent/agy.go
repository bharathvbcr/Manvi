package codingagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var agyHandshake = 15 * time.Second

type agyPending struct {
	source        Request
	wireID        json.RawMessage
	allowOptionID string
	denyOptionID  string
	sent          bool
}

type agyOutbound struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
}

// Agy adapts the Antigravity CLI (`agy acp`) Agent Client Protocol (ACP v1)
// JSON-RPC 2.0 stdio transport into a managed coding session.
type Agy struct {
	mu               sync.Mutex
	options          Options
	initializing     bool
	ready            bool
	wire             *connection
	effective        Effective
	sessionID        string
	turnID           string
	promptID         json.RawMessage
	emittedTurnStart bool
	started          bool
	finished         bool
	queue            []packet
	deferred         []Event
	pending          map[string]*agyPending
	seen             map[string]bool
	withdrawn        withdrawnIDs
	requestCount     int
	ledger           turnLedger
}

// agyArgs constructs the Antigravity ACP server command-line arguments.
//
// `--include-default-customizations=false` mirrors Claude's
// `--setting-sources user`: repository-local or user plugins/hooks cannot alter
// permission handling inside a managed checkout, and `available_commands_update`
// stays bounded to built-in commands.
func agyArgs(mode string) []string {
	if mode == "bypass" {
		return []string{"acp", "--dangerously-skip-permissions", "--include-default-customizations=false"}
	}
	return []string{"acp", "--sandbox", "--include-default-customizations=false"}
}

// StartAgy starts the Antigravity ACP stdio server process without negotiating
// a session or sending a turn yet, so the host can verify OS process birth first.
func StartAgy(ctx context.Context, options Options) (*Agy, error) {
	return startAgy(ctx, options, agyArgs)
}

func openAgy(ctx context.Context, options Options, argv func(mode string) []string) (*Agy, error) {
	s, err := startAgy(ctx, options, argv)
	if err != nil {
		return nil, err
	}
	if err := s.Initialize(ctx); err != nil {
		_, closeErr := s.Close()
		return nil, errors.Join(err, closeErr)
	}
	return s, nil
}

func startAgy(ctx context.Context, options Options, argv func(mode string) []string) (*Agy, error) {
	program, err := exec.LookPath(options.Program)
	if err != nil {
		return nil, beforeStart(fmt.Errorf("resolve managed Antigravity executable: %w", err))
	}
	options.Program = program
	if !filepath.IsAbs(program) || !filepath.IsAbs(options.Cwd) || strings.ContainsAny(program+options.Cwd, "\x00\r\n") {
		return nil, beforeStart(errors.New("managed Antigravity requires an absolute executable and checkout path"))
	}
	if err := requireLaunchableAgy(program); err != nil {
		return nil, beforeStart(err)
	}
	cwd, err := filepath.EvalSymlinks(options.Cwd)
	if err != nil {
		return nil, beforeStart(fmt.Errorf("resolve managed checkout: %w", err))
	}
	options.Cwd = cwd
	if _, _, _, _, err := agyPolicy(options); err != nil {
		return nil, beforeStart(err)
	}
	wire, err := startConnection(ctx, program, argv(options.Mode), cwd, validateAgyFrame)
	if err != nil {
		return nil, err
	}
	return &Agy{
		wire:    wire,
		options: options,
		pending: make(map[string]*agyPending),
		seen:    make(map[string]bool),
	}, nil
}

// agyPolicy maps GitPulse's six managed permission modes onto `agy acp`'s
// negotiated ACP mode (`plan`, `default`, `accept-edits`), recorded approval
// policy, reviewer, and sandbox description.
func agyPolicy(o Options) (acpMode, approval, reviewer string, sandbox Sandbox, err error) {
	if (o.Mode == "bypass") != o.AcknowledgeBypass {
		return "", "", "", Sandbox{}, errors.New("bypass requires acknowledgment for this managed attempt only")
	}
	workspace := Sandbox{
		Type:          "workspaceWrite",
		NetworkAccess: true,
		WritableRoots: []string{o.Cwd},
	}
	switch o.Mode {
	case "inspect":
		return "plan", "plan", "user", Sandbox{Type: "readOnly", NetworkAccess: true}, nil
	case "ask":
		return "default", "default", "user", workspace, nil
	case "edit":
		return "accept-edits", "accept-edits", "user", workspace, nil
	case "auto_review":
		return "default", "default", "auto_review", workspace, nil
	case "preapproved":
		return "accept-edits", "never", "user", workspace, nil
	case "bypass":
		return "accept-edits", "bypass", "user", Sandbox{Type: "dangerFullAccess", NetworkAccess: true}, nil
	default:
		return "", "", "", Sandbox{}, errors.New("unsupported managed Antigravity permission mode")
	}
}

func validateAgyFrame(raw []byte) error {
	seen, err := validateFrameShape(raw, "Antigravity")
	if err != nil {
		return err
	}
	if !seen["jsonrpc"] {
		return errors.New("managed Antigravity frame omits jsonrpc version")
	}
	if !seen["method"] && (!seen["id"] || seen["result"] == seen["error"]) {
		return errors.New("managed Antigravity frame is neither an event nor a response")
	}
	if seen["method"] && (seen["result"] || seen["error"]) {
		return errors.New("managed Antigravity frame mixes a request and response")
	}
	var header struct {
		JSONRPC string `json:"jsonrpc"`
	}
	if json.Unmarshal(raw, &header) != nil || header.JSONRPC != "2.0" {
		return errors.New("managed Antigravity frame must carry jsonrpc 2.0")
	}
	return nil
}

func (s *Agy) send(ctx context.Context, frame agyOutbound) error {
	frame.JSONRPC = "2.0"
	encoded, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	return s.wire.sendRaw(ctx, encoded)
}

func (s *Agy) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := s.wire.nextID()
	if err := s.send(ctx, agyOutbound{ID: id, Method: method, Params: params}); err != nil {
		return nil, err
	}
	for {
		p, err := s.wire.receive(ctx)
		if err != nil {
			return nil, err
		}
		if p.Method == "" {
			if string(p.ID) != string(id) {
				return nil, errors.New("managed Antigravity returned an unrecognized response identity")
			}
			if len(p.Error) > 0 {
				return nil, &agyProviderRejection{method: method}
			}
			if len(p.Result) == 0 {
				return nil, errors.New("managed Antigravity response has no result")
			}
			return p.Result, nil
		}
		if len(s.queue) >= 64 {
			return nil, errors.New("managed Antigravity exceeded the handshake event queue")
		}
		s.queue = append(s.queue, p)
	}
}

type agyProviderRejection struct{ method string }

func (e *agyProviderRejection) Error() string {
	return fmt.Sprintf("Antigravity rejected %s; provider error details are not copied into logs", e.method)
}
func (e *agyProviderRejection) Is(target error) bool { return target == ErrProviderRejected }

func (s *Agy) Initialize(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initializing {
		return errors.New("provider initialization is already consumed")
	}
	s.initializing = true
	acpMode, approval, reviewer, sandbox, err := agyPolicy(s.options)
	if err != nil {
		return err
	}
	ready, stop := context.WithTimeout(ctx, agyHandshake)
	defer stop()

	initReply, err := s.call(ready, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{},
	})
	if err != nil {
		return err
	}
	var identity struct {
		ProtocolVersion int `json:"protocolVersion"`
		AgentInfo       struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"agentInfo"`
	}
	if json.Unmarshal(initReply, &identity) != nil || identity.ProtocolVersion != 1 ||
		!opaque(identity.AgentInfo.Name) || !opaque(identity.AgentInfo.Version) ||
		!strings.Contains(strings.ToLower(identity.AgentInfo.Name), "antigravity") {
		return errors.New("managed Antigravity omitted or failed its ACP server identity check")
	}

	var sessionReply json.RawMessage
	resumed := opaque(s.options.Resume)
	if resumed {
		sessionReply, err = s.call(ready, "session/resume", map[string]any{
			"sessionId":  s.options.Resume,
			"cwd":        s.options.Cwd,
			"mcpServers": []any{},
		})
	} else {
		sessionReply, err = s.call(ready, "session/new", map[string]any{
			"cwd":        s.options.Cwd,
			"mcpServers": []any{},
		})
	}
	if err != nil {
		return err
	}

	var session struct {
		SessionID string `json:"sessionId"`
		Models    struct {
			CurrentModelID string `json:"currentModelId"`
		} `json:"models"`
		Modes struct {
			CurrentModeID  string `json:"currentModeId"`
			AvailableModes []struct {
				ID string `json:"id"`
			} `json:"availableModes"`
		} `json:"modes"`
		ConfigOptions []struct {
			Category     string `json:"category"`
			CurrentValue string `json:"currentValue"`
		} `json:"configOptions"`
	}
	if json.Unmarshal(sessionReply, &session) != nil || !opaque(session.SessionID) {
		return errors.New("invalid Antigravity ACP session configuration")
	}
	if resumed && session.SessionID != s.options.Resume {
		return errors.New("managed Antigravity resumed a different session than requested")
	}
	model := session.Models.CurrentModelID
	if !opaque(model) {
		for _, opt := range session.ConfigOptions {
			if opt.Category == "model" && opaque(opt.CurrentValue) {
				model = opt.CurrentValue
				break
			}
		}
	}
	if !opaque(model) {
		return errors.New("managed Antigravity omitted its active model identity")
	}

	if len(session.Modes.AvailableModes) > 0 {
		supported := false
		for _, m := range session.Modes.AvailableModes {
			if m.ID == acpMode {
				supported = true
				break
			}
		}
		if !supported {
			return fmt.Errorf("managed Antigravity ACP server does not advertise requested mode %q", acpMode)
		}
	}
	if session.Modes.CurrentModeID != acpMode {
		if _, err := s.call(ready, "session/set_mode", map[string]any{
			"sessionId": session.SessionID,
			"modeId":    acpMode,
		}); err != nil {
			return err
		}
	}

	s.sessionID = session.SessionID
	for _, queued := range s.queue {
		if len(queued.ID) > 0 {
			return errors.New("managed Antigravity issued a callback before any turn started")
		}
		if _, _, err := s.event(queued); err != nil {
			return err
		}
	}
	s.queue = nil

	s.effective = Effective{
		ProviderUserAgent: identity.AgentInfo.Name + "/" + identity.AgentInfo.Version,
		Cwd:               s.options.Cwd,
		ApprovalPolicy:    approval,
		ApprovalsReviewer: reviewer,
		Sandbox:           sandbox,
		Model:             model,
		ModelProvider:     "antigravity",
	}
	s.effective.Thread.ID = s.sessionID
	s.effective.Thread.Cwd = s.options.Cwd
	s.effective.Thread.Ephemeral = false
	s.ready = true
	return nil
}

func (s *Agy) PID() int              { return s.wire.pid }
func (s *Agy) Done() <-chan struct{} { return s.wire.done }

func (s *Agy) Effective() Effective {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return Effective{}
	}
	return s.effective.clone()
}

func (s *Agy) Close() (Exit, error) {
	s.ledger.closed.Store(true)
	return s.wire.close()
}

func (s *Agy) StartTurn(ctx context.Context, brief string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready || s.started || strings.TrimSpace(brief) == "" || len(brief) > 128*1024 || !utf8.ValidString(brief) || strings.ContainsRune(brief, 0) {
		return errors.New("a managed connection accepts one bounded, nonblank task brief")
	}
	s.started = true
	s.ledger.turns = 1
	return s.sendPrompt(ctx, brief)
}

func (s *Agy) sendPrompt(ctx context.Context, text string) error {
	id := s.wire.nextID()
	turnID := fmt.Sprintf("turn-%d", s.ledger.turns)
	err := s.send(ctx, agyOutbound{
		ID:     id,
		Method: "session/prompt",
		Params: map[string]any{
			"sessionId": s.sessionID,
			"prompt": []map[string]string{
				{"type": "text", "text": text},
			},
		},
	})
	if err != nil {
		return err
	}
	s.promptID = id
	s.turnID = turnID
	s.emittedTurnStart = false
	return nil
}

func (s *Agy) ContinueTurn(ctx context.Context, message string) error {
	if !s.mu.TryLock() {
		return RefuseContinue(ErrTurnInFlight)
	}
	defer s.mu.Unlock()
	drainErr := s.drain()
	if err := s.ledger.admit(ctx, message, s.ready, s.started, s.finished, s.backlogged(), s.wire); err != nil {
		return err
	}
	if drainErr != nil {
		return RefuseContinue(fmt.Errorf("the finished turn's events could not be read: %w", drainErr))
	}
	for id, p := range s.pending {
		if p.sent {
			delete(s.pending, id)
		}
	}
	s.finished = false
	s.ledger.turns++
	if err := s.sendPrompt(ctx, message); err != nil {
		s.ledger.halted = true
		return &unresolvedDelivery{cause: err}
	}
	return nil
}

// finishTurn ends the turn with status. A permission still awaiting an answer
// can never be answered once the turn is over, so each is forgotten here and
// reported withdrawn ahead of the status, as Claude's control_cancel_request
// does, and the host can resolve its decision row instead of leaving it live.
// The first event is returned; the rest are queued behind it in order.
func (s *Agy) finishTurn(status string) Event {
	s.finished = true
	s.ledger.last = status
	return leadWith(&s.deferred, append(s.withdrawPending(), Event{Status: status}))
}

// withdrawPending forgets every unanswered permission and returns one
// CancelledID event for each, in a stable order.
func (s *Agy) withdrawPending() []Event {
	var ids []string
	for id, p := range s.pending {
		if !p.sent {
			ids = append(ids, id)
			delete(s.pending, id)
		}
	}
	return s.withdrawn.withdrawalEvents(ids)
}

func (s *Agy) backlogged() bool {
	if len(s.deferred) > 0 || len(s.queue) > 0 {
		return true
	}
	for _, p := range s.pending {
		if !p.sent {
			return true
		}
	}
	return false
}

func (s *Agy) drain() error {
	return drainBacklog("Antigravity", s.wire, &s.queue, &s.deferred,
		func(next wireRead) packet { return next.value }, s.event)
}

func (s *Agy) Next(ctx context.Context) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.deferred) > 0 {
		e := s.deferred[0]
		s.deferred = s.deferred[1:]
		return e, nil
	}
	if s.started && !s.finished && !s.emittedTurnStart {
		if err := ctx.Err(); err != nil {
			return Event{}, err
		}
		s.emittedTurnStart = true
		return Event{TurnID: s.turnID}, nil
	}
	for {
		var p packet
		if len(s.queue) > 0 {
			p = s.queue[0]
			s.queue = s.queue[1:]
		} else {
			var err error
			p, err = s.wire.receive(ctx)
			if err != nil {
				// The session ended under a pending permission (closed, or the
				// process went away): tell the host before the error, once.
				// A caller's own cancellation leaves the session alive, so nothing
				// is withdrawn for that.
				if ctx.Err() == nil {
					if withdrawn := s.withdrawPending(); len(withdrawn) > 0 {
						return leadWith(&s.deferred, withdrawn), nil
					}
				}
				return Event{}, err
			}
		}
		e, meaningful, err := s.event(p)
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

func (s *Agy) event(p packet) (Event, bool, error) {
	if p.Method == "" {
		if !s.started || s.finished || len(s.promptID) == 0 || string(p.ID) != string(s.promptID) {
			return Event{}, false, errors.New("managed Antigravity returned an unrecognized response identity")
		}
		if len(p.Error) > 0 {
			return s.finishTurn("failed"), true, nil
		}
		var done struct {
			StopReason string `json:"stopReason"`
		}
		if json.Unmarshal(p.Result, &done) != nil {
			return Event{}, false, errors.New("invalid Antigravity turn completion")
		}
		var status string
		switch done.StopReason {
		case "end_turn":
			status = "completed"
		case "cancelled":
			status = "interrupted"
		case "max_tokens", "max_turn_requests", "refusal":
			status = "failed"
		default:
			return Event{}, false, errors.New("invalid Antigravity turn completion")
		}
		return s.finishTurn(status), true, nil
	}

	if len(p.ID) > 0 {
		return s.capture(p)
	}

	if p.Method != "session/update" {
		return Event{}, false, nil
	}
	var env struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if len(p.Params) == 0 || json.Unmarshal(p.Params, &env) != nil {
		return Event{}, false, errors.New("invalid Antigravity session update")
	}
	if env.SessionID == "" || env.SessionID != s.sessionID {
		return Event{}, false, errors.New("managed Antigravity event belongs to another session")
	}
	var update struct {
		Kind       string          `json:"sessionUpdate"`
		Content    json.RawMessage `json:"content"`
		ToolCallID string          `json:"toolCallId"`
		Name       string          `json:"name"`
		Title      string          `json:"title"`
		Status     string          `json:"status"`
		RawInput   json.RawMessage `json:"rawInput"`
		RawOutput  json.RawMessage `json:"rawOutput"`
	}
	if len(env.Update) == 0 || json.Unmarshal(env.Update, &update) != nil {
		return Event{}, false, errors.New("invalid Antigravity session update body")
	}
	if !s.started || s.finished {
		return Event{}, false, nil
	}
	switch update.Kind {
	case "agent_message_chunk":
		var chunk struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if len(update.Content) == 0 || json.Unmarshal(update.Content, &chunk) != nil {
			return Event{}, false, errors.New("invalid Antigravity message chunk")
		}
		if chunk.Type != "text" || chunk.Text == "" {
			return Event{}, false, nil
		}
		return Event{Text: neutraliseText(chunk.Text)}, true, nil
	case "tool_call", "tool_call_update":
		name := update.Name
		if !opaque(name) {
			name = update.Title
		}
		status := update.Status
		if !opaque(status) {
			status = "started"
		}
		tool, ok := BoundTool(Tool{
			ID:     update.ToolCallID,
			Name:   name,
			Status: status,
			Input:  agyRawString(update.RawInput),
			Output: agyToolOutput(update.RawOutput),
		})
		if !ok {
			return Event{}, false, nil
		}
		return Event{Tools: []Tool{tool}}, true, nil
	default:
		return Event{}, false, nil
	}
}

func agyRawString(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(trimmed, &s) == nil {
		return s
	}
	return string(trimmed)
}

func agyToolOutput(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return ""
	}
	var obj struct {
		Output *string `json:"output"`
	}
	if json.Unmarshal(trimmed, &obj) == nil && obj.Output != nil {
		return *obj.Output
	}
	return agyRawString(trimmed)
}

func (s *Agy) capture(p packet) (Event, bool, error) {
	if p.Method != "session/request_permission" {
		return Event{}, false, fmt.Errorf("unsupported Antigravity callback %q; no response was granted", p.Method)
	}
	var pms struct {
		SessionID string `json:"sessionId"`
		ToolCall  struct {
			ToolCallID string          `json:"toolCallId"`
			Name       string          `json:"name"`
			Title      string          `json:"title"`
			Kind       string          `json:"kind"`
			RawInput   json.RawMessage `json:"rawInput"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Name     string `json:"name"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	if len(p.Params) == 0 || json.Unmarshal(p.Params, &pms) != nil {
		return Event{}, false, errors.New("invalid Antigravity permission request")
	}
	if !s.started || s.finished || pms.SessionID != s.sessionID || !opaque(s.turnID) {
		return Event{}, false, errors.New("managed Antigravity callback has no matching active turn")
	}
	if !opaque(pms.ToolCall.ToolCallID) || (!opaque(pms.ToolCall.Name) && !opaque(pms.ToolCall.Title)) {
		return Event{}, false, errors.New("managed Antigravity approval omits its reviewable action")
	}
	allowOpt := ""
	denyOpt := ""
	for _, opt := range pms.Options {
		if !opaque(opt.OptionID) {
			continue
		}
		switch opt.Kind {
		case "allow_once":
			if allowOpt == "" {
				allowOpt = opt.OptionID
			}
		case "reject_once":
			if denyOpt == "" {
				denyOpt = opt.OptionID
			}
		}
	}
	if allowOpt == "" {
		return Event{}, false, errors.New("managed Antigravity approval offers no one-time allow option")
	}
	id, err := wireIdentity(p.ID)
	if err != nil {
		return Event{}, false, err
	}
	if s.seen[id] || len(s.pending) >= 32 || s.requestCount >= 2048 {
		return Event{}, false, errors.New("managed Antigravity callback is duplicated or exceeds the run budget")
	}
	if s.options.Mode == "preapproved" {
		s.seen[id] = true
		s.requestCount++
		var outcome map[string]any
		if denyOpt != "" {
			outcome = map[string]any{
				"outcome": map[string]any{
					"outcome":  "selected",
					"optionId": denyOpt,
				},
			}
		} else {
			outcome = map[string]any{
				"outcome": map[string]any{
					"outcome": "cancelled",
				},
			}
		}
		if err := s.send(s.wire.ctx, agyOutbound{
			ID:     append(json.RawMessage(nil), p.ID...),
			Result: outcome,
		}); err != nil {
			return Event{}, false, err
		}
		return Event{}, false, nil
	}
	payload, err := json.Marshal(struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}{p.Method, p.Params})
	if err != nil || len(payload) > 65536 {
		return Event{}, false, errors.New("complete Antigravity approval payload exceeds 64 KiB")
	}
	r := Request{
		ID:        id,
		ThreadID:  s.sessionID,
		TurnID:    s.turnID,
		Kind:      "permission",
		Payload:   string(payload),
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	r.Digest = payloadDigest(r.Payload)
	s.pending[id] = &agyPending{
		source:        r,
		wireID:        append(json.RawMessage(nil), p.ID...),
		allowOptionID: allowOpt,
		denyOptionID:  denyOpt,
	}
	s.seen[id] = true
	s.requestCount++
	return Event{Request: &r}, true, nil
}

func (s *Agy) ValidateResponse(request Request, response Response) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.drain(); err != nil {
		return err
	}
	_, err := s.response(request, response)
	return err
}

func (s *Agy) response(request Request, response Response) (any, error) {
	if s.withdrawn.has(request.ID) {
		return nil, ErrCallbackWithdrawn
	}
	p := s.pending[request.ID]
	if p == nil || callbackStale(request, p.source, p.sent, s.finished) {
		return nil, errors.New("managed Antigravity callback is stale, changed or already consumed")
	}
	if len(response.Answers) > 0 || (response.Decision != "allow_once" && response.Decision != "deny") {
		return nil, errors.New("choose a one-time approval or denial")
	}
	if response.Decision == "allow_once" {
		return map[string]any{
			"outcome": map[string]any{
				"outcome":  "selected",
				"optionId": p.allowOptionID,
			},
		}, nil
	}
	if p.denyOptionID != "" {
		return map[string]any{
			"outcome": map[string]any{
				"outcome":  "selected",
				"optionId": p.denyOptionID,
			},
		}, nil
	}
	return map[string]any{
		"outcome": map[string]any{
			"outcome": "cancelled",
		},
	}, nil
}

func (s *Agy) Respond(ctx context.Context, request Request, response Response) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.drain(); err != nil {
		return err
	}
	result, err := s.response(request, response)
	if err != nil {
		return err
	}
	p := s.pending[request.ID]
	if p == nil {
		return errors.New("managed Antigravity callback is stale, changed or already consumed")
	}
	p.sent = true
	return s.send(ctx, agyOutbound{ID: p.wireID, Result: result})
}
