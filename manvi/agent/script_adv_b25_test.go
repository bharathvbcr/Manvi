package agent

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// Adversarial tests for the devcouncil_script heap-growth bound (b25-scriptmem).

const advHeapBound = 64 << 20

func advIsHeapBreach(text string) bool {
	return strings.Contains(text, "heap grew by more than")
}

func advNotOtherBreach(text string) bool {
	return !strings.Contains(text, "interpreter steps") && !strings.Contains(text, "wall-time") && !strings.Contains(text, "script error")
}

// advRunShape runs script with the bound at 64 MiB and returns the result text.
func advRunShape(t *testing.T, script string, maxCalls int) (string, time.Duration) {
	t.Helper()
	shortHeapBound(t, advHeapBound, 5*time.Millisecond)
	h := scriptHarness(t, script, maxCalls)
	start := time.Now()
	_, text := runScriptHarness(t, h)
	return text, time.Since(start)
}

func TestAdvB25ScriptAllocationShapes(t *testing.T) {
	shapes := []struct{ name, script string }{
		{"list extend", "l = []\nchunk = [0] * (1 << 20)\nfor i in range(200):\n    l.extend(chunk)\nprint('after')"},
		{"list repeat copies", "keep = []\nfor i in range(200):\n    keep.append([0] * (1 << 20))\nprint('after')"},
		{"dict growth with list values", "d = {}\nfor i in range(100000):\n    d[i] = [0] * 1000\nprint('after')"},
		{"dict growth with string keys", "d = {}\npad = 'k' * 6000\nfor i in range(900000):\n    d[pad + str(i)] = i\nprint('after')"},
		{"string join", "keep = []\nparts = ['y' * (1 << 20)] * 8\nfor i in range(200):\n    keep.append(''.join(parts))\nprint('after')"},
		{"json encode nested", "keep = []\ninner = [1234567] * 100000\nouter = [inner] * 20\nfor i in range(100):\n    keep.append(json.encode(outer))\nprint('after')"},
		{"bytes", "keep = []\nb = bytes('z' * (8 << 20))\nfor i in range(100):\n    keep.append(b + b[1:])\nprint('after')"},
		{"list slicing copies", "keep = []\nl = [0] * (1 << 20)\nfor i in range(100):\n    keep.append(l[1:])\nprint('after')"},
		{"comprehension copies", "keep = []\nbase = ['a' * 100000 for i in range(600)]\nfor i in range(10):\n    keep.append([x + 'b' for x in base])\nprint('after')"},
		{"str.replace", "keep = []\ns = 'ab' * (4 << 20)\nfor i in range(100):\n    keep.append(s.replace('a', 'cc'))\nprint('after')"},
		{"str.format", "keep = []\ns = 'q' * (8 << 20)\nfor i in range(100):\n    keep.append('{}{}'.format(s, s))\nprint('after')"},
	}
	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			text, took := advRunShape(t, sh.script, 0)
			t.Logf("took %s\n%s", took, text)
			if !advIsHeapBreach(text) || strings.Contains(text, "after") || !advNotOtherBreach(text) {
				t.Errorf("expected a heap breach only, got:\n%s", text)
			}
			if took > 15*time.Second {
				t.Errorf("breach took %s", took)
			}
		})
	}
}

// One 200 MiB allocation with the bound at 64 MiB: the next interpreter steps
// must be cancelled. A busy tail gives the watcher time to sample.
func TestAdvB25ScriptSingleHugeAllocationThenSteps(t *testing.T) {
	text, took := advRunShape(t, "x = 'x' * (200 << 20)\nn = 0\nfor i in range(900000):\n    n += 1\nprint('after')", 0)
	t.Logf("took %s\n%s", took, text)
	if !advIsHeapBreach(text) || strings.Contains(text, "after") {
		t.Errorf("breach did not fire on the steps after one huge allocation:\n%s", text)
	}
}

// Same allocation with nothing after it: the script ends before any poll.
// Informational: a script that allocates once and finishes is not a leak.
func TestAdvB25ScriptSingleHugeAllocationThenEnd(t *testing.T) {
	text, _ := advRunShape(t, "x = 'x' * (200 << 20)\nprint(len(x))", 0)
	t.Logf("result:\n%s", text)
}

// Allocation inside a tool. (a) The script retains each returned text: the
// per-call clip (32 KiB) bounds what it can hold, so the heap does not grow and
// the bound correctly does not fire. (b) The tool itself retains 6 MiB per
// call: that is process-wide heap growth and must trip the bound.
func TestAdvB25ScriptToolAllocationCounts(t *testing.T) {
	t.Run("script retains clipped tool text", func(t *testing.T) {
		shortHeapBound(t, advHeapBound, 5*time.Millisecond)
		h := scriptHarness(t, "keep = []\nfor i in range(30):\n    keep.append(big().text)\nprint(len(keep))", 32)
		registerBatchable(t, h, "big", func(context.Context, tools.Call) tools.Result {
			return tools.Result{Text: strings.Repeat("t", 6<<20)}
		})
		_, text := runScriptHarness(t, h)
		t.Logf("result:\n%s", text)
		if advIsHeapBreach(text) {
			t.Errorf("retaining clipped tool text must not grow the heap:\n%s", text)
		}
	})
	t.Run("tool retains memory", func(t *testing.T) {
		shortHeapBound(t, advHeapBound, 5*time.Millisecond)
		h := scriptHarness(t, "for i in range(30):\n    hold(i=i)\nprint('after')", 32)
		var kept [][]byte
		var mu sync.Mutex
		registerBatchable(t, h, "hold", func(context.Context, tools.Call) tools.Result {
			b := make([]byte, 6<<20)
			for i := range b {
				b[i] = 1
			}
			mu.Lock()
			kept = append(kept, b)
			mu.Unlock()
			time.Sleep(15 * time.Millisecond)
			return tools.Result{Text: "ok"}
		})
		_, text := runScriptHarness(t, h)
		t.Logf("result (%d MiB held by the tool):\n%s", len(kept)*6, text)
		if !advIsHeapBreach(text) || strings.Contains(text, "after") {
			t.Errorf("tool-side retained allocation did not trip the bound:\n%s", text)
		}
	})
}

// Garbage only: large temporary strings, discarded. Reports the trip point.
func TestAdvB25ScriptGarbageOnlyFalsePositive(t *testing.T) {
	for _, bound := range []uint64{16 << 20, 32 << 20, 64 << 20} {
		shortHeapBound(t, bound, 2*time.Millisecond)
		h := scriptHarness(t, "n = 0\nfor i in range(300):\n    s = 'g' * (8 << 20)\n    n += len(s)\nprint('done', n)", 0)
		before, _ := heapSample()
		start := time.Now()
		_, text := runScriptHarness(t, h)
		t.Logf("bound=%d MiB: heap-before=%d MiB breached=%v took=%s", bound>>20, before>>20, advIsHeapBreach(text), time.Since(start))
	}
}

func TestAdvB25ScriptMetricIsSupported(t *testing.T) {
	const name = "/memory/classes/heap/objects:bytes"
	for _, d := range metrics.All() {
		if d.Name == name {
			if d.Kind != metrics.KindUint64 {
				t.Fatalf("%s has kind %v, want KindUint64", name, d.Kind)
			}
			if v, ok := heapSample(); !ok || v == 0 {
				t.Fatalf("heapSample returned 0 for a running process")
			}
			return
		}
	}
	t.Fatalf("%s is not in metrics.All(): the heap bound would be silently disabled", name)
}

// Fail-open: when the sample is unusable (0), the bound must not silently
// never fire. Expected: fail closed (an error or a refusal). A normal
// completion is a break.
func TestAdvB25ScriptFailsOpenWhenSampleIsZero(t *testing.T) {
	shortHeapBound(t, 16<<20, time.Millisecond)
	old := heapSample
	heapSample = func() (uint64, bool) { return 0, false }
	t.Cleanup(func() { heapSample = old })
	h := scriptHarness(t, "keep = []\nfor i in range(10):\n    keep.append('x' * (8 << 20))\n    wait()\nprint('after')", 0)
	registerBatchable(t, h, "wait", func(context.Context, tools.Call) tools.Result {
		time.Sleep(10 * time.Millisecond)
		return tools.Result{Text: "ok"}
	})
	_, text := runScriptHarness(t, h)
	t.Logf("result:\n%s", text)
	if strings.Contains(text, "after") && !strings.Contains(text, "script ended early") {
		t.Errorf("heapSample()==0 disables the bound silently; the script ran 80 MiB past a 16 MiB bound with no refusal:\n%s", text)
	}
}

// now < baseline mid-script must not underflow into a huge growth.
func TestAdvB25ScriptHeapBelowBaselineNoUnderflow(t *testing.T) {
	shortHeapBound(t, 1<<20, time.Millisecond)
	var mu sync.Mutex
	calls := 0
	old := heapSample
	heapSample = func() (uint64, bool) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return 8 << 30, true
		}
		return 1 << 30, true
	}
	t.Cleanup(func() { heapSample = old })
	h := scriptHarness(t, "print('before')\nwait()\nprint('after')", 0)
	registerBatchable(t, h, "wait", func(context.Context, tools.Call) tools.Result {
		time.Sleep(60 * time.Millisecond)
		return tools.Result{Text: "ok"}
	})
	_, text := runScriptHarness(t, h)
	mu.Lock()
	n := calls
	mu.Unlock()
	if n < 5 {
		t.Fatalf("only %d samples; the watcher did not run", n)
	}
	if advIsHeapBreach(text) || !strings.Contains(text, "after") {
		t.Errorf("heap below baseline was read as growth:\n%s", text)
	}
}

// Two scripts at once: one allocates heavily (and breaches itself); the other
// is a quiet script held 600 ms. Reports whether the quiet one is breached.
func TestAdvB25ScriptConcurrentFalsePositive(t *testing.T) {
	shortHeapBound(t, advHeapBound, 5*time.Millisecond)
	quiet := scriptHarness(t, "print('before')\nwait()\nprint('after')", 0)
	registerBatchable(t, quiet, "wait", func(context.Context, tools.Call) tools.Result {
		time.Sleep(600 * time.Millisecond)
		return tools.Result{Text: "ok"}
	})
	noisy := scriptHarness(t, "keep = []\nfor i in range(12):\n    keep.append('n' * (8 << 20))\n    wait()\nprint('noisy done')", 0)
	registerBatchable(t, noisy, "wait", func(context.Context, tools.Call) tools.Result {
		time.Sleep(20 * time.Millisecond)
		return tools.Result{Text: "ok"}
	})
	var wg sync.WaitGroup
	wg.Add(2)
	var qErr, nErr error
	go func() { defer wg.Done(); _, qErr = quiet.loop.Run(context.Background(), userMsg("go")) }()
	go func() {
		time.Sleep(100 * time.Millisecond)
		defer wg.Done()
		_, nErr = noisy.loop.Run(context.Background(), userMsg("go"))
	}()
	wg.Wait()
	if qErr != nil || nErr != nil {
		t.Fatal(qErr, nErr)
	}
	qText, nText := toolResults(t, quiet)["s1"], toolResults(t, noisy)["s1"]
	t.Logf("quiet breached by neighbour=%v\nquiet:\n%s\nnoisy:\n%s", advIsHeapBreach(qText), qText, nText)
}

func advGoroutinesSettle(base int) int {
	n := runtime.NumGoroutine()
	for i := 0; i < 100 && n > base; i++ {
		time.Sleep(20 * time.Millisecond)
		n = runtime.NumGoroutine()
	}
	return n
}

func TestAdvB25ScriptWatcherLifecycle(t *testing.T) {
	const iters = 8
	cases := []struct {
		name  string
		run   func(t *testing.T)
		wants string
	}{
		{"normal end", func(t *testing.T) {
			h := scriptHarness(t, "print(1)", 0)
			runScriptHarness(t, h)
		}, ""},
		{"heap breach", func(t *testing.T) {
			shortHeapBound(t, 1<<20, time.Millisecond)
			old := heapSample
			var n uint64
			heapSample = func() (uint64, bool) { n++; return 1<<30 + n*(4<<20), true }
			t.Cleanup(func() { heapSample = old })
			h := scriptHarness(t, "for i in range(900000):\n    pass", 0)
			runScriptHarness(t, h)
		}, ""},
		{"wall-time breach", func(t *testing.T) {
			shortWallTime(t, 20*time.Millisecond)
			h := scriptHarness(t, "for i in range(900000):\n    pass\nwhile True:\n    pass", 0)
			runScriptHarness(t, h)
		}, ""},
		{"cancelled ctx", func(t *testing.T) {
			h := scriptHarness(t, "block()\nprint('after')", 0)
			registerBatchable(t, h, "block", func(ctx context.Context, _ tools.Call) tools.Result {
				<-ctx.Done()
				return tools.Errorf("interrupted")
			})
			ctx, cancel := context.WithCancel(context.Background())
			go func() { time.Sleep(20 * time.Millisecond); cancel() }()
			_, _ = h.loop.Run(ctx, userMsg("go"))
			cancel()
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.run(t) // warm up
			base := advGoroutinesSettle(runtime.NumGoroutine())
			for i := 0; i < iters; i++ {
				c.run(t)
			}
			after := advGoroutinesSettle(base)
			t.Logf("goroutines base=%d after=%d", base, after)
			if after > base {
				t.Errorf("goroutines grew from %d to %d over %d runs (watcher leak)", base, after, iters)
			}
		})
	}
}

// The heap breach must read like the step and wall-time breaches: the same
// trailer, the same error flag.
func TestAdvB25ScriptBreachShapeMatches(t *testing.T) {
	run := func(setup func(t *testing.T), script string, reg ...func(*harness)) (string, bool) {
		var text string
		var isErr bool
		t.Run("", func(t *testing.T) {
			setup(t)
			h := scriptHarness(t, script, 0)
			for _, r := range reg {
				r(h)
			}
			_, _ = runScriptHarness(t, h)
			text = toolResults(t, h)["s1"]
			isErr = advToolResultIsError(t, h)
		})
		return text, isErr
	}
	blockTool := func(h *harness) {
		registerBatchable(t, h, "block", func(ctx context.Context, _ tools.Call) tools.Result {
			<-ctx.Done()
			return tools.Errorf("interrupted")
		})
	}
	stepText, stepErr := run(func(*testing.T) {}, "while True:\n    pass")
	wallText, wallErr := run(func(t *testing.T) { shortWallTime(t, 30*time.Millisecond) }, "print('before')\nblock()\nprint('after')", blockTool)
	var reads atomic.Uint64
	heapText, heapErr := run(func(t *testing.T) {
		shortHeapBound(t, 1<<20, time.Millisecond)
		old := heapSample
		heapSample = func() (uint64, bool) { return 1<<30 + reads.Add(1)*(4<<20), true }
		t.Cleanup(func() { heapSample = old })
	}, "print('before')\nwait()\nprint('after')", func(h *harness) {
		registerBatchable(t, h, "wait", func(context.Context, tools.Call) tools.Result {
			for reads.Load() < 2 {
				time.Sleep(time.Millisecond)
			}
			time.Sleep(20 * time.Millisecond)
			return tools.Result{Text: "ok"}
		})
	})
	t.Logf("step: %q err=%v\nwall: %q err=%v\nheap: %q err=%v", stepText, stepErr, wallText, wallErr, heapText, heapErr)
	if !advIsHeapBreach(heapText) || !strings.Contains(wallText, "wall-time") || !strings.Contains(stepText, "interpreter steps") {
		t.Fatalf("a probe ended for the wrong reason")
	}
	for _, text := range []string{stepText, wallText, heapText} {
		if !strings.HasPrefix(text, "script: ") || !strings.Contains(text, "call(s) made\n") ||
			!strings.HasSuffix(text, "; the output above is what it produced before that]\n") || !strings.Contains(text, "[script ended early: ") {
			t.Errorf("shape differs: %q", text)
		}
	}
	if !heapErr || !stepErr || !wallErr {
		t.Errorf("IsError flag: heap=%v step=%v wall=%v (want all true)", heapErr, stepErr, wallErr)
	}
	return
}

func advToolResultIsError(t *testing.T, h *harness) bool {
	t.Helper()
	msgs, err := h.log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		for _, block := range m.Content {
			if r, ok := block.(llm.ToolResultBlock); ok && r.ToolCallID == "s1" {
				return r.IsError
			}
		}
	}
	t.Fatal("no s1 result")
	return false
}

var _ = fmt.Sprintf

var advSink []byte

// Garbage-only script against a live heap of varying size. The metric counts
// unswept garbage, so GC pacing lets garbage reach about the live-set size
// before a cycle: false positive risk grows with the process's live heap.
func TestAdvB25ScriptGarbageVsLiveHeapFalsePositive(t *testing.T) {
	for _, ballast := range []int{0, 16 << 20, 48 << 20, 64 << 20, 128 << 20} {
		advSink = make([]byte, ballast)
		for i := range advSink {
			advSink[i] = 1
		}
		runtime.GC()
		shortHeapBound(t, advHeapBound, 2*time.Millisecond)
		h := scriptHarness(t, "n = 0\nfor i in range(100):\n    s = 'g' * (8 << 20)\n    n += len(s)\nprint('done', n)", 0)
		_, text := runScriptHarness(t, h)
		t.Logf("live ballast=%3d MiB bound=64 MiB garbage-only script (800 MiB of dead strings): breached=%v", ballast>>20, advIsHeapBreach(text))
		advSink = nil
	}
	runtime.GC()
}

// The baseline is read once. If it lands while unswept garbage inflates the
// heap and a GC then frees it, the script's headroom becomes bound plus that
// garbage. Expected: the bound still fires near 32 MiB of growth.
func TestAdvB25ScriptBaselineInflatedByGarbageMasksBound(t *testing.T) {
	old := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(old)
	garbage := make([]byte, 100<<20)
	for i := range garbage {
		garbage[i] = 1
	}
	advSink = garbage
	garbage = nil
	shortHeapBound(t, 32<<20, 2*time.Millisecond)
	h := scriptHarness(t, "gc()\nkeep = []\nfor i in range(16):\n    keep.append('k' * (8 << 20))\n    pause()\nprint('after')", 32)
	registerBatchable(t, h, "gc", func(context.Context, tools.Call) tools.Result {
		advSink = nil
		debug.SetGCPercent(old)
		runtime.GC()
		return tools.Result{Text: "ok"}
	})
	registerBatchable(t, h, "pause", func(context.Context, tools.Call) tools.Result {
		time.Sleep(10 * time.Millisecond)
		return tools.Result{Text: "ok"}
	})
	_, text := runScriptHarness(t, h)
	t.Logf("result:\n%s", text)
	if !advIsHeapBreach(text) || strings.Contains(text, "after") {
		t.Errorf("the script retained 128 MiB past a 32 MiB bound because the baseline was taken over 100 MiB of since-freed garbage:\n%s", text)
	}
}

// Garbage-only sweep: 4 MiB temporary strings, 200 per run, live set small and
// GC forced before each run. Logs how often each bound trips (false positives).
func TestAdvB25ScriptGarbageOnlyTripRate(t *testing.T) {
	for _, bound := range []uint64{32 << 20, 64 << 20, 128 << 20} {
		trips := 0
		const reps = 8
		for r := 0; r < reps; r++ {
			runtime.GC()
			shortHeapBound(t, bound, 2*time.Millisecond)
			h := scriptHarness(t, "n = 0\nfor i in range(200):\n    s = 'g' * (4 << 20)\n    n += len(s)\nprint('done', n)", 0)
			_, text := runScriptHarness(t, h)
			if advIsHeapBreach(text) {
				trips++
			}
		}
		t.Logf("garbage-only (800 MiB total, 4 MiB temporaries, live set ~small): bound=%d MiB tripped %d/%d runs", bound>>20, trips, reps)
	}
}
