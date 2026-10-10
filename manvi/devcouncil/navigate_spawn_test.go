package devcouncil

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/devmap"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// The code-graph tools are a stub devmap here: a script that logs the
// subcommand of every invocation, waits a fixed delay, and answers from a
// fixed table. What these tests hold is how many processes a tool call starts
// and how long the fan-out takes, which is the cost the real binary adds.

const stubHealthy = `{"generation_id":7,"node_count":50,"edge_count":90,"is_fresh":true,"degraded_reason":null}`

// stubDevmap returns a registry wired to the stub and a reader for its log.
// delay is applied to every invocation.
func stubDevmap(tb testing.TB, status string, delay time.Duration) (*Registry, func() []string) {
	tb.Helper()
	dir := tb.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\n" +
		"case \" $* \" in *\" --help \"*) printf '      --graph-output <PATH>\\n'; exit 0;; esac\n" +
		"echo \"$2\" >> " + log + "\n" +
		fmt.Sprintf("sleep %.3f\n", delay.Seconds()) +
		"case \"$2\" in\n" +
		"  status) cat <<'JSON'\n" + status + "\nJSON\n;;\n" +
		"  deps) for last; do :; done; printf '{\"items\":[{\"source_file\":\"%s\",\"target_file\":\"dep-of-%s\"}],\"hidden\":0}\\n' \"$last\" \"$last\";;\n" +
		"  *) echo '{\"items\":[{\"file_path\":\"a.go\",\"symbol_name\":\"A\",\"kind\":\"func\"}],\"hidden\":0}';;\n" +
		"esac\n"
	path := filepath.Join(dir, "devmap")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		tb.Fatal(err)
	}
	reg := &Registry{deps: Deps{Root: dir, Map: devmap.New(path, dir)}, session: &Session{}}
	return reg, func() []string {
		raw, err := os.ReadFile(log)
		if err != nil {
			return nil
		}
		return strings.Fields(string(raw))
	}
}

func spawns(calls []string, sub string) int {
	n := 0
	for _, c := range calls {
		if c == sub {
			n++
		}
	}
	return n
}

func callOf(tb testing.TB, args any) tools.Call {
	tb.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		tb.Fatal(err)
	}
	return tools.Call{ID: "c", Arguments: raw}
}

func files(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("pkg/file%02d.go", i)
	}
	return out
}

// graph_query was three processes — Available, the search, then the freshness
// read the search makes — and is two: one status and the search.
func TestGraphQuerySpawnBound(t *testing.T) {
	reg, calls := stubDevmap(t, stubHealthy, 0)
	res := reg.graphQuery(context.Background(), callOf(t, map[string]string{"query": "A"}))
	if res.IsError {
		t.Fatalf("graph_query failed: %s", res.Text)
	}
	got := calls()
	if spawns(got, "status") != 1 || spawns(got, "search") != 1 || len(got) != 2 {
		t.Fatalf("graph_query started %v, want exactly one status and one search", got)
	}
}

// codeDead has the same shape and the same bound.
func TestCodeDeadSpawnBound(t *testing.T) {
	reg, calls := stubDevmap(t, stubHealthy, 0)
	res := reg.codeDead(context.Background(), callOf(t, map[string]bool{}))
	if res.IsError {
		t.Fatalf("code_dead failed: %s", res.Text)
	}
	if got := calls(); spawns(got, "status") != 1 || len(got) != 2 {
		t.Fatalf("code_dead started %v, want one status and one dead", got)
	}
}

// The status is a call's, not the client's. A graph_query after an edit has to
// ask again, so two calls are two status reads: a verdict served across calls
// would call a stale index fresh.
func TestGraphStatusIsNotServedAcrossCalls(t *testing.T) {
	reg, calls := stubDevmap(t, stubHealthy, 0)
	for i := 0; i < 3; i++ {
		if res := reg.graphQuery(context.Background(), callOf(t, map[string]string{"query": "A"})); res.IsError {
			t.Fatal(res.Text)
		}
	}
	if got := spawns(calls(), "status"); got != 3 {
		t.Fatalf("three calls read status %d times, want 3", got)
	}
}

// An index that cannot answer is refused, on every tool, however many calls
// came before it.
func TestGraphToolsRefuseAnUnbuiltIndex(t *testing.T) {
	reg, calls := stubDevmap(t, `{"generation_id":0,"node_count":0,"is_fresh":false,"degraded_reason":null}`, 0)
	ctx := context.Background()
	for name, res := range map[string]tools.Result{
		"graph_query":   reg.graphQuery(ctx, callOf(t, map[string]string{"query": "A"})),
		"code_dead":     reg.codeDead(ctx, callOf(t, map[string]bool{})),
		"graph_context": reg.graphContext(ctx, callOf(t, map[string][]string{"files": {"a.go"}})),
	} {
		if !res.IsError || !strings.Contains(res.Text, "not a negative result") {
			t.Errorf("%s answered from an unbuilt index: %+v", name, res)
		}
	}
	if got := calls(); spawns(got, "search")+spawns(got, "dead")+spawns(got, "deps") != 0 {
		t.Fatalf("a query ran against an unbuilt index: %v", got)
	}
}

func TestGraphToolsRefuseADegradedIndex(t *testing.T) {
	reg, _ := stubDevmap(t, `{"generation_id":7,"node_count":50,"is_fresh":false,"degraded_reason":"schema mismatch"}`, 0)
	res := reg.graphQuery(context.Background(), callOf(t, map[string]string{"query": "A"}))
	if !res.IsError || !strings.Contains(res.Text, "schema mismatch") {
		t.Fatalf("a degraded index was served: %+v", res)
	}
}

type contextAnswer struct {
	Files []struct {
		Path      string   `json:"path"`
		DependsOn []string `json:"depends_on"`
		UsedBy    []string `json:"used_by"`
		Error     string   `json:"error"`
	} `json:"files"`
}

// 25 files: one status and 25 deps, in input order, in well under the 26
// sequential spawns' time.
func TestGraphContext25SpawnsAndOrder(t *testing.T) {
	const delay = 60 * time.Millisecond
	reg, calls := stubDevmap(t, stubHealthy, delay)
	in := files(25)
	start := time.Now()
	res := reg.graphContext(context.Background(), callOf(t, map[string][]string{"files": in}))
	elapsed := time.Since(start)
	if res.IsError {
		t.Fatalf("graph_context failed: %s", res.Text)
	}
	got := calls()
	if spawns(got, "status") != 1 || spawns(got, "deps") != 25 || len(got) != 26 {
		t.Fatalf("started %d processes (%d status, %d deps), want 26 (1, 25)", len(got), spawns(got, "status"), spawns(got, "deps"))
	}
	var answer contextAnswer
	if err := json.Unmarshal([]byte(res.Text), &answer); err != nil {
		t.Fatal(err)
	}
	if len(answer.Files) != len(in) {
		t.Fatalf("%d files answered, want %d", len(answer.Files), len(in))
	}
	for i, f := range answer.Files {
		if f.Path != in[i] {
			t.Fatalf("slot %d answered %s, want %s: results are out of input order", i, f.Path, in[i])
		}
		if len(f.DependsOn) != 1 || f.DependsOn[0] != "dep-of-"+in[i] {
			t.Fatalf("slot %d carries another file's answer: %+v", i, f)
		}
	}
	// The baseline is measured, not computed: process start-up adds to the
	// stub's delay, and a computed figure would pass or fail on how loaded the
	// machine is. It is the shape this tool had before — Available, then one
	// deps after another — run on the same stub just now.
	seqStart := time.Now()
	client := reg.deps.Map
	if _, err := client.Available(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, f := range in {
		if _, err := client.Deps(context.Background(), f); err != nil {
			t.Fatal(err)
		}
	}
	sequential := time.Since(seqStart)
	if limit := sequential * 4 / 10; elapsed >= limit {
		t.Fatalf("graph_context took %v, want under 40%% of the %v sequential time (%v)", elapsed, sequential, limit)
	}
}

// Cancelling the call stops the pool from starting more processes, and the
// call returns the cancellation rather than a half-filled answer.
func TestGraphContextCancelStopsOutstandingSpawns(t *testing.T) {
	reg, calls := stubDevmap(t, stubHealthy, 150*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(250 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	res := reg.graphContext(ctx, callOf(t, map[string][]string{"files": files(25)}))
	if !res.IsError {
		t.Fatalf("a cancelled graph_context answered: %.200s", res.Text)
	}
	time.Sleep(400 * time.Millisecond) // anything still started would show by now
	if got := spawns(calls(), "deps"); got >= 25 {
		t.Fatalf("%d deps processes started; cancel did not stop the fan-out", got)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("returned after %v", elapsed)
	}
}

func TestGraphContextCapsAtTwentyFiveAndSaysSo(t *testing.T) {
	reg, calls := stubDevmap(t, stubHealthy, 0)
	res := reg.graphContext(context.Background(), callOf(t, map[string][]string{"files": files(40)}))
	if res.IsError || !strings.Contains(res.Text, "15 file(s) beyond the first 25") {
		t.Fatalf("the cap was not reported: %+v", res)
	}
	if got := spawns(calls(), "deps"); got != 25 {
		t.Fatalf("%d deps spawned, want 25", got)
	}
}

// BenchmarkGraphQuery and BenchmarkGraphContext25 run each tool against the
// stub with a fixed 20 ms per process, beside the shape they had before: an
// unscoped Available, an unscoped query, and for graph_context a sequential
// loop. spawns/op is the stub's own count.
func BenchmarkGraphQuery(b *testing.B) {
	reg, calls := stubDevmap(b, stubHealthy, 20*time.Millisecond)
	client := reg.deps.Map
	ctx := context.Background()
	call := callOf(b, map[string]string{"query": "A"})
	var before, after int64
	var beforeSpawns, afterSpawns int
	for i := 0; i < b.N; i++ {
		n0 := len(calls())
		t0 := time.Now()
		if _, err := client.Available(ctx); err != nil {
			b.Fatal(err)
		}
		if _, err := client.Search(ctx, "A"); err != nil {
			b.Fatal(err)
		}
		d0 := time.Since(t0).Nanoseconds()
		n1 := len(calls())
		t1 := time.Now()
		if res := reg.graphQuery(ctx, call); res.IsError {
			b.Fatal(res.Text)
		}
		d1 := time.Since(t1).Nanoseconds()
		n2 := len(calls())
		if before == 0 || d0 < before {
			before = d0
		}
		if after == 0 || d1 < after {
			after = d1
		}
		beforeSpawns, afterSpawns = n1-n0, n2-n1
	}
	b.ReportMetric(float64(before)/1e6, "old-min-ms")
	b.ReportMetric(float64(after)/1e6, "new-min-ms")
	b.ReportMetric(float64(beforeSpawns), "old-spawns")
	b.ReportMetric(float64(afterSpawns), "new-spawns")
}

func BenchmarkGraphContext25(b *testing.B) {
	reg, calls := stubDevmap(b, stubHealthy, 20*time.Millisecond)
	client := reg.deps.Map
	ctx := context.Background()
	in := files(25)
	call := callOf(b, map[string][]string{"files": in})
	var before, after int64
	var beforeSpawns, afterSpawns int
	for i := 0; i < b.N; i++ {
		n0 := len(calls())
		t0 := time.Now()
		if _, err := client.Available(ctx); err != nil {
			b.Fatal(err)
		}
		for _, f := range in {
			if _, err := client.Deps(ctx, f); err != nil {
				b.Fatal(err)
			}
		}
		d0 := time.Since(t0).Nanoseconds()
		n1 := len(calls())
		t1 := time.Now()
		if res := reg.graphContext(ctx, call); res.IsError {
			b.Fatal(res.Text)
		}
		d1 := time.Since(t1).Nanoseconds()
		n2 := len(calls())
		if before == 0 || d0 < before {
			before = d0
		}
		if after == 0 || d1 < after {
			after = d1
		}
		beforeSpawns, afterSpawns = n1-n0, n2-n1
	}
	b.ReportMetric(float64(before)/1e6, "old-min-ms")
	b.ReportMetric(float64(after)/1e6, "new-min-ms")
	b.ReportMetric(float64(beforeSpawns), "old-spawns")
	b.ReportMetric(float64(afterSpawns), "new-spawns")
}
