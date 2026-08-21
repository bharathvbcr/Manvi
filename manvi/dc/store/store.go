// Package store is the Go side of the harness's state boundary.
//
// The execution plane is Go; the state store is Rust, because SQLite is. The
// two are joined by a process rather than by cgo: linking them in-process would
// forfeit CGO_ENABLED=0, simple cross-compilation, and the single static binary
// that the Go side was chosen for in the first place. So this package execs the
// `dcstore` binary and reads one JSON object from its stdout.
//
// The boundary is deliberately narrow. Everything that decides *whether* work
// may proceed — mutual exclusion, expiry, ownership — lives on the Rust side
// where the schema's partial unique index enforces it. This package transports
// answers; it does not compute them, and it must never be tempted to cache one.
// A cached lease is a lease that has already expired somewhere else.
package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"manvi/dc"
)

// Client runs the store binary.
type Client struct {
	// Binary is the path to `dcstore`.
	Binary string
	// DB is the SQLite file both this harness and DevCouncil read.
	DB string
	// Timeout bounds a single invocation. A store call is local and fast; a
	// hung one means something is wrong with the filesystem or the lock, and
	// blocking a turn forever is the wrong answer to that.
	Timeout time.Duration
}

// New builds a client with a default timeout.
func New(binary, db string) *Client {
	return &Client{Binary: binary, DB: db, Timeout: 10 * time.Second}
}

// Lease mirrors the store's lease record.
type Lease struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id"`
	Owner     string `json:"owner"`
	Agent     string `json:"agent,omitempty"`
	ClientID  string `json:"client_id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Token     string `json:"token"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// response is the envelope every command returns.
type response struct {
	OK              bool     `json:"ok"`
	Store           string   `json:"store"`
	SchemaVersion   int      `json:"schema_version"`
	Code            string   `json:"code"`
	Error           string   `json:"error"`
	Holder          string   `json:"holder"`
	Lease           *Lease   `json:"lease"`
	Leases          []Lease  `json:"leases"`
	Released        bool     `json:"released"`
	SuggestedAction string   `json:"suggested_action"`
	SuggestedTool   string   `json:"suggested_tool"`
	Task            *Task    `json:"task"`
	Tasks           []string `json:"tasks"`
	Written         bool     `json:"written"`
	// CurrentAppended is the appended-scope value the store holds, sent back
	// when a compare-and-swap found a different one. It is raw so the retry
	// compares the store's own bytes rather than a re-serialisation of them.
	CurrentAppended json.RawMessage `json:"current_appended"`
}

// Conflict reports that another agent holds the task. It is a distinct type
// because contention is an ordinary outcome an orchestrator routes on — take
// the next task — not a failure it should retry or surface as an outage.
type Conflict struct {
	TaskID string
	Holder string
}

func (c *Conflict) Error() string {
	return fmt.Sprintf("task %s is held by %s", c.TaskID, c.Holder)
}

// Code returns the lease code an agent branches on.
func (c *Conflict) Code() dc.LeaseCode { return dc.LeaseHeldByOther }

// AcquireRequest asks for a lease.
type AcquireRequest struct {
	TaskID   string
	Owner    string
	Agent    string
	ClientID string
	RunID    string
	Branch   string
	// TTL bounds how long the lease survives without renewal. Zero is refused:
	// a lease that cannot expire is a task that stays locked when its holder
	// dies, and the harness cannot tell the difference between a builder that
	// is thinking and one whose process is gone.
	TTL time.Duration
	// Force steals an active lease. Human intervention only — the grant seam
	// is how an agent asks for something like this, and it cannot grant it.
	Force bool
}

// Acquire takes a lease, or returns *Conflict when another agent holds it.
func (c *Client) Acquire(ctx context.Context, req AcquireRequest) (*Lease, error) {
	if req.TTL <= 0 {
		return nil, errors.New("store: a lease needs a TTL; an unexpiring lease strands the task when its holder dies")
	}
	args := []string{"acquire",
		"--task", req.TaskID,
		"--owner", req.Owner,
		"--ttl-seconds", strconv.Itoa(int(req.TTL.Seconds())),
	}
	for flag, value := range map[string]string{
		"--agent": req.Agent, "--client-id": req.ClientID,
		"--run-id": req.RunID, "--branch": req.Branch,
	} {
		if value != "" {
			args = append(args, flag, value)
		}
	}
	if req.Force {
		args = append(args, "--force", "true")
	}

	out, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	if !out.OK {
		if out.Code == string(dc.LeaseHeldByOther) {
			return nil, &Conflict{TaskID: req.TaskID, Holder: out.Holder}
		}
		return nil, fmt.Errorf("store: acquire failed: %s", out.Error)
	}
	return out.Lease, nil
}

// Diagnose classifies a token, returning the code and the recovery an agent
// should take. This is the check every mutating tool call passes through.
func (c *Client) Diagnose(ctx context.Context, taskID, token string) (dc.LeaseCode, string, string, error) {
	out, err := c.run(ctx, "diagnose", "--task", taskID, "--token", token)
	if err != nil {
		return "", "", "", err
	}
	code := dc.LeaseCode(out.Code)
	if code == "" {
		return "", "", "", errors.New("store: diagnose returned no code")
	}
	return code, out.SuggestedAction, out.SuggestedTool, nil
}

// Valid is the predicate the write gate uses.
func (c *Client) Valid(ctx context.Context, taskID, token string) (bool, error) {
	code, _, _, err := c.Diagnose(ctx, taskID, token)
	if err != nil {
		// A store that cannot be reached must not read as a valid lease. Fail
		// closed: the whole point of the check is to stop a write.
		return false, err
	}
	return code == dc.LeaseValid, nil
}

// Release ends a lease. It reports whether one was actually released, so a
// caller can tell "released" from "there was nothing to release".
func (c *Client) Release(ctx context.Context, taskID, token string) (bool, error) {
	out, err := c.run(ctx, "release", "--task", taskID, "--token", token)
	if err != nil {
		return false, err
	}
	return out.Released, nil
}

// Renew extends a lease before it expires. A nil lease with no error means the
// lease had already expired — renewal only works before the TTL passes, and the
// recovery is to check out again.
func (c *Client) Renew(ctx context.Context, taskID, token string, ttl time.Duration) (*Lease, error) {
	out, err := c.run(ctx, "renew", "--task", taskID, "--token", token,
		"--ttl-seconds", strconv.Itoa(int(ttl.Seconds())))
	if err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, nil
	}
	return out.Lease, nil
}

// Active returns the live lease on a task, or nil.
func (c *Client) Active(ctx context.Context, taskID string) (*Lease, error) {
	out, err := c.run(ctx, "active", "--task", taskID)
	if err != nil {
		return nil, err
	}
	return out.Lease, nil
}

// ActiveLeases lists every live lease — what `dev tasks` shows in its Lease
// column, read from the same file.
func (c *Client) ActiveLeases(ctx context.Context) ([]Lease, error) {
	out, err := c.run(ctx, "list")
	if err != nil {
		return nil, err
	}
	return out.Leases, nil
}

// Task is a task's scope, as the gate needs it.
type Task struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Status     string `json:"status"`
	Difficulty string `json:"difficulty,omitempty"`
	// PlannedFiles is the union the store returns: the plan, plus whatever an
	// executor appended to it. Domain splits the two apart.
	PlannedFiles     []dc.PlannedFile `json:"planned_files"`
	AllowedCommands  []string         `json:"allowed_commands"`
	ExpectedTests    []string         `json:"expected_tests"`
	ForbiddenChanges []string         `json:"forbidden_changes"`
	// AgentAppendedRaw is the appended-scope column exactly as stored. It is
	// kept in its wire form because the store's compare-and-swap compares text:
	// a re-serialisation would differ in whitespace or in the bare-string form
	// a hand-written plan may use, and every widening would report as a
	// conflict with itself.
	AgentAppendedRaw json.RawMessage `json:"agent_appended_planned_files"`
	// AgentAppended is AgentAppendedRaw decoded, filled in by Client.Task.
	AgentAppended []dc.PlannedFile `json:"-"`
}

// Domain converts to the type the policy gate evaluates against.
//
// The union the store returns is split back into the plan and the runtime
// widening, because the gate has to be able to say which authorised a write.
// The subtraction is by path: an entry that appears in both is attributed to
// the widening, which is the conservative direction — it under-reports scope as
// planned, never over-reports it.
func (t *Task) Domain() *dc.Task {
	if t == nil {
		return nil
	}
	planned := t.PlannedFiles
	if len(t.AgentAppended) > 0 {
		appended := make(map[string]bool, len(t.AgentAppended))
		for _, pf := range t.AgentAppended {
			appended[pf.Path] = true
		}
		planned = make([]dc.PlannedFile, 0, len(t.PlannedFiles))
		for _, pf := range t.PlannedFiles {
			if !appended[pf.Path] {
				planned = append(planned, pf)
			}
		}
	}
	return &dc.Task{
		ID:                        t.ID,
		Title:                     t.Title,
		Status:                    t.Status,
		Difficulty:                t.Difficulty,
		PlannedFiles:              planned,
		AgentAppendedPlannedFiles: t.AgentAppended,
		AllowedCommands:           t.AllowedCommands,
		ForbiddenChanges:          t.ForbiddenChanges,
	}
}

// Task reads one task's scope. A nil task with no error means the id is unknown
// — a normal answer a caller branches on, not a fault.
func (c *Client) Task(ctx context.Context, taskID string) (*Task, error) {
	out, err := c.run(ctx, "task", "--task", taskID)
	if err != nil {
		return nil, err
	}
	if out.Task == nil {
		return nil, nil
	}
	// Decoded here rather than lazily at the gate. An unreadable scope column
	// must not reach a write decision as an empty one: "the plan authorises
	// nothing extra" and "nobody could read what the plan authorises" are
	// different answers, and only one of them is safe to act on.
	if len(bytes.TrimSpace(out.Task.AgentAppendedRaw)) > 0 {
		if err := json.Unmarshal(out.Task.AgentAppendedRaw, &out.Task.AgentAppended); err != nil {
			return nil, fmt.Errorf("store: task %s has an unreadable agent-appended scope: %w", taskID, err)
		}
	}
	return out.Task, nil
}

// Scope-widening ceilings. They mirror the store's own, and exist on this side
// as well because this is where the list is assembled: a caller that would
// exceed them should be told before a 64 KiB argv is built, not after.
const (
	maxAppendedScopeEntries = 256
	maxAppendedScopeBytes   = 64 << 10
	// A stale swap means someone else widened this task between the read and
	// the write. Re-merging against what the store handed back is the right
	// response, and bounding the attempts is what stops a permanently-contended
	// task from spinning.
	//
	// Eight rather than a smaller number because each lost race is another
	// contender that has *succeeded*: under n-way contention a writer can be
	// displaced n-1 times before its own turn comes, and a ceiling below that
	// converts ordinary queueing into a refusal. Contention this high should be
	// impossible — one lease per task, one holder, sequential calls — so the
	// headroom costs nothing in the case that actually happens.
	maxScopeAppendAttempts = 8
)

// AppendPlannedFiles widens a task's own plan with paths its lease holder
// argued for, and returns the entries actually added.
//
// This is the durable half of the override seam. A grant makes one blocked
// write accountable and then expires; on a task longer than the grant TTL the
// agent meets the same block again and re-argues it, which is a worse use of
// everyone's attention than recording the conclusion. Writing the path into the
// task's appended scope records it where the gate reads scope from — and the
// gate marks every write it authorises as widened, so the record does not
// become indistinguishable from the plan.
//
// Already-scoped paths are dropped rather than appended again: the store has no
// set semantics, and a retried write would otherwise grow the column on every
// attempt. Nothing to add is not an error — it is the answer when the path was
// already in scope.
func (c *Client) AppendPlannedFiles(ctx context.Context, taskID, token string, add []dc.PlannedFile) ([]dc.PlannedFile, error) {
	if strings.TrimSpace(taskID) == "" || strings.TrimSpace(token) == "" {
		return nil, errors.New("store: widening scope needs the task and the lease token that authorises it")
	}

	wanted := make([]dc.PlannedFile, 0, len(add))
	for _, pf := range add {
		path := strings.TrimSpace(pf.Path)
		if path == "" {
			return nil, errors.New("store: a scope entry with no path widens nothing")
		}
		change := pf.AllowedChange
		if change == "" {
			change = dc.ChangeModify
		}
		// Appended scope may only widen. A read-only entry is a restriction,
		// and a restriction an executor writes about itself is not a
		// restriction — it is a way to make the plan look stricter than the
		// permissions actually in force.
		if change == dc.ChangeReadOnly {
			return nil, fmt.Errorf("store: %q cannot be appended read-only; appended scope only ever widens", path)
		}
		wanted = append(wanted, dc.PlannedFile{Path: path, AllowedChange: change})
	}
	if len(wanted) == 0 {
		return nil, nil
	}

	// The task is read once. The plan does not move while a lease is held, and
	// the appended half is carried forward from whatever the store hands back
	// on a lost race — so a retry costs one process, not two, and the window
	// between reading a value and swapping on it is as small as this boundary
	// allows. Re-reading instead measurably lost races it did not need to.
	task, err := c.Task(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, fmt.Errorf("store: no task %q; scope cannot be widened onto a task that does not exist", taskID)
	}
	// PlannedFiles is the union, so this is the plan and the existing widening
	// in one pass. Only the appended half can move under us.
	base := make(map[string]bool, len(task.PlannedFiles))
	for _, pf := range task.PlannedFiles {
		base[pf.Path] = true
	}
	appended := task.AgentAppended
	expected := rawArray(task.AgentAppendedRaw)

	stale := 0
	for attempt := 0; attempt < maxScopeAppendAttempts; attempt++ {
		have := make(map[string]bool, len(base)+len(appended))
		for path := range base {
			have[path] = true
		}
		for _, pf := range appended {
			have[pf.Path] = true
		}

		merged := append([]dc.PlannedFile(nil), appended...)
		var added []dc.PlannedFile
		for _, pf := range wanted {
			if have[pf.Path] {
				continue
			}
			have[pf.Path] = true
			merged = append(merged, pf)
			added = append(added, pf)
		}
		if len(added) == 0 {
			return nil, nil
		}
		if len(merged) > maxAppendedScopeEntries {
			return nil, fmt.Errorf("store: task %s would hold %d appended scope entries, past the ceiling of %d; "+
				"a plan that has grown this far past itself needs a human, not another append",
				taskID, len(merged), maxAppendedScopeEntries)
		}

		body, err := json.Marshal(merged)
		if err != nil {
			return nil, fmt.Errorf("store: encoding appended scope for %s: %w", taskID, err)
		}
		if len(body) > maxAppendedScopeBytes {
			return nil, fmt.Errorf("store: appended scope for %s is %d bytes, past the ceiling of %d",
				taskID, len(body), maxAppendedScopeBytes)
		}

		out, err := c.run(ctx, "scope-append",
			"--task", taskID, "--token", token,
			"--expected", expected, "--appended", string(body))
		if err != nil {
			return nil, err
		}
		if out.Written {
			return added, nil
		}
		if out.Code != scopeStaleCode {
			return nil, fmt.Errorf("store: task %s refused the scope widening: %s (code %q)",
				taskID, out.Error, out.Code)
		}

		// Lost the race. The store hands back what it holds now, so the merge
		// is recomputed against that rather than against a second read.
		stale++
		expected = rawArray(out.CurrentAppended)
		appended = nil
		if err := json.Unmarshal([]byte(expected), &appended); err != nil {
			return nil, fmt.Errorf("store: task %s reported an unreadable appended scope after a lost race: %w", taskID, err)
		}
	}
	return nil, fmt.Errorf("store: task %s had its scope changed under this one %d times running; "+
		"giving up rather than retrying forever", taskID, stale)
}

// rawArray is a stored JSON array as text, defaulting to the empty array.
//
// The store compares this text, so it is never re-encoded on the way through:
// a value that round-tripped through a decoder and back would differ from what
// is on disk and the swap would refuse a value it should have matched.
func rawArray(raw json.RawMessage) string {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 {
		return string(trimmed)
	}
	return "[]"
}

// scopeStaleCode is the store's answer when the appended scope moved between
// the read and the swap.
const scopeStaleCode = "scope_stale"

// ReadyTasks lists tasks that are workable and unheld. The two conditions are
// answered in one query on the store side, so a caller cannot be handed a task
// that was claimed between them.
func (c *Client) ReadyTasks(ctx context.Context) ([]string, error) {
	out, err := c.run(ctx, "ready")
	if err != nil {
		return nil, err
	}
	return out.Tasks, nil
}

// Available reports whether the store is reachable and usable.
//
// It requires an answer, not merely the absence of an error: a missing binary,
// an unreadable database, or a malformed reply all mean "unknown", and unknown
// must never be reported as an empty-but-healthy store. This is the same rule
// the Rust port learned the hard way — an unbuilt store that reported itself
// available returned a confident zero.
func (c *Client) Available(ctx context.Context) error {
	out, err := c.run(ctx, "health")
	if err != nil {
		return fmt.Errorf("store unavailable: %w", err)
	}
	// A positive identity, not the absence of an error. Any program that
	// prints `{"ok":true}` satisfies "no error"; only the real store answers
	// with its own name and a schema revision this build understands.
	if out.Store != storeIdentity {
		return fmt.Errorf("store unavailable: %s identified itself as %q, not %q",
			c.Binary, out.Store, storeIdentity)
	}
	if out.SchemaVersion != SchemaVersion {
		return fmt.Errorf("store unavailable: %s speaks schema %d, this harness speaks %d — "+
			"reading a lease through the wrong column layout would produce answers that look valid",
			c.Binary, out.SchemaVersion, SchemaVersion)
	}
	return nil
}

// SchemaVersion is the lease-schema revision this harness understands. It must
// match dc_store::SCHEMA_VERSION; Available refuses to proceed when it does not.
const SchemaVersion = 1

// storeIdentity is the name the real store answers to.
const storeIdentity = "dc-store"

// maxOutput bounds what a single store invocation may print. The replies are
// small and bounded by construction — one lease, one task, a list of ids — so
// anything approaching this is a store that has gone wrong, and reading it into
// memory unbounded would turn that into an out-of-memory kill of the harness.
const maxOutput = 8 << 20

// errOversize reports that the store produced more output than any legitimate
// reply.
var errOversize = errors.New("output exceeded the maximum a store reply may be")

// cappedBuffer collects up to a limit and then records that it stopped, so an
// overrun is a reported condition rather than a silently truncated document
// that might still parse as valid JSON.
type cappedBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if remaining := b.limit - b.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			b.buf.Write(p[:remaining])
			b.overflow = true
		} else {
			b.buf.Write(p)
		}
	} else if len(p) > 0 {
		b.overflow = true
	}
	// Always report a full write: returning short would make the copier treat
	// this as an I/O failure and mask the real diagnostic.
	return len(p), nil
}

func (c *Client) run(ctx context.Context, args ...string) (*response, error) {
	if c.Binary == "" {
		return nil, errors.New("store: no dcstore binary configured")
	}
	if c.DB == "" {
		return nil, errors.New("store: no database path configured")
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	full := append([]string{"--db", c.DB}, args...)
	cmd := exec.CommandContext(ctx, c.Binary, full...)
	stdout := &cappedBuffer{limit: maxOutput}
	stderr := &cappedBuffer{limit: 64 << 10}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// Killing the process is not enough to unblock Wait. Go copies a
	// non-*os.File stdout through a pipe, and Wait waits for that copy to
	// finish — so a store that spawns a child holding the write end leaves the
	// harness blocked for as long as the *grandchild* lives, regardless of this
	// timeout. WaitDelay bounds that second wait: after the context fires and
	// the process is killed, the pipes are closed and Wait returns.
	cmd.WaitDelay = 2 * time.Second
	configureProcessGroup(cmd)

	runErr := cmd.Run()
	if stdout.overflow {
		return nil, fmt.Errorf("store: %s %w (%d bytes)", args[0], errOversize, maxOutput)
	}

	// The contract is that every outcome — including a refusal — arrives as
	// JSON on stdout, so parse before judging the exit code. An exit code with
	// no parseable payload is the genuinely broken case.
	var out response
	if decodeErr := json.Unmarshal(bytes.TrimSpace(stdout.buf.Bytes()), &out); decodeErr != nil {
		if runErr != nil {
			return nil, fmt.Errorf("store: %s failed: %w (stderr: %s)",
				args[0], runErr, bytes.TrimSpace(stderr.buf.Bytes()))
		}
		return nil, fmt.Errorf("store: %s returned unparseable output: %w (output: %q)",
			args[0], decodeErr, stdout.buf.String())
	}
	if runErr != nil && out.Error != "" {
		return nil, fmt.Errorf("store: %s: %s", args[0], out.Error)
	}
	if runErr != nil {
		return nil, fmt.Errorf("store: %s failed: %w", args[0], runErr)
	}

	// A reply that says ok:false without naming a code is a failure the store
	// did not classify. Only the commands below use ok:false as a meaningful
	// answer a caller branches on; for the rest, letting it through would hand
	// back a zero value — no task, no leases — that reads exactly like a
	// healthy empty store.
	if !out.OK && !signalsOutcomeWithOK[args[0]] {
		reason := out.Error
		if reason == "" {
			reason = "no reason given"
		}
		return nil, fmt.Errorf("store: %s reported failure: %s", args[0], reason)
	}
	return &out, nil
}

// signalsOutcomeWithOK lists the commands for which ok:false is a real answer
// rather than a fault: a task held by someone else, a token that is not valid,
// a lease that has already expired.
var signalsOutcomeWithOK = map[string]bool{
	"acquire":  true,
	"diagnose": true,
	"renew":    true,
	// A widening loses a race with another widening on the same task, or is
	// asked for by something that no longer holds the lease. Both are answers
	// the caller acts on — re-read and merge, or stop — and turning them into
	// an error here would have made the retry loop in AppendPlannedFiles
	// unreachable: contention would have surfaced as a store fault, and the
	// merge that would have resolved it would never have been attempted.
	"scope-append": true,
}
