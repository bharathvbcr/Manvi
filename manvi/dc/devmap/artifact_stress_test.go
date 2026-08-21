package devmap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// The producer is not the only thing outside this program that this package
// reads. The adoption check in artifact.go opens two JSON files at known paths
// in a shared state directory, written by some other tool, at every session
// start. They get the same treatment as the binary next door in stress_test.go:
// they can be enormous, nested past any sane depth, truncated, doubled, or not
// JSON at all, and none of it may crash a session or hold one open.

// bigArtifact builds a document whose engine marker is the last thing in it,
// with `filler` nodes of padding in front, so a scan has to cross the whole file
// to reach the verdict. It is the shape of a real code graph: the marker in our
// own 8.9 MB artifact sits past the node list.
func bigArtifact(filler int) []byte {
	var b strings.Builder
	b.WriteString(`{"nodes":[`)
	for i := 0; i < filler; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":"pkg/file_%d.go::Symbol%d","kind":"function","area":"pkg","span":"%d:%d"}`,
			i, i, i, i+40)
	}
	b.WriteString(`],"meta":{"map_engine":"devmap-rust"}}`)
	return []byte(b.String())
}

// TestTheMarkerScanDoesNotHoldTheArtifact.
//
// The first version of this check read the file with os.ReadFile and handed it
// to json.Unmarshal to compare one string. The code graph for this repository is
// 8.9 MB and grows with the tree; the map for a monorepo is larger. Holding all
// of it — the bytes and the decoded tree at once — to answer "who wrote this" is
// the payload bound this package applies at every other boundary, unapplied at
// the one that runs first, at session start, before the first frame.
//
// What is measured is retained heap, not total allocation, and the distinction
// is the whole trade. Streaming allocates *more* in total than unmarshalling
// does: every token boxes into an interface, so a document of many small values
// churns through more garbage than one bulk decode. It keeps none of it. For a
// check that runs once per build the churn is free and the retention is not —
// the failure this guards is a session start that briefly holds a monorepo's
// graph in memory to read one word out of it.
//
// The bound is asserted against the implementation it replaced rather than
// against a byte count, because a byte count is a number someone raises when it
// starts failing. The property is that the scan does not scale with the artifact
// the way holding it does.
func TestTheMarkerScanDoesNotHoldTheArtifact(t *testing.T) {
	doc := bigArtifact(20000)
	t.Logf("artifact is %d bytes", len(doc))

	// retained is the heap still live once the callee has returned and whatever
	// it handed back is still referenced. A collection runs first so the churn
	// is not counted, and the result is what the caller is still holding.
	retained := func(f func() any) int64 {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		held := f()
		runtime.GC()
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(held)
		delta := int64(after.HeapAlloc) - int64(before.HeapAlloc)
		if delta < 0 {
			return 0
		}
		return delta
	}

	var engine string
	var wellFormed bool
	streamed := retained(func() any {
		engine, wellFormed = scanMarker(bytes.NewReader(doc), []string{"meta", "map_engine"})
		return nil
	})
	if !wellFormed || engine != consumerMapEngine {
		t.Fatalf("the scan did not reach the marker at the end of the document: %q wellFormed=%v",
			engine, wellFormed)
	}

	whole := retained(func() any {
		var into any
		if err := json.Unmarshal(doc, &into); err != nil {
			t.Fatal(err)
		}
		return into
	})

	t.Logf("streaming retained %d bytes; the decoded document retains %d", streamed, whole)
	if whole < int64(len(doc)) {
		t.Fatalf("the measurement did not see the decoded document at all (%d bytes retained "+
			"for a %d byte artifact); the comparison below would prove nothing", whole, len(doc))
	}
	if streamed >= whole/4 {
		t.Fatalf("the scan retains %d bytes against the decoded document's %d; it is holding "+
			"the artifact rather than streaming past it", streamed, whole)
	}
}

// TestADeeplyNestedArtifactIsAVerdictNotACrash. A recursive decoder meets a file
// of nothing but opening brackets by exhausting the goroutine stack, which on
// this path would take down a session at its first frame over a file some other
// tool left behind.
func TestADeeplyNestedArtifactIsAVerdictNotACrash(t *testing.T) {
	const depth = 200000
	doc := strings.Repeat("[", depth) + strings.Repeat("]", depth)

	done := make(chan struct{})
	var wellFormed bool
	go func() {
		defer close(done)
		_, wellFormed = scanMarker(strings.NewReader(doc), []string{"map_engine"})
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("scanning a deeply nested artifact did not finish")
	}
	// Well-formed JSON with no marker in it is foreign, which is the answer that
	// preserves the file. What is being asserted is that there is an answer.
	if !wellFormed {
		t.Log("the decoder reported the nesting as malformed, which is also a verdict")
	}
}

// TestAnArtifactVerdictMatchesTheProducersOwn.
//
// These two verdicts have to agree on every file. Where this boundary says
// "ours" and devmap says "foreign", the file stays put, devmap refuses it, and
// the manifest fails exactly as it did before any of this existed — the same
// wedge reached by a different road. The prefix cases are the ones that road
// runs through: a document whose marker is intact and whose tail is not. devmap
// parses the whole document (serde_json::from_str) before looking the key up, so
// this scan parses to the end even though the marker is usually found early.
func TestAnArtifactVerdictMatchesTheProducersOwn(t *testing.T) {
	cases := map[string]struct {
		body    string
		foreign bool
	}{
		"ours, whole":              {`{"map_engine":"devmap-rust","files":[]}`, false},
		"ours, then truncated":     {`{"map_engine":"devmap-rust","files":[{"path":"a.go"`, true},
		"ours, then trailing junk": {`{"map_engine":"devmap-rust"} trailing`, true},
		"ours, then a second doc":  {`{"map_engine":"devmap-rust"}{"map_engine":"devmap-rust"}`, true},
		"marker repeated, last wins away from us": {
			`{"map_engine":"devmap-rust","map_engine":"devcouncil-python"}`, true},
		"marker repeated, last wins toward us": {
			`{"map_engine":"devcouncil-python","map_engine":"devmap-rust"}`, false},
		"marker is not a string": {`{"map_engine":["devmap-rust"]}`, true},
		"marker is null":         {`{"map_engine":null}`, true},
		"marker nested one too deep": {
			`{"meta":{"map_engine":"devmap-rust"}}`, true},
		"top level is an array":      {`[{"map_engine":"devmap-rust"}]`, true},
		"top level is a bare string": {`"devmap-rust"`, true},
		"empty document":             {``, true},
		"whitespace only":            {"  \n\t ", true},
		"nul bytes":                  {"\x00\x00\x00", true},
		"a marker-shaped substring":  {`{"note":"map_engine devmap-rust"}`, true},
		"the value arrives escaped":  {`{"map_engine":"devmap-rust"}`, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "repo_map.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := (destination{path: path, marker: []string{"map_engine"}}).foreign()
			if err != nil {
				t.Fatalf("a readable file must produce a verdict, not an error: %v", err)
			}
			if got != tc.foreign {
				t.Fatalf("foreign=%v, want %v for %q", got, tc.foreign, tc.body)
			}
		})
	}
}

// TestHostileArtifactBytesNeverPanic sweeps shapes a hand-written table does not
// reach. The verdict does not matter here; producing one instead of a panic does.
func TestHostileArtifactBytesNeverPanic(t *testing.T) {
	fragments := []string{
		`{`, `}`, `[`, `]`, `"`, `:`, `,`, `null`, `1e400`, `-`, `\`,
		`"map_engine"`, `"devmap-rust"`, `{"meta":`, ` `, "\xff\xfe", "\x00",
		strings.Repeat("{", 64), strings.Repeat(`"a":`, 64),
	}
	path := filepath.Join(t.TempDir(), "code_graph.json")
	d := destination{path: path, marker: []string{"meta", "map_engine"}}

	for i, a := range fragments {
		for j, b := range fragments {
			for k, c := range fragments {
				body := a + b + c
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
				if _, err := d.foreign(); err != nil {
					t.Fatalf("%d/%d/%d %q produced an error rather than a verdict: %v", i, j, k, body, err)
				}
			}
		}
	}
}

// TestConcurrentForeignChecksAgree. Two sessions can refresh at once over one
// state directory, and the verdict on a file must not depend on who asked.
func TestConcurrentForeignChecksAgree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repo_map.json")
	if err := os.WriteFile(path, bigArtifact(2000), 0o644); err != nil {
		t.Fatal(err)
	}
	d := destination{path: path, marker: []string{"meta", "map_engine"}}

	const racers = 32
	var wg sync.WaitGroup
	verdicts := make([]bool, racers)
	errs := make([]error, racers)
	for i := range verdicts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			verdicts[i], errs[i] = d.foreign()
		}()
	}
	wg.Wait()
	for i := range verdicts {
		if errs[i] != nil {
			t.Fatalf("racer %d: %v", i, errs[i])
		}
		if verdicts[i] {
			t.Fatalf("racer %d called an artifact carrying the marker foreign", i)
		}
	}
}

// TestRepeatedAdoptionCyclesStayBounded. The state directory is shared, and the
// other producer can keep rewriting the path. Every cycle must preserve rather
// than overwrite, and the pile-up must stop at a bound with a message rather
// than growing until someone notices the disk.
func TestRepeatedAdoptionCyclesStayBounded(t *testing.T) {
	root, mapPath, graphPath := artifactPaths(t)
	c := guardedFake(t, root)
	seen := map[string]string{}

	for cycle := 1; cycle <= preserveLimit+4; cycle++ {
		body := fmt.Sprintf(`{"languages":["python"],"cycle":%d}`, cycle)
		write(t, mapPath, body)

		report, err := c.Manifest(context.Background(), mapPath, graphPath)
		if cycle > preserveLimit {
			if err == nil {
				t.Fatalf("cycle %d: past the bound the manifest must refuse rather than "+
					"reuse a preserved name", cycle)
			}
			if got := read(t, mapPath); got != body {
				t.Fatalf("cycle %d: the artifact must be left alone when it cannot be preserved", cycle)
			}
			continue
		}
		if err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		if len(report.Adopted) != 1 {
			t.Fatalf("cycle %d adopted %+v", cycle, report.Adopted)
		}
		at := report.Adopted[0].PreservedAs
		if prior, taken := seen[at]; taken {
			t.Fatalf("cycle %d reused %s, which already held %q", cycle, at, prior)
		}
		seen[at] = body
	}

	// Every cycle's bytes are still exactly where that cycle put them.
	for at, body := range seen {
		if got := read(t, at); got != body {
			t.Fatalf("%s holds %q, want %q", at, got, body)
		}
	}
	if len(seen) != preserveLimit {
		t.Fatalf("kept %d copies, want the bound of %d", len(seen), preserveLimit)
	}
}
