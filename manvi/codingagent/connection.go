package codingagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/proc"
	"github.com/bharathvbcr/Manvi/manvi/childenv"
)

// packet is the union of the two managed protocols' envelopes. Codex speaks
// JSON-RPC (id/method/params/result/error); Claude Code's stream-json speaks a
// tagged union (type/subtype), where the body varies too much per type to
// usefully predeclare, so its adapter decodes raw.
//
// One struct rather than two because the transport below — spawn, environment
// scrubbing, line framing, backpressure, close-and-reap — is genuinely one
// behavior, and only frame validation and decoding differ. Each adapter reads
// the fields its protocol defines and ignores the rest.
type packet struct {
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
	Type    string          `json:"type,omitempty"`
	Subtype string          `json:"subtype,omitempty"`
}
type wireRead struct {
	value packet
	raw   []byte
	err   error
}

// Exit proves the direct child was reaped. A successful coding result requires
// separate host evidence; an exit code alone proves neither. GroupLive says the
// agent's process group still had a live member after close() killed it and
// waited out its budget (see connection.close); it is never set where the group
// cannot be probed, and a descendant outside the group is not counted.
type Exit struct {
	Reaped    bool
	Code      int
	GroupLive bool
}
type connection struct {
	ctx    context.Context
	cancel context.CancelFunc
	// fail cancels the connection and records why, so a reader that wakes on the
	// cancellation reports the cause (see err) and not a bare context.Canceled.
	fail      context.CancelCauseFunc
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	pid       int
	sequence  uint64
	frames    chan wireRead
	done      chan struct{}
	waitErr   error
	closeOnce sync.Once
	// groupGone is set once the agent's process group is known to have no member:
	// empty when sweepGroup first looked, or confirmed so by close().
	groupGone atomic.Bool
	validate  func([]byte) error
}

// errStderrCap is the cause a stderr flood ends the connection with.
var errStderrCap = errors.New("managed provider stderr exceeded 64 KiB")

// limitedDiscard drops the provider's stderr and ends the connection when it
// writes more than remaining bytes. A breach is recorded through fail, the only
// way it ends the connection, so every reader reports the cap and serve classes
// the run failed, not interrupted.
type limitedDiscard struct {
	remaining int
	fail      context.CancelCauseFunc
}

func (w *limitedDiscard) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		w.fail(errStderrCap)
		return 0, errStderrCap
	}
	w.remaining -= len(p)
	return len(p), nil
}

// envOverrides describes environment keys to remove from a base environment
// before appending explicit KEY=VALUE assignments. Appending onto os.Environ()
// cannot unset an existing key, and case-folded keys on Windows must be removed
// before replacement.
type envOverrides struct {
	unsetKeys []string
	set       []string
}

func applyEnvOverrides(environ []string, overrides envOverrides) []string {
	return applyEnvOverridesKeyed(environ, overrides, childenv.Key)
}

func applyEnvOverridesKeyed(environ []string, overrides envOverrides, nameKey func(string) string) []string {
	if len(overrides.unsetKeys) == 0 && len(overrides.set) == 0 {
		return environ
	}
	drop := make(map[string]bool, len(overrides.unsetKeys)+len(overrides.set))
	for _, k := range overrides.unsetKeys {
		drop[nameKey(k)] = true
	}
	for _, entry := range overrides.set {
		name, _, _ := strings.Cut(entry, "=")
		drop[nameKey(name)] = true
	}
	out := make([]string, 0, len(environ)+len(overrides.set))
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if drop[nameKey(name)] {
			continue
		}
		out = append(out, entry)
	}
	return append(out, overrides.set...)
}

// startConnection spawns the provider and frames its stdout. `validate` is the
// protocol's own frame rule and is never optional: a transport that accepted
// any well-formed JSON would let one provider's frames be read as another's.
func startConnection(parent context.Context, program string, args []string, cwd string, validate func([]byte) error) (*connection, error) {
	return startConnectionOverrides(parent, program, args, cwd, childenv.Inherit(), envOverrides{}, validate)
}

func startConnectionOverrides(parent context.Context, program string, args []string, cwd string, baseEnv []string, overrides envOverrides, validate func([]byte) error) (*connection, error) {
	return startConnectionEnv(parent, program, args, cwd, applyEnvOverrides(baseEnv, overrides), validate)
}

func startConnectionEnv(parent context.Context, program string, args []string, cwd string, baseEnv []string, validate func([]byte) error) (*connection, error) {
	if err := parent.Err(); err != nil {
		return nil, beforeStart(err)
	}
	ctx, fail := context.WithCancelCause(parent)
	cancel := context.CancelFunc(func() { fail(nil) })
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Dir = cwd
	cmd.WaitDelay = 2 * time.Second
	proc.ConfigureGroup(cmd)
	cmd.Env = agentEnv(baseEnv, cwd)
	cmd.Stderr = &limitedDiscard{remaining: 65536, fail: fail}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, beforeStart(err)
	}
	// Cmd.Wait must not close a StdoutPipe ahead of its reader. Let os/exec
	// finish its output copy before closing our owned stream on process exit.
	stdout, output := io.Pipe()
	cmd.Stdout = output
	if err = cmd.Start(); err != nil {
		cancel()
		stdin.Close()
		stdout.Close()
		output.Close()
		return nil, beforeStart(fmt.Errorf("start managed provider: %w", err))
	}
	c := &connection{ctx: ctx, cancel: cancel, fail: fail, cmd: cmd, stdin: stdin, stdout: stdout, pid: cmd.Process.Pid, frames: make(chan wireRead, 64), done: make(chan struct{}), validate: validate}
	go c.read()
	go func() {
		c.waitErr = cmd.Wait()
		c.sweepGroup()
		output.Close()
		close(c.done)
	}()
	return c, nil
}

// probeGroup is groupAlive and signalGroup is proc.KillGroup; tests replace them
// to stand in for a member that outlives SIGKILL, which a real process cannot be
// made to do, and to count the signals sent.
var (
	probeGroup  = groupAlive
	signalGroup = proc.KillGroup
)

// sweepGroup is the one place the agent's process group is signalled after the
// agent was reaped, and it runs as soon as Wait returns. cmd.Cancel reaches the
// group only while the agent is still unreaped; once it is reaped, the group
// number is held by whatever members remain and nothing else. An empty group
// cannot gain a member, and its number is free for an unrelated process to take,
// so a group found empty is latched (groupGone) and never signalled or probed
// again, however late close() comes. A group found with members is still ours
// (its number cannot be reused while a member exists) and is killed now rather
// than at close(), by which time it may have emptied and been reused. The only
// residue is the interval between this probe and this signal, in which the last
// member would have to exit and a stranger's group take the number.
func (c *connection) sweepGroup() {
	if !probeGroup(c.pid) {
		c.groupGone.Store(true)
		return
	}
	signalGroup(c.pid)
}

// reapBudget bounds how long close() waits for the agent to be reaped, and,
// separately and from the moment it is, for the rest of its process group to be
// gone. Tests shorten it.
var reapBudget = 5 * time.Second

// groupPoll is how often close() re-probes a group it has killed.
const groupPoll = 10 * time.Millisecond

// agentEnv is the environment a managed provider starts with, given one that
// childenv has already stripped of provider credentials. The agent keeps its
// own login and the operator's ordinary preferences — it is the operator's
// tool and its needs cannot be enumerated — but never the host's model keys:
// handed ANTHROPIC_API_KEY, Claude Code bills the API instead of the
// subscription it is logged in to, whether or not a spend cap is set. Git
// repository-routing overrides are removed and the Manvi root is replaced.
func agentEnv(environ []string, cwd string) []string {
	return agentEnvKeyed(environ, cwd, childenv.Key)
}

// agentEnvKeyed is agentEnv with the name comparison supplied, so both the
// Windows (case-insensitive) and unix behaviours are testable on any host.
func agentEnvKeyed(environ []string, cwd string, nameKey func(string) string) []string {
	env := make([]string, 0, len(environ)+2)
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		key := nameKey(name)
		switch key {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CEILING_DIRECTORIES", "GIT_CONFIG", "GIT_CONFIG_COUNT", "DEVCOUNCIL_ROOT", "PWD":
			continue
		}
		if strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "DEVCOUNCIL_ROOT="+cwd, "PWD="+cwd)
}

func (c *connection) read() {
	defer close(c.frames)
	defer c.stdout.Close()
	scanner := bufio.NewScanner(c.stdout)
	scanner.Buffer(make([]byte, 4096), MaxFrameBytes+1)
	total := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		total += len(line)
		var p packet
		err := c.validate(line)
		if total > 32*1024*1024 {
			err = errors.New("managed provider output exceeded 32 MiB for the whole session; the cap counts every frame the provider wrote, including tool results the run does not record")
		}
		if err == nil {
			err = json.Unmarshal(line, &p)
		}
		// The scanner reuses its buffer, so a frame handed to another goroutine
		// must own its bytes.
		var raw []byte
		if err == nil {
			raw = append(raw, line...)
		}
		select {
		case c.frames <- wireRead{p, raw, err}:
		case <-c.ctx.Done():
			return
		}
		if err != nil {
			// The cause is recorded before the cancellation is visible, and
			// err() prefers it, so a reader that sees the cancel first still
			// reports the cap (or protocol) error that caused it.
			c.fail(err)
			return
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	select {
	case c.frames <- wireRead{err: err}:
	case <-c.ctx.Done():
	}
}

// validateFrameShape is what both protocols require of a line: bounded, valid
// UTF-8, one JSON object, no duplicate keys, nothing trailing. It deliberately
// says nothing about which fields must be present — that is the protocol's own
// rule and belongs to its validator.
func validateFrameShape(raw []byte, protocol string) (map[string]bool, error) {
	if len(raw) > MaxFrameBytes || !utf8.Valid(raw) {
		return nil, fmt.Errorf("invalid or oversized %s protocol frame", protocol)
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("%s protocol frame must be an object", protocol)
	}
	seen := make(map[string]bool)
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return nil, fmt.Errorf("%s protocol has a duplicate field", protocol)
		}
		seen[key] = true
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return nil, err
		}
	}
	if _, err = d.Token(); err != nil {
		return nil, err
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, fmt.Errorf("%s protocol frame has trailing content", protocol)
	}
	return seen, nil
}

// validateCodexFrame is the JSON-RPC rule: every frame is exactly one of an
// event (method) or a response (id plus exactly one of result/error).
func validateCodexFrame(raw []byte) error {
	seen, err := validateFrameShape(raw, "Codex")
	if err != nil {
		return err
	}
	if !seen["method"] && (!seen["id"] || seen["result"] == seen["error"]) {
		return errors.New("Codex frame is neither an event nor a response")
	}
	if seen["method"] && (seen["result"] || seen["error"]) {
		return errors.New("Codex frame mixes a request and response")
	}
	return nil
}

// validateClaudeFrame is the stream-json rule: every frame is a tagged union
// member, so `type` is the whole contract at this layer. A frame carrying
// JSON-RPC's fields instead is refused here rather than being half-decoded by
// an adapter that would find every field empty.
func validateClaudeFrame(raw []byte) error {
	seen, err := validateFrameShape(raw, "Claude")
	if err != nil {
		return err
	}
	if !seen["type"] {
		return errors.New("Claude frame has no type")
	}
	if seen["method"] || seen["params"] {
		return errors.New("Claude frame carries another protocol's envelope")
	}
	return nil
}
func (c *connection) nextID() json.RawMessage {
	c.sequence++
	return []byte(strconv.FormatUint(c.sequence, 10))
}
func (c *connection) send(ctx context.Context, p packet) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return c.sendRaw(ctx, data)
}

// sendRaw writes one already-encoded frame, for a protocol whose outbound
// bodies are not the JSON-RPC envelope.
func (c *connection) sendRaw(ctx context.Context, data []byte) error {
	if err := c.failure(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(data) > MaxFrameBytes {
		return errors.New("managed request exceeds its wire budget")
	}
	done := make(chan error, 1)
	go func() { _, err := c.stdin.Write(append(data, '\n')); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			c.cancel()
		}
		return err
	case <-ctx.Done():
		if err := c.failure(); err != nil {
			return err
		}
		c.cancel()
		return ctx.Err()
	case <-c.ctx.Done():
		return c.err()
	}
}

// err is why the connection ended: the error recorded by fail when a frame was
// refused, else the context's own (Canceled for a close or an interrupt).
func (c *connection) err() error {
	return context.Cause(c.ctx)
}

// failure is the recorded reason the connection failed, or nil when it is
// running or ended only by a plain cancel (Close, an interrupt). A failure that
// already happened outranks a caller's cancel that is ready at the same moment,
// so the outcome does not depend on which select case the runtime picks.
func (c *connection) failure() error {
	if c.ctx.Err() == nil {
		return nil
	}
	if cause := context.Cause(c.ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return nil
}
func (c *connection) receive(ctx context.Context) (packet, error) {
	p, _, err := c.receiveFrame(ctx)
	return p, err
}

// receiveFrame also returns the frame's own bytes, for a protocol whose bodies
// vary per type and are decoded by the adapter rather than by the envelope.
func (c *connection) receiveFrame(ctx context.Context) (packet, []byte, error) {
	// A timeout observing a live session does not restart or terminate it.
	select {
	case next, ok := <-c.frames:
		if !ok {
			return packet{}, nil, io.EOF
		}
		return next.value, next.raw, next.err
	case <-ctx.Done():
		if err := c.failure(); err != nil {
			return packet{}, nil, err
		}
		return packet{}, nil, ctx.Err()
	case <-c.ctx.Done():
		// The refused frame is queued ahead of the cancellation; take it first so
		// the reader sees the same error whichever case select picked.
		select {
		case next, ok := <-c.frames:
			if ok {
				return next.value, next.raw, next.err
			}
		default:
		}
		return packet{}, nil, c.err()
	}
}

// close ends the agent and reports whether everything in its process group is
// gone. Exit.Reaped says only that the direct child was reaped. The error says
// the rest: os/exec's, for a grandchild holding the agent's pipes, and one for a
// group member still alive after SIGKILL within the budget, which also sets
// Exit.GroupLive for callers that must not read it from the message. The group
// is killed by cmd.Cancel while the agent is unreaped and by sweepGroup the
// moment it is reaped, so a grandchild that redirected its stdio away is dead
// before close() looks; close() itself never signals, because by then the group
// number may belong to a stranger. A caller must not read a nil error as a clean
// exit when this returns one.
//
// A descendant that left the group with setsid or setpgid is out of reach of a
// group kill and a group probe alike; it is not chased. Its own children that
// stayed in the group are in reach, and a zombie among them whose parent is
// outside the group and never waits keeps the group probe positive for as long
// as that parent lives; that is reported as a live group, failing closed.
//
// Time: Wait gets reapBudget, and the group then gets a reapBudget of its own
// that starts when Wait returns, so close() takes at most twice reapBudget (a
// Wait that never returns still ends the call after one, with nothing said of
// the group). No caller in this module adds a timeout of its own; the one bound
// above them is ManagedRunner.Close, which waits five seconds for jobs to finish.
func (c *connection) close() (Exit, error) {
	c.closeOnce.Do(func() { c.stdin.Close(); c.cancel(); c.stdout.Close() })
	reaped := time.NewTimer(reapBudget)
	defer reaped.Stop()
	select {
	case <-c.done:
	case <-reaped.C:
		return Exit{}, errors.New("managed provider did not confirm process reaping before shutdown deadline")
	}
	result := Exit{Reaped: true, Code: c.cmd.ProcessState.ExitCode()}
	var err error
	var exit *exec.ExitError
	if c.waitErr != nil && !errors.As(c.waitErr, &exit) && !errors.Is(c.waitErr, context.Canceled) {
		err = c.waitErr
	}
	// The group's budget starts now, not with close(): had Wait used most of the
	// one budget, a group that empties a moment later would be reported live.
	gone := time.NewTimer(reapBudget)
	defer gone.Stop()
	groupErr := c.awaitGroup(gone.C)
	result.GroupLive = groupErr != nil
	return result, errors.Join(err, groupErr)
}

// awaitGroup waits, until deadline fires, for the agent's process group, killed
// by sweepGroup, to have no member. It only probes: nothing is signalled after
// the agent was reaped except by sweepGroup.
func (c *connection) awaitGroup(deadline <-chan time.Time) error {
	for !c.groupGone.Load() {
		if !probeGroup(c.pid) {
			c.groupGone.Store(true)
			break
		}
		select {
		case <-deadline:
			return fmt.Errorf("managed provider process group %d still has a live member after SIGKILL", c.pid)
		case <-time.After(groupPoll):
		}
	}
	return nil
}

// maxDeferredEvents bounds both the events one response preflight may drain
// and the backlog it may leave deferred, for every managed adapter: the same
// run is being protected, so one number.
const maxDeferredEvents = 128

// drainBacklog is the one bounded preflight drain shared by the managed
// adapters. It consumes already-buffered handshake-queue items and wire frames
// (never blocking), decodes each through event, and parks the meaningful ones
// in deferred so Next still delivers them in order.
func drainBacklog[T any](agent string, wire *connection, queue *[]T, deferred *[]Event,
	item func(wireRead) T, event func(T) (Event, bool, error)) error {
	for i := 0; i < maxDeferredEvents; i++ {
		var next T
		if len(*queue) > 0 {
			next = (*queue)[0]
			*queue = (*queue)[1:]
		} else {
			select {
			case frame, ok := <-wire.frames:
				if !ok {
					return errors.New(agent + " connection ended before response validation")
				}
				if frame.err != nil {
					return frame.err
				}
				next = item(frame)
			default:
				return wire.err()
			}
		}
		before := len(*deferred)
		e, meaningful, err := event(next)
		if err != nil {
			return err
		}
		if meaningful {
			if len(*deferred) >= maxDeferredEvents {
				return errors.New(agent + " response preflight exceeded its event budget")
			}
			// event may itself have queued follow-ups behind the one it returns
			// (a turn end queues its status after the withdrawals it returns first).
			// The returned event goes ahead of them, the order Next delivers.
			*deferred = slices.Insert(*deferred, before, e)
		}
	}
	return errors.New(agent + " response preflight could not drain its bounded event backlog")
}

// maxWithdrawnIDs bounds withdrawnIDs. It matches the per-run callback budget
// (2048 requests), so a run cannot withdraw more ids than the set holds and
// nothing is evicted in practice; the bound is what keeps it finite.
const maxWithdrawnIDs = 2048

// withdrawnIDs remembers the callbacks a provider (or the end of its turn)
// withdrew, so an answer that races the withdrawal is told it was withdrawn
// (ErrCallbackWithdrawn, which the host treats as benign) and not that it was
// stale. It is the one owner of that bookkeeping for every adapter. The zero
// value is ready; the oldest id is dropped first past maxWithdrawnIDs.
type withdrawnIDs struct {
	set   map[string]struct{}
	order []string
}

// add records id. Recording an id twice is harmless.
func (w *withdrawnIDs) add(id string) {
	if _, ok := w.set[id]; ok {
		return
	}
	if w.set == nil {
		w.set = make(map[string]struct{})
	}
	if len(w.order) >= maxWithdrawnIDs {
		delete(w.set, w.order[0])
		w.order = w.order[1:]
	}
	w.set[id] = struct{}{}
	w.order = append(w.order, id)
}

func (w *withdrawnIDs) has(id string) bool {
	_, ok := w.set[id]
	return ok
}

// withdrawalEvents records ids as withdrawn and returns one CancelledID event
// for each, in a stable order. The caller has already forgotten the callbacks.
func (w *withdrawnIDs) withdrawalEvents(ids []string) []Event {
	slices.Sort(ids)
	events := make([]Event, 0, len(ids)+1)
	for _, id := range ids {
		w.add(id)
		events = append(events, Event{CancelledID: id})
	}
	return events
}

// leadWith returns events[0] and queues the rest behind whatever is already
// deferred, so Next delivers them in order. It is how a turn end reports every
// withdrawal ahead of its status for every adapter. events must not be empty.
func leadWith(deferred *[]Event, events []Event) Event {
	*deferred = append(*deferred, events[1:]...)
	return events[0]
}

// callbackStale reports whether a host's copy of a callback no longer matches
// the one the adapter retained when the provider asked: already sent, run
// finished, expired, or any of payload, digest (stored and recomputed),
// thread, turn or kind changed. It is the single predicate that gates approval
// delivery, so a field added here protects every adapter at once.
func callbackStale(request, source Request, sent, finished bool) bool {
	return sent || finished || !time.Now().Before(source.ExpiresAt) ||
		request.ID != source.ID ||
		request.Payload != source.Payload || request.Digest != source.Digest ||
		payloadDigest(request.Payload) != source.Digest ||
		request.ThreadID != source.ThreadID || request.TurnID != source.TurnID ||
		request.Kind != source.Kind
}

// neutraliseText is the one place agent-authored text is made safe to record and
// to show: a terminal or a bidi control in it would otherwise reach the host's
// record, its terminal and its UI as an instruction rather than as what the agent
// said. Nothing is dropped, so the record still shows what the agent sent: ESC
// becomes the literal `\x1b`, every other C0 or C1 control and DEL becomes
// `\xNN`, a bidi override, isolate or mark becomes `<U+202E>`, and a byte that is
// not UTF-8 becomes `\xNN`. Newline, tab and a carriage return that starts a CRLF are kept. A valid multi-byte rune is
// copied whole. Text with nothing to neutralise is returned as it came, without
// allocating; otherwise one linear pass writes one buffer.
func neutraliseText(s string) string {
	first := -1
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if mustNeutralise(r, n) && (r != '\r' || !crlfAt(s, i)) {
			first = i
			break
		}
		i += n
	}
	if first < 0 {
		return s
	}
	const hexDigits, lowerHex = "0123456789ABCDEF", "0123456789abcdef"
	var b strings.Builder
	b.Grow(len(s) + 3*(len(s)-first)/4 + 16)
	b.WriteString(s[:first])
	for i := first; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case !mustNeutralise(r, n) || (r == '\r' && crlfAt(s, i)):
			b.WriteString(s[i : i+n])
		case r == utf8.RuneError || r < 0x100:
			// Invalid input is the byte itself (n == 1); a C0, DEL or C1 rune is
			// below U+0100 and is shown by its code point.
			v := r
			if r == utf8.RuneError {
				v = rune(s[i])
			}
			b.WriteString(`\x`)
			b.WriteByte(lowerHex[v>>4])
			b.WriteByte(lowerHex[v&0xf])
		default:
			b.WriteString("<U+")
			for shift := 12; shift >= 0; shift -= 4 {
				b.WriteByte(hexDigits[(r>>shift)&0xf])
			}
			b.WriteByte('>')
		}
		i += n
	}
	return b.String()
}

// crlfAt reports whether s[i] is a carriage return immediately followed by a
// line feed, the line ending Windows tools and some providers write.
func crlfAt(s string, i int) bool {
	return s[i] == '\r' && i+1 < len(s) && s[i+1] == '\n'
}

// mustNeutralise reports whether r (decoded with width n) is invalid UTF-8, a
// C0 control other than newline and tab, DEL, a C1 control, or a bidi control.
// A carriage return is reported here; neutraliseText keeps the one that starts a
// CRLF (crlfAt) before it asks.
func mustNeutralise(r rune, n int) bool {
	switch {
	case r == utf8.RuneError:
		return n == 1 // a real U+FFFD is three bytes and stays
	case r < 0x20:
		return r != '\n' && r != '\t'
	case r < 0x7f:
		return false
	case r <= 0x9f:
		return true
	case r == 0x061c, r == 0x200e, r == 0x200f:
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}
