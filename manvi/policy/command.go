package policy

import (
	"regexp"
	"strings"

	"manvi/dc"
	"manvi/internal/fnmatch"
)

// Git-safety patterns, ported from DevCouncil. Compiled once: they are checked
// on every shell command the harness runs.
var (
	hardResetProtectedRe   = regexp.MustCompile(`\bgit\s+reset\s+--hard\s+(origin/)?(main|master)\b`)
	forcePushFlagRe        = regexp.MustCompile(`\bgit\s+push\b.*(\s--force(-with-lease)?\b|\s-f\b)`)
	forcePushPlusRefspecRe = regexp.MustCompile(`\bgit\s+push\s+\S+\s+\+\S`)
	protectedBranchPushRe  = regexp.MustCompile(`\bgit\s+push\s+\S+\s+((head:)?(main|master)|(main|master):\S+)\b`)

	uvRunDirFlagRe = regexp.MustCompile(`^(uv\s+run)((?:\s+(?:--project|--directory|-p)\s+\S+)+)(\s+.+)$`)
	cdSegmentRe    = regexp.MustCompile(`^(?:cd|pushd|popd)(\s|$)`)
	// Trailing shell redirections break glob matching against patterns like
	// "dev map *". Stripped for matching only, in a loop, so the pattern itself
	// stays single-clause and cannot backtrack pathologically.
	redirectTailRe = regexp.MustCompile(`(?: \d*(?:>>|>|<|&>|&>>) ?\S+| \d*>&\d+)$`)
)

// devBinaries are the executable names normalised back to a bare "dev", so a
// hook-installed ".venv/bin/dev map" still matches the "dev map" allowlist
// entry instead of failing closed.
var devBinaries = map[string]bool{
	"dev":            true,
	"dev.exe":        true,
	"devcouncil":     true,
	"devcouncil.exe": true,
}

// NoTaskAllowedCommands run without any lease: orientation and the bootstrap
// commands an agent needs in order to acquire one in the first place.
var NoTaskAllowedCommands = []string{
	"dev status", "dev status *",
	"uv run dev status", "uv run dev status *",
	"dev tasks", "dev tasks *",
	"uv run dev tasks", "uv run dev tasks *",
	"dev approve", "dev approve *",
	"uv run dev approve", "uv run dev approve *",
	"dev checkout *", "uv run dev checkout *",
	"dev next-task", "dev next-task *",
	"uv run dev next-task", "uv run dev next-task *",
	"git status", "git diff", "git diff *",
	"echo", "echo *", "true", ":",
}

// LeaseLifecycleAllowedCommands are always available to a lease holder,
// independent of the task's own allowed_commands.
var LeaseLifecycleAllowedCommands = []string{
	"dev release *", "uv run dev release *",
	"dev lease *", "uv run dev lease *",
	"dev scope *", "uv run dev scope *",
	"dev map", "dev map *",
	"uv run dev map", "uv run dev map *",
	"dev doctor", "dev doctor *",
	"uv run dev doctor", "uv run dev doctor *",
	"dev graph", "dev graph *",
	"uv run dev graph", "uv run dev graph *",
	"dev run-cmd *", "uv run dev run-cmd *",
	"python -m pytest *", "uv run python -m pytest *",
	"uv run pytest *", "pytest *",
}

// CommandGate evaluates shell commands against a task's allowlist and against
// the git-safety rules.
type CommandGate struct {
	// GlobalAllowedCommands supplement every task's own allowlist.
	GlobalAllowedCommands []string
	// HardRules mirrors flags.PolicyHardRules.
	HardRules bool
}

// EvaluateCommand walks DevCouncil's command ladder.
// EvaluateCommand walks DevCouncil's command ladder.
//
// Git safety runs first and unconditionally. In the Python engine the two
// evaluations are separate entry points — `evaluate_command` for the allowlist
// and `evaluate_hook_command` for git safety — which means a caller that only
// invokes one gets only half the protection. Folding safety into the front of
// the single entry point closes that, and it cannot loosen anything: every
// git-safety outcome is a denial or a warning, never an allow that skips the
// allowlist below.
func (g CommandGate) EvaluateCommand(command string, task *dc.Task) Decision {
	parts := SplitCommandChain(command)
	if len(parts) > 1 {
		var warnDecision *Decision
		for _, part := range parts {
			d := g.evaluateSingleCommand(part, task)
			if d.Action == Deny {
				return d
			}
			if d.Action == Warn && warnDecision == nil {
				warnDecision = &d
			}
		}
		if warnDecision != nil {
			return *warnDecision
		}
		taskID := ""
		if task != nil {
			taskID = task.ID
		}
		return g.noteHardRules(allow("Compound command allowed.", command, taskID))
	}
	return g.evaluateSingleCommand(command, task)
}

func (g CommandGate) evaluateSingleCommand(command string, task *dc.Task) Decision {
	raw := collapseSpaces(command)
	normalized := NormalizeAllowlistCommand(command)
	taskID := ""
	if task != nil {
		taskID = task.ID
	}

	if normalized == "" {
		return g.noteHardRules(deny(RuleCommandEmpty, "Empty command is not allowed.", command, taskID))
	}

	if g.HardRules {
		if d, fired := gitSafety(normalized, taskID); fired && d.Action == Deny {
			return d
		}
	}

	// A bare directory change cannot write, and agents chain it before
	// allowlisted commands.
	if cdSegmentRe.MatchString(normalized) {
		return g.noteHardRules(allow("Working-directory change is not gated.", normalized, taskID))
	}

	if fnmatch.MatchAny(LeaseLifecycleAllowedCommands, normalized) {
		return g.finish(allow("Lease lifecycle or repo maintenance command allowed.", normalized, taskID), normalized, taskID)
	}
	if fnmatch.MatchAny(NoTaskAllowedCommands, normalized) {
		return g.finish(allow("Bootstrap or read-only command allowed.", normalized, taskID), normalized, taskID)
	}

	if task == nil {
		return g.noteHardRules(deny(RuleCommandNoLease,
			"Shell commands require an active task lease. Bootstrap with `dev checkout <TASK>`, "+
				"or use allowlisted orientation commands (`dev status`, `dev map …`, `dev doctor`).",
			normalized, ""))
	}

	// Allowlist entries are matched against both the raw and normalized forms,
	// so a task listing ".venv/bin/dev *" still works while a path-prefixed
	// "dev map" continues to hit the lifecycle patterns after normalization.
	if matchesEither(task.AllowedCommands, normalized, raw) {
		return g.finish(allow("Command matches task allowed_commands.", normalized, task.ID), normalized, task.ID)
	}
	if matchesEither(g.GlobalAllowedCommands, normalized, raw) {
		return g.finish(allow("Command matches global allowed commands.", normalized, task.ID), normalized, task.ID)
	}

	return g.noteHardRules(deny(RuleCommandNotAllowed,
		"Command is not in task or global allowlists.", normalized, task.ID))
}

// SplitCommandChain splits a shell command line on &&, ||, ;, and | operators,
// respecting single and double quotes.
func SplitCommandChain(command string) []string {
	var parts []string
	var cur strings.Builder
	var quote rune
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case quote == '\'' && r == '\'':
			quote = 0
			cur.WriteRune(r)
		case quote == '"' && r == '"':
			quote = 0
			cur.WriteRune(r)
		case quote == '"' && r == '\\' && i+1 < len(runes):
			cur.WriteRune(r)
			i++
			cur.WriteRune(runes[i])
		case quote != 0:
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			cur.WriteRune(r)
		case r == ';' || r == '|':
			if r == '|' && i+1 < len(runes) && runes[i+1] == '|' {
				i++
			}
			if s := strings.TrimSpace(cur.String()); s != "" {
				parts = append(parts, s)
			}
			cur.Reset()
		case r == '&' && i+1 < len(runes) && runes[i+1] == '&':
			i++
			if s := strings.TrimSpace(cur.String()); s != "" {
				parts = append(parts, s)
			}
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		parts = append(parts, s)
	}
	return parts
}

// finish applies the git-safety warnings to a command the allowlist accepted,
// so a protected-branch push is still flagged even when a task allows "git *".
func (g CommandGate) finish(d Decision, normalized, taskID string) Decision {
	if g.HardRules {
		if safety, fired := gitSafety(normalized, taskID); fired {
			return g.noteHardRules(safety)
		}
	}
	return g.noteHardRules(d)
}

// GitSafety evaluates only the git-safety rules, mirroring DevCouncil's
// `evaluate_hook_command` exactly so the two can be compared command for
// command. Returns an allow when no rule fires.
//
// EvaluateCommand calls the same rules; this entry point exists because the
// incumbent exposes them separately and parity has to be provable at that
// granularity.
func GitSafety(command string) Decision {
	normalized := collapseSpaces(command)
	if normalized == "" {
		return allow("No command detected.", normalized, "")
	}
	if d, fired := gitSafety(normalized, ""); fired {
		return d
	}
	return allow("Command is allowed.", normalized, "")
}

// gitSafety returns the git-safety verdict and whether any rule fired.
func gitSafety(normalized, taskID string) (Decision, bool) {
	lowered := strings.ToLower(normalized)

	if strings.Contains(lowered, "--no-verify") || strings.Contains(lowered, "--no-gpg-sign") {
		return deny(RuleCommandBypassFlag, "Verification bypass flags are not allowed.", normalized, taskID), true
	}
	if hardResetProtectedRe.MatchString(lowered) {
		return deny(RuleCommandProtectedReset, "Protected branch hard resets are not allowed.", normalized, taskID), true
	}
	// The refspec form (`git push origin +HEAD:master`) forces a
	// non-fast-forward update without carrying --force.
	if forcePushFlagRe.MatchString(lowered) || forcePushPlusRefspecRe.MatchString(lowered) {
		return deny(RuleCommandForcePush, "Force pushes are not allowed.", normalized, taskID), true
	}
	if protectedBranchPushRe.MatchString(lowered) {
		return warn(RuleCommandProtectedPush,
			"Direct pushes to protected branches should go through verification gates.", normalized, taskID), true
	}
	return Decision{}, false
}

func (g CommandGate) noteHardRules(d Decision) Decision {
	if !g.HardRules {
		d.Degraded = append(d.Degraded, "policy.hard_rules.disabled")
	}
	return d
}

// NormalizeAllowlistCommand collapses the forms an allowlist would otherwise
// miss: path-prefixed dev binaries, `uv run --project X` wrappers, and trailing
// shell redirections.
//
// Only a bare `dev`/`devcouncil` token, or one under a bin/Scripts directory,
// is rewritten — never a repository folder whose basename happens to be
// "DevCouncil".
func NormalizeAllowlistCommand(command string) string {
	normalized := collapseSpaces(command)
	if normalized == "" {
		return normalized
	}
	for {
		stripped := redirectTailRe.ReplaceAllString(normalized, "")
		if stripped == normalized {
			break
		}
		normalized = stripped
	}
	if m := uvRunDirFlagRe.FindStringSubmatch(normalized); m != nil {
		normalized = m[1] + m[3]
	}

	tokens := strings.Fields(normalized)
	for i, token := range tokens {
		posix := strings.ReplaceAll(token, `\`, "/")
		name, parent := baseAndParent(posix)
		bare := !strings.Contains(posix, "/") && devBinaries[strings.ToLower(name)]
		underBin := devBinaries[strings.ToLower(name)] &&
			(strings.EqualFold(parent, "bin") || strings.EqualFold(parent, "scripts"))
		if bare || underBin {
			tokens[i] = "dev"
		}
	}
	return strings.Join(tokens, " ")
}

// baseAndParent splits a posix-style path into its last and second-to-last
// components, matching pathlib's .name and .parent.name.
func baseAndParent(p string) (name, parent string) {
	parts := strings.Split(p, "/")
	name = parts[len(parts)-1]
	if len(parts) >= 2 {
		parent = parts[len(parts)-2]
	}
	return name, parent
}

func matchesEither(patterns []string, normalized, raw string) bool {
	for _, pattern := range patterns {
		if fnmatch.Match(pattern, normalized) || fnmatch.Match(pattern, raw) {
			return true
		}
	}
	return false
}

func collapseSpaces(s string) string { return strings.Join(strings.Fields(s), " ") }
