package policy

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"manvi/dc"
	"manvi/internal/fnmatch"
)

// Path pattern sets, ported verbatim from DevCouncil's policy engine. They are
// Python fnmatch patterns and are matched with those semantics — see
// internal/fnmatch for why that distinction is load-bearing.

// ProtectedWritePatterns are high-impact files: allowed, but flagged.
var ProtectedWritePatterns = []string{
	"package.json",
	"pyproject.toml",
	"uv.lock",
	"package-lock.json",
	"yarn.lock",
	"pnpm-lock.yaml",
	"Dockerfile",
	"docker-compose.yml",
	".github/workflows/*.yml",
	".github/workflows/*.yaml",
	"schema.prisma",
	"wrangler.toml",
	"index.html",
}

// SecretPathPatterns are never writable, by any task, under any grant.
//
// Every "**/x" entry needs a bare "x" beside it, and that is a rule about this
// list rather than a stylistic preference. "**/" matches one or more leading
// segments, never zero — the fnmatch parity fixture pins `**/.env  .env
// false` against Python, so the engine is right and the list must carry the
// root case itself. This is the same defect the RestrictedPathPatterns comment
// below describes for ".git".
//
// It had drifted: ".env", "id_rsa", "id_ed25519", ".npmrc", ".pypirc" and
// ".netrc" had their bare twins and "secrets/", "credentials/", "*.pem",
// "*.key", "id_dsa" and "id_ecdsa" did not, so a write to ./server.key was
// allowed while sub/server.key was refused. A pattern added here without its
// bare form is a hole at the repository root, which is where a checkout puts
// deploy keys.
var SecretPathPatterns = []string{
	".env",
	".env.*",
	"**/.env",
	"**/.env.*",
	"credentials/**",
	"**/credentials/**",
	"secrets/**",
	"**/secrets/**",
	// "*" crosses "/" in this engine, as it does in Python's fnmatch, so these
	// two subsume the "**/*.pem" and "**/*.key" forms they replace.
	"*.pem",
	"*.key",
	"**/id_rsa",
	"**/id_dsa",
	"**/id_ecdsa",
	"**/id_ed25519",
	"id_rsa",
	"id_dsa",
	"id_ecdsa",
	"id_ed25519",
	".npmrc",
	"**/.npmrc",
	".pypirc",
	"**/.pypirc",
	".netrc",
	"**/.netrc",
}

// RestrictedPathPatterns protect the repository's own machinery — including
// the agent client configs, so an agent cannot disarm the gate that is
// checking it.
var RestrictedPathPatterns = []string{
	// These two are a deliberate addition to the incumbent's list. In a git
	// worktree or a submodule, ".git" is a *file* holding "gitdir: ...", and
	// rewriting it repoints the repository at another one — which is the whole
	// of what this rung protects, achieved without touching ".git/". Python's
	// rule is a glob plus a prefix test, and ".git".startswith(".git/") is
	// false, so the incumbent misses this. Matching it is strictly safer.
	//
	// Only these two ever got a bare twin by hand, and the omission was the
	// hole: ".claude", ".codex", ".cursor", ".gemini", ".grok", ".opencode"
	// and ".agents" carried "/*" and "/**" forms only, so a fresh checkout
	// could take a *file* named ".claude" and pre-empt the config directory
	// this rung exists to protect. Hand-added twins fix the cases; the rule
	// they state belongs to matchesRestricted, which now compares whole path
	// components and therefore covers the bare form of every entry here,
	// including any added later.
	".git",
	".devcouncil",
	".git/*",
	".devcouncil/*",
	".claude/*",
	".claude/**",
	".codex/*",
	".codex/**",
	".cursor/*",
	".cursor/**",
	".gemini/*",
	".gemini/**",
	".grok/*",
	".grok/**",
	".opencode/*",
	".opencode/**",
	".agents/*",
	".agents/**",
	"opencode.json",
}

// SubsystemMap answers the two questions the neighbour rule needs. It is an
// interface so its absence is a reported degradation rather than a swallowed
// exception: the Python implementation wraps the whole neighbour lookup in a
// bare `except Exception: return None`, which turns an unreadable repo map into
// a silent fall-through to deny.
type SubsystemMap interface {
	// AreaForPath returns the subsystem owning a path, and whether one was found.
	AreaForPath(path string) (string, bool)
	// AreNeighbors reports whether two subsystems are declared neighbours.
	AreNeighbors(a, b string) bool
	// NeighborsArePermissive reports whether the neighbour relation covers so
	// much of the repository that the rung is close to allowing everything.
	//
	// It is on the interface, and not an optional one discovered by type
	// assertion, because the answer must never be absent by default. A map that
	// silently declined to answer would let every allow look tight, which is
	// the exact condition this exists to expose.
	NeighborsArePermissive() bool
}

// FileGate evaluates file writes against a task's declared scope.
type FileGate struct {
	Root string
	// Subsystems may be nil. When it is, the neighbour rung cannot run and
	// every decision that would have consulted it is marked Degraded.
	Subsystems SubsystemMap
	// AllowNeighbors mirrors flags.PolicyNeighborScope.
	AllowNeighbors bool
	// AllowSameDir mirrors flags.PolicyScopeSameDir. It only ever matters when
	// AllowNeighbors is on: the same-directory rung is the neighbour rung's
	// fallback, not a second, independent way to widen scope, so switching the
	// rung off switches off what it falls back to.
	AllowSameDir bool
	// HardRules mirrors flags.PolicyHardRules. False is reported, never silent.
	HardRules bool
}

// EvaluateFileChange walks DevCouncil's decision ladder in its original order.
// Order is the contract: the secret-path rung must run before the task rung, or
// a task could authorise a write to .env by listing it as a planned file.
func (g FileGate) EvaluateFileChange(path string, task *dc.Task, op dc.Operation, internal bool) Decision {
	normalized, outside := NormalizeRepoPath(g.Root, path)
	taskID := ""
	if task != nil {
		taskID = task.ID
	}

	if g.HardRules {
		// Checked on the raw input and again on the normalized result: a ".."
		// segment can promote an interior component to the final one, so
		// "a /b/.." resolves to the aliasing path "a " from an input whose own
		// last component is harmless.
		if reason, bad := malformedPath(path); bad {
			return deny(RuleMalformedPath, reason, "", taskID)
		}
		if reason, bad := malformedPath(normalized); bad {
			return deny(RuleMalformedPath, reason, normalized, taskID)
		}
		if outside {
			return deny(RuleOutsideRoot, "Path is outside the project root.", normalized, taskID)
		}
		// Case-folded: on APFS and NTFS ".ENV" and ".env" are one file, so a
		// case-sensitive check would read the pattern and still allow the write.
		if fnmatch.MatchAnyFold(SecretPathPatterns, normalized) {
			return deny(RuleSecretPath, "Secret and credential paths are never writable.", normalized, taskID)
		}
		if !internal && matchesRestricted(normalized) {
			return deny(RuleRestrictedPath, "Protected repository paths cannot be modified.", normalized, taskID)
		}
	}

	if task == nil {
		d := deny(RuleNoTask, "No running DevCouncil task authorizes this file write.", normalized, "")
		return g.noteHardRules(d)
	}

	// Folded like the secret and restricted rungs above: ".ENV and .env are
	// one file" on the case-insensitive filesystems this harness ships on,
	// and a prohibition the planner wrote is voided by a case change unless
	// the match folds too. (The planned-file match below stays
	// case-sensitive on purpose: its failure direction is denial, which is
	// safe, and folding it would let a differently-cased path claim another
	// entry's authorisation.)
	if fnmatch.MatchAnyFold(task.ForbiddenChanges, normalized) {
		return g.noteHardRules(deny(RuleForbiddenChange, "Path is listed in forbidden_changes.", normalized, task.ID))
	}
	if entry, aliased := forbiddenAlias(g.Root, normalized, task.ForbiddenChanges); aliased {
		return g.noteHardRules(deny(RuleForbiddenChange,
			"Path is another name for `"+entry+"`, which is listed in forbidden_changes.",
			normalized, task.ID))
	}

	planned, widened := plannedFileFor(normalized, task)
	if planned == nil {
		return g.noteHardRules(g.unplannedDecision(normalized, task))
	}

	if refusal, refused := operationRefusal(planned, op, normalized, task); refused {
		// Scope an executor appended to its own task may widen what that
		// executor can reach; it must never narrow it. Without this, appending
		// "src/a.go" as a modify would turn a later *delete* of the same path
		// from scope.unplanned — soft, and on the agent-grantable list — into
		// scope.operation, which is soft but which no agent may clear. The
		// widening would have taken away a recovery the agent had before it.
		//
		// So an appended entry that does not cover the operation stands aside
		// and the path is judged as what it was before anyone appended it. The
		// planner's entries do not get this treatment: a read-only file, or one
		// planned for creation only, is a decision someone made on purpose, and
		// the whole point of the search order above is that an appended entry
		// cannot reach past it.
		if widened != "" {
			return g.noteHardRules(g.unplannedDecision(normalized, task))
		}
		return g.noteHardRules(refusal)
	}

	// Folded like the hard rungs above, and for the same reason: on APFS and
	// NTFS "PACKAGE.JSON" and "package.json" are one file, so a case-sensitive
	// match here read the pattern list and still let the write through with no
	// warning and no verification gate — the rung silently absent for exactly
	// the spelling an agent would reach for to make it absent. Over-blocking a
	// file that differs from a manifest only by case costs a warning, which is
	// all this rung ever produces.
	if fnmatch.MatchAnyFold(ProtectedWritePatterns, normalized) {
		d := warn(RuleProtectedWrite,
			normalized+" is a protected high-impact file; verification gates must approve it.", normalized, task.ID)
		d.Widened = widened
		return g.noteHardRules(d)
	}

	if widened != "" {
		d := allow("File change is allowed by scope task "+task.ID+
			" appended at runtime (`"+widened+"`), not by the plan it was created with.",
			normalized, task.ID)
		d.Widened = widened
		return g.noteHardRules(d)
	}
	return g.noteHardRules(allow("File change is allowed.", normalized, task.ID))
}

// operationRefusal reports whether a planned entry forbids the operation being
// attempted, and the denial that says so.
//
// Split out because the answer is consumed twice — once to refuse, and once to
// decide whether an appended entry should stand aside — and a second copy of
// this ladder rung would be a second thing to keep in step with DevCouncil.
func operationRefusal(planned *dc.PlannedFile, op dc.Operation, path string, task *dc.Task) (Decision, bool) {
	if planned.AllowedChange == dc.ChangeReadOnly {
		return deny(RuleReadOnly, "Planned file is read-only.", path, task.ID), true
	}
	if op == dc.OpWrite {
		if planned.AllowedChange != dc.ChangeCreate && planned.AllowedChange != dc.ChangeModify {
			return deny(RuleOperation,
				"Operation "+string(op)+" not allowed for planned file.", path, task.ID), true
		}
		return Decision{}, false
	}
	if string(planned.AllowedChange) != string(op) {
		return deny(RuleOperation,
			"Operation "+string(op)+" not allowed for planned file.", path, task.ID), true
	}
	return Decision{}, false
}

// unplannedDecision is the soft-block rung: a write outside the plan is allowed
// when it lands close enough to it — in the same subsystem as a planned file,
// in a declared neighbour of one, or, when the repo map cannot say, in the same
// directory as one.
//
// Only the *plan* lends its neighbourhood, never scope an executor appended to
// itself. Otherwise the widening ratchets: an agent argues one file into scope,
// and that file's subsystem — or directory — becomes writable, which is a
// larger permission than the one anybody granted it.
func (g FileGate) unplannedDecision(path string, task *dc.Task) Decision {
	base := deny(RuleUnplannedScope,
		"Task "+task.ID+" does not authorize changes to "+path+".", path, task.ID)

	if !g.AllowNeighbors {
		base.Degraded = append(base.Degraded, "scope.neighbors.disabled")
		return base
	}
	if g.Subsystems == nil {
		// The Python engine returns None here and falls through to deny. Same
		// outcome, but recorded: a deny reached without the map is not evidence
		// that the path is outside the plan's neighbourhood.
		base.Degraded = append(base.Degraded, "repo_map.unavailable")
		return g.sameDirDecision(path, task, base)
	}

	targetArea, ok := g.Subsystems.AreaForPath(path)
	if !ok || targetArea == "" {
		base.Degraded = append(base.Degraded, "subsystem.unknown_for_target")
		return g.sameDirDecision(path, task, base)
	}
	if isRootArea(targetArea) {
		base.Degraded = append(base.Degraded, "scope.root_not_a_subsystem")
		return g.sameDirDecision(path, task, base)
	}

	plannedAreas := make([]string, 0, len(task.PlannedFiles))
	seen := map[string]bool{}
	droppedRoot := false
	for _, pf := range task.PlannedFiles {
		// A read-only entry says "look at this, do not change it". Lending its
		// subsystem as writable would turn that into a permission over every
		// file beside it, which is the opposite of what it declares.
		if pf.AllowedChange == dc.ChangeReadOnly {
			continue
		}
		area, ok := g.Subsystems.AreaForPath(normalizeSlashes(pf.Path))
		if !ok || area == "" || seen[area] {
			continue
		}
		if isRootArea(area) {
			// A planned file at the root lends nothing, for the reason
			// isRootArea states. Recorded so the refusal below names the rule
			// rather than reporting a map that answered as one that did not.
			droppedRoot = true
			continue
		}
		seen[area] = true
		plannedAreas = append(plannedAreas, area)
	}
	if len(plannedAreas) == 0 {
		if droppedRoot {
			base.Degraded = append(base.Degraded, "scope.root_not_a_subsystem")
		} else {
			base.Degraded = append(base.Degraded, "subsystem.unknown_for_planned")
		}
		return g.sameDirDecision(path, task, base)
	}

	for _, area := range plannedAreas {
		if area == targetArea {
			d := allow("File is in the same subsystem as a planned file.", path, task.ID)
			if g.Subsystems.NeighborsArePermissive() {
				// The map reports its own rungs as near-total, and this one
				// degenerates faster than the neighbour rung below it: a graph
				// holding a single named area puts every indexed file in "the
				// same subsystem as a planned file", so the question has one
				// possible answer and every unplanned write was recorded as a
				// scope check that ran and passed. isRootArea already covers a
				// whole-repo area spelled ".", which is why the named-single-area
				// shape survived — same condition, a spelling the guard did not
				// recognise.
				//
				// Its own marker, not the neighbour one: the neighbour relation
				// did not decide this allow, and a signal that names the wrong
				// rung is a signal an operator learns to discount. Recorded
				// rather than refused, for the reason the neighbour rung states.
				d.Degraded = append(d.Degraded, "scope.subsystem.permissive")
			}
			return d
		}
	}
	for _, area := range plannedAreas {
		if g.Subsystems.AreNeighbors(targetArea, area) {
			d := allow("File is in a neighboring subsystem of a planned file.", path, task.ID)
			if g.Subsystems.NeighborsArePermissive() {
				// The rung ran and said yes, and in this repository it says yes
				// to nearly everything: the widest area neighbours at least
				// half the map, so "is the target next door to the plan" is a
				// question with almost one answer.
				//
				// Recorded rather than refused. The relation is a fact about
				// how the repository is coupled, not a misconfiguration, and
				// denying on it would break every task in a densely connected
				// tree. What must not happen is this allow reading like a
				// scope check that meant something — `manvi map status` has
				// been reporting the condition all along, to an operator who
				// would have to know to go and look.
				d.Degraded = append(d.Degraded, "scope.neighbors.permissive")
			}
			return d
		}
	}

	// The map answered, and its answer was no. Proximity is a fallback for a
	// question that could not be asked, never a second opinion on one that was.
	base.Reason = "Task " + task.ID + " does not authorize changes to " + path +
		" (subsystem `" + targetArea + "` is outside planned files and not a declared neighbor). " +
		"Expand scope with `dev scope update`."
	return base
}

// sameDirDecision is the neighbour rung's fallback: with no repo map to consult,
// a write is allowed when it lands in the same directory as a file the plan
// authorises changing.
//
// This exists because the alternative is a cliff. Without a built repo map the
// subsystem rung cannot run at all, so *every* write outside the enumerated
// paths is refused — including the test file beside the file being fixed, and
// the helper it calls. The map is an artifact a repository may simply not have
// built, and "we have no map" is not evidence that a sibling file is out of
// scope. A directory is the coarsest honest stand-in for a subsystem: it is
// what the repository itself groups by, and it is bounded by construction.
//
// Two limits keep it a fallback rather than a hole. The repository root is not a
// directory for this purpose — it is where the build, the CI config, and the
// dependency manifests live, and one planned file at the top level would
// otherwise lend writability to all of them. And the allow keeps the degradation
// that brought it here, so a run that leaned on proximity can never be reported
// as one where the scope rules were fully checked.
func (g FileGate) sameDirDecision(path string, task *dc.Task, base Decision) Decision {
	if !g.AllowSameDir {
		base.Degraded = append(base.Degraded, "scope.same_dir.disabled")
		return base
	}

	dir, ok := scopeDir(path)
	if !ok {
		base.Reason = "Task " + task.ID + " does not authorize changes to " + path +
			", the repo map that would place it in a subsystem is unavailable, and a path " +
			"at the repository root has no directory that could stand in for one. " +
			"Expand scope with `dev scope update`."
		return base
	}

	for _, pf := range task.PlannedFiles {
		if pf.AllowedChange == dc.ChangeReadOnly {
			continue
		}
		plannedDir, ok := scopeDir(pf.Path)
		if !ok || plannedDir != dir {
			continue
		}
		allowed := allow("File is in the same directory (`"+dir+"`) as planned file `"+
			pf.Path+"`; the repo map that would place it in a subsystem is unavailable.",
			path, task.ID)
		// The reason the map could not answer travels onto the allow. A pass
		// reached by proximity is not the pass the subsystem rung would have
		// produced, and nothing downstream may treat the two as one.
		allowed.Degraded = base.Degraded
		return allowed
	}

	base.Reason = "Task " + task.ID + " does not authorize changes to " + path +
		" (no planned file is writable in `" + dir + "`, and the repo map that would " +
		"place it in a subsystem is unavailable). Expand scope with `dev scope update`."
	return base
}

// scopeDir is the directory a path lends or borrows scope through, and whether
// it has one at all.
//
// It has none in two cases. A path at the repository root gets no directory, for
// the reason sameDirDecision states. A pattern whose directory part still holds
// glob metacharacters — `src/*/main.go` — names a set of directories rather than
// one, and comparing that set's textual form against a real directory would
// either never match or match by accident.
// isRootArea reports whether a subsystem is the repository root.
//
// This is scopeDir's rule applied one rung higher, and it was missing there.
// scopeDir refuses to let a path at the root lend scope through its directory
// because the root is where the build, the CI config and the dependency
// manifests live. A code graph groups top-level files under `.` — this
// repository's own graph does, for verify.sh — which offers the subsystem rung
// exactly that widening under a different name, past a guard that only ever
// existed in the fallback beneath it. One planned file at the top level would
// have made every other one writable, and the allow would have read
// `File is in the same subsystem as a planned file.`
func isRootArea(area string) bool {
	switch strings.TrimSpace(normalizeSlashes(area)) {
	case "", ".", "/", "./":
		return true
	}
	return false
}

func scopeDir(p string) (string, bool) {
	normalized := normalizeSlashes(p)
	for strings.HasPrefix(normalized, "./") {
		normalized = normalized[2:]
	}
	idx := strings.LastIndex(normalized, "/")
	if idx <= 0 {
		return "", false
	}
	dir := normalized[:idx]
	if strings.ContainsAny(dir, "*?[") {
		return "", false
	}
	return dir, true
}

// noteHardRules marks any decision reached while the hard rules were disabled.
// Turning them off is a startup-only, human-only act, and every decision made
// under that configuration says so.
func (g FileGate) noteHardRules(d Decision) Decision {
	if !g.HardRules {
		d.Degraded = append(d.Degraded, "policy.hard_rules.disabled")
	}
	return d
}

// malformedPath rejects paths whose textual form and whose meaning to the
// kernel are not the same thing. A NUL terminates the path at every syscall
// boundary, so "src/a.go\x00.env" is matched against the secret patterns as one
// string and opened as another. Nothing legitimate contains one.
func malformedPath(raw string) (string, bool) {
	// Control characters are refused as a class, and NUL is named separately
	// because its failure mode is the worst: it terminates the path at every
	// syscall boundary, so "src/a.go\x00.env" is matched against the secret
	// patterns as one string and opened as another.
	//
	// The rest are refused for two reasons that reinforce each other. A newline
	// in a filename aliases — surrounding whitespace is trimmed from a path on
	// its way in, so "a\n" and "a" become the same string on a second pass but
	// name different files — and it also breaks every line-oriented consumer
	// downstream: the diff parser that finds the changed files, the
	// newline-delimited transcript, the terminal renderer. Nothing legitimate
	// has one.
	if i := strings.IndexFunc(raw, unicode.IsControl); i >= 0 {
		if raw[i] == 0 {
			return "Path contains a NUL byte, which the filesystem would truncate at.", true
		}
		return "Path contains the control character " + strconv.QuoteRune(rune(raw[i])) +
			", which aliases between names and breaks every line-oriented consumer of this path.", true
	}
	// Whitespace padding or a trailing dot on any component aliases to a
	// different file. Windows strips both when opening, so ".env " and ".env"
	// are one file there and two here; and this normalizer itself trims
	// surrounding whitespace, so " a" and "a" collapse to one string on a
	// second pass while naming two files on disk. That is the same class of bug
	// as the case-variant bypass — a check that is a statement about strings
	// rather than about files — and nothing legitimate has one, so it is
	// refused rather than normalised into something that looks valid.
	cleaned := normalizeSlashes(cleanInput(raw))
	for _, component := range strings.Split(cleaned, "/") {
		if component == "." || component == ".." || component == "" {
			continue
		}
		if trimmed := strings.TrimSpace(component); trimmed != component {
			return "Path component " + strconv.Quote(component) +
				" is padded with whitespace, which aliases: this path is cleaned of surrounding " +
				"whitespace on its way in, so it and its trimmed form would be one string here " +
				"and two files on disk.", true
		}
		if component != strings.TrimRight(component, ".") {
			return "Path component " + strconv.Quote(component) +
				" ends in a dot, which some filesystems strip when opening — " +
				"it would not name the file the check examined.", true
		}
	}
	return "", false
}

// cleanInput strips the wrapping a path may arrive with — surrounding
// whitespace, and a matched pair of surrounding quotes — until it settles.
//
// Two defects a fuzzer found are fixed here, and both had the same shape: the
// function did not settle, so the same input normalized to two different paths
// on successive calls, and a normalizer that does not settle is one where the
// matcher and the filesystem can be looking at different strings.
//
//   - Whitespace was trimmed before quotes, so a quote could hide trailing
//     whitespace that then survived. Hence the loop.
//   - Quotes were stripped with strings.Trim, which removes them from either
//     end independently — so a path legitimately ending in a quote character
//     lost it, even though nothing was wrapping it. Only a matched pair is
//     unwrapping; a lone quote is part of the name.
func cleanInput(raw string) string {
	for {
		trimmed := strings.TrimSpace(raw)
		if len(trimmed) >= 2 && trimmed[0] == '"' && trimmed[len(trimmed)-1] == '"' {
			trimmed = trimmed[1 : len(trimmed)-1]
		}
		if trimmed == raw {
			return trimmed
		}
		raw = trimmed
	}
}

// NormalizeRepoPath resolves raw against root and reports containment.
//
// It returns (posix path relative to root, isOutsideRoot). Callers fail closed
// on the second value: anything that escapes the project — an absolute path
// elsewhere, a ".." traversal, a symlink pointing out — is outside.
func NormalizeRepoPath(root, raw string) (string, bool) {
	// A NUL terminates the path at every syscall boundary, so the kernel and
	// the pattern matcher would be looking at different strings: "a.go\x00.env"
	// matches no secret pattern and opens ".env"-adjacent paths depending on the
	// caller. Nothing legitimate contains one, so it is refused as uncontained
	// rather than sanitised into something that looks valid.
	if _, bad := malformedPath(raw); bad {
		// Defence in depth for the caller that does not run the ladder: a path
		// whose text and whose meaning to the kernel differ is never contained.
		return "", true
	}
	cleaned, settled := settlePath(normalizeSlashes(raw))
	if !settled {
		// A path whose normalisation does not converge is one where this
		// function and the filesystem can be looking at different strings, which
		// is the whole failure this normalizer exists to prevent. Refused as
		// uncontained rather than used at whatever round the loop gave up on.
		return "", true
	}

	rootResolved, rootOK := resolveExisting(root)
	if !rootOK {
		// Without a resolved root there is nothing to be contained *by*, so
		// every answer below would be a guess. Uncontained is the honest one.
		return "", true
	}

	var candidate string
	if filepath.IsAbs(cleaned) {
		candidate = cleaned
	} else {
		candidate = filepath.Join(rootResolved, cleaned)
	}
	resolved, resolvedOK := resolveExisting(candidate)
	if !resolvedOK {
		return "", true
	}

	rel, err := filepath.Rel(rootResolved, resolved)
	if err != nil {
		return strings.TrimPrefix(cleaned, "./"), true
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return filepath.ToSlash(resolved), true
	}
	if rel == "." {
		return "", true
	}
	return rel, false
}

// settlePath applies the two normalisations to a fixed point, and reports
// whether it reached one.
//
// Neither step is idempotent with respect to the other, so a fixed number of
// rounds is a guess about how many an input needs. Running each of them once,
// then once more, was such a guess, and a fuzzer found the input that needs a
// third: `""0"/"/` has its trailing slash removed by Clean, which exposes a
// quote pair that the second cleanInput unwraps to `"0"/` — still holding a
// trailing slash, and still holding quotes. It normalised to `"0"`, and `"0"`
// normalised again to `0`: two spellings the gate would evaluate as two
// different files while the filesystem opens one.
//
// Iterating removes the guess. The bound is a backstop, not the mechanism —
// each round can only shorten the string or leave it unchanged, so convergence
// is quick — and exhausting it is reported rather than swallowed, because the
// caller must fail closed on a path that will not settle.
func settlePath(raw string) (string, bool) {
	cleaned := raw
	for i := 0; i < 16; i++ {
		next := filepath.ToSlash(filepath.Clean(cleanInput(cleaned)))
		if next == cleaned {
			return cleaned, true
		}
		cleaned = next
	}
	return "", false
}

// resolveExisting resolves symlinks as far up the path as actually exists,
// then reattaches the remainder. Go's EvalSymlinks fails outright on a path
// that does not exist yet, and a write gate is asked about new files
// constantly — but skipping symlink resolution entirely would let a symlinked
// directory carry a write out of the repository.
// The second return value is whether the path could be resolved at all.
// Callers fail closed on false: a path this function could not follow is one
// where it and the kernel may be looking at different files, which is the whole
// condition it exists to prevent.
//
// A component that does not resolve is not automatically a component that does
// not exist, and conflating the two was a hole. EvalSymlinks reports ENOENT for
// a *dangling* symlink — one whose target is missing — exactly as it does for a
// name that was never created. The old loop peeled such a component off and
// reattached it literally, which discarded the link and answered about the
// link's own name rather than where it points: `notes.md ->
// /elsewhere/authorized_keys` normalised to `notes.md`, was reported contained,
// and the write that *created* the target landed outside the repository. Only a
// second write, once the target existed, was refused — so the escaping write was
// always the first one. Git carries dangling symlinks in a tree, so a clone can
// deliver this.
func resolveExisting(p string) (string, bool) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	cur := abs
	rest := ""
	// One bound for both kinds of step: peeling a component and following a
	// link. Real paths are bounded by the OS and link chains by ELOOP, so
	// exhausting this means the input was built to exhaust it — reported rather
	// than answered at whatever round the loop gave up on.
	for steps := 0; steps < 256; steps++ {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			if rest == "" {
				return resolved, true
			}
			return filepath.Join(resolved, rest), true
		}
		// Dangling link: follow it by hand and keep resolving from its target,
		// carrying whatever tail has already been peeled.
		if fi, lerr := os.Lstat(cur); lerr == nil && fi.Mode()&fs.ModeSymlink != 0 {
			target, rerr := os.Readlink(cur)
			if rerr != nil {
				return "", false
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(cur), target)
			}
			cur = filepath.Clean(target)
			continue
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return filepath.Clean(abs), true
		}
		if rest == "" {
			rest = filepath.Base(cur)
		} else {
			rest = filepath.Join(filepath.Base(cur), rest)
		}
		cur = parent
	}
	return "", false
}

// NormalizePlannedCandidate strips the leading "./" the way write
// authorization does, so both surfaces match the same string.
func NormalizePlannedCandidate(path string) string {
	normalized := normalizeSlashes(path)
	for strings.HasPrefix(normalized, "./") {
		normalized = normalized[2:]
	}
	return normalized
}

// MatchesPlannedPath reports whether path matches a planned file exactly or by
// glob. CLI and MCP surfaces in DevCouncil drifted on this once; one helper is
// the fix, so the harness uses one too.
func MatchesPlannedPath(path string, planned []dc.PlannedFile) bool {
	normalized := NormalizePlannedCandidate(path)
	for _, pf := range planned {
		p := normalizeSlashes(pf.Path)
		if normalized == p || fnmatch.Match(p, normalized) {
			return true
		}
	}
	return false
}

// plannedFileFor finds the entry that authorises a path, and reports the
// appended pattern when the entry came from scope an executor added to its own
// task rather than from the plan.
//
// The plan is searched first, and the order is the rule rather than an
// optimisation. An appended entry must never reach past what the planner said
// about the same path: a file planned read-only stays read-only however many
// times an agent appends it, because the read-only entry is found first and
// decides.
func plannedFileFor(path string, task *dc.Task) (*dc.PlannedFile, string) {
	if pf := matchPlannedFile(path, task.PlannedFiles); pf != nil {
		return pf, ""
	}
	if pf := matchPlannedFile(path, task.AgentAppendedPlannedFiles); pf != nil {
		return pf, pf.Path
	}
	return nil, ""
}

func matchPlannedFile(path string, planned []dc.PlannedFile) *dc.PlannedFile {
	for i := range planned {
		p := normalizeSlashes(planned[i].Path)
		if path == p || fnmatch.Match(p, path) {
			return &planned[i]
		}
	}
	return nil
}

// forbiddenAlias reports which forbidden_changes entry names the same *file*
// as path under a different name, and whether one does.
//
// The pattern rung above compares strings, and a filesystem hands out more than
// one string for the same file. APFS is normalisation-insensitive as well as
// case-insensitive, so "café.txt" spelled NFC and spelled NFD are two strings
// and one file: a prohibition written in one form was cleared by submitting the
// other, and the matcher saw a name it had never been told about while the
// kernel opened the file it had. A hard link does the same thing for pure
// ASCII, and so does any spelling a future filesystem decides to fold.
//
// Normalising the strings would need Unicode tables this module does not carry,
// and it would be the wrong answer anyway on a normalisation-sensitive
// filesystem, where the two spellings really are two files and refusing the
// second would be an over-refusal. Asking the kernel which file each name opens
// is exact on both kinds of filesystem. It costs one stat per glob-free entry,
// and only for a task that declared a prohibition at all.
//
// Two bounds, stated rather than hidden. A glob entry names a set, not a file,
// so resolving one would mean walking the repository on every write; glob
// entries are left to the pattern rung. And a prohibition on a file that does
// not exist yet has no identity to compare against, so the alias spelling can
// still create it — the rung protects a file, and there is not yet a file.
func forbiddenAlias(root, path string, forbidden []string) (string, bool) {
	if root == "" || path == "" || len(forbidden) == 0 {
		return "", false
	}
	target, err := os.Stat(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		return "", false
	}
	for _, raw := range forbidden {
		entry := NormalizePlannedCandidate(raw)
		if entry == "" || entry == path || strings.ContainsAny(entry, "*?[") {
			continue
		}
		if filepath.IsAbs(entry) || entry == ".." || strings.HasPrefix(entry, "../") ||
			strings.Contains(entry, "/../") || strings.HasSuffix(entry, "/..") {
			// An entry that leaves the repository names nothing this gate is
			// responsible for; stat'ing it would only widen what a planner
			// string can reach.
			continue
		}
		fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(entry)))
		if err != nil {
			continue
		}
		if os.SameFile(fi, target) {
			return raw, true
		}
	}
	return "", false
}

func matchesAny(patterns []string, path string) bool {
	for _, raw := range patterns {
		p := normalizeSlashes(raw)
		if path == p || fnmatch.Match(p, path) {
			return true
		}
	}
	return false
}

// matchesRestricted mirrors the Python rule, which tests both a glob match and
// a prefix match against the pattern with its stars stripped — so ".git/*"
// also catches a bare ".git" and everything beneath it.
//
// The prefix half compares whole path components, and that is the fix for two
// defects that were the same defect. Python's rule is `path.startswith(pattern
// .strip("*"))`, a test on characters: ".git" is a character prefix of
// ".gitignore", ".gitattributes", ".gitmodules" and ".github/workflows/ci.yml",
// none of which are the repository's own machinery, and every one of them was
// denied Hard. That also made the two ".github/workflows/*" entries in
// ProtectedWritePatterns dead code — a rung that runs earlier can never let
// them fire — so the "allowed, but flagged" behaviour those entries document
// did not exist, and a routine CI edit was unreachable rather than reviewed.
//
// In the other direction the character test needed the pattern to end in "/"
// to cover a subtree, which meant it never covered the directory itself:
// ".claude/*" stripped to ".claude/", and ".claude".startswith(".claude/") is
// false. Bare ".git" and ".devcouncil" were added to the list by hand to
// paper over that; the other seven agent-config entries were not. Trimming the
// separator and comparing components states the rule once, for every entry.
func matchesRestricted(path string) bool {
	lower := strings.ToLower(path)
	for _, pattern := range RestrictedPathPatterns {
		if fnmatch.MatchFold(pattern, path) {
			return true
		}
		// Folded here for the same filesystem reason as the glob above.
		prefix := strings.ToLower(strings.TrimSuffix(strings.Trim(pattern, "*"), "/"))
		if prefix == "" {
			continue
		}
		if lower == prefix || strings.HasPrefix(lower, prefix+"/") {
			return true
		}
	}
	return false
}

func normalizeSlashes(p string) string { return strings.ReplaceAll(p, `\`, "/") }
