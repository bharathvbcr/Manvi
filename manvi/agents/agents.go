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
	"sort"
	"strings"
	"sync"
	"time"

	"manvi/flags"
)

// Errors callers branch on.
var (
	ErrDepthExceeded  = errors.New("agents: spawn depth limit reached")
	ErrFanoutExceeded = errors.New("agents: concurrent fan-out limit reached")
	ErrClosed         = errors.New("agents: the pool is closed")
)

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
	Duration       time.Duration
}

// Pool runs bounded fan-out.
type Pool struct {
	// MaxDepth bounds delegation depth. A pool at depth d spawns children at
	// depth d+1, and a child at MaxDepth cannot spawn at all.
	MaxDepth int
	// MaxFanout bounds how many children run at once.
	MaxFanout int
	// Depth is this pool's own position in the tree.
	Depth int
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

// Child returns a pool for the next level down, or an error when the depth
// limit is reached. The limit is checked here, at the point of descent, rather
// than trusted to the caller.
func (p *Pool) Child() (*Pool, error) {
	if p.Depth >= p.MaxDepth {
		return nil, fmt.Errorf("%w: depth %d of %d", ErrDepthExceeded, p.Depth, p.MaxDepth)
	}
	child := *p
	child.Depth = p.Depth + 1
	return &child, nil
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

	results := make([]Result, len(tasks))
	var wg sync.WaitGroup
	for i, task := range tasks {
		wg.Add(1)
		go func(i int, task Task) {
			defer wg.Done()
			results[i] = p.runOne(ctx, task)
		}(i, task)
	}
	wg.Wait()
	return results, nil
}

func (p *Pool) runOne(ctx context.Context, task Task) Result {
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
	Children int      `json:"children"`
	Failed   int      `json:"failed"`
	Released int      `json:"released_leases"`
	Orphaned []string `json:"orphaned_leases,omitempty"`
}

// Clean reports whether every child finished and every lease came back. An
// orphaned lease keeps a task locked until its TTL expires, so a fan-out with
// one is not a clean fan-out however well the children did.
func (r Report) Clean() bool { return r.Failed == 0 && len(r.Orphaned) == 0 }

// Summarise builds the report.
func Summarise(results []Result) Report {
	rep := Report{Children: len(results)}
	for _, r := range results {
		if r.Err != nil {
			rep.Failed++
		}
		rep.Released += len(r.ReleasedLeases)
		rep.Orphaned = append(rep.Orphaned, r.OrphanedLeases...)
	}
	sort.Strings(rep.Orphaned)
	return rep
}
