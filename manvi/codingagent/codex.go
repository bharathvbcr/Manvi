// Package codingagent adapts managed coding-provider protocols. It never infers
// permission from terminal text or retries a provider response after uncertainty.
package codingagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const MaxFrameBytes = 256 * 1024

// Options contains host-selected settings. Program is an absolute executable
// path or a name resolved through the host PATH. Bypass is explicit for this connection and is never inferred from defaults.
type Options struct {
	Program, Cwd, Mode string
	AcknowledgeBypass  bool
}
type Sandbox struct {
	Type                string   `json:"type"`
	NetworkAccess       bool     `json:"networkAccess"`
	WritableRoots       []string `json:"writableRoots,omitempty"`
	ExcludeSlashTmp     bool     `json:"excludeSlashTmp,omitempty"`
	ExcludeTmpdirEnvVar bool     `json:"excludeTmpdirEnvVar,omitempty"`
}
type Effective struct {
	ProviderUserAgent string  `json:"provider_user_agent"`
	Cwd               string  `json:"cwd"`
	ApprovalPolicy    string  `json:"approvalPolicy"`
	ApprovalsReviewer string  `json:"approvalsReviewer"`
	Sandbox           Sandbox `json:"sandbox"`
	Model             string  `json:"model"`
	ModelProvider     string  `json:"modelProvider"`
	Thread            struct {
		ID        string `json:"id"`
		Cwd       string `json:"cwd"`
		Ephemeral bool   `json:"ephemeral"`
	} `json:"thread"`
}
type Request struct {
	ID, ThreadID, TurnID, Kind, Payload, Digest string
	Questions                                   []Question
	ExpiresAt                                   time.Time
}
type Question struct {
	ID       string `json:"id"`
	Header   string `json:"header"`
	Question string `json:"question"`
	IsSecret bool   `json:"isSecret"`
}
type Response struct {
	Decision string
	Answers  map[string][]string
}

// Event carries provider evidence. Status is turn completion, not task acceptance
// or process termination. A resolved callback does not prove its tool succeeded.
type Event struct {
	Request                  *Request
	ResolvedID, Text, Status string
	TurnID                   string
}
type pendingRequest struct {
	source Request
	wireID json.RawMessage
	method string
	sent   bool
}

// Codex owns one ephemeral thread and one turn. Operations serialize; Close can
// interrupt a blocked read. Persist a human decision and consume its durable host
// claim before Respond. This in-memory one-use guard supplements that transaction.
type Codex struct {
	mu           sync.Mutex
	options      Options
	initializing bool
	ready        bool
	wire         *connection
	effective    Effective
	turnID       string
	started      bool
	finished     bool
	queue        []packet
	deferred     []Event
	items        map[string]json.RawMessage
	pending      map[string]*pendingRequest
	seen         map[string]bool
	requestCount int
}

// StartCodex starts only the stdio process. The owner records its OS process
// identity before Initialize negotiates the provider thread or StartTurn sends work.
func StartCodex(ctx context.Context, options Options) (*Codex, error) {
	return startCodex(ctx, options, []string{"app-server", "--listen", "stdio://"})
}
func OpenCodex(ctx context.Context, options Options) (*Codex, error) {
	return openCodex(ctx, options, []string{"app-server", "--listen", "stdio://"})
}
func openCodex(ctx context.Context, options Options, args []string) (*Codex, error) {
	s, err := startCodex(ctx, options, args)
	if err != nil {
		return nil, err
	}
	if err := s.Initialize(ctx); err != nil {
		_, closeErr := s.Close()
		return nil, errors.Join(err, closeErr)
	}
	return s, nil
}
func startCodex(ctx context.Context, options Options, args []string) (*Codex, error) {
	program, err := exec.LookPath(options.Program)
	if err != nil {
		return nil, beforeStart(fmt.Errorf("resolve managed Codex executable: %w", err))
	}
	options.Program = program
	if !filepath.IsAbs(program) || !filepath.IsAbs(options.Cwd) || strings.ContainsAny(program+options.Cwd, "\x00\r\n") {
		return nil, beforeStart(errors.New("managed Codex requires an absolute executable and checkout path"))
	}
	cwd, err := filepath.EvalSymlinks(options.Cwd)
	if err != nil {
		return nil, beforeStart(fmt.Errorf("resolve managed checkout: %w", err))
	}
	options.Cwd = cwd
	if _, _, _, err := policy(options); err != nil {
		return nil, beforeStart(err)
	}
	wire, err := startConnection(ctx, program, args, cwd)
	if err != nil {
		return nil, err
	}
	return &Codex{wire: wire, options: options, items: make(map[string]json.RawMessage), pending: make(map[string]*pendingRequest), seen: make(map[string]bool)}, nil
}
func (s *Codex) Initialize(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initializing {
		return errors.New("provider initialization is already consumed")
	}
	s.initializing = true
	options := s.options
	sandbox, approval, reviewer, err := policy(options)
	if err != nil {
		return err
	}
	ready, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	init := json.RawMessage(`{"clientInfo":{"name":"manvi_gitpulse","version":"1"},"capabilities":{"experimentalApi":false}}`)
	initialized, err := s.call(ready, "initialize", init)
	if err != nil {
		return err
	}
	var identity struct {
		UserAgent string `json:"userAgent"`
	}
	if json.Unmarshal(initialized, &identity) != nil || !opaque(identity.UserAgent) {
		return errors.New("Codex omitted its initialized provider identity")
	}
	s.effective.ProviderUserAgent = identity.UserAgent
	if err = s.wire.send(ready, packet{Method: "initialized", Params: json.RawMessage(`{}`)}); err != nil {
		return err
	}
	params, err := json.Marshal(struct {
		Cwd       string `json:"cwd"`
		Approval  string `json:"approvalPolicy"`
		Reviewer  string `json:"approvalsReviewer"`
		Sandbox   string `json:"sandbox"`
		Ephemeral bool   `json:"ephemeral"`
	}{options.Cwd, approval, reviewer, sandbox, true})
	if err != nil {
		return err
	}
	result, err := s.call(ready, "thread/start", params)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(result, &s.effective); err != nil {
		return errors.New("invalid Codex thread configuration")
	}
	if err = s.verify(options, sandbox, approval, reviewer); err != nil {
		return err
	}
	s.ready = true
	return nil
}
func policy(o Options) (string, string, string, error) {
	if (o.Mode == "bypass") != o.AcknowledgeBypass {
		return "", "", "", errors.New("bypass requires acknowledgment for this managed attempt only")
	}
	switch o.Mode {
	case "inspect":
		return "read-only", "never", "user", nil
	case "ask":
		return "read-only", "on-request", "user", nil
	case "edit":
		return "workspace-write", "on-request", "user", nil
	case "preapproved":
		return "workspace-write", "never", "user", nil
	case "auto_review":
		return "workspace-write", "on-request", "auto_review", nil
	case "bypass":
		return "danger-full-access", "never", "user", nil
	default:
		return "", "", "", errors.New("unsupported managed Codex permission mode")
	}
}
func (s *Codex) verify(o Options, sandbox, approval, reviewer string) error {
	e := s.effective
	actual, err := filepath.EvalSymlinks(e.Cwd)
	threadRoot, threadErr := filepath.EvalSymlinks(e.Thread.Cwd)
	if err != nil || threadErr != nil || actual != o.Cwd || threadRoot != o.Cwd || !opaque(e.Thread.ID) || !e.Thread.Ephemeral || !opaque(e.Model) || !opaque(e.ModelProvider) || e.ApprovalPolicy != approval || e.ApprovalsReviewer != reviewer {
		return errors.New("Codex effective thread, checkout or approval settings differ from the requested run")
	}
	expected := map[string]string{"read-only": "readOnly", "workspace-write": "workspaceWrite", "danger-full-access": "dangerFullAccess"}[sandbox]
	if e.Sandbox.Type != expected || (sandbox != "danger-full-access" && e.Sandbox.NetworkAccess) || len(e.Sandbox.WritableRoots) > 32 {
		return errors.New("Codex effective sandbox widens the requested managed run")
	}
	for _, root := range e.Sandbox.WritableRoots {
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil || resolved != o.Cwd {
			return errors.New("Codex configured extra writable roots outside the selected checkout")
		}
	}
	return nil
}
func (s *Codex) PID() int { return s.wire.pid }
func (s *Codex) Effective() Effective {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return Effective{}
	}
	e := s.effective
	e.Sandbox.WritableRoots = append([]string(nil), e.Sandbox.WritableRoots...)
	return e
}
func (s *Codex) Close() (Exit, error) { return s.wire.close() }

func (s *Codex) StartTurn(ctx context.Context, brief string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready || s.started || strings.TrimSpace(brief) == "" || len(brief) > 128*1024 {
		return errors.New("a managed connection accepts one bounded, nonblank task brief")
	}
	s.started = true // Uncertain send can never become a second model turn.
	input, err := json.Marshal(struct {
		ThreadID string `json:"threadId"`
		Input    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"input"`
	}{s.effective.Thread.ID, []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{{"text", brief}}})
	if err != nil {
		return err
	}
	reply, err := s.call(ctx, "turn/start", input)
	if err != nil {
		return err
	}
	var result struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(reply, &result) != nil || !opaque(result.Turn.ID) {
		return errors.New("Codex did not return a valid turn identity")
	}
	s.turnID = result.Turn.ID
	return nil
}
func (s *Codex) call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	id := s.wire.nextID()
	if err := s.wire.send(ctx, packet{ID: id, Method: method, Params: params}); err != nil {
		return nil, err
	}
	for {
		p, err := s.wire.receive(ctx)
		if err != nil {
			return nil, err
		}
		if p.Method == "" {
			if string(p.ID) != string(id) {
				return nil, errors.New("Codex returned an unrecognized response identity")
			}
			if len(p.Error) > 0 {
				return nil, fmt.Errorf("Codex rejected %s; provider error details are not copied into logs", method)
			}
			if len(p.Result) == 0 {
				return nil, errors.New("Codex response has no result")
			}
			return p.Result, nil
		}
		if len(s.queue) >= 64 {
			return nil, errors.New("Codex exceeded the handshake event queue")
		}
		s.queue = append(s.queue, p)
	}
}
func (s *Codex) Next(ctx context.Context) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.deferred) > 0 {
		e := s.deferred[0]
		s.deferred = s.deferred[1:]
		return e, nil
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
func opaque(s string) bool {
	return strings.TrimSpace(s) != "" && len(s) <= 256 && !strings.ContainsRune(s, '\x00')
}
func wireIdentity(id json.RawMessage) (string, error) {
	if len(id) == 0 || len(id) > 256 {
		return "", errors.New("missing or oversized provider request identity")
	}
	var str string
	if json.Unmarshal(id, &str) == nil && opaque(str) {
		return "s:" + str, nil
	}
	var n json.Number
	if json.Unmarshal(id, &n) == nil {
		if _, err := n.Int64(); err == nil {
			return "n:" + n.String(), nil
		}
	}
	return "", errors.New("unsupported provider request identity")
}
func payloadDigest(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:])
}

func (s *Codex) event(p packet) (Event, bool, error) {
	var v struct {
		ThreadID  string          `json:"threadId"`
		TurnID    string          `json:"turnId"`
		ItemID    string          `json:"itemId"`
		RequestID json.RawMessage `json:"requestId"`
		Item      json.RawMessage `json:"item"`
		Delta     string          `json:"delta"`
		Turn      struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turn"`
	}
	if len(p.Params) > 0 && json.Unmarshal(p.Params, &v) != nil {
		return Event{}, false, errors.New("invalid Codex event parameters")
	}
	if v.ThreadID != "" && v.ThreadID != s.effective.Thread.ID {
		return Event{}, false, errors.New("Codex event belongs to another thread")
	}
	if v.TurnID != "" && v.TurnID != s.turnID {
		return Event{}, false, errors.New("Codex event belongs to another turn")
	}
	if (strings.HasPrefix(p.Method, "item/") || strings.HasPrefix(p.Method, "turn/") || p.Method == "serverRequest/resolved") && v.ThreadID != s.effective.Thread.ID {
		return Event{}, false, errors.New("Codex control event has no matching thread identity")
	}
	if p.Method == "serverRequest/resolved" {
		id, err := wireIdentity(v.RequestID)
		if err != nil {
			return Event{}, false, err
		}
		if s.pending[id] == nil {
			return Event{}, false, errors.New("Codex resolved an unknown callback")
		}
		delete(s.pending, id)
		return Event{ResolvedID: id}, true, nil
	}
	if len(p.ID) > 0 {
		return s.capture(p, v.ThreadID, v.TurnID, v.ItemID)
	}
	switch p.Method {
	case "turn/started":
		if v.Turn.ID != s.turnID {
			return Event{}, false, errors.New("Codex started an unexpected turn")
		}
		return Event{TurnID: s.turnID}, true, nil
	case "turn/completed":
		if v.Turn.ID != s.turnID || s.finished || !s.started || !(v.Turn.Status == "completed" || v.Turn.Status == "failed" || v.Turn.Status == "interrupted") {
			return Event{}, false, errors.New("invalid Codex turn completion")
		}
		s.finished = true
		return Event{Status: v.Turn.Status}, true, nil
	case "item/started", "item/completed":
		var item struct {
			ID   string `json:"id"`
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(v.Item, &item) != nil || !opaque(item.ID) {
			return Event{}, false, errors.New("invalid Codex item")
		}
		if p.Method == "item/started" && item.Type == "fileChange" {
			if len(s.items) >= 32 || len(v.Item) > 64*1024 {
				return Event{}, false, errors.New("Codex file review exceeds the retained context budget")
			}
			s.items[item.ID] = append(json.RawMessage(nil), v.Item...)
		}
		if p.Method == "item/completed" {
			delete(s.items, item.ID)
			if item.Type == "agentMessage" {
				return Event{Text: item.Text}, true, nil
			}
		}
	case "item/commandExecution/outputDelta":
		return Event{Text: v.Delta}, true, nil
	}
	return Event{}, false, nil
}
func (s *Codex) capture(p packet, thread, turn, item string) (Event, bool, error) {
	if !s.started || s.finished || thread != s.effective.Thread.ID || turn != s.turnID || !opaque(item) {
		return Event{}, false, errors.New("Codex callback has no matching active turn")
	}
	id, err := wireIdentity(p.ID)
	if err != nil {
		return Event{}, false, err
	}
	if s.seen[id] || len(s.pending) >= 32 || s.requestCount >= 2048 {
		return Event{}, false, errors.New("Codex callback is duplicated or exceeds the run budget")
	}
	r := Request{ID: id, ThreadID: thread, TurnID: turn, Kind: "permission", ExpiresAt: time.Now().Add(5 * time.Minute)}
	var extra json.RawMessage
	switch p.Method {
	case "item/commandExecution/requestApproval":
		var pms struct {
			Command *string         `json:"command"`
			Network json.RawMessage `json:"networkApprovalContext"`
		}
		if json.Unmarshal(p.Params, &pms) != nil || (pms.Command == nil && (len(pms.Network) == 0 || string(pms.Network) == "null")) {
			return Event{}, false, errors.New("Codex approval omits its reviewable action")
		}
	case "item/fileChange/requestApproval":
		var pms struct {
			GrantRoot *string `json:"grantRoot"`
		}
		if json.Unmarshal(p.Params, &pms) != nil {
			return Event{}, false, errors.New("invalid file request")
		}
		if pms.GrantRoot != nil {
			return Event{}, false, errors.New("session-wide file grants are unsupported; use the provider terminal")
		}
		extra = s.items[item]
		if len(extra) == 0 {
			return Event{}, false, errors.New("Codex file approval has no complete proposed changes")
		}
	case "item/tool/requestUserInput":
		r.Kind = "question"
		var pms struct {
			Questions      []Question `json:"questions"`
			AutoResolution *uint64    `json:"autoResolutionMs"`
		}
		if json.Unmarshal(p.Params, &pms) != nil || len(pms.Questions) == 0 || len(pms.Questions) > 3 {
			return Event{}, false, errors.New("unsupported Codex question set")
		}
		ids := make(map[string]bool)
		for _, q := range pms.Questions {
			if !opaque(q.ID) || q.IsSecret || ids[q.ID] {
				return Event{}, false, errors.New("secret or duplicate questions require the provider terminal")
			}
			ids[q.ID] = true
		}
		r.Questions = pms.Questions
		if pms.AutoResolution != nil && *pms.AutoResolution < 300000 {
			r.ExpiresAt = time.Now().Add(time.Duration(*pms.AutoResolution) * time.Millisecond)
		}
	default:
		return Event{}, false, fmt.Errorf("unsupported Codex callback %q; no response was granted", p.Method)
	}
	payload, err := json.Marshal(struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Item   json.RawMessage `json:"item,omitempty"`
	}{p.Method, p.Params, extra})
	if err != nil || len(payload) > 65536 {
		return Event{}, false, errors.New("complete Codex approval payload exceeds 64 KiB")
	}
	r.Payload = string(payload)
	r.Digest = payloadDigest(r.Payload)
	retained := r
	retained.Questions = append([]Question(nil), r.Questions...)
	s.pending[id] = &pendingRequest{source: retained, wireID: append(json.RawMessage(nil), p.ID...), method: p.Method}
	s.seen[id] = true
	s.requestCount++
	return Event{Request: &r}, true, nil
}

// ValidateResponse checks the complete response before the host consumes its
// durable delivery claim. It observes already-buffered control events; remote
// resolution can still race the eventual write, so Respond repeats the check.
func (s *Codex) ValidateResponse(request Request, response Response) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.drain(); err != nil {
		return err
	}
	_, err := s.response(request, response)
	return err
}
func (s *Codex) drain() error {
	for i := 0; i < 128; i++ {
		var p packet
		if len(s.queue) > 0 {
			p = s.queue[0]
			s.queue = s.queue[1:]
		} else {
			select {
			case next, ok := <-s.wire.frames:
				if !ok {
					return errors.New("Codex connection ended before response validation")
				}
				if next.err != nil {
					return next.err
				}
				p = next.value
			default:
				return s.wire.ctx.Err()
			}
		}
		e, meaningful, err := s.event(p)
		if err != nil {
			return err
		}
		if meaningful {
			if len(s.deferred) >= 128 {
				return errors.New("Codex response preflight exceeded its event budget")
			}
			s.deferred = append(s.deferred, e)
		}
	}
	return errors.New("Codex response preflight could not drain its bounded event backlog")
}
func (s *Codex) response(request Request, response Response) (json.RawMessage, error) {
	p := s.pending[request.ID]
	if p == nil || p.sent || s.finished || !time.Now().Before(p.source.ExpiresAt) || request.Payload != p.source.Payload || request.Digest != p.source.Digest || payloadDigest(request.Payload) != p.source.Digest || request.ThreadID != p.source.ThreadID || request.TurnID != p.source.TurnID || request.Kind != p.source.Kind {
		return nil, errors.New("Codex callback is stale, changed or already consumed")
	}
	var raw json.RawMessage
	if p.source.Kind == "permission" {
		if len(response.Answers) > 0 || (response.Decision != "allow_once" && response.Decision != "deny") {
			return nil, errors.New("choose a one-time approval or denial")
		}
		choice := "decline"
		if response.Decision == "allow_once" {
			choice = "accept"
		}
		raw = []byte(`{"decision":"` + choice + `"}`)
	} else {
		answers := make(map[string]struct {
			Answers []string `json:"answers"`
		})
		if response.Decision == "answer" {
			if len(response.Answers) != len(p.source.Questions) {
				return nil, errors.New("answer every captured question by its identity")
			}
			for _, q := range p.source.Questions {
				a := response.Answers[q.ID]
				if len(a) != 1 || strings.TrimSpace(a[0]) == "" || len(a[0]) > 16384 {
					return nil, errors.New("question answer is missing or exceeds its limit")
				}
				answers[q.ID] = struct {
					Answers []string `json:"answers"`
				}{a}
			}
		} else if response.Decision != "deny" || len(response.Answers) > 0 {
			return nil, errors.New("a question accepts answers or denial, not permission")
		}
		encoded, err := json.Marshal(struct {
			Answers map[string]struct {
				Answers []string `json:"answers"`
			} `json:"answers"`
		}{answers})
		if err != nil {
			return nil, err
		}
		raw = encoded
	}
	return raw, nil
}
func (s *Codex) Respond(ctx context.Context, request Request, response Response) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.drain(); err != nil {
		return err
	}
	raw, err := s.response(request, response)
	if err != nil {
		return err
	}
	p := s.pending[request.ID]
	p.sent = true // Consume before writing; even a failed/partial write must not retry.
	return s.wire.send(ctx, packet{ID: p.wireID, Result: raw})
}
