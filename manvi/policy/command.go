package policy

import (
	"fmt"
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

// maxSubstitutionDepth bounds the recursion into command-substitution
// contents. Each level judges strictly less text than its parent, so the cap
// is unreachable except by adversarial input — and adversarial input is
// exactly what gets refused rather than recursed into forever.
const maxSubstitutionDepth = 8

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
	return g.evaluate(command, task, 0)
}

func (g CommandGate) evaluate(command string, task *dc.Task, depth int) Decision {
	parts := SplitCommandChain(command)
	if len(parts) > 1 {
		var warnDecision *Decision
		for _, part := range parts {
			d := g.evaluateSingleCommand(part, task, depth)
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
	return g.evaluateSingleCommand(command, task, depth)
}

func (g CommandGate) evaluateSingleCommand(command string, task *dc.Task, depth int) Decision {
	raw := collapseSpaces(command)
	normalized := NormalizeAllowlistCommand(command)
	taskID := ""
	if task != nil {
		taskID = task.ID
	}

	if normalized == "" {
		return g.noteHardRules(deny(RuleCommandEmpty, "Empty command is not allowed.", command, taskID))
	}

	// A live substitution or a heredoc carries code this ladder cannot read:
	// an allowlist entry matched against the surrounding line never judged
	// what `sh -c` would actually execute inside it. Substitution contents are
	// extracted and run through this same gate — so `echo $(date)` is judged
	// as both echo and date — and anything the scanner cannot bound is refused
	// outright rather than guessed at. A heredoc body is expanded data with no
	// reliable static end, so it has no extraction path and is refused.
	if hasHeredoc(raw) {
		return g.noteHardRules(deny(RuleCommandHeredoc,
			"Heredocs carry expanded data with no statically checkable end and are not allowed; "+
				"write the content to a file instead.", normalized, taskID))
	}
	spans, subErr := liveSubstitutions(raw)
	switch {
	case subErr != nil:
		return g.noteHardRules(deny(RuleCommandSubstitution,
			"Command substitution could not be analysed to its end and is not allowed; "+
				"rewrite without $(), backticks, <() or >().", normalized, taskID))
	case len(spans) > 0 && depth >= maxSubstitutionDepth:
		return g.noteHardRules(deny(RuleCommandSubstitution,
			"Command substitution nested beyond the analysis limit is not allowed; "+
				"run the inner commands separately.", normalized, taskID))
	case len(spans) > 0:
		var warnDecision *Decision
		for _, span := range spans {
			d := g.evaluate(span, task, depth+1)
			if d.Action == Deny {
				return g.noteHardRules(deny(RuleCommandSubstitution,
					fmt.Sprintf("Substituted command was denied: %s", d.Reason), normalized, taskID))
			}
			if d.Action == Warn && warnDecision == nil {
				warnDecision = &d
			}
		}
		if warnDecision != nil {
			return *warnDecision
		}
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

// SplitCommandChain splits a shell command line on &&, ||, ;, |, a lone &,
// and unquoted newlines — every operator sh treats as a command boundary.
// Respecting single and double quotes. A boundary the splitter misses is a
// second command hidden inside one string the gate judges once, so anything
// the shell could read as "start another command" splits here; splitting too
// much costs nothing, because each part is judged on its own merits anyway.
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
		case r == '&':
			// sh reads '&' as a control operator except where it completes a
			// redirection: `2>&1` and `>&2` duplicate descriptors, and `&>` /
			// `&>>` are themselves redirections. Those stay text. Anything
			// else — backgrounding, with or without a trailing space — is a
			// command boundary, because the backgrounded job runs just the
			// same.
			if i > 0 && runes[i-1] == '>' {
				cur.WriteRune(r)
				break
			}
			if i+1 < len(runes) && runes[i+1] == '>' {
				cur.WriteRune('&')
				cur.WriteRune('>')
				i++
				if i+1 < len(runes) && runes[i+1] == '>' {
					cur.WriteRune('>')
					i++
				}
				break
			}
			if s := strings.TrimSpace(cur.String()); s != "" {
				parts = append(parts, s)
			}
			cur.Reset()
		case r == '\n' || r == '\r':
			// An unquoted newline is sh's statement separator.
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
//
// The rules are matched against the line as normalised *and* against a
// dequoted reading of it. Quoting is how sh hides characters from word
// splitting while still concatenating them into one argument, so
// `--no-'v'erify` reaches git as `--no-verify` and `git "reset" --hard` as an
// ordinary reset — a check that only reads the raw text is bypassed by
// exactly the commands worth catching. Checking both readings costs at most a
// false positive on a command that merely prints forbidden text, which is the
// safe direction for this rung.
func gitSafety(normalized, taskID string) (Decision, bool) {
	variants := []string{normalized}
	if dq := shellDequote(normalized); dq != normalized {
		variants = append(variants, dq)
	}
	for _, variant := range variants {
		if d, fired := gitSafetyVariant(strings.ToLower(variant), taskID); fired {
			return d, true
		}
	}
	return Decision{}, false
}

func gitSafetyVariant(lowered, taskID string) (Decision, bool) {
	if strings.Contains(lowered, "--no-verify") || strings.Contains(lowered, "--no-gpg-sign") {
		return deny(RuleCommandBypassFlag, "Verification bypass flags are not allowed.", lowered, taskID), true
	}
	if hardResetProtectedRe.MatchString(lowered) {
		return deny(RuleCommandProtectedReset, "Protected branch hard resets are not allowed.", lowered, taskID), true
	}
	// The refspec form (`git push origin +HEAD:master`) forces a
	// non-fast-forward update without carrying --force.
	if forcePushFlagRe.MatchString(lowered) || forcePushPlusRefspecRe.MatchString(lowered) {
		return deny(RuleCommandForcePush, "Force pushes are not allowed.", lowered, taskID), true
	}
	if protectedBranchPushRe.MatchString(lowered) {
		return warn(RuleCommandProtectedPush,
			"Direct pushes to protected branches should go through verification gates.", lowered, taskID), true
	}
	return Decision{}, false
}

func (g CommandGate) noteHardRules(d Decision) Decision {
	if !g.HardRules {
		d.Degraded = append(d.Degraded, "policy.hard_rules.disabled")
	}
	return d
}

// substitutionSpan is one construct the scanner claimed, with the rune range
// it occupies in the line it was scanned from.
//
// Arithmetic expansions are recorded alongside the live substitutions even
// though sh runs nothing inside them, because a reader that only learned
// where the live spans were would still have to rediscover where the
// arithmetic ended: `$(( a > b ))` carries a '>' that is a comparison, and a
// redirect scanner that walked into it would read a write that never happens.
type substitutionSpan struct {
	text  string
	start int // index of the construct's first rune
	end   int // index one past its last rune
	live  bool
}

// scanSubstitutions returns every command substitution sh would execute in
// this line — $( … ), a legacy backtick span, and the process substitutions
// <( … ) and >( … ) — plus the arithmetic expansions it would not.
//
// This is the one definition of "a span the shell runs". Both readers of a
// command line go through it: liveSubstitutions, which judges the contents as
// commands, and RedirectTargets, which has to skip past them and then look
// inside for writes. They used to scan separately, and the halves disagreed —
// a redirect hidden in $( … ) was skipped by one and never seen by the other,
// so `echo $(echo forged > .devcouncil/harness-grants.json)` was allowed
// outright.
//
// Substitutions inside single quotes are data; inside double quotes they are
// live, which is why the quote state is tracked here rather than delegated to
// a pre-pass. An unterminated span is an error, not an empty list — a scanner
// that lost track of where code ends must refuse rather than guess.
func scanSubstitutions(runes []rune) ([]substitutionSpan, error) {
	var spans []substitutionSpan
	n := len(runes)
	quote := rune(0)
	i := 0
	for i < n {
		r := runes[i]
		switch {
		case (quote == 0 || quote == '"') && r == '\\' && i+1 < n:
			i += 2
		case quote == 0 && r == '\'':
			quote = '\''
			i++
		case quote == '\'' && r == '\'':
			quote = 0
			i++
		case quote == 0 && r == '"':
			quote = '"'
			i++
		case quote == '"' && r == '"':
			quote = 0
			i++
		case quote == '\'':
			i++
		case r == '`':
			end := -1
			for j := i + 1; j < n; j++ {
				if runes[j] == '`' {
					end = j
					break
				}
				if runes[j] == '\\' {
					j++
				}
			}
			if end < 0 {
				return nil, fmt.Errorf("unterminated backtick substitution")
			}
			spans = append(spans, substitutionSpan{
				text: string(runes[i+1 : end]), start: i, end: end + 1, live: true,
			})
			i = end + 1
		case r == '$' && i+2 < n && runes[i+1] == '(' && runes[i+2] == '(':
			next, ok := skipArithmetic(runes, i)
			if !ok {
				return nil, fmt.Errorf("unterminated arithmetic expansion")
			}
			spans = append(spans, substitutionSpan{start: i, end: next, live: false})
			i = next
		case r == '$' && i+1 < n && runes[i+1] == '(':
			text, next, err := scanParenSpan(runes, i+1)
			if err != nil {
				return nil, err
			}
			spans = append(spans, substitutionSpan{text: text, start: i, end: next, live: true})
			i = next
		case (r == '<' || r == '>') && i+1 < n && runes[i+1] == '(':
			text, next, err := scanParenSpan(runes, i+1)
			if err != nil {
				return nil, err
			}
			spans = append(spans, substitutionSpan{text: text, start: i, end: next, live: true})
			i = next
		default:
			i++
		}
	}
	return spans, nil
}

// liveSubstitutions returns the inner text of every command substitution sh
// would execute in this line. Arithmetic expansions are excluded: they expand
// variables but cannot execute commands.
func liveSubstitutions(command string) ([]string, error) {
	spans, err := scanSubstitutions([]rune(command))
	if err != nil {
		return nil, err
	}
	var texts []string
	for _, span := range spans {
		if span.live {
			texts = append(texts, span.text)
		}
	}
	return texts, nil
}

// scanParenSpan reads from the opening parenthesis at position open to its
// matching close, honouring nested parentheses and quoted spans within the
// substitution. It returns the inner text and the index one past the closing
// parenthesis.
func scanParenSpan(runes []rune, open int) (string, int, error) {
	depth := 0
	quote := rune(0)
	for j := open; j < len(runes); j++ {
		r := runes[j]
		switch {
		case quote == '\'' && r == '\'':
			quote = 0
		case quote == '"' && r == '"':
			quote = 0
		case quote != 0:
		case r == '\'' || r == '"':
			quote = r
		case r == '(':
			depth++
		case r == ')':
			depth--
			if depth == 0 {
				return string(runes[open+1 : j]), j + 1, nil
			}
		}
	}
	return "", 0, fmt.Errorf("unterminated $( substitution")
}

// skipArithmetic consumes a $(( … )) span and returns the index one past it.
// The two-paren form is recognised up front; anything that does not close as
// arithmetic is reported so the caller can refuse rather than misparse.
func skipArithmetic(runes []rune, start int) (int, bool) {
	depth := 0
	for j := start; j < len(runes); j++ {
		switch runes[j] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return j + 1, true
			}
		}
	}
	return 0, false
}

// hasHeredoc reports whether the command carries a heredoc introducer outside
// quotes. A heredoc body undergoes expansion yet has no statically checkable
// end — the terminator is whatever word the author chose on some following
// line — so no extraction is attempted; the construct is refused instead.
// Arithmetic contexts are skipped so `$((a << 2))` is not misread as one.
func hasHeredoc(command string) bool {
	runes := []rune(command)
	n := len(runes)
	quote := rune(0)
	i := 0
	for i < n {
		r := runes[i]
		switch {
		case quote == 0 && r == '\\':
			i += 2
		case quote == 0 && r == '\'':
			quote = '\''
			i++
		case quote == '\'' && r == '\'':
			quote = 0
			i++
		case quote == 0 && r == '"':
			quote = '"'
			i++
		case quote == '"' && r == '"':
			quote = 0
			i++
		case quote != 0:
			i++
		case r == '$' && i+2 < n && runes[i+1] == '(' && runes[i+2] == '(':
			next, ok := skipArithmetic(runes, i)
			if !ok {
				return false // malformed arithmetic; the substitution rung refuses it
			}
			i = next
		case r == '<' && i+1 < n && runes[i+1] == '<':
			return true
		default:
			i++
		}
	}
	return false
}

// shellDequote removes quote characters the way sh concatenates their content:
// '…' contributes literally, "…" contributes everything except the quotes and
// escapes, and an escaped character contributes itself. An unterminated quote
// contributes the rest of the line as data. The result is what the shell would
// hand a single command after quote removal — the reading safety rules must
// also see, because quoting exists precisely to hide characters from naive
// substring checks.
func shellDequote(s string) string {
	var b strings.Builder
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch r {
		case '\\':
			if i+1 < len(runes) {
				b.WriteRune(runes[i+1])
				i++
			}
		case '\'':
			// Scan for the closing quote in rune space. IndexRune on a
			// re-encoded substring returns a BYTE offset, and mixing the two
			// desyncs on the first multibyte character — found by the chain
			// fuzzer as a slice-bounds panic on input like 'ααααααα'.
			close := -1
			for j := i + 1; j < len(runes); j++ {
				if runes[j] == '\'' {
					close = j
					break
				}
			}
			if close < 0 {
				b.WriteString(string(runes[i+1:]))
				return b.String()
			}
			b.WriteString(string(runes[i+1 : close]))
			i = close
		case '"':
			j := i + 1
			for ; j < len(runes); j++ {
				if runes[j] == '\\' && j+1 < len(runes) && (runes[j+1] == '"' || runes[j+1] == '\\') {
					b.WriteRune(runes[j+1])
					j++
					continue
				}
				if runes[j] == '"' {
					break
				}
				b.WriteRune(runes[j])
			}
			if j >= len(runes) {
				return b.String()
			}
			i = j
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// RedirectTargets returns every file an output redirection in this command
// would write: the targets of >, >>, &>, >& and their fd-prefixed forms.
// Input redirections (<) are reads, which this ladder does not gate, and a
// heredoc introducer (<<) is not a path at all; neither is returned.
//
// Substitution contents are searched too, at every nesting level. A shell runs
// the code inside $( … ), a backtick span, and <( … ) / >( … ), so a
// redirection written there is a write like any other — and it is the one the
// allowlist is least likely to have looked at, because the surrounding line
// reads as a harmless `echo`. Skipping those spans is what let
// `echo $(echo forged > .devcouncil/harness-grants.json)` through the whole
// ladder under the strict posture: nothing above judged the inner redirect as
// a command, and nothing here judged it as a write.
//
// The second return value reports whether a target could not be resolved to a
// literal path — it carries an expansion ($HOME, ${VAR}, ~, a nested
// substitution) whose value only the shell knows, or it is nested deeper than
// this scanner will follow. A target the gate cannot name is a write it cannot
// judge, so the caller must treat that as a refusal rather than skip the
// check. "I could not check" and "there was nothing to check" are the same
// empty target list, and only this flag tells them apart.
//
// Matching strips trailing redirections so patterns like "dev map *" stay
// single-clause, which is exactly why the executed form has to be re-read
// here: the string that matched is not the string that runs.
func RedirectTargets(command string) ([]string, bool, error) {
	return redirectTargets(command, 0)
}

func redirectTargets(command string, depth int) ([]string, bool, error) {
	var targets []string
	opaque := false
	runes := []rune(command)
	n := len(runes)

	// The substitution spans come from the one scanner that defines them, so
	// this walk and the command ladder's agree on where shell code starts and
	// ends. Skipping a span here is not ignoring it: each live one is searched
	// below, in its own right.
	spans, err := scanSubstitutions(runes)
	if err != nil {
		return nil, false, err
	}
	spanAt := make(map[int]substitutionSpan, len(spans))
	for _, span := range spans {
		spanAt[span.start] = span
	}

	quote := rune(0)
	i := 0
	readTarget := func(start int) (string, int, error) {
		j := start
		for j < n && runes[j] == ' ' {
			j++
		}
		if j >= n {
			return "", 0, fmt.Errorf("redirection with no target")
		}
		if runes[j] == '&' || (runes[j] >= '0' && runes[j] <= '9') && j+1 < n && runes[j+1] == '&' {
			// >&N / &N — dup to a descriptor, not a path.
			for j < n && runes[j] != ' ' {
				j++
			}
			return "", j, nil
		}
		var b strings.Builder
		q := rune(0)
		for j < n {
			r := runes[j]
			if q == 0 && r == '\\' && j+1 < n {
				b.WriteRune(runes[j+1])
				j += 2
				continue
			}
			if q == 0 && (r == '\'' || r == '"') {
				q = r
				j++
				continue
			}
			if q != 0 && r == q {
				q = 0
				j++
				continue
			}
			if q == 0 && (r == ' ' || r == '\n' || r == ';' || r == '|' || r == '&') {
				break
			}
			b.WriteRune(r)
			j++
		}
		return b.String(), j, nil
	}
	for i < n {
		r := runes[i]
		// A span is claimed before anything else reads its characters, in or
		// out of double quotes: `"$(cmd > f)"` is live, and the '>' inside it
		// belongs to the inner command, not to this line.
		if span, ok := spanAt[i]; ok {
			i = span.end
			continue
		}
		switch {
		case (quote == 0 || quote == '"') && r == '\\' && i+1 < n:
			i += 2
		case quote == 0 && r == '\'':
			quote = '\''
			i++
		case quote == '\'' && r == '\'':
			quote = 0
			i++
		case quote == 0 && r == '"':
			quote = '"'
			i++
		case quote == '"' && r == '"':
			quote = 0
			i++
		case quote != 0:
			i++
		case r == '>' || (r >= '0' && r <= '9' && i+1 < n && runes[i+1] == '>'):
			fdStart := i
			for i < n && runes[i] >= '0' && runes[i] <= '9' {
				i++
			}
			if i >= n || runes[i] != '>' {
				i = fdStart + 1
				continue
			}
			i++                           // consume '>'
			if i < n && runes[i] == '>' { // append form >>
				i++
			} else if i < n && runes[i] == '&' { // >&N duplicates descriptors
				i++
				for i < n && runes[i] >= '0' && runes[i] <= '9' {
					i++
				}
				continue
			}
			target, next, err := readTarget(i)
			if err != nil {
				return nil, false, err
			}
			// A target carrying an expansion or a substitution is a path only
			// the shell can name; `> $HOME/x`, `> ~/.ssh/keys` and
			// `` > `pick-a-file` `` are all writes this gate cannot judge.
			if strings.ContainsAny(target, "$~`") {
				opaque = true
			} else if target != "" {
				targets = append(targets, target)
			} else {
				opaque = true
			}
			i = next
		default:
			i++
		}
	}

	// Now the spans themselves. The bound is the ladder's own, and exhausting
	// it reports opaque rather than an empty result: a span too deep to search
	// is a write that was not checked, and the caller has to hear that as a
	// refusal.
	for _, span := range spans {
		if !span.live {
			continue
		}
		if depth >= maxSubstitutionDepth {
			opaque = true
			break
		}
		inner, innerOpaque, err := redirectTargets(span.text, depth+1)
		if err != nil {
			return nil, false, err
		}
		targets = append(targets, inner...)
		opaque = opaque || innerOpaque
	}
	return targets, opaque, nil
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
