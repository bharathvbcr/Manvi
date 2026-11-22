package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	mapclient "github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/devmap"
)

const (
	// maxWarmDevmapPoolEntries bounds how many `devmap mcp` children the pool
	// keeps warm at once.
	maxWarmDevmapPoolEntries = 16
	// warmDevmapIdleTTL is how long a warm client may go without a call before
	// the pool closes its child. The pool is otherwise only bounded by count, so
	// a long-lived sidecar that saw a root once kept its process for good.
	warmDevmapIdleTTL = 10 * time.Minute
	// maxWarmDevmapEvictErrors bounds the close errors the pool remembers for
	// Close to report.
	maxWarmDevmapEvictErrors = 8
)

// WarmDevmapPool maintains a bounded per-root pool of warm `devmap mcp` stdio
// clients (`P0.3`) as an LRU with idle expiry.
//
// At most maxWarmDevmapPoolEntries canonical roots hold a warm session. A new
// root arriving at a full pool evicts the least recently used client that has
// no call in flight; when every client is mid-call it receives an unpooled CLI
// client instead, so a session with an RPC in flight is never closed under it.
// A client idle for warmDevmapIdleTTL, or whose root no longer exists, is
// dropped. Every dropped client is Closed, so no `devmap` child is left behind.
type WarmDevmapPool struct {
	binary string
	// now is the pool's clock and closeClient how it retires a client; both are
	// fields so a test can drive idle expiry without sleeping and count closes.
	now         func() time.Time
	closeClient func(*mapclient.Client) error

	mu      sync.Mutex
	closed  bool
	clients map[string]*PooledDevmap
	reaper  *time.Timer
	// pendingClose holds clients already removed from clients whose child has
	// not been closed yet, so a close happens outside the lock but never gets lost.
	pendingClose []*PooledDevmap
	evictErrs    []error
}

// PooledDevmap is the handle ForRoot returns: a client for one root whose every
// call is counted while it runs, which is what lets the pool tell an idle
// client from one it must not close. It satisfies DevmapClient and ScanDevmap.
type PooledDevmap struct {
	pool   *WarmDevmapPool
	root   string
	client *mapclient.Client
	// pooled is false for the unpooled CLI client an overflowing or closed
	// pool hands out; such a handle owns no child and is never counted.
	pooled bool

	// The fields below are guarded by pool.mu.
	inflight int
	lastUsed time.Time
	evicted  bool
}

// NewWarmDevmapPool builds a bounded warm `devmap mcp` client pool for binary.
func NewWarmDevmapPool(binary string) *WarmDevmapPool {
	return &WarmDevmapPool{
		binary:      binary,
		now:         time.Now,
		closeClient: (*mapclient.Client).Close,
		clients:     make(map[string]*PooledDevmap),
	}
}

// ForRoot returns a warm `devmap` client scoped to root (canonicalized when
// resolvable).
func (p *WarmDevmapPool) ForRoot(root string) *PooledDevmap {
	canon := root
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != "" {
		canon = resolved
	}
	p.mu.Lock()
	dropped := p.sweepLocked()
	w := p.entryLocked(canon)
	p.armReaperLocked()
	p.mu.Unlock()
	p.retire(dropped)
	return w
}

// entryLocked is the pooled handle for canon, evicting the least recently used
// idle one to make room. The caller holds p.mu.
func (p *WarmDevmapPool) entryLocked(canon string) *PooledDevmap {
	if p.closed {
		return p.unpooled(canon)
	}
	if existing, ok := p.clients[canon]; ok {
		return existing
	}
	if len(p.clients) >= maxWarmDevmapPoolEntries && !p.evictOldestIdleLocked() {
		return p.unpooled(canon)
	}
	c := mapclient.New(p.binary, canon)
	c.EnableWarmMCP()
	w := &PooledDevmap{pool: p, root: canon, client: c, pooled: true, lastUsed: p.now()}
	p.clients[canon] = w
	return w
}

func (p *WarmDevmapPool) unpooled(canon string) *PooledDevmap {
	return &PooledDevmap{pool: p, root: canon, client: mapclient.New(p.binary, canon)}
}

// evictOldestIdleLocked removes the least recently used client with no call in
// flight and reports whether it made room. The evicted client is closed by the
// caller of retire, so it is parked in pendingClose until then.
func (p *WarmDevmapPool) evictOldestIdleLocked() bool {
	var oldest *PooledDevmap
	for _, w := range p.clients {
		if w.inflight == 0 && (oldest == nil || w.lastUsed.Before(oldest.lastUsed)) {
			oldest = w
		}
	}
	if oldest == nil {
		return false
	}
	delete(p.clients, oldest.root)
	oldest.evicted = true
	p.pendingClose = append(p.pendingClose, oldest)
	return true
}

// sweepLocked drops every idle client that has expired or whose root is gone
// and returns everything awaiting a close. The caller holds p.mu and passes the
// result to retire after unlocking.
func (p *WarmDevmapPool) sweepLocked() []*PooledDevmap {
	if !p.closed {
		now := p.now()
		for root, w := range p.clients {
			if w.inflight > 0 {
				continue
			}
			if now.Sub(w.lastUsed) >= warmDevmapIdleTTL || rootGone(root) {
				delete(p.clients, root)
				w.evicted = true
				p.pendingClose = append(p.pendingClose, w)
			}
		}
	}
	dropped := p.pendingClose
	p.pendingClose = nil
	return dropped
}

// rootGone reports that root is known not to exist. An error other than
// not-exist (a permissions failure, say) is not evidence the root is gone.
func rootGone(root string) bool {
	_, err := os.Stat(root)
	return errors.Is(err, fs.ErrNotExist)
}

// retire closes dropped clients outside the lock, since closing waits on a
// child process. A failure is kept, bounded, for Close to report.
func (p *WarmDevmapPool) retire(dropped []*PooledDevmap) {
	var errs []error
	for _, w := range dropped {
		if err := p.closeClient(w.client); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return
	}
	p.mu.Lock()
	for _, err := range errs {
		if len(p.evictErrs) < maxWarmDevmapEvictErrors {
			p.evictErrs = append(p.evictErrs, err)
		}
	}
	p.mu.Unlock()
}

// armReaperLocked schedules the next idle sweep while any client is pooled, so
// an expired client is closed even when no further request arrives.
func (p *WarmDevmapPool) armReaperLocked() {
	if p.reaper != nil || p.closed || len(p.clients) == 0 {
		return
	}
	p.reaper = time.AfterFunc(warmDevmapIdleTTL, p.reap)
}

func (p *WarmDevmapPool) reap() {
	p.mu.Lock()
	p.reaper = nil
	dropped := p.sweepLocked()
	p.armReaperLocked()
	p.mu.Unlock()
	p.retire(dropped)
}

// lease marks one call as in flight on the client for w's root and returns it
// with the function that ends the call. A handle the pool has since evicted
// resolves to the root's current handle first, so a call never runs on a client
// that was closed, which would restart its child with nothing left to close it.
func (w *PooledDevmap) lease() (*mapclient.Client, func()) {
	if !w.pooled {
		return w.client, func() {}
	}
	p := w.pool
	p.mu.Lock()
	dropped := p.sweepLocked()
	target := w
	if w.evicted || p.clients[w.root] != w {
		target = p.entryLocked(w.root)
	}
	if !target.pooled {
		p.mu.Unlock()
		p.retire(dropped)
		return target.client, func() {}
	}
	target.inflight++
	target.lastUsed = p.now()
	p.armReaperLocked()
	p.mu.Unlock()
	p.retire(dropped)
	return target.client, func() {
		p.mu.Lock()
		target.inflight--
		target.lastUsed = p.now()
		p.mu.Unlock()
	}
}

// Status is the DevmapClient and ScanDevmap status call, counted as in flight.
func (w *PooledDevmap) Status(ctx context.Context) (*mapclient.Status, error) {
	c, done := w.lease()
	defer done()
	return c.Status(ctx)
}

// Dead is the ScanDevmap dead-symbol call, counted as in flight.
func (w *PooledDevmap) Dead(ctx context.Context) (mapclient.Result[mapclient.DeadSymbol], error) {
	c, done := w.lease()
	defer done()
	return c.Dead(ctx)
}

// Advanced is the DevmapClient and ScanDevmap advanced query, counted as in flight.
func (w *PooledDevmap) Advanced(ctx context.Context, q mapclient.AdvancedQuery) (mapclient.AdvancedResult, error) {
	c, done := w.lease()
	defer done()
	return c.Advanced(ctx, q)
}

// Close terminates every pooled warm `devmap mcp` subprocess and reports any
// close failure the pool kept while evicting.
func (p *WarmDevmapPool) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	if p.reaper != nil {
		p.reaper.Stop()
		p.reaper = nil
	}
	toClose := p.pendingClose
	p.pendingClose = nil
	for _, w := range p.clients {
		w.evicted = true
		toClose = append(toClose, w)
	}
	p.clients = nil
	p.mu.Unlock()

	p.mu.Lock()
	joined := errors.Join(p.evictErrs...)
	p.evictErrs = nil
	p.mu.Unlock()
	for _, w := range toClose {
		joined = errors.Join(joined, p.closeClient(w.client))
	}
	return joined
}

// DevmapClient is the narrow code-intelligence service a host module needs.
// A host can supply the stock subprocess client or its own in-process adapter.
type DevmapClient interface {
	Status(context.Context) (*mapclient.Status, error)
	Advanced(context.Context, mapclient.AdvancedQuery) (mapclient.AdvancedResult, error)
}

// DevmapModule exposes deep code-intelligence queries on the host plane. Every
// request names the repository it is about; ForRoot builds the client for that
// validated root. There is deliberately no client bound at startup: a sidecar's
// working directory is wherever its host launched it, and an answer about that
// tree would be indistinguishable from an answer about the right one.
type DevmapModule struct {
	ForRoot func(root string) DevmapClient
}

func (m DevmapModule) Configure(r *Router) error {
	if m.ForRoot == nil {
		return errors.New("devmap module has no client source")
	}
	if err := r.Register(OpDevmapStatus, m.status); err != nil {
		return err
	}
	return r.Register(OpDevmapQuery, m.query)
}

// client validates the request's root and returns the client bound to it.
func (m DevmapModule) client(ctx context.Context, op, raw string) (DevmapClient, string, *Error) {
	root, e := canonicalRepoRoot(ctx, "root", raw)
	if e != nil {
		return nil, "", e
	}
	client := m.ForRoot(root)
	if nilInterface(client) {
		return nil, "", dependencyError(op, errors.New("no client for this root"))
	}
	return client, root, nil
}

type devmapStatusParams struct {
	Root string `json:"root"`
}

func (m DevmapModule) status(ctx context.Context, raw json.RawMessage) (any, *Error) {
	var p devmapStatusParams
	if e := scanStrict(raw, &p); e != nil {
		return nil, badRequest("devmap.status params: %v", e.Message)
	}
	client, _, e := m.client(ctx, "devmap status", p.Root)
	if e != nil {
		return nil, e
	}
	status, err := client.Status(ctx)
	if err != nil {
		return nil, dependencyError("devmap status", err)
	}
	if status == nil {
		return nil, dependencyError("devmap status", errors.New("adapter returned no status"))
	}
	return status, nil
}

type devmapQueryParams struct {
	Root          string              `json:"root"`
	Kind          mapclient.QueryKind `json:"kind"`
	Query         string              `json:"query,omitempty"`
	To            string              `json:"to,omitempty"`
	Targets       []string            `json:"targets,omitempty"`
	Depth         int                 `json:"depth"`
	Budget        int                 `json:"budget,omitempty"`
	MinConfidence float64             `json:"min_confidence,omitempty"`
	MinRung       string              `json:"min_rung,omitempty"`
}

func (m DevmapModule) query(ctx context.Context, raw json.RawMessage) (any, *Error) {
	var p devmapQueryParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, badRequest("devmap.query params: %v", err)
	}
	client, root, e := m.client(ctx, "devmap query", p.Root)
	if e != nil {
		return nil, e
	}
	query := mapclient.AdvancedQuery{
		Kind: p.Kind, Query: p.Query, To: p.To, Targets: p.Targets,
		Depth: p.Depth, Budget: p.Budget, MinConfidence: p.MinConfidence, MinRung: p.MinRung,
	}
	if err := mapclient.ValidateAdvancedQuery(query); err != nil {
		return nil, badRequest("devmap.query params: %v", err)
	}
	result, err := client.Advanced(ctx, query)
	if err != nil {
		return nil, dependencyError("devmap query", err)
	}
	if err := mapclient.ValidateAdvancedResult(query.Kind, result); err != nil {
		return nil, dependencyError("devmap query", err)
	}
	if err := envelopeRootMatches(result.Data, root); err != nil {
		return nil, dependencyError("devmap query", err)
	}
	return result, nil
}

// envelopeRootMatches refuses an answer whose envelope names a different
// repository than the one asked about. An envelope that names none is not
// refused: not every query kind carries repository.root. One whose repository
// field is present but unreadable is refused, because "could not read the
// root" must not pass as "no root to check".
func envelopeRootMatches(data json.RawMessage, root string) error {
	var env struct {
		Repository *struct {
			Root string `json:"root"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("the answer's repository field cannot be read: %w", err)
	}
	if env.Repository == nil || env.Repository.Root == "" {
		return nil
	}
	got := env.Repository.Root
	if resolved, err := filepath.EvalSymlinks(got); err == nil {
		got = resolved
	}
	if got != root {
		return fmt.Errorf("the answer is about %s, not the requested root %s", env.Repository.Root, root)
	}
	return nil
}

func dependencyError(operation string, err error) *Error {
	retryable := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
	return &Error{Code: ErrDependency, Message: fmt.Sprintf("%s unavailable: %v", operation, err), Retryable: retryable}
}
