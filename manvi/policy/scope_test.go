package policy

import (
	"strings"
	"testing"

	"manvi/dc"
)

// areas is a SubsystemMap a test states outright, so a test about the neighbour
// rung is not also a test of the repo map's inference.
type areas struct {
	of         map[string]string
	neighbors  map[string][]string
	permissive bool
}

func (a areas) AreaForPath(path string) (string, bool) {
	for prefix, area := range a.of {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return area, true
		}
	}
	return "", false
}

// permissive lets a test declare that this map's neighbour relation is wide
// enough to make the rung near-unconditional. Default false, so the existing
// cases keep asserting a tight relation.
func (a areas) NeighborsArePermissive() bool { return a.permissive }

func (a areas) AreNeighbors(x, y string) bool {
	for _, n := range a.neighbors[x] {
		if n == y {
			return true
		}
	}
	return false
}

func planTask(files ...dc.PlannedFile) *dc.Task {
	return &dc.Task{ID: "TASK-1", PlannedFiles: files}
}

func modify(path string) dc.PlannedFile {
	return dc.PlannedFile{Path: path, AllowedChange: dc.ChangeModify}
}

// mapless is the gate a repository without a built code graph actually gets.
func mapless(t *testing.T) FileGate {
	t.Helper()
	return FileGate{Root: t.TempDir(), AllowNeighbors: true, AllowSameDir: true, HardRules: true}
}

func decide(t *testing.T, g FileGate, path string, task *dc.Task, op dc.Operation) Decision {
	t.Helper()
	return g.EvaluateFileChange(path, task, op, false)
}

// --- the same-directory rung ---

// The cliff this rung exists to remove: with no repo map, the neighbour rung
// cannot run at all, and every write beside a planned file was refused.
func TestASiblingOfAPlannedFileIsAllowedWhenNoRepoMapExists(t *testing.T) {
	d := decide(t, mapless(t), "src/helper.go", planTask(modify("src/calc.go")), dc.OpWrite)
	if d.Blocked() {
		t.Fatalf("a sibling of a planned file must not be refused for want of a map: %+v", d)
	}
	// And it must not look like a pass the rules produced. The subsystem check
	// did not run, and the allow says so.
	if d.Clean() {
		t.Fatalf("a proximity allow is not a clean allow: %+v", d)
	}
	if len(d.Degraded) == 0 || d.Degraded[0] != "repo_map.unavailable" {
		t.Fatalf("the allow must carry the check that could not run: %+v", d.Degraded)
	}
}

// The repository root is not a subsystem. One planned file at the top level
// would otherwise make the build, the CI config, and every dependency manifest
// writable.
func TestTheRepositoryRootNeverLendsScope(t *testing.T) {
	g := mapless(t)
	task := planTask(modify("README.md"))

	for _, target := range []string{"Makefile", "package.json", "go.mod", ".gitignore"} {
		d := decide(t, g, target, task, dc.OpWrite)
		if !d.Blocked() {
			t.Errorf("a planned file at the root must not lend %s: %+v", target, d)
		}
	}

	// The same rule from the other side: a planned file inside a directory does
	// not make the root writable.
	d := decide(t, g, "Makefile", planTask(modify("src/calc.go")), dc.OpWrite)
	if !d.Blocked() {
		t.Fatalf("a planned file in src/ must not lend the repository root: %+v", d)
	}
	if !strings.Contains(d.Reason, "repository root") {
		t.Fatalf("the refusal must say why the root is different: %q", d.Reason)
	}
}

// A read-only entry says "look at this, do not change it". Lending its
// directory would convert that into a permission over everything beside it.
func TestAReadOnlyPlannedFileLendsNothing(t *testing.T) {
	g := mapless(t)
	task := planTask(dc.PlannedFile{Path: "docs/notes.md", AllowedChange: dc.ChangeReadOnly})

	if d := decide(t, g, "docs/other.md", task, dc.OpWrite); !d.Blocked() {
		t.Fatalf("a read-only planned file must not lend its directory: %+v", d)
	}

	// Same rule one rung up, where the map can answer.
	withMap := g
	withMap.Subsystems = areas{of: map[string]string{"docs": "docs"}}
	if d := decide(t, withMap, "docs/other.md", task, dc.OpWrite); !d.Blocked() {
		t.Fatalf("a read-only planned file must not lend its subsystem: %+v", d)
	}
}

// A pattern whose directory part is itself a glob names a set of directories,
// and comparing that set's textual form against a real one matches by accident
// or not at all.
func TestAGlobbedDirectoryLendsNothingButAConcreteOneDoes(t *testing.T) {
	g := mapless(t)

	if d := decide(t, g, "src/a/y.go", planTask(modify("src/*/x.go")), dc.OpWrite); !d.Blocked() {
		t.Fatalf("`src/*/x.go` names no single directory and must lend none: %+v", d)
	}
	// A glob with a concrete directory does lend that directory.
	if d := decide(t, g, "src/data.json", planTask(modify("src/*.go")), dc.OpWrite); d.Blocked() {
		t.Fatalf("`src/*.go` names the directory src and should lend it: %+v", d)
	}
}

// Proximity is a fallback for a question that could not be asked, never a
// second opinion on one that was answered.
func TestProximityDoesNotOverrideAMapThatSaidNo(t *testing.T) {
	g := mapless(t)
	g.Subsystems = areas{of: map[string]string{
		"pkg/one": "one",
		"pkg/two": "two",
	}}
	task := planTask(modify("pkg/one/a.go"))

	// Different subsystem, and not a declared neighbour: refused, even though a
	// same-directory rule would never have been consulted here anyway.
	if d := decide(t, g, "pkg/two/b.go", task, dc.OpWrite); !d.Blocked() {
		t.Fatalf("the map answered no; proximity must not overturn it: %+v", d)
	}

	// The map answering yes still produces a clean allow — the fallback has not
	// displaced the rung it backs up.
	if d := decide(t, g, "pkg/one/b.go", task, dc.OpWrite); !d.Clean() {
		t.Fatalf("a same-subsystem allow must stay clean: %+v", d)
	}
}

// Both switches, and the asymmetry between them: same-dir is the neighbour
// rung's fallback, so turning the rung off turns off the fallback with it.
func TestTheRungsCanBeSwitchedOffAndSayWhichOneWas(t *testing.T) {
	task := planTask(modify("src/calc.go"))

	noSameDir := mapless(t)
	noSameDir.AllowSameDir = false
	d := decide(t, noSameDir, "src/helper.go", task, dc.OpWrite)
	if !d.Blocked() || !contains(d.Degraded, "scope.same_dir.disabled") {
		t.Fatalf("with the fallback off the write is refused and says so: %+v", d)
	}

	noNeighbors := mapless(t)
	noNeighbors.AllowNeighbors = false
	d = decide(t, noNeighbors, "src/helper.go", task, dc.OpWrite)
	if !d.Blocked() {
		t.Fatalf("with the neighbour rung off its fallback must not run: %+v", d)
	}
	if !contains(d.Degraded, "scope.neighbors.disabled") || contains(d.Degraded, "scope.same_dir.disabled") {
		t.Fatalf("the refusal must name the rung that was off, once: %+v", d.Degraded)
	}
}

// Proximity is a soft rung and reaches nothing the hard rungs decide first.
func TestProximityNeverReachesAHardRule(t *testing.T) {
	g := mapless(t)
	task := planTask(modify("src/calc.go"))

	for _, target := range []string{"src/.env", "src/deploy.key", "src/id_rsa"} {
		d := decide(t, g, target, task, dc.OpWrite)
		if !d.Blocked() || d.Severity != Hard {
			t.Errorf("%s sits beside a planned file and is still never writable: %+v", target, d)
		}
	}
}

// Forbidden changes are consulted before any scope rung, and proximity does not
// get a second look at them.
func TestForbiddenChangesBeatProximity(t *testing.T) {
	g := mapless(t)
	task := planTask(modify("src/calc.go"))
	task.ForbiddenChanges = []string{"src/legacy/**", "src/frozen.go"}

	if d := decide(t, g, "src/frozen.go", task, dc.OpWrite); !d.Blocked() || d.Rule != RuleForbiddenChange {
		t.Fatalf("a forbidden path beside a planned file stays forbidden: %+v", d)
	}
}

// --- scope an executor appended to its own task ---

func widenedTask() *dc.Task {
	t := planTask(modify("src/calc.go"))
	t.AgentAppendedPlannedFiles = []dc.PlannedFile{modify("internal/helper.go")}
	return t
}

func TestAWriteOnAppendedScopeIsAllowedAndMarked(t *testing.T) {
	d := decide(t, mapless(t), "internal/helper.go", widenedTask(), dc.OpWrite)
	if d.Blocked() {
		t.Fatalf("appended scope authorises the write: %+v", d)
	}
	if d.Widened != "internal/helper.go" {
		t.Fatalf("the write must name the appended pattern that authorised it: %+v", d)
	}
	if d.Clean() {
		t.Fatalf("a write the executor authorised for itself is never a clean pass: %+v", d)
	}
}

// The ratchet this prevents: one argued-for file must not turn its whole
// neighbourhood into scope.
func TestAppendedScopeLendsNeitherDirectoryNorSubsystem(t *testing.T) {
	g := mapless(t)
	if d := decide(t, g, "internal/other.go", widenedTask(), dc.OpWrite); !d.Blocked() {
		t.Fatalf("an appended file must not lend its directory: %+v", d)
	}

	withMap := g
	withMap.Subsystems = areas{of: map[string]string{"internal": "internal", "src": "src"}}
	if d := decide(t, withMap, "internal/other.go", widenedTask(), dc.OpWrite); !d.Blocked() {
		t.Fatalf("an appended file must not lend its subsystem: %+v", d)
	}
}

// The plan is searched first, so an executor cannot append its way past a
// decision the planner made about the same path.
func TestAppendedScopeCannotOverrideThePlan(t *testing.T) {
	task := planTask(dc.PlannedFile{Path: "docs/notes.md", AllowedChange: dc.ChangeReadOnly})
	task.AgentAppendedPlannedFiles = []dc.PlannedFile{modify("docs/notes.md")}

	d := decide(t, mapless(t), "docs/notes.md", task, dc.OpWrite)
	if !d.Blocked() || d.Rule != RuleReadOnly {
		t.Fatalf("a read-only planned file stays read-only however it is appended: %+v", d)
	}
	if d.Widened != "" {
		t.Fatalf("the plan decided this, not the widening: %+v", d)
	}
}

// A widening may widen and must never narrow. An entry appended as a modify
// would otherwise turn a later delete from scope.unplanned — which an agent may
// clear for itself — into scope.operation, which it may not.
func TestAppendedScopeNeverTakesAwayARecovery(t *testing.T) {
	d := decide(t, mapless(t), "internal/helper.go", widenedTask(), dc.OpDelete)
	if !d.Blocked() {
		t.Fatalf("a modify-only widening does not authorise a delete: %+v", d)
	}
	if d.Rule != RuleUnplannedScope {
		t.Fatalf("rule = %q, want the rule the path had before anyone appended it", d.Rule)
	}
	if !AgentGrantable(d.Rule) {
		t.Fatal("the agent must keep the recovery it had before it widened its scope")
	}

	// The planner's own entries keep their meaning: a create-only planned file
	// refuses a delete under scope.operation and does not fall through.
	planned := planTask(dc.PlannedFile{Path: "src/new.go", AllowedChange: dc.ChangeCreate})
	if d := decide(t, mapless(t), "src/new.go", planned, dc.OpDelete); d.Rule != RuleOperation {
		t.Fatalf("a planned entry decides the operation itself: %+v", d)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// Appended scope is judged by the same ladder as the plan, and the ladder puts
// the credential, restricted-path and outside-root rungs above the task
// entirely. A row that somehow carried a secret — a planner's mistake, an
// upstream tool, a hostile edit of the store — still authorises nothing.
func TestAppendedScopeReachesNoHardRule(t *testing.T) {
	task := planTask(modify("src/calc.go"))
	task.AgentAppendedPlannedFiles = []dc.PlannedFile{
		modify(".env"), modify("**"), modify(".git/config"), modify("deploy.pem"),
	}
	g := mapless(t)

	for _, target := range []string{".env", "src/.env", ".git/config", "deploy.pem", "sub/id_rsa"} {
		d := decide(t, g, target, task, dc.OpWrite)
		if !d.Blocked() {
			t.Errorf("%s must never be writable, appended or not: %+v", target, d)
		}
		if d.Severity != Hard {
			t.Errorf("%s must be refused by a rule no grant clears, got %s", target, d.Severity)
		}
		if d.Widened != "" {
			t.Errorf("%s was decided above the task rung and cannot be a widening: %+v", target, d)
		}
	}
}

// A wildcard that reached the appended column would be scope over the whole
// repository. Nothing the harness writes there can be one — every path it
// appends is quoted first — and this pins what such a row would do if one
// arrived from somewhere else: it is still bounded by the ladder, and the
// widening it grants is recorded rather than silent.
func TestAWildcardInAppendedScopeIsStillJudgedByTheLadder(t *testing.T) {
	task := planTask(modify("src/calc.go"))
	task.AgentAppendedPlannedFiles = []dc.PlannedFile{modify("**")}
	g := mapless(t)

	// It does widen — that is what the pattern says — and it can never do so
	// silently, nor reach a hard rule.
	d := decide(t, g, "anywhere/at/all.go", task, dc.OpWrite)
	if d.Widened != "**" || d.Clean() {
		t.Fatalf("a wildcard widening is still marked as one: %+v", d)
	}
	if secret := decide(t, g, "anywhere/.env", task, dc.OpWrite); !secret.Blocked() {
		t.Fatalf("a wildcard widening reaches no secret: %+v", secret)
	}
}

// TestTheRepositoryRootIsNotASubsystem.
//
// scopeDir refuses to let a path at the repository root lend scope through its
// directory, and says why: the root is where the build, the CI config and the
// dependency manifests live, so one planned file at the top level would
// otherwise make all of them writable. The subsystem rung had no equivalent
// rule. A code graph that groups top-level files under `.` — which is what a
// producer emits for them, and what this repository's own graph carries for
// verify.sh — hands that same widening to the rung above, where the guard was
// never written.
func TestTheRepositoryRootIsNotASubsystem(t *testing.T) {
	for _, root := range []string{".", "", "/"} {
		t.Run("area "+root, func(t *testing.T) {
			g := FileGate{
				Root:           "/repo",
				AllowNeighbors: true,
				AllowSameDir:   true,
				Subsystems: areas{of: map[string]string{
					"verify.sh":  root,
					"Makefile":   root,
					"go.work":    root,
					"manvi/a.go": "manvi",
				}},
			}
			// The plan authorises one top-level file. It must not authorise the
			// dependency manifest beside it.
			task := planTask(modify("verify.sh"))
			d := decide(t, g, "go.work", task, dc.OpWrite)
			if !d.Blocked() {
				t.Fatalf("a plan naming verify.sh must not lend write access to go.work "+
					"through a root `%s` subsystem: %s", root, d.Reason)
			}
		})
	}
}

// TestARealSubsystemStillLendsScope is the guard against a fix that refuses
// everything. The rung's whole purpose is the file beside the one being edited.
func TestARealSubsystemStillLendsScope(t *testing.T) {
	g := FileGate{
		Root:           "/repo",
		AllowNeighbors: true,
		AllowSameDir:   true,
		Subsystems: areas{of: map[string]string{
			"manvi/policy": "manvi/policy",
			"manvi/gate":   "manvi/gate",
		}},
	}
	task := planTask(modify("manvi/policy/file.go"))
	d := decide(t, g, "manvi/policy/scope.go", task, dc.OpWrite)
	if d.Blocked() {
		t.Fatalf("a file in the same real subsystem as a planned file must still pass: %s", d.Reason)
	}
}
