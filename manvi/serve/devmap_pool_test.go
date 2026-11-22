package serve

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	mapclient "github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/devmap"
)

// poolFixture is a pool on a clock the test advances, whose closes are counted
// by root instead of reaching a child process.
type poolFixture struct {
	pool   *WarmDevmapPool
	mu     sync.Mutex
	clock  time.Time
	closed map[string]int
}

func newPoolFixture(t *testing.T) *poolFixture {
	t.Helper()
	f := &poolFixture{clock: time.Unix(1_700_000_000, 0), closed: map[string]int{}}
	f.pool = NewWarmDevmapPool("devmap-nonexistent-binary")
	f.pool.now = func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.clock
	}
	f.pool.closeClient = func(c *mapclient.Client) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.closed[c.Root]++
		return nil
	}
	t.Cleanup(func() { _ = f.pool.Close() })
	return f
}

func (f *poolFixture) advance(d time.Duration) {
	f.mu.Lock()
	f.clock = f.clock.Add(d)
	f.mu.Unlock()
}

func (f *poolFixture) closes(root string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed[root]
}

func (f *poolFixture) size() int {
	f.pool.mu.Lock()
	defer f.pool.mu.Unlock()
	return len(f.pool.clients)
}

// The 17th root is warm once the least recently used idle client is evicted,
// and the evicted client is closed.
func TestWarmDevmapPoolEvictsTheLeastRecentlyUsedIdleClient(t *testing.T) {
	f := newPoolFixture(t)
	roots := make([]string, maxWarmDevmapPoolEntries)
	for i := range roots {
		roots[i] = resolvedDir(t)
		f.pool.ForRoot(roots[i])
		f.advance(time.Second)
	}
	// Touch the first root so the second is now the oldest.
	_, touched := f.pool.ForRoot(roots[0]).lease()
	touched()
	f.advance(time.Second)

	seventeenth := resolvedDir(t)
	w := f.pool.ForRoot(seventeenth)
	if !w.pooled {
		t.Fatal("the 17th root got an unpooled client although an idle one could be evicted")
	}
	if f.pool.ForRoot(seventeenth) != w {
		t.Fatal("the 17th root is not warm: a repeat lookup built a new client")
	}
	if got := f.size(); got != maxWarmDevmapPoolEntries {
		t.Fatalf("pool holds %d clients, want the bound %d", got, maxWarmDevmapPoolEntries)
	}
	if f.closes(roots[1]) != 1 {
		t.Fatalf("the evicted client was closed %d times, want 1", f.closes(roots[1]))
	}
	for _, r := range append([]string{roots[0]}, roots[2:]...) {
		if f.closes(r) != 0 {
			t.Fatalf("root %s was closed but was not the oldest idle client", r)
		}
	}
}

// A client with a call in flight is never evicted, however old it is.
func TestWarmDevmapPoolNeverEvictsAClientMidCall(t *testing.T) {
	f := newPoolFixture(t)
	busyRoot := resolvedDir(t)
	busy := f.pool.ForRoot(busyRoot)
	_, done := busy.lease() // the oldest client, with a call running
	f.advance(time.Second)
	for i := 1; i < maxWarmDevmapPoolEntries; i++ {
		f.pool.ForRoot(resolvedDir(t))
		f.advance(time.Second)
	}
	for i := 0; i < maxWarmDevmapPoolEntries; i++ {
		f.pool.ForRoot(resolvedDir(t))
		f.advance(time.Second)
	}
	if f.closes(busyRoot) != 0 {
		t.Fatal("a client mid-call was closed")
	}
	if f.pool.ForRoot(busyRoot) != busy {
		t.Fatal("a client mid-call was evicted")
	}
	done()
}

// When every client is mid-call the pool stays bounded and the newcomer gets an
// unpooled client rather than closing one.
func TestWarmDevmapPoolFullOfBusyClientsHandsOutAnUnpooledOne(t *testing.T) {
	f := newPoolFixture(t)
	var dones []func()
	for i := 0; i < maxWarmDevmapPoolEntries; i++ {
		_, done := f.pool.ForRoot(resolvedDir(t)).lease()
		dones = append(dones, done)
	}
	extra := resolvedDir(t)
	over1, over2 := f.pool.ForRoot(extra), f.pool.ForRoot(extra)
	if over1.pooled || over1 == over2 {
		t.Fatal("a full pool of busy clients pooled a 17th")
	}
	if f.size() != maxWarmDevmapPoolEntries {
		t.Fatalf("pool size %d, want %d", f.size(), maxWarmDevmapPoolEntries)
	}
	for _, done := range dones {
		done()
	}
}

// A client idle past the TTL is closed; one used since is kept.
func TestWarmDevmapPoolClosesAnIdleClientAfterTheTTL(t *testing.T) {
	f := newPoolFixture(t)
	stale, fresh := resolvedDir(t), resolvedDir(t)
	f.pool.ForRoot(stale)
	f.advance(warmDevmapIdleTTL / 2)
	f.pool.ForRoot(fresh)
	f.advance(warmDevmapIdleTTL/2 + time.Millisecond)

	other := resolvedDir(t)
	f.pool.ForRoot(other) // any lookup sweeps
	if f.closes(stale) != 1 {
		t.Fatalf("the idle client was closed %d times, want 1", f.closes(stale))
	}
	if f.closes(fresh) != 0 {
		t.Fatal("a client used inside the TTL was closed")
	}
}

// A root that was deleted is dropped and its client closed.
func TestWarmDevmapPoolDropsARootThatNoLongerExists(t *testing.T) {
	f := newPoolFixture(t)
	gone, kept := resolvedDir(t), resolvedDir(t)
	f.pool.ForRoot(gone)
	f.pool.ForRoot(kept)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	f.pool.ForRoot(kept)
	if f.closes(gone) != 1 {
		t.Fatalf("the deleted root's client was closed %d times, want 1", f.closes(gone))
	}
	if f.size() != 1 {
		t.Fatalf("pool holds %d clients after the root vanished, want 1", f.size())
	}
	if f.closes(kept) != 0 {
		t.Fatal("a live root was dropped")
	}
}

// A handle that outlived its eviction does not run on the closed client: the
// call resolves to the root's current client, which the pool still owns.
func TestWarmDevmapPoolCallOnAnEvictedHandleUsesTheCurrentClient(t *testing.T) {
	f := newPoolFixture(t)
	root := resolvedDir(t)
	old := f.pool.ForRoot(root)
	f.advance(warmDevmapIdleTTL + time.Second)
	f.pool.ForRoot(resolvedDir(t)) // sweeps old away
	if f.closes(root) != 1 {
		t.Fatal("setup: the stale client was not closed")
	}
	_, _ = old.Status(context.Background()) // fails fast: no such binary
	f.pool.mu.Lock()
	current := f.pool.clients[root]
	inflight := 0
	if current != nil {
		inflight = current.inflight
	}
	f.pool.mu.Unlock()
	if current == nil || current == old {
		t.Fatal("the call did not resolve to a fresh pooled client")
	}
	if inflight != 0 {
		t.Fatalf("the call left %d in flight", inflight)
	}
	if err := f.pool.Close(); err != nil {
		t.Fatal(err)
	}
	if f.closes(root) != 2 {
		t.Fatalf("Close closed the root's clients %d times in total, want 2 (evicted + current)", f.closes(root))
	}
}
