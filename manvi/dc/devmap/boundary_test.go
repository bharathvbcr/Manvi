package devmap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file is about the boundary itself rather than the answers crossing it:
// what the harness does when the binary on the other side floods it, when a
// command it believed wrote a file did not, and when a string the model chose
// reaches a command line unescorted.

// recording writes a fake devmap that appends its whole argv to a file and then
// answers from the reply table. What reaches the command line is a contract in
// its own right: these arguments carry model-chosen text.
func recording(t *testing.T, replies map[string]string) (*Client, string) {
	t.Helper()
	dir := t.TempDir()
	argv := filepath.Join(dir, "argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + argv + "\nprintf '\\036\\n' >> " + argv + "\n"
	script += "for a in \"$@\"; do case \"$a\" in\n"
	for command, reply := range replies {
		script += "  " + command + ")\ncat <<'JSON'\n" + reply + "\nJSON\n  exit 0;;\n"
	}
	script += "esac; done\necho '{}'\n"
	path := filepath.Join(dir, "devmap")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return New(path, dir), argv
}

// invocations reads back the argv log as one slice per call.
func invocations(t *testing.T, argvPath string) [][]string {
	t.Helper()
	raw, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for _, block := range strings.Split(string(raw), "\036\n") {
		block = strings.TrimSuffix(block, "\n")
		if block == "" {
			continue
		}
		out = append(out, strings.Split(block, "\n"))
	}
	return out
}

// find returns the first invocation containing the named subcommand.
func find(t *testing.T, calls [][]string, command string) []string {
	t.Helper()
	for _, c := range calls {
		for _, a := range c {
			if a == command {
				return c
			}
		}
	}
	t.Fatalf("no invocation of %q in %v", command, calls)
	return nil
}

// TestModelChosenTextIsSeparatedFromFlags.
//
// `devcouncil_graph_query` puts a string the model wrote where devmap expects
// <QUERY>, and `graph_context` does the same with <FILE>. devmap parses its
// command line with clap, which reads any leading-dash argument as a flag: a
// query for a symbol named `-x` is a parse error rather than a search, and the
// binary's own error message tells the caller to use `--`. Today nothing does.
//
// The consequence is not only the confusing failure. `search` and `deps` both
// take `--budget`, and this package reports how much of an answer the budget
// suppressed — an argument that reached the parser as a flag would make that
// accounting describe a bound the harness did not set.
func TestModelChosenTextIsSeparatedFromFlags(t *testing.T) {
	c, argv := recording(t, map[string]string{
		"status": healthyStatus,
		"search": `{"items":[{"file_path":"a.go","symbol_name":"x"}],"hidden":0}`,
		"deps":   `{"items":[{"source_file":"a.go","target_file":"b.go"}],"hidden":0}`,
	})

	if _, err := c.Search(context.Background(), "--budget"); err != nil {
		t.Fatalf("a query that looks like a flag is still a query: %v", err)
	}
	if _, err := c.Deps(context.Background(), "-rf"); err != nil {
		t.Fatalf("a path that looks like a flag is still a path: %v", err)
	}

	calls := invocations(t, argv)
	for _, probe := range []struct{ command, positional string }{
		{"search", "--budget"},
		{"deps", "-rf"},
	} {
		got := find(t, calls, probe.command)
		sep, value := -1, -1
		for i, a := range got {
			if a == "--" && sep == -1 {
				sep = i
			}
			if a == probe.positional && i > 0 {
				value = i
			}
		}
		if sep == -1 {
			t.Fatalf("devmap %s must fence its positional behind `--`; got %v", probe.command, got)
		}
		if value < sep {
			t.Fatalf("devmap %s passed %q where the parser reads flags (before `--` at %d); got %v",
				probe.command, probe.positional, sep, got)
		}
	}
}

// TestTheSeparatorDoesNotSwallowOurOwnFlags guards the obvious wrong fix. Every
// argument after `--` is positional, so a separator placed before `--budget`
// would hand the budget to the parser as a second query.
func TestTheSeparatorDoesNotSwallowOurOwnFlags(t *testing.T) {
	c, argv := recording(t, map[string]string{
		"status": healthyStatus,
		"search": `{"items":[],"hidden":0}`,
	})
	if _, err := c.Search(context.Background(), "Client"); err != nil {
		t.Fatal(err)
	}
	got := find(t, invocations(t, argv), "search")
	for i, a := range got {
		if a == "--" {
			for _, later := range got[i+1:] {
				if strings.HasPrefix(later, "--budget") {
					t.Fatalf("--budget after the separator is read as a query, not a flag: %v", got)
				}
			}
		}
	}
}

// TestAFloodOfOutputIsBoundedRatherThanBuffered.
//
// maxOutput was checked after cmd.Run() returned, which is after os/exec has
// already copied the whole of the child's stdout into a bytes.Buffer. A binary
// that goes wrong in the way the constant describes takes the harness's memory
// with it before the guard is reached, and stderr had no guard at all.
func TestAFloodOfOutputIsBoundedRatherThanBuffered(t *testing.T) {
	dir := t.TempDir()
	// Emits far more than the limit set below, and would not stop on its own.
	script := "#!/bin/sh\nwhile :; do printf 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'; done\n"
	path := filepath.Join(dir, "devmap")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c := New(path, dir)
	c.maxOutput = 64 << 10

	done := make(chan error, 1)
	go func() {
		_, err := c.Status(context.Background())
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a binary that never stops writing must not produce an answer")
		}
		if !strings.Contains(err.Error(), "more than") {
			t.Fatalf("the failure must name the bound that stopped it, got %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the read was not bounded: a flooding binary held the call open")
	}
}

// TestStderrIsBoundedAndSaysThatItWas. stderr carries the notices, so it cannot
// simply fail the command — but a report built from a truncated stream must not
// read like one built from the whole of it.
func TestStderrIsBoundedAndSaysThatItWas(t *testing.T) {
	dir := t.TempDir()
	script := clapHelp("#!/bin/sh\ni=0\nwhile [ $i -lt 4000 ]; do echo \"noise line $i\" >&2; i=$((i+1)); done\n" +
		"echo '{\"files_indexed\":1,\"symbols\":1,\"edges\":1}'\n")
	path := filepath.Join(dir, "devmap")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c := New(path, dir)
	c.maxStderr = 4 << 10

	report, err := c.Build(context.Background(), 30*time.Second)
	if err != nil {
		t.Fatalf("a noisy build still built: %v", err)
	}
	if report.Clean() {
		t.Fatal("a build whose notices were truncated is not a clean build")
	}
	if !strings.Contains(strings.Join(report.Degraded(), " "), "truncated") {
		t.Fatalf("the truncation must be reported, not silently shortening the notices: %v", report.Degraded())
	}
}

// TestAManifestThatWroteNothingIsNotReportedAsWritten.
//
// `manvi map build` printed "wrote <path>" on the strength of an exit code. The
// artifact is a separate file with its own lifetime — the build advances the
// index whether or not the manifest lands — and a manifest that exits zero
// having written nothing leaves the previous artifact in place for the gate to
// keep deciding from, under a line saying it had just been rewritten.
func TestAManifestThatWroteNothingIsNotReportedAsWritten(t *testing.T) {
	c := fake(t, map[string]string{
		"status":   healthyStatus,
		"manifest": `{"generation_id":4,"output":"map.json","graph_output":"graph.json"}`,
	})
	dir := t.TempDir()
	report, err := c.Manifest(context.Background(),
		filepath.Join(dir, "repo_map.json"), filepath.Join(dir, "code_graph.json"))
	if err == nil {
		t.Fatal("a manifest whose artifacts are not on disk afterwards has not written them")
	}
	if !strings.Contains(err.Error(), "code_graph.json") && !strings.Contains(err.Error(), "repo_map.json") {
		t.Fatalf("the failure must name the artifact that is missing: %v", err)
	}
	_ = report
}

// TestAManifestReportsTheGenerationItWrote. devmap answers `manifest` with the
// generation it rendered, and this package decoded that payload into a map and
// dropped it. It is the one exact way to tell whether the file the gate reads
// came from the index the tools answer from.
func TestAManifestReportsTheGenerationItWrote(t *testing.T) {
	dir := t.TempDir()
	mapPath := filepath.Join(dir, "repo_map.json")
	graphPath := filepath.Join(dir, "code_graph.json")
	c := fakeSaying(t, map[string]string{
		"status":   healthyStatus,
		"manifest": `{"generation_id":4,"output":"x","graph_output":"y"}`,
	}, nil, mapPath, graphPath)

	report, err := c.Manifest(context.Background(), mapPath, graphPath)
	if err != nil {
		t.Fatal(err)
	}
	if report.GenerationID != 4 {
		t.Fatalf("the generation the manifest wrote must be carried out of it, got %d", report.GenerationID)
	}
}

// TestAManifestDisagreeingWithItsIndexIsReported: the manifest names the
// generation it rendered and the index names the one it holds. They are written
// by the same command against the same store, so a difference means the
// artifact on disk is not the current graph.
func TestAManifestDisagreeingWithItsIndexIsReported(t *testing.T) {
	dir := t.TempDir()
	mapPath := filepath.Join(dir, "repo_map.json")
	graphPath := filepath.Join(dir, "code_graph.json")
	// The index has moved to generation 9; the manifest rendered generation 4.
	status := `{"db_path":"x","generation_id":9,"node_count":1200,"edge_count":9000,
"pending_count":0,"quarantined_count":0,"is_fresh":true,"degraded_reason":null}`
	c := fakeSaying(t, map[string]string{
		"status":   status,
		"manifest": `{"generation_id":4,"output":"x","graph_output":"y"}`,
	}, nil, mapPath, graphPath)

	report, err := c.Manifest(context.Background(), mapPath, graphPath)
	if err != nil {
		t.Fatal(err)
	}
	if report.Clean() {
		t.Fatal("an artifact rendered from a generation the index has moved past is not clean")
	}
	joined := strings.Join(report.Degraded(), " ")
	if !strings.Contains(joined, "4") || !strings.Contains(joined, "9") {
		t.Fatalf("both generations must be named: %q", joined)
	}
}
