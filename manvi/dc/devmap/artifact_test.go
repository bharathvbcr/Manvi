package devmap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The defect these pin, in the words of the harness that hit it:
//
//	the repository index could not be built: devmap manifest failed: exit
//	status 1 (Error: refuse to overwrite a non-devmap-rust repo map at
//	/Users/…/curio/.devcouncil/repo_map.json (pass --force to replace))
//	run 'manvi map build' to see the whole error
//
// Every session start in that repository ended there, and the remedy it offered
// ran the same command that had just failed. The fake below reproduces the
// producer's guard — refuse when the destination holds a file some other
// producer wrote — because that guard is the environment this boundary has to
// work in, not a defect to be mocked away.

// pythonRepoMap is the shape DevCouncil's Python producer writes: no engine
// marker at all, which is exactly how the Rust producer recognises it.
const pythonRepoMap = `{"languages":["python"],"files":[],"subsystems":[]}`

// guardedFake writes a stand-in devmap that enforces the same refusal the real
// one does, and otherwise writes both artifacts with their engine markers.
func guardedFake(t *testing.T, root string) *Client {
	t.Helper()
	script := `#!/bin/sh
cmd=""
map=""
graph=""
while [ $# -gt 0 ]; do
  case "$1" in
    status) cmd=status ;;
    manifest) cmd=manifest ;;
    --output) shift; map="$1" ;;
    --graph-output) shift; graph="$1" ;;
  esac
  shift
done
if [ "$cmd" = status ]; then
  cat <<'JSON'
` + healthyStatus + `
JSON
  exit 0
fi
if [ "$cmd" = manifest ]; then
  for f in "$map" "$graph"; do
    if [ -e "$f" ] && ! grep -q 'devmap-rust' "$f"; then
      echo "Error: refuse to overwrite a non-devmap-rust repo map at $f (pass --force to replace)" >&2
      exit 1
    fi
  done
  printf '%s' '{"map_engine":"devmap-rust","files":[]}' > "$map"
  printf '%s' '{"meta":{"map_engine":"devmap-rust"},"nodes":[],"edges":[]}' > "$graph"
  echo '{"generation_id":3,"output":"map","graph_output":"graph"}'
  exit 0
fi
echo '{}'
`
	bin := filepath.Join(t.TempDir(), "devmap")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return New(bin, root)
}

// refusingFake never writes anything and always fails, for the rollback path.
func refusingFake(t *testing.T, root string) *Client {
	t.Helper()
	script := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in\n" +
		"  status) cat <<'JSON'\n" + healthyStatus + "\nJSON\n  exit 0;;\n" +
		"  manifest) echo 'Error: something else went wrong' >&2; exit 1;;\n" +
		"esac; done\necho '{}'\n"
	bin := filepath.Join(t.TempDir(), "devmap")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return New(bin, root)
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// artifactPaths is the pair every test here writes to.
func artifactPaths(t *testing.T) (root, mapPath, graphPath string) {
	t.Helper()
	root = t.TempDir()
	state := filepath.Join(root, ".devcouncil")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	return root, filepath.Join(state, "repo_map.json"), filepath.Join(state, "code_graph.json")
}

// TestAForeignRepoMapIsAdoptedRatherThanFailedOn is the defect.
//
// Before this, a repository whose `.devcouncil/repo_map.json` came from the
// Python producer could never have its index artifacts written: the manifest
// failed, the failure was reported as an index that could not be built, and the
// gate's neighbour rule reported unavailable on every write for as long as the
// file sat there. Nothing in the harness could get past it, in any session, ever.
func TestAForeignRepoMapIsAdoptedRatherThanFailedOn(t *testing.T) {
	root, mapPath, graphPath := artifactPaths(t)
	write(t, mapPath, pythonRepoMap)

	report, err := guardedFake(t, root).Manifest(context.Background(), mapPath, graphPath)
	if err != nil {
		t.Fatalf("a foreign repo map must not wedge the manifest: %v", err)
	}

	if got := read(t, mapPath); !strings.Contains(got, consumerMapEngine) {
		t.Fatalf("the path was not taken over; it still holds %q", got)
	}
	preserved := filepath.Join(filepath.Dir(mapPath), "repo_map.foreign.json")
	if !exists(preserved) {
		t.Fatal("the foreign map was overwritten rather than preserved")
	}
	if got := read(t, preserved); got != pythonRepoMap {
		t.Fatalf("the preserved copy is not the original bytes: %q", got)
	}

	if len(report.Adopted) != 1 {
		t.Fatalf("the adoption must be carried out on the report, got %+v", report.Adopted)
	}
	if a := report.Adopted[0]; a.Path != mapPath || a.PreservedAs != preserved {
		t.Fatalf("the adoption must name both paths, got %+v", a)
	}
	if report.Clean() {
		t.Fatal("taking over another producer's file is not a clean run; it must render as a notice")
	}
	said := strings.Join(report.Degraded(), " ")
	if !strings.Contains(said, preserved) || !strings.Contains(said, mapPath) {
		t.Fatalf("the operator must be told which file moved and where: %v", report.Degraded())
	}
}

// TestAForeignCodeGraphIsRecognisedByItsOwnMarker. The two artifacts stamp the
// producer's name in different places — the map at the top level, the graph
// under `meta` — and reading one rule for both is wrong in both directions: it
// would preserve every code graph this harness ever wrote, or hand a foreign one
// to devmap to overwrite.
func TestAForeignCodeGraphIsRecognisedByItsOwnMarker(t *testing.T) {
	cases := map[string]struct {
		body   string
		hidden bool // must be preserved
	}{
		"python graph, no marker anywhere": {`{"nodes":[],"edges":[]}`, true},
		"marker at the map's position only": {
			`{"map_engine":"devmap-rust","nodes":[]}`, true},
		"marker where the graph carries it": {
			`{"meta":{"map_engine":"devmap-rust"},"nodes":[]}`, false},
		"meta present but not an object":  {`{"meta":"devmap-rust"}`, true},
		"marker present but not a string": {`{"meta":{"map_engine":7}}`, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root, mapPath, graphPath := artifactPaths(t)
			write(t, graphPath, tc.body)

			report, err := guardedFake(t, root).Manifest(context.Background(), mapPath, graphPath)
			if err != nil {
				t.Fatalf("manifest: %v", err)
			}
			preserved := filepath.Join(filepath.Dir(graphPath), "code_graph.foreign.json")
			if exists(preserved) != tc.hidden {
				t.Fatalf("preserved=%v, want %v (adopted: %+v)", exists(preserved), tc.hidden, report.Adopted)
			}
			if tc.hidden && read(t, preserved) != tc.body {
				t.Fatal("the preserved copy is not the original bytes")
			}
		})
	}
}

// TestAnArtifactThisBoundaryWroteIsLeftAlone. Adoption is a one-time event per
// artifact: what devmap writes carries the marker, so the next build must find
// nothing foreign. A boundary that preserved its own output would fill the state
// directory a copy per build, and the notice would stop meaning anything.
func TestAnArtifactThisBoundaryWroteIsLeftAlone(t *testing.T) {
	root, mapPath, graphPath := artifactPaths(t)
	write(t, mapPath, pythonRepoMap)
	c := guardedFake(t, root)

	first, err := c.Manifest(context.Background(), mapPath, graphPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Adopted) != 1 {
		t.Fatalf("the first run must adopt exactly the foreign map, got %+v", first.Adopted)
	}

	for run := 2; run <= 5; run++ {
		report, err := c.Manifest(context.Background(), mapPath, graphPath)
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if len(report.Adopted) != 0 {
			t.Fatalf("run %d adopted its own artifacts: %+v", run, report.Adopted)
		}
		if !report.Clean() {
			t.Fatalf("run %d over its own artifacts is a clean run, got %v", run, report.Degraded())
		}
	}
	if exists(filepath.Join(filepath.Dir(mapPath), "repo_map.foreign-2.json")) {
		t.Fatal("a second preserved copy was made of a file this boundary had written")
	}
}

// TestAnUnparseableArtifactIsPreservedNotOverwritten. A file this boundary
// cannot parse is one it cannot claim, and the producer's guard reads it the
// same way. Claiming it is the answer that destroys something.
func TestAnUnparseableArtifactIsPreservedNotOverwritten(t *testing.T) {
	for name, body := range map[string]string{
		"truncated json": `{"languages": ["py`,
		"not json":       "<html>this is not a repo map</html>",
		"empty":          "",
	} {
		t.Run(name, func(t *testing.T) {
			root, mapPath, graphPath := artifactPaths(t)
			write(t, mapPath, body)

			if _, err := guardedFake(t, root).Manifest(context.Background(), mapPath, graphPath); err != nil {
				t.Fatalf("manifest: %v", err)
			}
			preserved := filepath.Join(filepath.Dir(mapPath), "repo_map.foreign.json")
			if !exists(preserved) {
				t.Fatal("an unreadable file was overwritten instead of preserved")
			}
			if got := read(t, preserved); got != body {
				t.Fatalf("preserved %q, want the original %q", got, body)
			}
		})
	}
}

// TestAFailedManifestPutsThePreservedArtifactBack.
//
// The move happens before the command runs, so a command that then fails has
// left the state directory rearranged for a rebuild that never happened. An
// operator meeting an error is entitled to find their files where they left
// them; the next attempt has to see the same repository this one did, or the
// adoption notice fires once and the evidence of why is gone.
func TestAFailedManifestPutsThePreservedArtifactBack(t *testing.T) {
	root, mapPath, graphPath := artifactPaths(t)
	write(t, mapPath, pythonRepoMap)

	_, err := refusingFake(t, root).Manifest(context.Background(), mapPath, graphPath)
	if err == nil {
		t.Fatal("a manifest that failed must be reported as failing")
	}
	if got := read(t, mapPath); got != pythonRepoMap {
		t.Fatalf("the foreign map was not put back; the path holds %q", got)
	}
	if exists(filepath.Join(filepath.Dir(mapPath), "repo_map.foreign.json")) {
		t.Fatal("a preserved copy was left behind by a manifest that did not land")
	}
	if !strings.Contains(err.Error(), "something else went wrong") {
		t.Fatalf("the rollback must not replace the reason the manifest failed: %v", err)
	}
}

// TestAManifestThatWritesNothingRollsBackToo. The producer can exit zero having
// written nothing, and Manifest already caught that. What it did not do was undo
// the move it had made to let the write happen.
func TestAManifestThatWritesNothingRollsBackToo(t *testing.T) {
	root, mapPath, graphPath := artifactPaths(t)
	write(t, mapPath, pythonRepoMap)

	// Exits zero, writes neither artifact.
	c := fake(t, map[string]string{
		"status":   healthyStatus,
		"manifest": `{"generation_id":3}`,
	})
	c.Root = root

	if _, err := c.Manifest(context.Background(), mapPath, graphPath); err == nil {
		t.Fatal("a manifest whose artifacts are not on disk afterwards has not written them")
	}
	if got := read(t, mapPath); got != pythonRepoMap {
		t.Fatalf("the foreign map was not put back; the path holds %q", got)
	}
}

// TestPreserveNeverOverwritesAnEarlierCopy. The Python producer can rewrite the
// path after the harness has taken it over, which brings this boundary back to a
// foreign file at a path whose obvious preserved name is already taken. Reusing
// that name would destroy the first copy — the exact loss the whole mechanism
// exists to prevent.
func TestPreserveNeverOverwritesAnEarlierCopy(t *testing.T) {
	root, mapPath, graphPath := artifactPaths(t)
	dir := filepath.Dir(mapPath)
	c := guardedFake(t, root)

	for run := 1; run <= 4; run++ {
		body := fmt.Sprintf(`{"languages":["python"],"run":%d}`, run)
		write(t, mapPath, body)
		report, err := c.Manifest(context.Background(), mapPath, graphPath)
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if len(report.Adopted) != 1 {
			t.Fatalf("run %d must adopt the map the other producer rewrote, got %+v", run, report.Adopted)
		}
		if got := read(t, report.Adopted[0].PreservedAs); got != body {
			t.Fatalf("run %d preserved %q, want %q", run, got, body)
		}
	}

	// Every copy is still there, each holding its own run.
	for run, name := range map[int]string{
		1: "repo_map.foreign.json",
		2: "repo_map.foreign-2.json",
		3: "repo_map.foreign-3.json",
		4: "repo_map.foreign-4.json",
	} {
		want := fmt.Sprintf(`{"languages":["python"],"run":%d}`, run)
		if got := read(t, filepath.Join(dir, name)); got != want {
			t.Fatalf("%s holds %q, want %q", name, got, want)
		}
	}
}

// TestPreserveRefusesPastItsBoundRatherThanClobbering. Reaching the bound means
// two producers are overwriting one path every build. Silently preserving a
// seventeenth copy answers that by filling the directory; reusing a name answers
// it by losing data. The bound is refused out loud, with the artifact left where
// it is so nothing is lost either way.
func TestPreserveRefusesPastItsBoundRatherThanClobbering(t *testing.T) {
	root, mapPath, graphPath := artifactPaths(t)
	dir := filepath.Dir(mapPath)
	write(t, mapPath, pythonRepoMap)
	write(t, filepath.Join(dir, "repo_map.foreign.json"), "copy-1")
	for n := 2; n <= preserveLimit; n++ {
		write(t, filepath.Join(dir, fmt.Sprintf("repo_map.foreign-%d.json", n)), fmt.Sprintf("copy-%d", n))
	}

	_, err := guardedFake(t, root).Manifest(context.Background(), mapPath, graphPath)
	if err == nil {
		t.Fatal("past the bound the manifest must fail rather than reuse a preserved name")
	}
	if !strings.Contains(err.Error(), mapPath) {
		t.Fatalf("the failure must name the artifact it would not preserve: %v", err)
	}
	if got := read(t, mapPath); got != pythonRepoMap {
		t.Fatalf("the artifact must be left alone when it cannot be preserved, got %q", got)
	}
	if got := read(t, filepath.Join(dir, "repo_map.foreign.json")); got != "copy-1" {
		t.Fatalf("an earlier preserved copy was overwritten: %q", got)
	}
	for n := 2; n <= preserveLimit; n++ {
		name := fmt.Sprintf("repo_map.foreign-%d.json", n)
		if got := read(t, filepath.Join(dir, name)); got != fmt.Sprintf("copy-%d", n) {
			t.Fatalf("%s was overwritten: %q", name, got)
		}
	}
}

// TestAnUnreadableArtifactIsNeitherAdoptedNorOverwritten. A check that could not
// run must never report the same result as a check that ran: an artifact this
// boundary cannot read is not evidence that it is foreign, and it is not
// evidence that it is ours. Both conclusions act on a file nobody looked at, and
// one of them destroys it.
func TestAnUnreadableArtifactIsNeitherAdoptedNorOverwritten(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can read a file with no permission bits; the check is unverified in this run")
	}
	root, mapPath, graphPath := artifactPaths(t)
	write(t, mapPath, pythonRepoMap)
	if err := os.Chmod(mapPath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(mapPath, 0o644) })

	_, err := guardedFake(t, root).Manifest(context.Background(), mapPath, graphPath)
	if err == nil {
		t.Fatal("an artifact that could not be read must stop the manifest, not be guessed at")
	}
	if !strings.Contains(err.Error(), mapPath) {
		t.Fatalf("the failure must name the file it could not read: %v", err)
	}
	if exists(filepath.Join(filepath.Dir(mapPath), "repo_map.foreign.json")) {
		t.Fatal("a file this boundary could not read was moved anyway")
	}
	if err := os.Chmod(mapPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := read(t, mapPath); got != pythonRepoMap {
		t.Fatalf("a file this boundary could not read was overwritten anyway: %q", got)
	}
}

// TestAnAbsentArtifactIsNotAnAdoption. Nothing there is nothing to protect, and
// reporting an adoption for a fresh repository would put a line about another
// producer's file in front of every operator who has never had one.
func TestAnAbsentArtifactIsNotAnAdoption(t *testing.T) {
	root, mapPath, graphPath := artifactPaths(t)
	report, err := guardedFake(t, root).Manifest(context.Background(), mapPath, graphPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Adopted) != 0 {
		t.Fatalf("a repository with no artifacts adopted: %+v", report.Adopted)
	}
	if !report.Clean() {
		t.Fatalf("a first build in a fresh repository is a clean run, got %v", report.Degraded())
	}
}

// TestConcurrentPreservesNeverLoseACopy. Two sessions can refresh at once, and
// the state directory is shared. A check that the preserved name is free
// followed by a rename is a window in which the second writer destroys the
// first's copy; os.Link closes it by failing instead of overwriting.
func TestConcurrentPreservesNeverLoseACopy(t *testing.T) {
	const racers = 16
	_, mapPath, _ := artifactPaths(t)
	write(t, mapPath, pythonRepoMap)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var kept []string
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			preserved, err := preserve(mapPath)
			if err != nil {
				return
			}
			mu.Lock()
			kept = append(kept, preserved)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(kept) == 0 {
		t.Fatal("every racer failed; the artifact would be unpreservable under concurrency")
	}
	if exists(mapPath) {
		t.Fatal("the source survived a successful preserve")
	}
	for _, path := range kept {
		if got := read(t, path); got != pythonRepoMap {
			t.Fatalf("%s reported preserved but holds %q", path, got)
		}
	}
}

// TestAdoptIsAllOrNothing. A run that preserves the map and then cannot read the
// graph must put the map back: the caller's next act is to fail, and a failure
// that has already half-rearranged the state directory leaves the operator worse
// off than the error they were about to be shown.
func TestAdoptIsAllOrNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; the unreadable-graph half of this check cannot be set up")
	}
	_, mapPath, graphPath := artifactPaths(t)
	write(t, mapPath, pythonRepoMap)
	write(t, graphPath, `{"nodes":[]}`)
	if err := os.Chmod(graphPath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(graphPath, 0o644) })

	adopted, err := adopt(mapPath, graphPath)
	if err == nil {
		t.Fatal("an unreadable graph must stop the adoption")
	}
	if len(adopted) != 0 {
		t.Fatalf("a failed adoption must report nothing adopted, got %+v", adopted)
	}
	if got := read(t, mapPath); got != pythonRepoMap {
		t.Fatalf("the map preserved before the failure was not put back: %q", got)
	}
	if exists(filepath.Join(filepath.Dir(mapPath), "repo_map.foreign.json")) {
		t.Fatal("a preserved copy was left behind by an adoption that did not complete")
	}
}

// TestAdoptionSurvivesTheFoldIntoOneReport. `manvi map build` runs two commands
// and prints one account of them. An adoption dropped in that fold is a change
// to the operator's files that nothing tells them about.
func TestAdoptionSurvivesTheFoldIntoOneReport(t *testing.T) {
	build := &BuildReport{Stats: map[string]any{"files_indexed": float64(3)}}
	manifest := &ManifestReport{Adopted: []Adoption{{Path: "/x/repo_map.json", PreservedAs: "/x/repo_map.foreign.json"}}}

	build.Merge(manifest.Notices)
	build.Disagreements = append(build.Disagreements, manifest.Disagreements...)
	build.Adopted = append(build.Adopted, manifest.Adopted...)

	if build.Clean() {
		t.Fatal("a folded report carrying an adoption is not clean")
	}
	if !strings.Contains(strings.Join(build.Degraded(), " "), "repo_map.foreign.json") {
		t.Fatalf("the adoption was lost in the fold: %v", build.Degraded())
	}
}

// TestPreserveNamesAreDerivedFromTheArtifact. The name is what an operator reads
// months later to decide whether the file matters, so it keeps the stem and the
// extension and says what it is in between.
func TestPreserveNamesAreDerivedFromTheArtifact(t *testing.T) {
	dir := t.TempDir()
	for name, want := range map[string]string{
		"repo_map.json":   "repo_map.foreign.json",
		"code_graph.json": "code_graph.foreign.json",
		"noextension":     "noextension.foreign",
		"two.dots.json":   "two.dots.foreign.json",
	} {
		path := filepath.Join(dir, name)
		write(t, path, pythonRepoMap)
		got, err := preserve(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if filepath.Base(got) != want {
			t.Errorf("%s preserved as %s, want %s", name, filepath.Base(got), want)
		}
	}
}

// TestForeignReportsAnUnreadableFileAsAnError rather than as a verdict, which is
// the same rule assertShape and Available are built on: absence of a positive
// answer is not a negative one.
func TestForeignReportsAnUnreadableFileAsAnError(t *testing.T) {
	dir := t.TempDir()
	// A directory where a file is expected: readable as an entry, never as JSON.
	path := filepath.Join(dir, "repo_map.json")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := (destination{path: path, marker: []string{"map_engine"}}).foreign()
	if err == nil {
		t.Fatalf("a path that cannot be read as a file returned a verdict: foreign=%v", got)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("the error must name the path: %v", err)
	}
}
