package devmap

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"manvi/repomap"
)

// Every other test in this package drives a fake, which is right: what needs
// testing here is this boundary's behaviour when the producer answers badly,
// and a real binary cannot be made to answer badly on demand.
//
// The gap a fake cannot close is that it agrees with whatever this build
// believes. devmap is built from another repository and resolved from PATH, so
// every field name here — `generation_id`, `node_count`, `symbol_name`,
// `source_file`, `edge_kind` — is an assumption about a contract nothing in
// this repository compiles against. A fake asserting those names asserts only
// that the test and the code were written by the same hand.
//
// So this file runs the real thing once, end to end, over a repository it
// builds itself. It skips when devmap is absent, and a skip is visible in the
// test output: an unrun check must never read the same as one that passed.

func devmapBinary(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("MANVI_MAP_BINARY"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		t.Skipf("MANVI_MAP_BINARY=%s is not there; the live contract is unverified in this run", p)
	}
	p, err := exec.LookPath("devmap")
	if err != nil {
		t.Skip("devmap is not on PATH; the live contract with the producer is unverified in this run")
	}
	return p
}

// fixtureRepo writes a small repository with a known shape: pkgb calls into
// pkga, and pkga has one symbol nothing calls.
func fixtureRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for path, body := range map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.24\n",
		"pkga/a.go": "package pkga\n\n" +
			"func Alpha() int { return Helper() }\n" +
			"func Helper() int { return 1 }\n" +
			"func NeverCalled() int { return 2 }\n",
		"pkgb/b.go": "package pkgb\n\n" +
			"import \"example.com/m/pkga\"\n\n" +
			"func Beta() int { return pkga.Alpha() }\n",
	} {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestTheLiveContractHolds is one round trip through every command this package
// calls, asserting the field names it decodes into actually arrive.
func TestTheLiveContractHolds(t *testing.T) {
	bin := devmapBinary(t)
	root := fixtureRepo(t)
	c := New(bin, root)
	c.Timeout = 60 * time.Second
	ctx := context.Background()

	report, err := c.Build(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// At least the two sources. Not an exact count: what the producer chooses
	// to index beyond them — go.mod here — is its business, and a test that
	// pinned the number would fail on a change that broke nothing.
	if files, ok := report.Stat("files_indexed"); !ok || files < 2 {
		t.Fatalf("files_indexed = %d (present=%v), want at least the 2 sources written", files, ok)
	}
	if !report.Clean() {
		t.Fatalf("a build of two clean files reported: %v", report.Degraded())
	}

	status, err := c.Available(ctx)
	if err != nil {
		t.Fatalf("available after a build: %v", err)
	}
	if status.GenerationID <= 0 || status.NodeCount <= 0 || status.EdgeCount <= 0 {
		t.Fatalf("status decoded to empty counts, so its field names have moved: %+v", status)
	}
	if !status.IsFresh {
		t.Errorf("an index built from the tree as it stands must report fresh: %+v", status)
	}

	// search: the names this package reads are file_path and symbol_name, and
	// assertShape turns a rename into an error rather than an empty answer.
	found, err := c.Search(ctx, "Alpha")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found.Items) == 0 {
		t.Fatal("a symbol that exists was not found; the query contract has moved")
	}
	var sawAlpha bool
	for _, s := range found.Items {
		if s.Name == "Alpha" && strings.HasSuffix(s.FilePath, "pkga/a.go") {
			sawAlpha = true
		}
	}
	if !sawAlpha {
		t.Fatalf("search returned items carrying no name or path this build reads: %+v", found.Items)
	}

	// deps: source_file/target_file, and the direction derived from them rather
	// than from the subcommand's name.
	edges, err := c.Deps(ctx, "pkga/a.go")
	if err != nil {
		t.Fatalf("deps: %v", err)
	}
	if len(edges.Items) == 0 {
		t.Fatal("a file with callers and callees reported no edges")
	}
	touching := 0
	for _, e := range edges.Items {
		if strings.Contains(e.SourceFile, "pkga/a.go") || strings.Contains(e.TargetFile, "pkga/a.go") {
			touching++
		}
	}
	if touching == 0 {
		t.Fatalf("no returned edge names the file it was asked about: %+v", edges.Items)
	}

	if _, err := c.Dead(ctx); err != nil {
		t.Fatalf("dead: %v", err)
	}

	// manifest: the generation it reports is what the artifact check rests on,
	// and it must equal what the index holds moments later.
	mapPath := filepath.Join(root, "repo_map.json")
	graphPath := filepath.Join(root, "code_graph.json")
	manifest, err := c.Manifest(ctx, mapPath, graphPath)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if manifest.GenerationID <= 0 {
		t.Fatal("manifest reported no generation; the artifact check would be unverified against every index")
	}
	if !manifest.Clean() {
		t.Fatalf("a manifest of a clean two-file index reported: %v", manifest.Degraded())
	}
	if manifest.GenerationID != status.GenerationID {
		t.Fatalf("manifest rendered generation %d and the index holds %d",
			manifest.GenerationID, status.GenerationID)
	}

	// And the file on disk must agree with both. This is the check the harness
	// runs after every build, exercised against a real artifact rather than a
	// hand-written one.
	m, err := repomap.Load(graphPath)
	if err != nil {
		t.Fatalf("the artifact devmap just wrote does not load: %v", err)
	}
	p := m.Provenance()
	if !p.Stamped {
		t.Fatal("the artifact carries no generation stamp, so the divergence check cannot run on it")
	}
	if p.GenerationID != status.GenerationID {
		t.Fatalf("artifact generation %d, index generation %d", p.GenerationID, status.GenerationID)
	}
	if notes := m.DisagreementsWith(status.GenerationID, status.NodeCount); len(notes) != 0 {
		t.Fatalf("a freshly written artifact must not read as diverged from the index it came from: %v", notes)
	}
	if p.SchemaVersion != repomap.SupportedSchema {
		t.Errorf("the producer now writes schema %d and this build reads %d; "+
			"check what moved before raising SupportedSchema", p.SchemaVersion, repomap.SupportedSchema)
	}
}

// TestTheLiveParserAcceptsTheSeparator is the reason the `--` fence can be
// trusted. clap's behaviour is the producer's, not this build's: without the
// fence a query beginning with a dash is a parse error, and with it the same
// query is a query. Only the real parser can settle that.
func TestTheLiveParserAcceptsTheSeparator(t *testing.T) {
	bin := devmapBinary(t)
	root := fixtureRepo(t)
	c := New(bin, root)
	c.Timeout = 60 * time.Second
	ctx := context.Background()

	if _, err := c.Build(ctx, 5*time.Minute); err != nil {
		t.Fatalf("build: %v", err)
	}

	// Each of these is read as a flag by clap when it arrives unfenced.
	for _, query := range []string{"--budget", "-b", "--help", "--version", "-"} {
		if _, err := c.Search(ctx, query); err != nil {
			t.Errorf("search %q reached the parser as a flag: %v", query, err)
		}
	}
	for _, path := range []string{"--budget", "-b", "--min-confidence"} {
		if _, err := c.Deps(ctx, path); err != nil {
			t.Errorf("deps %q reached the parser as a flag: %v", path, err)
		}
	}
}

// TestTheLiveProducerRefusalIsRecoverable is the one that matters, because the
// guard it works against belongs to the producer.
//
// Everything the adoption path does rests on two facts about a binary built from
// another repository: that it stamps `map_engine: devmap-rust` into what it
// writes, and that its refusal is conditioned on the destination existing, so
// moving the file aside lifts it without --force. Both are read off the producer
// rather than agreed with a fake — a fake asserting them asserts only that the
// test and the code were written by the same hand — and both would break this
// boundary silently if they moved: a renamed marker makes every build preserve
// its own artifacts, and a guard that refused on an absent path would take the
// harness straight back to the wedge this replaced.
func TestTheLiveProducerRefusalIsRecoverable(t *testing.T) {
	bin := devmapBinary(t)
	root := fixtureRepo(t)
	c := New(bin, root)
	c.Timeout = 60 * time.Second
	ctx := context.Background()

	if _, err := c.Build(ctx, 5*time.Minute); err != nil {
		t.Fatalf("build: %v", err)
	}

	state := filepath.Join(root, ".devcouncil")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	mapPath := filepath.Join(state, "repo_map.json")
	graphPath := filepath.Join(state, "code_graph.json")

	// The shape DevCouncil's Python producer leaves behind, which is what the
	// harness met in a real repository and could not get past.
	if err := os.WriteFile(mapPath, []byte(pythonRepoMap), 0o644); err != nil {
		t.Fatal(err)
	}

	// First: the refusal is real, and it is what this boundary is working
	// against. If the producer stops refusing, the adoption becomes dead weight
	// and this test says so rather than passing quietly.
	direct := exec.Command(bin, "--json", "manifest", ".",
		"--output", mapPath, "--graph-output", graphPath)
	direct.Dir = root
	said, err := direct.CombinedOutput()
	if err == nil {
		t.Fatalf("the producer no longer refuses a foreign repo map; the adoption path in "+
			"artifact.go is working around a guard that is gone: %s", said)
	}
	if !strings.Contains(string(said), "refuse to overwrite") {
		t.Fatalf("the producer failed for some other reason than the guard: %s", said)
	}

	// Second: this boundary gets past it, and nothing is lost doing so.
	report, err := c.Manifest(ctx, mapPath, graphPath)
	if err != nil {
		t.Fatalf("the boundary did not recover from the producer's refusal: %v", err)
	}
	if len(report.Adopted) != 1 || report.Adopted[0].Path != mapPath {
		t.Fatalf("the adoption must name the map it took over, got %+v", report.Adopted)
	}
	preserved, err := os.ReadFile(report.Adopted[0].PreservedAs)
	if err != nil {
		t.Fatalf("the preserved copy is not readable: %v", err)
	}
	if string(preserved) != pythonRepoMap {
		t.Fatalf("the preserved copy is not the original bytes: %s", preserved)
	}

	// Third: the marker this build tests identity on is the one the producer
	// actually writes, in both artifacts and at their two different positions.
	// A drift here is what would make every subsequent build preserve its own
	// output, so it is asserted against the file rather than assumed.
	for _, d := range manifestDestinations(mapPath, graphPath) {
		isForeign, err := d.foreign()
		if err != nil {
			t.Fatalf("reading back %s: %v", d.path, err)
		}
		if isForeign {
			body, _ := os.ReadFile(d.path)
			t.Fatalf("%s was written by the producer this build consumes but does not carry %q "+
				"at %v, so the next build would preserve it as foreign: %s",
				d.path, consumerMapEngine, d.marker, body)
		}
	}

	// Fourth: a second run over the producer's own artifacts adopts nothing.
	// This is the property that keeps the state directory from filling with a
	// copy per build, and it can only be verified against real output.
	again, err := c.Manifest(ctx, mapPath, graphPath)
	if err != nil {
		t.Fatalf("second manifest: %v", err)
	}
	if len(again.Adopted) != 0 {
		t.Fatalf("a run over the producer's own artifacts adopted them: %+v", again.Adopted)
	}
	if !again.Clean() {
		t.Fatalf("a run over the producer's own artifacts is a clean run, got %v", again.Degraded())
	}
}
