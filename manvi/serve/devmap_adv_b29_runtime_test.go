package serve

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mapclient "github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/devmap"
)

// advClose records every close by client pointer, and notices a close that
// lands under a call that is still running.
type advClose struct {
	mu         sync.Mutex
	closed     map[*mapclient.Client]int
	active     map[*mapclient.Client]int
	violations []string
}

func newAdvClose() *advClose {
	return &advClose{closed: map[*mapclient.Client]int{}, active: map[*mapclient.Client]int{}}
}

func (a *advClose) close(c *mapclient.Client) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed[c]++
	if a.closed[c] > 1 {
		a.violations = append(a.violations, fmt.Sprintf("client for %s closed %d times", c.Root, a.closed[c]))
	}
	if a.active[c] > 0 {
		a.violations = append(a.violations, fmt.Sprintf("client for %s closed with %d calls in flight", c.Root, a.active[c]))
	}
	return nil
}

func (a *advClose) enter(c *mapclient.Client) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.active[c]++
	if a.closed[c] > 0 {
		a.violations = append(a.violations, fmt.Sprintf("call started on already-closed client for %s", c.Root))
	}
}

func (a *advClose) leave(c *mapclient.Client) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.active[c]--
}

func (a *advClose) count(c *mapclient.Client) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closed[c]
}

// advClock is an injectable clock that tests move in either direction.
type advClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *advClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *advClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func advPool(t *testing.T) (*WarmDevmapPool, *advClose, *advClock) {
	t.Helper()
	rec, clk := newAdvClose(), &advClock{t: time.Unix(1_700_000_000, 0)}
	p := NewWarmDevmapPool("devmap-nonexistent-binary")
	p.now = clk.now
	p.closeClient = rec.close
	t.Cleanup(func() { _ = p.Close() })
	return p, rec, clk
}

func closesByRoot(rec *advClose, root string) int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	n := 0
	for c, k := range rec.closed {
		if c.Root == root {
			n += k
		}
	}
	return n
}

// --- 2. concurrency -------------------------------------------------------

func TestAdvB29RuntimePoolHammerNeverClosesUnderACallOrExceedsTheBound(t *testing.T) {
	p, rec, clk := advPool(t)
	const nRoots, nGoroutines, iters = 40, 64, 150
	roots := make([]string, nRoots)
	for i := range roots {
		roots[i] = resolvedDir(t)
	}
	var seenMu sync.Mutex
	seen := map[*mapclient.Client]bool{}
	var maxSize, maxPending atomic.Int64
	bump := func(m *atomic.Int64, v int64) {
		for {
			cur := m.Load()
			if v <= cur || m.CompareAndSwap(cur, v) {
				return
			}
		}
	}
	stop := make(chan struct{})
	var bg sync.WaitGroup
	bg.Add(1)
	go func() { // moves time forward so the TTL path races the LRU path, and fires the reaper body
		defer bg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				clk.add(90 * time.Second)
				p.reap()
				time.Sleep(time.Millisecond)
			}
		}
	}()
	var wg sync.WaitGroup
	for g := 0; g < nGoroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				h := p.ForRoot(roots[(g*7+i*13)%nRoots])
				if h.pooled {
					seenMu.Lock()
					seen[h.client] = true
					seenMu.Unlock()
				}
				c, done := h.lease()
				rec.enter(c)
				if i%5 == 0 {
					runtime.Gosched()
				}
				p.mu.Lock()
				bump(&maxSize, int64(len(p.clients)))
				bump(&maxPending, int64(len(p.pendingClose)))
				p.mu.Unlock()
				rec.leave(c)
				done()
			}
		}(g)
	}
	wg.Wait()
	close(stop)
	bg.Wait()
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, v := range rec.violations {
		t.Error(v)
	}
	if maxSize.Load() > maxWarmDevmapPoolEntries {
		t.Errorf("pool reached %d entries, bound is %d", maxSize.Load(), maxWarmDevmapPoolEntries)
	}
	for c := range seen {
		if rec.closed[c] != 1 {
			t.Errorf("pooled client for %s closed %d times after Close, want exactly 1", c.Root, rec.closed[c])
		}
	}
	t.Logf("max pool size %d, max pendingClose %d", maxSize.Load(), maxPending.Load())
}

func TestAdvB29RuntimeCloseWhileCallsAreInFlight(t *testing.T) {
	p, rec, _ := advPool(t)
	type held struct {
		h    *PooledDevmap
		c    *mapclient.Client
		done func()
	}
	var hs []held
	for i := 0; i < maxWarmDevmapPoolEntries; i++ {
		h := p.ForRoot(resolvedDir(t))
		c, done := h.lease()
		rec.enter(c)
		hs = append(hs, held{h, c, done})
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	for _, x := range hs {
		if n := rec.count(x.c); n != 1 {
			t.Errorf("in-flight client for %s closed %d times, want 1", x.c.Root, n)
		}
		rec.leave(x.c)
		x.done() // must not panic on a closed pool
	}
	p.mu.Lock()
	if len(p.clients) != 0 || len(p.pendingClose) != 0 {
		t.Errorf("pool still holds %d clients and %d pending closes after Close", len(p.clients), len(p.pendingClose))
	}
	p.mu.Unlock()
	// A handle used after Close must not resurrect a pooled entry.
	for _, x := range hs {
		c, done := x.h.lease()
		done()
		if c == x.c {
			t.Errorf("a handle used after Close ran on the client Close already closed (%s)", c.Root)
		}
		if n := rec.count(x.c); n != 1 {
			t.Errorf("client for %s closed %d times after a post-Close call, want 1", x.c.Root, n)
		}
	}
	if h := p.ForRoot(resolvedDir(t)); h.pooled {
		t.Error("ForRoot after Close handed out a pooled client")
	}
	p.mu.Lock()
	if len(p.clients) != 0 || p.reaper != nil {
		t.Errorf("post-Close use re-populated the pool (%d) or re-armed the reaper", len(p.clients))
	}
	p.mu.Unlock()
	// Status through a handle after Close must still answer (fails fast: no binary).
	_, _ = hs[0].h.Status(context.Background())
}

func TestAdvB29RuntimeReaperAndCloseRace(t *testing.T) {
	for iter := 0; iter < 200; iter++ {
		p, rec, clk := advPool(t)
		roots := []string{resolvedDir(t), resolvedDir(t), resolvedDir(t)}
		hs := make([]*PooledDevmap, len(roots))
		for i, r := range roots {
			hs[i] = p.ForRoot(r)
		}
		clk.add(warmDevmapIdleTTL + time.Second)
		var wg sync.WaitGroup
		wg.Add(4)
		go func() { defer wg.Done(); _ = p.Close() }()
		go func() { defer wg.Done(); p.reap() }()
		go func() { defer wg.Done(); _ = p.Close() }()
		go func() { defer wg.Done(); _, done := hs[0].lease(); done() }()
		wg.Wait()
		rec.mu.Lock()
		for _, v := range rec.violations {
			t.Fatalf("iteration %d: %s", iter, v)
		}
		rec.mu.Unlock()
		p.reap() // a timer firing after Close
		p.mu.Lock()
		if p.reaper != nil || len(p.clients) != 0 {
			t.Fatalf("iteration %d: reaper armed or clients present after Close", iter)
		}
		p.mu.Unlock()
	}
}

func TestAdvB29RuntimeNoGoroutineLeakAfterClose(t *testing.T) {
	settle := func() int {
		var n int
		for i := 0; i < 50; i++ {
			runtime.GC()
			time.Sleep(20 * time.Millisecond)
			n = runtime.NumGoroutine()
		}
		return n
	}
	base := settle()
	p := NewWarmDevmapPool("devmap-nonexistent-binary")
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				h := p.ForRoot(filepath.Join(os.TempDir(), fmt.Sprintf("advb29-no-such-%d", (g+i)%24)))
				_, _ = h.Status(context.Background())
			}
		}(g)
	}
	wg.Wait()
	_ = p.Close()
	_ = p.Close()
	if after := settle(); after > base+2 {
		buf := make([]byte, 1<<16)
		buf = buf[:runtime.Stack(buf, true)]
		t.Fatalf("goroutines before %d, after Close %d\n%s", base, after, buf)
	}
}

// --- 2b. a real child: Close while a call is mid-handshake -----------------

func TestAdvB29RuntimeCloseDuringRealCallLeavesNoChildBehind(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell script as the devmap binary")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "starts.log")
	script := filepath.Join(dir, "devmap")
	body := "#!/bin/sh\necho \"$$ $*\" >> '" + logPath + "'\ncase \" $* \" in *\" mcp \"*) exec sleep 30;; esac\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { //nolint:gosec // test fixture script must be executable
		t.Fatal(err)
	}
	root := resolvedDir(t)
	p := NewWarmDevmapPool(script)
	h := p.ForRoot(root)
	errc := make(chan error, 1)
	go func() { _, err := h.Status(context.Background()); errc <- err }()
	deadline := time.Now().Add(20 * time.Second)
	var pids []string
	for time.Now().Before(deadline) {
		raw, _ := os.ReadFile(logPath)
		if strings.Contains(string(raw)+" ", " mcp ") || strings.HasSuffix(strings.TrimSpace(string(raw)), " mcp") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = p.Close()
	select {
	case <-errc:
	case <-time.After(40 * time.Second):
		t.Error("the in-flight call never returned after Close")
	}
	time.Sleep(500 * time.Millisecond)
	raw, _ := os.ReadFile(logPath)
	mcpStarts := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.Fields(line)
		if len(f) < 1 {
			continue
		}
		pids = append(pids, f[0])
		if strings.Contains(" "+line+" ", " mcp ") {
			mcpStarts++
		}
	}
	alive := []string{}
	for _, pid := range pids {
		var n int
		_, _ = fmt.Sscanf(pid, "%d", &n)
		if n > 1 && syscallAlive(n) {
			alive = append(alive, pid)
		}
	}
	for _, pid := range alive { // only PIDs this test started
		var n int
		_, _ = fmt.Sscanf(pid, "%d", &n)
		syscallKill(n)
	}
	if mcpStarts != 1 {
		t.Errorf("devmap mcp was started %d times for one call and one Close, want 1\n%s", mcpStarts, raw)
	}
	if len(alive) > 0 {
		t.Errorf("children still alive after Close and the call's return: %v\n%s", alive, raw)
	}
}

// --- 3. bounds --------------------------------------------------------------

func TestAdvB29RuntimeEveryClientBusyNewcomersStayUnpooledAndBounded(t *testing.T) {
	p, rec, _ := advPool(t)
	var dones []func()
	busy := map[string]bool{}
	for i := 0; i < maxWarmDevmapPoolEntries; i++ {
		r := resolvedDir(t)
		busy[r] = true
		_, done := p.ForRoot(r).lease()
		dones = append(dones, done)
	}
	var overflow []string
	for i := 0; i < 100; i++ {
		r := resolvedDir(t)
		overflow = append(overflow, r)
		h := p.ForRoot(r)
		if h.pooled {
			t.Fatalf("newcomer %d was pooled although every client is mid-call", i)
		}
		_, done := h.lease()
		done()
	}
	p.mu.Lock()
	if n := len(p.clients); n != maxWarmDevmapPoolEntries {
		t.Errorf("pool size %d, want %d", n, maxWarmDevmapPoolEntries)
	}
	p.mu.Unlock()
	for _, r := range overflow {
		if n := closesByRoot(rec, r); n != 0 {
			t.Errorf("unpooled client for %s was passed to Close %d times; it owns no child so this signals a pooled client was created and dropped", r, n)
		}
	}
	for _, done := range dones {
		done()
	}
	// With the busy clients idle again the pool must recover and pool a newcomer.
	if h := p.ForRoot(resolvedDir(t)); !h.pooled {
		t.Error("the pool did not recover once its clients went idle")
	}
}

func TestAdvB29RuntimeEachDropPathClosesExactlyOnce(t *testing.T) {
	t.Run("ttl", func(t *testing.T) {
		p, rec, clk := advPool(t)
		r := resolvedDir(t)
		p.ForRoot(r)
		clk.add(warmDevmapIdleTTL)
		p.ForRoot(resolvedDir(t))
		p.ForRoot(resolvedDir(t))
		_ = p.Close()
		_ = p.Close()
		if n := closesByRoot(rec, r); n != 1 {
			t.Fatalf("closed %d times", n)
		}
	})
	t.Run("deleted root", func(t *testing.T) {
		p, rec, _ := advPool(t)
		r := resolvedDir(t)
		p.ForRoot(r)
		_ = os.RemoveAll(r)
		p.ForRoot(resolvedDir(t))
		p.ForRoot(resolvedDir(t))
		_ = p.Close()
		if n := closesByRoot(rec, r); n != 1 {
			t.Fatalf("closed %d times", n)
		}
	})
	t.Run("lru eviction then immediate Close", func(t *testing.T) {
		p, rec, clk := advPool(t)
		var first string
		for i := 0; i < maxWarmDevmapPoolEntries; i++ {
			r := resolvedDir(t)
			if i == 0 {
				first = r
			}
			p.ForRoot(r)
			clk.add(time.Second)
		}
		p.ForRoot(resolvedDir(t)) // evicts first; close is parked
		_ = p.Close()
		if n := closesByRoot(rec, first); n != 1 {
			t.Fatalf("evicted-then-Closed client closed %d times, want 1", n)
		}
	})
}

// An evicted client's child must be gone when ForRoot returns, not whenever the
// next lookup or the reaper happens to run: until then the pool's live children
// exceed its stated bound.
func TestAdvB29RuntimeEvictionClosesTheChildBeforeForRootReturns(t *testing.T) {
	p, rec, clk := advPool(t)
	roots := make([]string, maxWarmDevmapPoolEntries)
	for i := range roots {
		roots[i] = resolvedDir(t)
		p.ForRoot(roots[i])
		clk.add(time.Second)
	}
	p.ForRoot(resolvedDir(t))
	if n := closesByRoot(rec, roots[0]); n != 1 {
		p.mu.Lock()
		pending := len(p.pendingClose)
		p.mu.Unlock()
		t.Fatalf("after the evicting ForRoot returned the evicted client was closed %d times (pendingClose=%d); its child is still running", n, pending)
	}
}

// Same, through lease: a stale handle that re-resolves into a full pool evicts
// another client and must not leave that one parked.
func TestAdvB29RuntimeLeaseEvictionClosesTheChildBeforeLeaseReturns(t *testing.T) {
	p, _, clk := advPool(t)
	roots := make([]string, maxWarmDevmapPoolEntries)
	var h0 *PooledDevmap
	for i := range roots {
		roots[i] = resolvedDir(t)
		h := p.ForRoot(roots[i])
		if i == 0 {
			h0 = h
		}
		clk.add(time.Second)
	}
	p.ForRoot(resolvedDir(t)) // evicts roots[0]
	p.ForRoot(resolvedDir(t)) // evicts roots[1]; this lookup's sweep closes roots[0]
	_, done := h0.lease()     // stale: re-resolves, evicting another idle client
	done()
	p.mu.Lock()
	pending := len(p.pendingClose)
	p.mu.Unlock()
	if pending != 0 {
		t.Fatalf("lease returned with %d evicted clients still awaiting close", pending)
	}
}

// --- 4. root identity -------------------------------------------------------

func TestAdvB29RuntimeSymlinkedRootAndTargetShareOneEntry(t *testing.T) {
	p, _, _ := advPool(t)
	target := resolvedDir(t)
	link := filepath.Join(resolvedDir(t), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	if p.ForRoot(link) != p.ForRoot(target) {
		t.Fatal("a symlinked root and its target got two pool entries")
	}
	other := resolvedDir(t)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if p.ForRoot(link) == p.ForRoot(target) {
		t.Fatal("a retargeted symlink still resolves to the old target's client")
	}
}

func TestAdvB29RuntimeRootDeletedAndRecreatedGetsAFreshClient(t *testing.T) {
	p, rec, _ := advPool(t)
	root := resolvedDir(t)
	before, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	old := p.ForRoot(root)
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Skip("the filesystem reused the inode; identity cannot be told apart")
	}
	got := p.ForRoot(root)
	if got == old {
		t.Fatalf("the warm child started in the deleted directory (old inode) still serves the recreated root; closes=%d", closesByRoot(rec, root))
	}
}

func TestAdvB29RuntimeRootReplacedByAFileDropsItsClient(t *testing.T) {
	p, rec, _ := advPool(t)
	root := resolvedDir(t)
	old := p.ForRoot(root)
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.ForRoot(resolvedDir(t)) // any lookup sweeps
	if n := closesByRoot(rec, root); n != 1 {
		t.Fatalf("a root that is now a regular file kept its warm client (closes=%d)", n)
	}
	if again := p.ForRoot(root); again == old && again.pooled {
		t.Fatal("a file path is still served by the pooled client for the old directory")
	}
}

func TestAdvB29RuntimeCaseVariantOfARootIsTheSameEntry(t *testing.T) {
	p, _, _ := advPool(t)
	parent := resolvedDir(t)
	mixed := filepath.Join(parent, "MixedCaseRoot")
	if err := os.Mkdir(mixed, 0o700); err != nil {
		t.Fatal(err)
	}
	lower := filepath.Join(parent, "mixedcaseroot")
	if _, err := os.Stat(lower); err != nil {
		t.Skip("case-sensitive filesystem")
	}
	if p.ForRoot(mixed) != p.ForRoot(lower) {
		p.mu.Lock()
		n := len(p.clients)
		p.mu.Unlock()
		t.Fatalf("two spellings of one directory got %d warm clients (two children)", n)
	}
}

// --- 5. time ------------------------------------------------------------------

func TestAdvB29RuntimeTTLBoundary(t *testing.T) {
	p, rec, clk := advPool(t)
	edge, justInside := resolvedDir(t), resolvedDir(t)
	p.ForRoot(edge)
	p.ForRoot(justInside)
	clk.add(warmDevmapIdleTTL - time.Nanosecond)
	p.ForRoot(resolvedDir(t))
	if closesByRoot(rec, edge) != 0 || closesByRoot(rec, justInside) != 0 {
		t.Fatal("a client was closed one nanosecond inside the TTL")
	}
	clk.add(time.Nanosecond) // idle exactly TTL
	p.ForRoot(resolvedDir(t))
	if closesByRoot(rec, edge) != 1 {
		t.Fatalf("a client idle exactly the TTL was closed %d times, want 1", closesByRoot(rec, edge))
	}
}

func TestAdvB29RuntimeProductionClockIsMonotonic(t *testing.T) {
	p := NewWarmDevmapPool("devmap-nonexistent-binary")
	defer p.Close()
	if !strings.Contains(p.now().String(), "m=") {
		t.Fatal("the pool's clock has no monotonic reading: a wall-clock step would corrupt idle expiry")
	}
}

// Latent: only reachable if the clock loses its monotonic reading (see above).
func TestAdvB29RuntimeClockStepBackwardsStillExpiresIdleClients(t *testing.T) {
	p, rec, clk := advPool(t)
	old := resolvedDir(t)
	p.ForRoot(old)
	clk.add(-time.Hour)
	p.ForRoot(resolvedDir(t))
	clk.add(warmDevmapIdleTTL + time.Second) // TTL+1s of real time since old was last used
	p.ForRoot(resolvedDir(t))
	if closesByRoot(rec, old) != 1 {
		t.Fatalf("a client idle for more than the TTL since a backwards clock step survives (closes=%d)", closesByRoot(rec, old))
	}
}

func TestAdvB29RuntimeSweepWithTheLockContended(t *testing.T) {
	p, _, clk := advPool(t)
	roots := []string{resolvedDir(t), resolvedDir(t)}
	p.ForRoot(roots[0])
	clk.add(warmDevmapIdleTTL + time.Second)
	p.mu.Lock()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h := p.ForRoot(roots[i%2])
			_, done := h.lease()
			done()
		}(i)
	}
	time.Sleep(100 * time.Millisecond)
	p.mu.Unlock()
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("lookups deadlocked behind a contended sweep")
	}
}

// fakeMCPDevmap writes a shell script that speaks just enough MCP for a warm
// session: it completes the handshake and answers tools/call with an error,
// except that the first tools/call it ever receives is held open when hang is
// set. Every start is logged as "<pid> <args>".
func fakeMCPDevmap(t *testing.T, hang bool) (script, logPath, calledPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell script as the devmap binary")
	}
	dir := t.TempDir()
	logPath, calledPath = filepath.Join(dir, "starts.log"), filepath.Join(dir, "called")
	script = filepath.Join(dir, "devmap")
	hangBranch := ""
	if hang {
		hangBranch = "if [ ! -e '" + calledPath + "' ]; then : > '" + calledPath + "'; sleep 60; fi\n"
	}
	body := "#!/bin/sh\n" +
		"echo \"$$ $*\" >> '" + logPath + "'\n" +
		"case \" $* \" in *\" mcp \"*) ;; *) exit 1;; esac\n" +
		"while IFS= read -r line; do\n" +
		"  id=$(printf '%s' \"$line\" | sed -n 's/.*\"id\":\\([0-9][0-9]*\\).*/\\1/p')\n" +
		"  case \"$line\" in\n" +
		"    *'\"method\":\"initialize\"'*) printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"protocolVersion\":\"2025-11-25\",\"capabilities\":{},\"serverInfo\":{\"name\":\"f\",\"version\":\"1\"}}}\\n' \"$id\";;\n" +
		"    *'\"method\":\"tools/call\"'*)\n" + hangBranch +
		"      printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"error\":{\"code\":-32000,\"message\":\"fake\"}}\\n' \"$id\";;\n" +
		"  esac\n" +
		"done\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { //nolint:gosec // test fixture script must be executable
		t.Fatal(err)
	}
	return script, logPath, calledPath
}

// liveMCPChildren returns, and then kills (only PIDs this test's script logged),
// every logged "mcp" child that is still alive.
func liveMCPChildren(logPath string) []int {
	raw, _ := os.ReadFile(logPath)
	var alive []int
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if !strings.Contains(" "+line+" ", " mcp ") {
			continue
		}
		var pid int
		if _, err := fmt.Sscanf(line, "%d", &pid); err == nil && pid > 1 && syscallAlive(pid) {
			alive = append(alive, pid)
		}
	}
	for _, pid := range alive {
		syscallKill(pid)
	}
	return alive
}

// A call that was leased before Close but reaches the client after it runs on a
// client Close already closed, and mapclient restarts its child: nothing is
// left to close it.
func TestAdvB29RuntimeCallReachingAClientAfterPoolCloseLeaksNoChild(t *testing.T) {
	script, logPath, _ := fakeMCPDevmap(t, false)
	p := NewWarmDevmapPool(script)
	h := p.ForRoot(resolvedDir(t))
	c, done := h.lease() // the call is in flight as far as the pool knows
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	_, _ = c.Status(context.Background()) // the call arrives late
	done()
	time.Sleep(300 * time.Millisecond)
	if alive := liveMCPChildren(logPath); len(alive) > 0 {
		t.Fatalf("a devmap mcp child survived pool Close: pids %v", alive)
	}
}

// Close kills the session under a call in flight; mapclient treats a dead
// session as retryable and starts a fresh child for the same call.
func TestAdvB29RuntimeCloseDuringAnInFlightWarmCallLeaksNoChild(t *testing.T) {
	script, logPath, calledPath := fakeMCPDevmap(t, true)
	p := NewWarmDevmapPool(script)
	h := p.ForRoot(resolvedDir(t))
	errc := make(chan error, 1)
	go func() { _, err := h.Status(context.Background()); errc <- err }()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(calledPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(calledPath); err != nil {
		t.Fatal("the fake server never received the call")
	}
	_ = p.Close()
	select {
	case <-errc:
	case <-time.After(45 * time.Second):
		t.Error("the call never returned after Close")
	}
	time.Sleep(300 * time.Millisecond)
	if alive := liveMCPChildren(logPath); len(alive) > 0 {
		raw, _ := os.ReadFile(logPath)
		t.Fatalf("a devmap mcp child survived pool Close and the call's return: pids %v\n%s", alive, raw)
	}
}
