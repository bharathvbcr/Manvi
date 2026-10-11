// Package agents is the fan-out seam: bounded concurrent sub-agents, and the
// cleanup that has to happen when they are cut short.
//
// The bounds are in code rather than in a prompt. A depth limit expressed as an
// instruction is a limit the model can decline to follow, and a runaway spawn
// tree is not a quality problem — it is a bill and a set of leases nobody holds
// on purpose.
//
// The part worth stating plainly is cancellation. A child agent that has
// checked a task out holds a lease in the store, and cancelling its context
// stops its work without touching that row. The task then stays locked until
// its TTL expires, which is minutes during which the orchestrator's own next
// attempt is refused by its own abandoned lease. So cancellation here means two
// things — stop the work, and release what it held — and the release runs on a
// *fresh* context, because the cancelled one cannot perform the call that
// undoes the cancellation's damage.
package agents

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
)

// Errors callers branch on.
var (
	ErrFanoutExceeded = errors.New("agents: concurrent fan-out limit reached")
	ErrClosed         = errors.New("agents: the pool is closed")
	// ErrBudget refuses a whole fan-out whose children's first requests cannot
	// all be reserved against the spend cap. Nothing has started when it is
	// returned.
	ErrBudget = errors.New("agents: the fan-out does not fit the spend cap")
	// ErrNotStarted is the error a child that was never started carries, so a
	// caller reading only Err never mistakes it for a success. Result.NotStarted
	// says why.
	ErrNotStarted = errors.New("agents: the child was not started")
)

// NotStartedBudget is Result.NotStarted for a child held back because the
// spend cap could no longer cover it.
const NotStartedBudget = "budget"

// NotStartedCancelled is Result.NotStarted for a child held back because the
// dispatch was cancelled while it waited for the spend cap.
const NotStartedCancelled = "cancelled"

// Budget is the spend cap a fan-out's children draw on, as the pool sees it.
//
// An interface rather than the ledger itself, so the package that bounds work
// does not take on the provider layer that prices it.
type Budget interface {
	// Headroom is what may still be reserved against the cap. An error means
	// the cap could not be read, and the pool then starts nothing rather than
	// guessing that there is room.
	Headroom(ctx context.Context) (int64, error)
	// Exhausted reports whether err is the cap refusing a reservation.
	Exhausted(err error) bool
	// Render shows an amount the way the cap's own messages do.
	Render(amount int64) string
}

// Bounds are the delegation limits one dispatch runs under.
type Bounds struct {
	// MaxDepth is how deep delegation may go; zero means not at all.
	MaxDepth int
	// MaxFanout is how many children may run at once.
	MaxFanout int
	// DepthReason explains a MaxDepth this harness imposed rather than read
	// from the setting, so a refusal can name what actually decided it.
	//
	// Without it the refusal cites agents.max_spawn_depth and its value, which
	// is a lie whenever the operator set that flag to two and something else
	// overrode it — and "the setting says 2 but the tool says the setting is 0"
	// is the kind of contradiction that costs an afternoon.
	DepthReason string
}

// ResolveBounds reads the delegation bounds from the flag registry, narrowed
// for the provider that will serve the children.
//
// It exists because this was two copies. The devcouncil handlers for
// spawn_subagents and invoke_subagent each carried the same block verbatim, and
// both hardcoded a no-registry fallback of four against a catalogue default of
// eight — one rule, two implementations, already disagreeing about its own
// value. The fallbacks here are the catalogue's own constants, so there is
// nothing left for a copy to drift from.
//
// A nil registry means the bounds could not be read at all, which is why the
// fallback is the catalogue default rather than something wider: a dispatch
// whose limits could not be resolved must not run looser than one whose limits
// were. The ceiling itself is enforced where the value enters the registry —
// see flags.Def.Max — so a value that reaches here has already been judged.
func ResolveBounds(reg *flags.Registry) Bounds {
	b := Bounds{
		MaxDepth:  flags.DefaultAgentsMaxSpawnDepth,
		MaxFanout: flags.DefaultAgentsMaxFanout,
	}
	if reg == nil {
		return b
	}
	if depth, _, err := reg.Int(flags.AgentsMaxSpawnDepth); err == nil && depth >= 0 {
		b.MaxDepth = depth
	}
	if fanout, _, err := reg.Int(flags.AgentsMaxFanout); err == nil && fanout >= 1 {
		b.MaxFanout = fanout
	}
	if provider, _, err := reg.String(flags.LLMDefaultProvider); err == nil {
		b.MaxFanout = AdaptiveFanoutLimit(provider, b.MaxFanout)
		// A local session delegates nothing. Not "narrowly" — at all.
		//
		// The fan-out cap above is the wrong instrument for this and always
		// was: its floor is one, so the tightest it can express is "one child
		// at a time", and one child at a time is still a team. On a single
		// consumer GPU a team is the worst available shape — the children
		// contend for the same weights the parent is using, each one pays a
		// full prefill of its own prompt, and the wall clock is the sum rather
		// than the maximum. The measurement that motivated the fan-out cap
		// (VRAM exhaustion, Metal command-buffer panics, prefill starvation)
		// is the same measurement that says the right number here is zero.
		//
		// It overrides the setting rather than yielding to it, and that is
		// deliberate: this is a bound on the hardware, and the operator's
		// escape is to point the session at a provider that has the headroom,
		// not to raise a number until the machine falls over. DepthReason
		// carries the "why" so the refusal never blames the flag.
		if provider == localProviderName {
			b.MaxDepth = 0
			b.DepthReason = "the default provider is " + localProviderName +
				", and a local session runs solo: children would contend with the parent for the " +
				"same weights and the same memory"
		}
	}
	return b
}

// FanoutFor narrows the dispatch bounds to where the children will actually
// run.
//
// ResolveBounds answers from the session's default provider, which is the right
// answer for a fan-out whose children inherit it and the wrong one for a mixed
// team. A frontier parent placing builders on a local model resolved to the
// frontier width — measured at eight — and dispatched all eight onto the single
// GPU the local cap exists to protect. The cap and the placement never met: one
// is decided here from a setting, the other in the runner from a role's model
// spec, and nothing carried the second back to the first.
//
// The rule is deliberately blunt: if any child will run locally, the whole
// fan-out takes the local width. The pool enforces one concurrency limit across
// its tasks, so a per-provider answer would need a per-provider pool, and
// splitting the pool to let six frontier children run wide beside two local ones
// is a large change to buy back parallelism on the plane that has least of it.
// Over-narrowing costs wall clock; under-narrowing costs the machine.
//
// It is never silent. The returned reason is empty when nothing was narrowed and
// names the cause when something was — an operator who set max_fanout to eight
// and watched two children run needs to know which of their settings did not
// decide it.
//
// placements are the model specs the children will run under, in the form
// Definition.Model uses.
func FanoutFor(b Bounds, placements []string) (int, string) {
	for _, spec := range placements {
		if !PlacesLocally(spec) {
			continue
		}
		narrowed := AdaptiveFanoutLimit(localProviderName, b.MaxFanout)
		if narrowed >= b.MaxFanout {
			return b.MaxFanout, ""
		}
		return narrowed, fmt.Sprintf(
			"%d of %d concurrent children: at least one runs on %s, and children there contend "+
				"with each other and with the parent for one device's memory and prefill queue",
			narrowed, b.MaxFanout, localProviderName)
	}
	return b.MaxFanout, ""
}

// PlacesLocally reports whether a model spec puts a child on the local plane.
//
// It answers from the spec alone rather than through ParsePlacement, because
// the caller that needs it — a dispatch handler — has no provider registry to
// resolve bare names against, and threading one there to answer a yes/no
// question would put a second reader of the registry in a package that has
// managed without it.
//
// "inherit" and the empty spec are not local *here*: they take the session's
// provider, and ResolveBounds has already narrowed for that. Saying otherwise
// would double-count the ordinary case.
func PlacesLocally(spec string) bool {
	spec = strings.TrimSpace(spec)
	if provider, _, ok := strings.Cut(spec, "/"); ok {
		return strings.EqualFold(strings.TrimSpace(provider), localProviderName)
	}
	return strings.EqualFold(spec, localProviderName)
}

// localProviderName is the provider whose sessions run solo.
//
// Named by provider rather than inferred from the context window, because the
// property that matters is where the weights live. A local model with a 128k
// window is still one GPU being asked to serve a parent and its children at
// once; a hosted model with a small window is not.
const localProviderName = "local"

// AdaptiveFanoutLimit returns safe concurrency bounds for subagents based on the provider.
// For local LLMs running on single consumer GPUs or Apple Silicon unified memory,
// bounding concurrent generation streams to 2 prevents VRAM exhaustion, Metal command
// buffer panics, and prefill queue starvation.
func AdaptiveFanoutLimit(providerName string, defaultFanout int) int {
	if providerName == "local" {
		if defaultFanout > 2 {
			return 2
		}
		if defaultFanout < 1 {
			return 1
		}
		return defaultFanout
	}
	if defaultFanout < 1 {
		return 1
	}
	return defaultFanout
}

// Lease is what a child holds and what must be given back.
type Lease struct {
	TaskID string
	// Token is the lease credential. It is carried here and never rendered:
	// a released lease is logged by task, not by token.
	Token string
}

// Releaser gives a lease back to the store.
type Releaser interface {
	Release(ctx context.Context, taskID, token string) (bool, error)
}

// Task is one unit of sub-agent work.
type Task struct {
	// Label names the work for the run report.
	Label string
	// Reserve is what this child's first request is expected to reserve
	// against Pool.Budget. Zero means the cap does not meter the provider the
	// child runs on, and the child is admitted without asking it.
	Reserve int64
	// Run performs the work. It receives a context that is cancelled when the
	// parent is, and a Holder it must register any lease with so cleanup can
	// find it.
	Run func(ctx context.Context, h *Holder) (any, error)
}

// Holder records the leases one child has taken.
//
// A child registers a lease as soon as it acquires one, before it does any
// work with it. Registering afterwards leaves a window in which a cancellation
// finds nothing to release and the lease outlives the agent that took it.
type Holder struct {
	mu     sync.Mutex
	leases []Lease
}

// Add records a lease this child now holds.
func (h *Holder) Add(lease Lease) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.leases = append(h.leases, lease)
}

// Drop forgets a lease the child released itself, so cleanup does not try again.
func (h *Holder) Drop(taskID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	kept := h.leases[:0]
	for _, l := range h.leases {
		if l.TaskID != taskID {
			kept = append(kept, l)
		}
	}
	h.leases = kept
}

// Held returns the leases still outstanding.
func (h *Holder) Held() []Lease {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Lease(nil), h.leases...)
}

// Result is one child's outcome.
type Result struct {
	Label string `json:"label"`
	Value any    `json:"-"`
	Err   error  `json:"-"`
	// Error is the rendered error, for the report.
	Error string `json:"error,omitempty"`
	// ReleasedLeases names tasks whose leases were handed back during cleanup.
	ReleasedLeases []string `json:"released_leases,omitempty"`
	// OrphanedLeases names tasks whose leases could not be handed back. This is
	// the field that must never be quietly empty: an orphan holds a task locked
	// until its TTL runs out, and an operator needs to know which.
	OrphanedLeases []string `json:"orphaned_leases,omitempty"`
	// NotStarted, when set, says why the child was never started; Err then
	// wraps ErrNotStarted. Such a child ran nothing, wrote nothing and held
	// nothing, and is counted apart from the children that started and failed.
	NotStarted string `json:"not_started,omitempty"`
	Duration   time.Duration
}

// Pool runs bounded fan-out.
type Pool struct {
	// MaxDepth is the delegation depth this pool was configured with. The
	// bound itself is structural: a child's registry holds no dispatching
	// tools, so there is no grandchild for a counter to refuse.
	MaxDepth int
	// MaxFanout bounds how many children run at once.
	MaxFanout int
	// Releaser hands leases back. Required: a pool without one cannot clean up
	// after a cancellation, and constructing one is refused rather than
	// discovering it at the moment it matters.
	Releaser Releaser
	// ReleaseTimeout bounds the cleanup call. Cleanup runs on a fresh context
	// precisely because the parent's is cancelled, so it needs its own bound or
	// a wedged store would hang the shutdown it is meant to complete.
	ReleaseTimeout time.Duration
	// Clock is injected for tests.
	Now func() time.Time
	// Budget, when set, is the spend cap the children's requests reserve
	// against; see Run for how it admits them. Nil means no cap.
	Budget Budget
}

// New builds a pool.
func New(maxDepth, maxFanout int, releaser Releaser) (*Pool, error) {
	if releaser == nil {
		return nil, errors.New("agents: a pool needs a releaser; without one a cancelled child leaves its task locked until the TTL expires")
	}
	if maxDepth < 0 || maxFanout < 1 {
		return nil, fmt.Errorf("agents: bounds must be depth >= 0 and fanout >= 1, got %d/%d", maxDepth, maxFanout)
	}
	return &Pool{
		MaxDepth: maxDepth, MaxFanout: maxFanout,
		Releaser: releaser, ReleaseTimeout: 10 * time.Second,
	}, nil
}

func (p *Pool) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Run executes tasks concurrently, bounded by MaxFanout, and returns one result
// per task in the order given.
//
// Every child's leases are released when it finishes, whether it succeeded,
// failed, or was cancelled. That is the whole point: the failure modes that
// strand a lease are exactly the ones where nobody is left to release it.
//
// With a Budget, admission happens in two steps and never exceeds the cap.
//
// Before anything starts, each child's first-request reservation is checked
// against the cap's headroom, and a fan-out holding a child that cannot fit
// even alone is refused whole with ErrBudget and the arithmetic. Then children
// are started in order, each only while the reservations of the children
// already running, plus its own, fit the headroom read when the fan-out began
// and the headroom read now. A wide fan-out is therefore narrowed, not
// refused: reservations are a worst case per request, and the sum of every
// child's worst case is far more than the sum of what they spend, so insisting
// that all of them fit at once refused fan-outs that cost a few percent of the
// cap. A child that has to wait starts when a running sibling finishes and its
// reservation settles to what it really spent. Once a started child's request
// has been refused by the cap, or nothing is running and the next child still
// does not fit, no further child is started, and each one held back is
// reported with NotStarted rather than run.
//
// The second check exists because the first is a snapshot. Every session on
// the machine draws on the same ledger, and without it a fan-out admitted
// against yesterday's headroom started every child anyway and let the cap
// refuse them one request at a time — part-way through their turns, after
// some had already written to the tree.
func (p *Pool) Run(ctx context.Context, tasks []Task) ([]Result, error) {
	if len(tasks) == 0 {
		return nil, nil
	}
	if len(tasks) > p.MaxFanout {
		// Refused rather than silently trimmed. Running the first N of a
		// caller's list and reporting success would answer a question the
		// caller did not ask, with no sign that anything was left out.
		return nil, fmt.Errorf("%w: %d tasks requested, limit is %d",
			ErrFanoutExceeded, len(tasks), p.MaxFanout)
	}
	capacity, err := p.admitAll(ctx, tasks)
	if err != nil {
		return nil, err
	}

	results := make([]Result, len(tasks))
	// exhausted is set by a started child whose request the cap refused, and
	// read before each later start.
	var exhausted atomic.Bool
	// held is what the running children's first requests may reserve, and
	// running how many there are; both are guarded by mu. finished carries one
	// token per child that has returned and given its share back.
	var (
		mu       sync.Mutex
		held     int64
		running  int
		finished = make(chan struct{}, len(tasks))
	)
	var wg sync.WaitGroup
	for i, task := range tasks {
		var kind, why string
		for {
			mu.Lock()
			heldNow, runningNow := held, running
			mu.Unlock()
			var wait bool
			why, wait = p.admitOne(ctx, task, capacity-heldNow, &exhausted)
			if why == "" {
				break
			}
			kind = NotStartedBudget
			if !wait || runningNow == 0 {
				break
			}
			select {
			case <-finished:
				continue
			case <-ctx.Done():
				kind, why = NotStartedCancelled, "the dispatch was cancelled while it waited for the spend cap"
			}
			break
		}
		if why != "" {
			// One held back means every later one is too. Starting a cheaper
			// child after refusing a dearer one would make which children ran
			// depend on their prices rather than on the order they were asked
			// for in.
			results[i] = notStarted(task.Label, kind, why)
			for j := i + 1; j < len(tasks); j++ {
				results[j] = notStarted(tasks[j].Label, kind,
					fmt.Sprintf("%s, started before it, was held back", task.Label))
			}
			break
		}
		mu.Lock()
		held += task.Reserve
		running++
		mu.Unlock()
		wg.Add(1)
		go func(i int, task Task) {
			defer wg.Done()
			results[i] = p.runOne(ctx, task, func(err error) {
				if p.Budget != nil && p.Budget.Exhausted(err) {
					exhausted.Store(true)
				}
			})
			mu.Lock()
			held -= task.Reserve
			running--
			mu.Unlock()
			finished <- struct{}{}
		}(i, task)
	}
	wg.Wait()
	return results, nil
}

// admitAll refuses a fan-out holding a child whose first request cannot be
// reserved against the cap's current headroom even with nothing else running,
// and returns that headroom, which bounds what the running children may hold
// together. Zero when no child is metered.
func (p *Pool) admitAll(ctx context.Context, tasks []Task) (int64, error) {
	if p.Budget == nil {
		return 0, nil
	}
	var total, dearest int64
	var dearestLabel string
	metered := 0
	for _, task := range tasks {
		if task.Reserve < 0 {
			return 0, fmt.Errorf("agents: %s has a negative reservation %d", task.Label, task.Reserve)
		}
		if task.Reserve == 0 {
			continue
		}
		if total > math.MaxInt64-task.Reserve {
			return 0, fmt.Errorf("%w: the children's reservations overflow", ErrBudget)
		}
		total += task.Reserve
		metered++
		if task.Reserve > dearest {
			dearest, dearestLabel = task.Reserve, task.Label
		}
	}
	if metered == 0 {
		return 0, nil
	}
	headroom, err := p.Budget.Headroom(ctx)
	if err != nil {
		return 0, fmt.Errorf("%w: the spend cap could not be read, so nothing was started: %w", ErrBudget, err)
	}
	if dearest > headroom {
		return 0, fmt.Errorf("%w: %s may reserve up to %s for its first request, and %s is left under the cap "+
			"(%d metered children, %s if all ran at once); nothing was started",
			ErrBudget, dearestLabel, p.Budget.Render(dearest), p.Budget.Render(headroom),
			metered, p.Budget.Render(total))
	}
	return headroom, nil
}

// admitOne decides whether the next child may start now, and returns why not
// when it may not. wait is true when the refusal can lift as running children
// finish: the child fits the cap on its own, but not beside the children
// already running.
//
// room is what the fan-out's own accounting leaves: the headroom read when it
// began, less what the running children may reserve. The headroom read now is
// the cap's own answer, which also shows spend by other sessions; the child
// starts only if both have room for it.
func (p *Pool) admitOne(ctx context.Context, task Task, room int64, exhausted *atomic.Bool) (why string, wait bool) {
	if p.Budget == nil {
		return "", false
	}
	var headroom int64
	var err error
	if task.Reserve > 0 {
		headroom, err = p.Budget.Headroom(ctx)
	}
	// Read after the headroom, so a sibling that finished while it was being
	// read is seen.
	if exhausted.Load() {
		return "a sibling's request was refused by the spend cap", false
	}
	switch {
	case task.Reserve == 0:
		return "", false
	case err != nil:
		return "the spend cap could not be read: " + err.Error(), false
	case task.Reserve > headroom:
		return fmt.Sprintf("its first request may reserve up to %s, and %s is left under the cap",
			p.Budget.Render(task.Reserve), p.Budget.Render(headroom)), true
	case task.Reserve > room:
		return fmt.Sprintf("its first request may reserve up to %s, and the children already running may "+
			"reserve all but %s of the cap", p.Budget.Render(task.Reserve), p.Budget.Render(max(room, 0))), true
	}
	return "", false
}

func notStarted(label, kind, why string) Result {
	err := fmt.Errorf("%w: %s", ErrNotStarted, why)
	return Result{
		Label:      label,
		Err:        err,
		Error:      "not started (" + kind + "): " + why,
		NotStarted: kind,
	}
}

// runOne runs one child and releases what it held. failed, when the child
// returns an error, is told before that release: handing leases back can wait
// on the store, and a sibling's admission should not wait with it.
func (p *Pool) runOne(ctx context.Context, task Task, failed func(error)) Result {
	started := p.now()
	holder := &Holder{}
	result := Result{Label: task.Label}

	func() {
		// A panicking child must not take the pool down with it, and must not
		// skip the lease cleanup below. Its leases are exactly as stranded as a
		// cancelled child's.
		defer func() {
			if r := recover(); r != nil {
				result.Err = fmt.Errorf("agents: %s panicked: %v", task.Label, r)
			}
		}()
		result.Value, result.Err = task.Run(ctx, holder)
	}()
	if result.Err != nil {
		failed(result.Err)
	}

	released, orphaned := p.releaseAll(holder)
	result.ReleasedLeases = released
	result.OrphanedLeases = orphaned
	result.Duration = p.now().Sub(started)
	if result.Err != nil {
		result.Error = result.Err.Error()
	}
	return result
}

// releaseAll hands back every lease a child still holds.
//
// The context is created here rather than derived from the caller's. The case
// this exists for is a cancelled parent, and a context derived from a cancelled
// one is already dead — the release would fail immediately and the lease would
// be stranded by the very code written to prevent it.
func (p *Pool) releaseAll(holder *Holder) (released, orphaned []string) {
	leases := holder.Held()
	if len(leases) == 0 {
		return nil, nil
	}
	timeout := p.ReleaseTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), timeout)
	defer cancel()

	for _, lease := range leases {
		ok, err := p.Releaser.Release(ctx, lease.TaskID, lease.Token)
		if err != nil || !ok {
			orphaned = append(orphaned, lease.TaskID)
			continue
		}
		released = append(released, lease.TaskID)
		holder.Drop(lease.TaskID)
	}
	sort.Strings(released)
	sort.Strings(orphaned)
	return released, orphaned
}

// Report summarises a fan-out for the evidence trail.
type Report struct {
	Children int `json:"children"`
	// Failed counts children that started and did not succeed.
	Failed int `json:"failed"`
	// NotStarted counts children that were never started. They are not
	// failures — no work was attempted — and not successes either.
	NotStarted int      `json:"not_started,omitempty"`
	Released   int      `json:"released_leases"`
	Orphaned   []string `json:"orphaned_leases,omitempty"`
}

// Clean reports whether every child finished and every lease came back. An
// orphaned lease keeps a task locked until its TTL expires, so a fan-out with
// one is not a clean fan-out however well the children did; nor is one that
// left children unstarted.
func (r Report) Clean() bool { return r.Failed == 0 && r.NotStarted == 0 && len(r.Orphaned) == 0 }

// Summarise builds the report.
func Summarise(results []Result) Report {
	rep := Report{Children: len(results)}
	for _, r := range results {
		switch {
		case r.NotStarted != "":
			rep.NotStarted++
		case r.Err != nil:
			rep.Failed++
		}
		rep.Released += len(r.ReleasedLeases)
		rep.Orphaned = append(rep.Orphaned, r.OrphanedLeases...)
	}
	sort.Strings(rep.Orphaned)
	return rep
}
