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
	"syscall"
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
	// Close to report; further ones are counted, not kept.
	maxWarmDevmapEvictErrors = 8
)

// errDevmapPoolClosed is what a call through a handle answers once the pool is
// closed. A closed pool must not start another child, and the only client it
// could hand out then is one nothing would ever close.
var errDevmapPoolClosed = errors.New("the devmap pool is closed")

// WarmDevmapPool maintains a bounded per-root pool of warm `devmap mcp` stdio
// clients (`P0.3`) as an LRU with idle expiry.
//
// At most maxWarmDevmapPoolEntries roots hold a warm session, keyed by the
// directory's identity (rootIdent) rather than its spelling. A new root
// arriving at a full pool evicts the least recently used client that has no
// call in flight; when every client is mid-call it receives an unpooled CLI
// client instead, so a session with an RPC in flight is never closed under it.
// A client idle for warmDevmapIdleTTL, or whose root is gone, is no longer a
// directory or is now a different directory, is dropped. Every dropped client
// is Closed before the call that dropped it returns.
type WarmDevmapPool struct {
	binary string
	// now is the pool's clock and closeClient how it retires a client; both are
	// fields so a test can drive idle expiry without sleeping and count closes.
	now         func() time.Time
	closeClient func(*mapclient.Client) error

	mu      sync.Mutex
	closed  bool
	clients map[rootIdent]*PooledDevmap
	reaper  *time.Timer
	// pendingClose holds clients already removed from clients whose child has
	// not been closed yet, so a close happens outside the lock but never gets lost.
	pendingClose []*PooledDevmap
	evictErrs    []error
	evictDropped int
}

// PooledDevmap is the handle ForRoot returns: a client for one root whose every
// call is counted while it runs, which is what lets the pool tell an idle
// client from one it must not close. It satisfies DevmapClient and ScanDevmap.
type PooledDevmap struct {
	pool   *WarmDevmapPool
	root   string
	id     rootIdent
	client *mapclient.Client
	// pooled is false for the unpooled CLI client an overflowing pool or an
	// unusable root gets; such a handle owns no child and is never counted.
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
		clients:     make(map[rootIdent]*PooledDevmap),
	}
}

// resolveRoot canonicalizes root and reads its identity. usable is false when
// root is not an existing directory, and such a root is never pooled. The
// filesystem is touched here, outside p.mu, so a slow disk stalls the caller
// and never the pool.
func resolveRoot(root string) (canon string, id rootIdent, usable bool) {
	canon = root
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != "" {
		canon = resolved
	}
	info, err := os.Stat(canon)
	if err != nil || !info.IsDir() {
		return canon, rootIdent{}, false
	}
	return canon, identOf(info, canon), true
}

// staleEntries stats, without holding p.mu, every idle pooled root and returns
// those that are gone, no longer a directory, or no longer the directory the
// client was started in. dropStaleLocked confirms each under the lock.
func (p *WarmDevmapPool) staleEntries() []*PooledDevmap {
	p.mu.Lock()
	snapshot := make([]*PooledDevmap, 0, len(p.clients))
	for _, w := range p.clients {
		if w.inflight == 0 {
			snapshot = append(snapshot, w)
		}
	}
	p.mu.Unlock()
	var stale []*PooledDevmap
	for _, w := range snapshot {
		info, err := os.Stat(w.root)
		switch {
		case err == nil && info.IsDir() && identOf(info, w.root) == w.id:
		case err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR):
			// An unreadable root is not evidence it is gone.
		default:
			stale = append(stale, w)
		}
	}
	return stale
}

// dropStaleLocked evicts the entries staleEntries named that are still pooled
// and still idle, and every idle one past the TTL. The caller holds p.mu.
func (p *WarmDevmapPool) dropStaleLocked(stale []*PooledDevmap) {
	if p.closed {
		return
	}
	for _, w := range stale {
		if p.clients[w.id] == w && w.inflight == 0 {
			p.evictLocked(w)
		}
	}
	now := p.now()
	for _, w := range p.clients {
		// A clock that stepped backwards must not make a client look newer than
		// now forever.
		if w.lastUsed.After(now) {
			w.lastUsed = now
		}
		if w.inflight == 0 && now.Sub(w.lastUsed) >= warmDevmapIdleTTL {
			p.evictLocked(w)
		}
	}
}

func (p *WarmDevmapPool) evictLocked(w *PooledDevmap) {
	delete(p.clients, w.id)
	w.evicted = true
	p.pendingClose = append(p.pendingClose, w)
}

// takePendingLocked returns every client awaiting a close. The caller holds
// p.mu and passes the result to retire after unlocking.
func (p *WarmDevmapPool) takePendingLocked() []*PooledDevmap {
	dropped := p.pendingClose
	p.pendingClose = nil
	return dropped
}

// ForRoot returns a warm `devmap` client scoped to root (canonicalized when
// resolvable).
func (p *WarmDevmapPool) ForRoot(root string) *PooledDevmap {
	canon, id, usable := resolveRoot(root)
	stale := p.staleEntries()
	p.mu.Lock()
	p.dropStaleLocked(stale)
	w := p.entryLocked(canon, id, usable)
	dropped := p.takePendingLocked()
	p.armReaperLocked()
	p.mu.Unlock()
	p.retire(dropped)
	return w
}

// entryLocked is the pooled handle for the root, evicting the least recently
// used idle one to make room. The caller holds p.mu.
func (p *WarmDevmapPool) entryLocked(canon string, id rootIdent, usable bool) *PooledDevmap {
	if p.closed || !usable {
		return p.unpooled(canon)
	}
	if existing, ok := p.clients[id]; ok {
		return existing
	}
	if len(p.clients) >= maxWarmDevmapPoolEntries && !p.evictOldestIdleLocked() {
		return p.unpooled(canon)
	}
	c := mapclient.New(p.binary, canon)
	c.EnableWarmMCP()
	w := &PooledDevmap{pool: p, root: canon, id: id, client: c, pooled: true, lastUsed: p.now()}
	p.clients[id] = w
	return w
}

func (p *WarmDevmapPool) unpooled(canon string) *PooledDevmap {
	return &PooledDevmap{pool: p, root: canon, client: mapclient.New(p.binary, canon)}
}

// evictOldestIdleLocked evicts the least recently used client with no call in
// flight and reports whether it made room.
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
	p.evictLocked(oldest)
	return true
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
		} else {
			p.evictDropped++
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
	stale := p.staleEntries()
	p.mu.Lock()
	p.reaper = nil
	p.dropStaleLocked(stale)
	dropped := p.takePendingLocked()
	p.armReaperLocked()
	p.mu.Unlock()
	p.retire(dropped)
}

// lease marks one call as in flight on the client for w's root and returns it
// with the function that ends the call. A handle the pool has since evicted
// resolves to the root's current handle first, so a call never runs on a client
// that was closed, which would restart its child with nothing left to close it.
//
// When the call ends on a client the pool has since been closed under, the
// client is closed once more: a call that was in flight at Close may have made
// mapclient start a replacement child, and nothing else would stop it.
func (w *PooledDevmap) lease() (*mapclient.Client, func()) {
	if !w.pooled {
		return w.client, func() {}
	}
	p := w.pool
	p.mu.Lock()
	stale := w.evicted
	p.mu.Unlock()
	var canon string
	var id rootIdent
	var usable bool
	if stale {
		canon, id, usable = resolveRoot(w.root)
	}
	idle := p.staleEntries()
	p.mu.Lock()
	p.dropStaleLocked(idle)
	target := w
	if w.evicted || p.clients[w.id] != w {
		if !stale {
			canon, id, usable = w.root, w.id, true
		}
		target = p.entryLocked(canon, id, usable)
	}
	if !target.pooled {
		dropped := p.takePendingLocked()
		p.mu.Unlock()
		p.retire(dropped)
		return target.client, func() {}
	}
	target.inflight++
	target.lastUsed = p.now()
	dropped := p.takePendingLocked()
	p.armReaperLocked()
	p.mu.Unlock()
	p.retire(dropped)
	return target.client, func() {
		p.mu.Lock()
		target.inflight--
		target.lastUsed = p.now()
		reclose := target.evicted && target.inflight == 0 && p.closed
		p.mu.Unlock()
		if reclose {
			// Idempotent, and deliberately not p.closeClient: the first close
			// was already counted, and this one only stops a restarted child.
			_ = target.client.Close()
		}
	}
}

func (w *PooledDevmap) open() (*mapclient.Client, func(), error) {
	w.pool.mu.Lock()
	closed := w.pool.closed
	w.pool.mu.Unlock()
	if closed {
		return nil, nil, errDevmapPoolClosed
	}
	c, done := w.lease()
	return c, done, nil
}

// Status is the DevmapClient and ScanDevmap status call, counted as in flight.
func (w *PooledDevmap) Status(ctx context.Context) (*mapclient.Status, error) {
	c, done, err := w.open()
	if err != nil {
		return nil, err
	}
	defer done()
	return c.Status(ctx)
}

// Dead is the ScanDevmap dead-symbol call, counted as in flight.
func (w *PooledDevmap) Dead(ctx context.Context) (mapclient.Result[mapclient.DeadSymbol], error) {
	c, done, err := w.open()
	if err != nil {
		return mapclient.Result[mapclient.DeadSymbol]{}, err
	}
	defer done()
	return c.Dead(ctx)
}

// Advanced is the DevmapClient and ScanDevmap advanced query, counted as in flight.
func (w *PooledDevmap) Advanced(ctx context.Context, q mapclient.AdvancedQuery) (mapclient.AdvancedResult, error) {
	c, done, err := w.open()
	if err != nil {
		return mapclient.AdvancedResult{}, err
	}
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
	toClose := p.takePendingLocked()
	for _, w := range p.clients {
		w.evicted = true
		toClose = append(toClose, w)
	}
	p.clients = nil
	joined := errors.Join(p.evictErrs...)
	if p.evictDropped > 0 {
		joined = errors.Join(joined, fmt.Errorf("and %d more devmap close errors", p.evictDropped))
	}
	p.evictErrs, p.evictDropped = nil, 0
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
