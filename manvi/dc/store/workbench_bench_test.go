package store

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/internal/testsupport"
)

// BenchmarkWorkbenchProfile uses the real host client and persistent Rust
// process. The normal fixture has 10,000 tasks, 100 repositories and 100,110
// committed events. Setup calls the public transactional API, not raw SQL.
// Run explicitly with -run '^$' -bench '^BenchmarkWorkbenchProfile$' -benchtime=20x.
// This measures the data boundary, not rendering, native notifications or agents.
func BenchmarkWorkbenchProfile(b *testing.B) {
	benchmarkWorkbenchProfile(b, 10_000, 10)
}

// BenchmarkWorkbenchProfileStress keeps the same number of committed mutations
// as the normal fixture but spreads them across 100,000 distinct tasks.
func BenchmarkWorkbenchProfileStress(b *testing.B) {
	benchmarkWorkbenchProfile(b, 100_000, 1)
}

func benchmarkWorkbenchProfile(b *testing.B, tasks, revisions int) {
	b.Helper()
	client := New(testsupport.DCStore(b), filepath.Join(b.TempDir(), "profile.sqlite"))
	b.Cleanup(client.Close)
	call := func(method string, raw json.RawMessage) json.RawMessage {
		b.Helper()
		result, err := client.Workbench(b.Context(), method, raw)
		if err != nil {
			b.Fatalf("%s: %v", method, err)
		}
		return result
	}
	const repositories = 100
	seedStarted := time.Now()
	for i := 0; i < repositories; i++ {
		call("repositories.put", json.RawMessage(fmt.Sprintf(`{"request_id":"r%d","id":"r%d","expected_revision":0,"name":"Repository %d","identity_key":"benchmark:r%d"}`, i, i, i, i)))
	}
	for i := 0; i < 10; i++ {
		ids := make([]string, 10)
		for j := range ids {
			ids[j] = fmt.Sprintf(`"r%d"`, i*10+j)
		}
		call("workspaces.put", json.RawMessage(fmt.Sprintf(`{"request_id":"w%d","id":"w%d","expected_revision":0,"name":"Workspace %d","repository_ids":[%s]}`, i, i, i, strings.Join(ids, ","))))
	}
	description := strings.Repeat("Grounded task evidence. ", 24)
	for i := 0; i < tasks; i++ {
		for rev := 0; rev < revisions; rev++ {
			call("items.put", json.RawMessage(fmt.Sprintf(`{"request_id":"t%d-v%d","id":"t%d","expected_revision":%d,"title":"Benchmark startup latency task %d","description":"%s","repository_ids":["r%d"],"primary_repository_id":"r%d","home_workspace_id":"w%d","position":%d}`, i, rev, i, rev, i, description, i%repositories, i%repositories, i%10, i)))
		}
	}
	var events struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(call("events.list", json.RawMessage(`{"limit":1}`)), &events); err != nil {
		b.Fatal(err)
	}
	if events.Total != tasks*revisions+repositories+10 {
		b.Fatalf("fixture has %d events", events.Total)
	}
	b.Logf("Fixture: %d tasks, %d repositories, %d events; seed %s", tasks, repositories, events.Total, time.Since(seedStarted))
	for _, query := range []struct {
		name, method, input string
		total               int
	}{
		{"global", "items.list", `{"limit":200}`, tasks},
		{"workspace", "items.list", `{"workspace_id":"w0","limit":200}`, tasks * 19 / 100},
		{"repository", "items.list", `{"repository_id":"r0","limit":200}`, tasks / repositories},
		{"search", "items.list", `{"query":"startup latency","limit":200}`, tasks},
		{"history", "items.history", `{"id":"t0","limit":200}`, revisions},
		{"rare_search", "items.list", fmt.Sprintf(`{"query":"%d","limit":200}`, tasks-1), 1},
		{"repository_search", "items.list", `{"repository_id":"r0","query":"startup latency","limit":200}`, tasks / repositories},
		{"workspace_search", "items.list", `{"workspace_id":"w0","query":"startup latency","limit":200}`, tasks * 19 / 100},
		{"empty_status", "items.list", `{"status":"review","limit":200}`, 0},
	} {
		b.Run(query.name, func(b *testing.B) {
			raw := json.RawMessage(query.input)
			var page struct {
				Total int `json:"total"`
				Shown int `json:"shown"`
			}
			response, err := client.Workbench(b.Context(), query.method, raw)
			if err != nil {
				b.Fatal(err)
			}
			if err := json.Unmarshal(response, &page); err != nil {
				b.Fatal(err)
			}
			if page.Total != query.total || page.Shown != min(query.total, 200) {
				b.Fatalf("unexpected page: %+v; expected total %d", page, query.total)
			}
			samples := make([]time.Duration, 0, b.N)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				started := time.Now()
				if _, err := client.Workbench(b.Context(), query.method, raw); err != nil {
					b.Fatal(err)
				}
				samples = append(samples, time.Since(started))
			}
			b.StopTimer()
			slices.Sort(samples)
			if len(samples) > 0 {
				b.ReportMetric(float64(samples[(len(samples)-1)*95/100].Microseconds())/1000, "p95-ms")
			}
		})
	}
	// Read timing alone does not establish edit latency. Every measured edit
	// changes saved text and commits a revision, event and automatic queue update.
	// Go may calibrate a sub-benchmark more than once; retain the live revision.
	revision := revisions
	b.Run("edit", func(b *testing.B) {
		samples := make([]time.Duration, 0, b.N)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			raw := json.RawMessage(fmt.Sprintf(`{"request_id":"edit-%d","id":"t0","expected_revision":%d,"title":"Edited startup latency task %d","description":"%s","repository_ids":["r0"],"primary_repository_id":"r0","home_workspace_id":"w0","position":0}`, revision, revision, revision, description))
			started := time.Now()
			if _, err := client.Workbench(b.Context(), "items.put", raw); err != nil {
				b.Fatal(err)
			}
			samples = append(samples, time.Since(started))
			revision++
		}
		b.StopTimer()
		slices.Sort(samples)
		if len(samples) > 0 {
			b.ReportMetric(float64(samples[(len(samples)-1)*95/100].Microseconds())/1000, "p95-ms")
		}
	})
	if err := json.Unmarshal(call("events.list", json.RawMessage(`{"limit":1}`)), &events); err != nil {
		b.Fatal(err)
	}
	if expected := tasks*revisions + repositories + 10 + revision - revisions; events.Total != expected {
		b.Fatalf("edits did not commit once each: %d events; expected %d", events.Total, expected)
	}
}
