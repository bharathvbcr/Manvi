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
	// members is the agent's process group as sweepGroup found it, and
	// membersKnown says whether that listing succeeded. Both are written before
	// done is closed and read only after it. Without a known listing the group is
	// counted live whenever it answers a probe.
	members      []groupMember
	membersKnown bool
	validate     func([]byte) error
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
	probeGroup     = groupAlive
	signalGroup    = proc.KillGroup
	snapshotGroup  = groupMembers
	membersInGroup = anyMemberInGroup
)

// groupMember is one process of the agent's group: its pid and the time it
// started, which tells it from a later process handed the same pid.
type groupMember struct {
	pid   int
	start int64
}

// sweepGroup is the one place the agent's process group is signalled after the
// agent was reaped, and it runs as soon as Wait returns. cmd.Cancel reaches the
// group only while the agent is still unreaped; once it is reaped, the group
// number is held by whatever members remain and nothing else. An empty group
// cannot gain a member, and its number is free for an unrelated process to take,
// so a group found empty is latched (groupGone) and never signalled or probed
// again, however late close() comes. A group found with members is still ours
// (its number cannot be reused while a member exists) and is killed now rather
// than at close(), by which time it may have emptied and been reused.
//
// A member existing is also the only moment membership is provably ours, so the
// members (pid and start time) are listed here, and groupLive counts the group
// as the agent's only while one of them is still in it. A number found to have
// no member when listed was free, and any group that holds it later is a
// stranger's. If the listing fails, groupLive falls back to the bare probe,
// which fails closed.
//
// The group is listed twice, before and after the signal, and the two listings
// are unioned (pid and start time). Before the signal, because that is the last
// moment nothing can have killed the members and let a stranger take the number.
// That listing alone misses a process forked after it by a listed member that
// then exits: the group would read free while that process lives, which the bare
// probe never did. After the signal, because a number cannot be handed on while
// a member exists, so whatever is in the group right after the signal is still
// ours, and it catches such a fork. What each order misses: the first listing
// misses forks after it; the second misses members that died in the signal and
// anything forked after it, and may record a stranger only if every member died
// and a stranger took the same pid with the same start time inside the
// list-kill-list window, which needs both numbers reused. The remaining
// residue is a fork after the second listing by a process that survives the
// SIGKILL. It is not reachable: SIGKILL cannot be blocked and a dying process
// cannot fork. If either listing fails, groupLive falls
// back to the bare probe, which fails closed. A listed zombie keeps counting
// until its parent reaps it, as kill(2) counts it. The residue between the probe
// and the first listing is the same as before: the last member would have to
// exit and a stranger's group take the number in between.
func (c *connection) sweepGroup() {
	if !probeGroup(c.pid) {
		c.groupGone.Store(true)
		return
	}
	before, errBefore := snapshotGroup(c.pid)
	signalGroup(c.pid)
	after, errAfter := snapshotGroup(c.pid)
	c.members, c.membersKnown = unionMembers(before, after), errBefore == nil && errAfter == nil
}

// unionMembers is a and b without duplicates, by pid and start time.
func unionMembers(a, b []groupMember) []groupMember {
	out := slices.Clone(a)
	for _, m := range b {
		if !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	return out
}

// groupLive says whether the agent's process group, as sweepGroup left it, still
// has a member that is the agent's own. It runs every groupPoll, so it asks about
// the snapshotted members only (anyMemberInGroup) and never lists the group: the
// full listing is a /proc scan on Linux and belongs to the two sweep-time
// snapshots.
func (c *connection) groupLive() bool {
	if !probeGroup(c.pid) {
		return false
	}
	if !c.membersKnown {
		return true
	}
	return membersInGroup(c.pid, c.members)
}

// reapBudget bounds how long close() waits for the agent to be reaped, and,
// separately and from the moment it is, for the rest of its process group to be
// gone. Tests shorten it.
var reapBudget = 5 * time.Second

// CloseBudget is the longest close() can take: the agent gets reapBudget to be
// reaped and its process group a reapBudget of its own from the moment it is.
// A caller that waits for close() to finish (serve's ManagedRunner.Close) takes
// its deadline from here, so it never gives up before the reap does.
func CloseBudget() time.Duration { return 2 * reapBudget }

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
	// Once the connection has failed, the outcome does not depend on which of
	// several ready cases select picks. The rules, in order: a frame the write
	// delivered was delivered (the next read reports the failure); otherwise the
	// recorded failure is the reason. A plain cancel (Close, an interrupt) that
	// lands as the write completes may still report either the cancel or the
	// delivery; both end the run on the next read.
	select {
	case err := <-done:
		return c.written(err)
	case <-ctx.Done():
		if c.failure() == nil {
			// A plain caller cancel: the connection is cancelled with it, unless
			// the write has already reported.
			select {
			case err := <-done:
				return c.written(err)
			default:
			}
			c.cancel()
			return ctx.Err()
		}
		// The caller's context is done because the connection failed (a caller
		// may pass the connection's own context): the failure rule below decides.
	case <-c.ctx.Done():
	}
	// The connection ended while the write was in flight, for example because the
	// agent read the frame and answered with one the reader refused. The write's
	// own result decides. A failed connection's process is killed and a closed
	// one's stdin is closed, either of which ends a write still blocked on its
	// pipe; writeSettle bounds the wait regardless.
	select {
	case err := <-done:
		if err == nil {
			return nil
		}
	case <-time.After(writeSettle):
	}
	return c.err()
}

// written is sendRaw's answer once the write has reported: nil for a delivered
// frame; for a failed write, the connection's recorded failure when there is
// one (it says why the pipe broke), else the pipe's error, ending the
// connection.
func (c *connection) written(err error) error {
	if err == nil {
		return nil
	}
	if failure := c.failure(); failure != nil {
		return failure
	}
	c.cancel()
	return err
}

// writeSettle is how long sendRaw waits, once the connection has ended, for an
// in-flight write to report whether it delivered its frame.
const writeSettle = time.Second

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
		if !c.groupLive() {
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

// The per-run callback budgets every managed adapter enforces: the same run is
// being protected, so one number each. A callback is admitted only while fewer
// than maxPendingCallbacks are awaiting an answer and fewer than
// maxRunCallbacks have been admitted in the run, and `seen` (the ids admitted,
// kept so a replayed id is refused) gains an id only where the run's count does,
// after those checks, so it never holds more than maxRunCallbacks.
const (
	maxPendingCallbacks = 32
	maxRunCallbacks     = 2048
)

// maxDrainItems bounds how many already-buffered items one response preflight
// reads. maxDeferredBacklog bounds the events it may leave parked in deferred,
// and it is the one number that does: it counts the events those items decode
// to plus what a turn end queues behind itself, which is one withdrawal per
// callback still pending (at most maxPendingCallbacks) and the status. Every
// item decodes to at most one event except the turn end, which happens once per
// turn, so a preflight cannot pass it by construction (the check after each
// insertion is the enforcement). A turn end is what releases the run, so a
// preflight that reaches it behind a full read is allowed through to the bound
// and is never failed for the withdrawals it carries.
const (
	maxDrainItems      = 128
	maxDeferredBacklog = maxDrainItems + maxPendingCallbacks + 1
)

// drainBacklog is the one bounded preflight drain shared by the managed
// adapters. It consumes already-buffered handshake-queue items and wire frames
// (never blocking), decodes each through event, and parks the meaningful ones
// in deferred so Next still delivers them in order.
func drainBacklog[T any](agent string, wire *connection, queue *[]T, deferred *[]Event,
	item func(wireRead) T, event func(T) (Event, bool, error)) error {
	for i := 0; i <= maxDrainItems; i++ {
		if i == maxDrainItems && (len(*queue) > 0 || len(wire.frames) > 0) {
			// Something is still buffered past the budget. It is detected by
			// length, not by reading a frame: a frame read only to find out
			// would be lost with the failed preflight, and it may be the turn
			// end. The one reader holds the session lock, so a non-zero length
			// is not racing a consumer.
			return errors.New(agent + " response preflight could not drain its bounded event backlog")
		}
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
			// event may itself have queued follow-ups behind the one it returns
			// (a turn end queues its withdrawals and status behind the first).
			// The returned event goes ahead of them, the order Next delivers.
			*deferred = slices.Insert(*deferred, before, e)
			if len(*deferred) > maxDeferredBacklog {
				return errors.New(agent + " response preflight exceeded its event budget")
			}
		}
	}
	return errors.New(agent + " response preflight could not drain its bounded event backlog")
}

// maxWithdrawnIDs bounds withdrawnIDs. It matches the per-run callback budget
// (2048 requests), so a run cannot withdraw more ids than the set holds and
// nothing is evicted in practice; the bound is what keeps it finite.
const maxWithdrawnIDs = maxRunCallbacks

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

// neutraliseText makes agent-authored text safe to record and to show: a terminal
// or a bidi control in it would otherwise reach the host's record, its terminal
// and its UI as an instruction rather than as what the agent said. Nothing is
// dropped, so the record still shows what the agent sent. The rendering has one
// escape character, the backslash, and the backslash itself is escaped, so a
// reader can always tell a neutralised byte from text the agent wrote:
//
//   - a doubled backslash is one literal backslash;
//   - `\xNN` is ESC, another C0 control, DEL, or a byte that is not UTF-8
//     (so a literal `\x1b` in agent text reads `\\x1b`);
//   - `\u{XXXX}` is a C1 control rune (U+0080..U+009F) or an invisible,
//     line-separator or bidi rune (U+00AD, U+061C,
//     U+180E, U+200E/F, U+2028/2029, U+202A-202E, U+2060-2064, U+2066-2069,
//     U+206A-206F, U+FEFF, U+FFF9-FFFB and the Unicode tag block U+E0000-E007F).
//
// Newline, tab and a carriage return that starts a CRLF are kept. A valid
// multi-byte rune is copied whole. Text with nothing to neutralise is returned
// as it came, without allocating; otherwise one linear pass writes one buffer.
// The mapping is not idempotent (a second pass escapes the first pass's
// backslashes), so each field is neutralised exactly once, at neutraliseEvent.
func neutraliseText(s string) string { return neutralise(s, false) }

// neutraliseJSON is neutraliseText for a field that is a JSON document: it keeps
// the document parseable and meaning-preserving by writing each offending rune as
// the JSON escape of that rune (\u001b, \u202e, a surrogate pair above U+FFFF)
// and leaving every backslash alone. Whitespace between tokens is kept.
func neutraliseJSON(s string) string { return neutralise(s, true) }

// NeutraliseToolInput renders Tool.Input, which the adapters keep raw in the
// Event, for a host record. A valid JSON document stays valid JSON; anything
// else (a string input, or a document cut at the byte cap) is neutralised as text.
func NeutraliseToolInput(s string) string {
	if json.Valid([]byte(s)) {
		return neutraliseJSON(s)
	}
	return neutraliseText(s)
}

func neutralise(s string, asJSON bool) string {
	first := nextEscape(s, 0, asJSON)
	if first < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 3*(len(s)-first)/4 + 16)
	i := 0
	for first >= 0 {
		b.WriteString(s[i:first])
		r, n := utf8.DecodeRuneInString(s[first:])
		writeEscape(&b, s[first:first+n], r, asJSON)
		i = first + n
		first = nextEscape(s, i, asJSON)
	}
	b.WriteString(s[i:])
	return b.String()
}

// asciiSpecial marks the ASCII bytes needsEscape must look at: C0 controls other
// than newline and tab, DEL, and the backslash. Everything else below 0x80 is
// copied without decoding, which keeps the clean path a byte scan.
var asciiSpecial = func() (t [utf8.RuneSelf]bool) {
	for c := range t {
		t[c] = (c < 0x20 && c != '\n' && c != '\t') || c == 0x7f || c == '\\'
	}
	return t
}()

// nextEscape returns the index of the first rune at or after from that must be
// rewritten, or -1.
func nextEscape(s string, from int, asJSON bool) int {
	for i := from; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			if asciiSpecial[c] && needsEscape(s, i, rune(c), 1, asJSON) {
				return i
			}
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if mustNeutralise(r, n) {
			return i
		}
		i += n
	}
	return -1
}

// writeEscape writes the rewritten form of the rune r, whose source text is raw.
func writeEscape(b *strings.Builder, raw string, r rune, asJSON bool) {
	const upperHex, lowerHex = "0123456789ABCDEF", "0123456789abcdef"
	switch {
	case r == '\\' && !asJSON:
		b.WriteString(`\\`)
	case asJSON:
		writeJSONEscape(b, r, len(raw))
	case r == utf8.RuneError || r < 0x80:
		// Invalid input is the byte itself (one byte); a C0 control or DEL is one
		// valid byte and is shown by it. A C1 rune (U+0080..U+009F) is two bytes and
		// takes the rune form below, so it cannot read like an invalid byte.
		v := r
		if r == utf8.RuneError {
			v = rune(raw[0])
		}
		b.WriteString(`\x`)
		b.WriteByte(lowerHex[v>>4])
		b.WriteByte(lowerHex[v&0xf])
	default:
		b.WriteString(`\u{`)
		for shift := 20; shift >= 0; shift -= 4 {
			if d := (r >> shift) & 0xf; d != 0 || shift <= 12 || r>>(shift+4) != 0 {
				b.WriteByte(upperHex[d])
			}
		}
		b.WriteByte('}')
	}
}

// neutraliseEvent is the one seam agent-authored strings leave codingagent
// through: every adapter's Next returns its event via this function (a source
// test fails if one does not). It neutralises each string the host records or
// shows, once:
//
//	neutralised: Event.Text, Event.Status; Tool.ID, Name, Status, ParentID, Output
//	             (the same mapping on ID and ParentID, so a child still names its parent)
//	raw by design, with the reason:
//	  Tool.Input      a JSON document the host merges and truncates; serve renders it
//	                  through NeutraliseToolInput when it writes the [tools] snapshot
//	  Request         the whole value is the approval's evidence: callbackStale compares
//	                  it with the adapter's retained copy and the digest is over the raw
//	                  Payload, so any rewrite would stale every approval; its ID, Kind,
//	                  ThreadID, TurnID, Questions[].ID are keys the provider matches on
//	  TurnID, ResolvedID, CancelledID   opaque provider ids the host matches on
//
// The Tools slice is copied before a change, so adapter state is never mutated.
func neutraliseEvent(e Event) Event {
	e.Text = neutraliseText(e.Text)
	e.Status = neutraliseText(e.Status)
	var copied bool
	for i, t := range e.Tools {
		n := t
		n.ID, n.Name, n.Status = neutraliseText(t.ID), neutraliseText(t.Name), neutraliseText(t.Status)
		n.ParentID, n.Output = neutraliseText(t.ParentID), neutraliseText(t.Output)
		if n == t {
			continue
		}
		if !copied {
			e.Tools = append([]Tool(nil), e.Tools...)
			copied = true
		}
		e.Tools[i] = n
	}
	return e
}

// writeJSONEscape writes r as the JSON escape of that rune. An invalid byte
// (n == 1) becomes U+FFFD, as encoding/json writes it.
func writeJSONEscape(b *strings.Builder, r rune, n int) {
	const lowerHex = "0123456789abcdef"
	if r == utf8.RuneError && n == 1 {
		r = 0xfffd
	}
	unit := func(u rune) {
		b.WriteString(`\u`)
		for shift := 12; shift >= 0; shift -= 4 {
			b.WriteByte(lowerHex[(u>>shift)&0xf])
		}
	}
	if r >= 0x10000 {
		r -= 0x10000
		unit(0xd800 + r>>10)
		unit(0xdc00 + r&0x3ff)
		return
	}
	unit(r)
}

// needsEscape reports whether the rune at s[i] (decoded with width n) must be
// rewritten. In text mode a backslash is escaped and a carriage return is kept
// only when it starts a CRLF; in JSON mode backslashes are the document's own
// and every carriage return is inter-token whitespace.
func needsEscape(s string, i int, r rune, n int, asJSON bool) bool {
	switch r {
	case '\\':
		return !asJSON
	case '\r':
		return !asJSON && !crlfAt(s, i)
	}
	return mustNeutralise(r, n)
}

// crlfAt reports whether s[i] is a carriage return immediately followed by a
// line feed, the line ending Windows tools and some providers write.
func crlfAt(s string, i int) bool {
	return s[i] == '\r' && i+1 < len(s) && s[i+1] == '\n'
}

// mustNeutralise reports whether r (decoded with width n) is invalid UTF-8, a
// C0 control other than newline and tab, DEL, a C1 control, or an invisible,
// line-separator or bidi rune (the list is on neutraliseText). A carriage return
// is reported here; needsEscape keeps the one that starts a CRLF.
func mustNeutralise(r rune, n int) bool {
	switch {
	case r == utf8.RuneError:
		return n == 1 // a real U+FFFD is three bytes and stays
	case r < 0x20:
		return r != '\n' && r != '\t'
	case r < 0x7f:
		return false
	case r <= 0x9f, r == 0x00ad, r == 0x061c, r == 0x180e, r == 0x200e, r == 0x200f:
		return true
	case r == 0x2028, r == 0x2029, r >= 0x202a && r <= 0x202e, r >= 0x2060 && r <= 0x2064, r >= 0x2066 && r <= 0x206f:
		return true
	case r == 0xfeff, r >= 0xfff9 && r <= 0xfffb, r >= 0xe0000 && r <= 0xe007f:
		return true
	}
	return false
}

// MaxEscapeGrowth is the most a neutralised string can grow: one invalid byte
// becomes `\xNN` and a two-byte C1 rune becomes `\u{0085}`, both 4x. A host that
// bounds an already-neutralised tool uses it to scale the adapter limits.
const MaxEscapeGrowth = 4

// CutNeutralised returns the longest prefix of s, at most n bytes, that ends
// between whole units of neutralised text: a valid rune, a `\\`, a `\xNN` or a
// `\u{...}`. A byte cut would otherwise leave a fragment such as `\x1`, `\u{202E`
// or a lone backslash, which the doubled-backslash rule says never occurs in the
// record. Text that fits is returned unchanged.
func CutNeutralised(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n <= 0 {
		return ""
	}
	i := 0
	for i < len(s) {
		w := neutralisedUnit(s[i:])
		if i+w > n {
			break
		}
		i += w
	}
	return s[:i]
}

// neutralisedUnit is the width of the first unit of s: a whole escape, or one
// rune (one byte when it is invalid UTF-8). A backslash that starts no
// well-formed escape, which only host-written text can hold, takes the next rune
// with it so a cut never separates the pair.
func neutralisedUnit(s string) int {
	if s[0] != '\\' {
		_, w := utf8.DecodeRuneInString(s)
		return w
	}
	switch {
	case len(s) >= 2 && s[1] == '\\':
		return 2
	case len(s) >= 4 && s[1] == 'x' && isHexDigit(s[2]) && isHexDigit(s[3]):
		return 4
	case len(s) >= 3 && s[1] == 'u' && s[2] == '{':
		for j := 3; j < len(s); j++ {
			if s[j] == '}' {
				return j + 1
			}
			if !isHexDigit(s[j]) {
				break
			}
		}
	}
	if len(s) == 1 {
		return 1
	}
	_, w := utf8.DecodeRuneInString(s[1:])
	return 1 + w
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}
