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

// ansiCQuote is the scanner state inside a $'…' span.
//
// sh's ANSI-C quoting is not an ordinary single quote: a backslash escapes
// inside it, so a dollar-quote span holding a backslash-escaped quote is one
// word containing a quote character, and everything after that span is
// UNQUOTED. Reading it as a plain '…' swallows the rest of the line — which is
// how `echo $[backslash-quote span] ; mkdir OWNED` reached the gate as a
// single command. It is therefore its own state, not a flag on the
// single-quote one.
const ansiCQuote = rune(-1)

// shellQuoteStep advances past exactly one quoting construct at runes[i],
// given the quote state on entry. It returns the index one past what it
// consumed, the resulting state, and whether it consumed anything at all.
//
// Every scanner in this file routes its quote handling through here. They each
// used to carry their own copy of the rules and had drifted: the chain splitter
// alone had no unquoted-backslash case, so `echo \'` flipped it INTO a quoted
// span while sh — which reads \' as a literal quote character and stays
// unquoted — went on to run whatever followed the next `;`. One owner for the
// rules is what makes that drift impossible rather than merely fixed once.
//
// handled=false means the rune is not part of the quoting grammar and the
// caller must decide what it is. That happens in the unquoted state, and in the
// "…" state, where command substitutions still execute.
func shellQuoteStep(runes []rune, i int, quote rune) (next int, newQuote rune, handled bool) {
	n := len(runes)
	r := runes[i]
	switch quote {
	case ansiCQuote:
		if r == '\\' && i+1 < n {
			return i + 2, quote, true
		}
		if r == '\'' {
			return i + 1, 0, true
		}
		return i + 1, quote, true
	case '\'':
		// Inside '…' nothing is special but the closing quote — a backslash
		// there is an ordinary character.
		if r == '\'' {
			return i + 1, 0, true
		}
		return i + 1, quote, true
	case '"':
		if r == '\\' && i+1 < n {
			return i + 2, quote, true
		}
		if r == '"' {
			return i + 1, 0, true
		}
		return i, quote, false
	default: // unquoted
		switch {
		case r == '\\':
			// A backslash quotes the single next character, whatever it is —
			// a quote, an operator, or a newline. A trailing one quotes
			// nothing and is consumed on its own.
			if i+1 < n {
				return i + 2, 0, true
			}
			return i + 1, 0, true
		case r == '$' && i+1 < n && runes[i+1] == '\'':
			return i + 2, ansiCQuote, true
		case r == '\'':
			return i + 1, '\'', true
		case r == '"':
			return i + 1, '"', true
		}
		return i, 0, false
	}
}

// SplitCommandChain splits a shell command line on &&, ||, ;, |, a lone &,
// and unquoted newlines — every operator sh treats as a command boundary.
// Quoting is read through shellQuoteStep, so single quotes, double quotes,
// $'…' spans and backslash escapes are honoured exactly as sh honours them. A
// boundary the splitter misses is a second command hidden inside one string
// the gate judges once, so anything the shell could read as "start another
// command" splits here; splitting too much costs nothing, because each part is
// judged on its own merits anyway.
func SplitCommandChain(command string) []string {
	var parts []string
	var cur strings.Builder
	quote := rune(0)
	runes := []rune(command)
	n := len(runes)
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			parts = append(parts, s)
		}
		cur.Reset()
	}
	for i := 0; i < n; {
		if next, nq, handled := shellQuoteStep(runes, i, quote); handled {
			cur.WriteString(string(runes[i:next]))
			quote = nq
			i = next
			continue
		}
		r := runes[i]
		if quote != 0 {
			// Only the "…" state reaches here. Its contents are one word as
			// far as command boundaries go; the substitution rung judges the
			// code that still executes inside it.
			cur.WriteRune(r)
			i++
			continue
		}
		switch {
		case r == ';' || r == '|':
			if r == '|' && i+1 < n && runes[i+1] == '|' {
				i++
			}
			flush()
			i++
		case r == '&' && i+1 < n && runes[i+1] == '&':
			i += 2
			flush()
		case r == '&':
			// sh reads '&' as a control operator except where it completes a
			// redirection: `2>&1` and `>&2` duplicate descriptors, and `&>` /
			// `&>>` are themselves redirections. Those stay text. Anything
			// else — backgrounding, with or without a trailing space — is a
			// command boundary, because the backgrounded job runs just the
			// same.
			if i > 0 && runes[i-1] == '>' {
				cur.WriteRune(r)
				i++
				break
			}
			if i+1 < n && runes[i+1] == '>' {
				cur.WriteRune('&')
				cur.WriteRune('>')
				i += 2
				if i < n && runes[i] == '>' {
					cur.WriteRune('>')
					i++
				}
				break
			}
			flush()
			i++
		case r == '\n' || r == '\r':
			// An unquoted newline is sh's statement separator.
			flush()
			i++
		default:
			cur.WriteRune(r)
			i++
		}
	}
	flush()
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

// liveSubstitutions returns the inner text of every command substitution sh
// would execute in this line: $( … ), a legacy backtick span, and the process
// substitutions <( … ) and >( … ).
//
// Substitutions inside single quotes are data; inside double quotes they are
// live, which is why the quote state is tracked here rather than delegated to
// a pre-pass. Arithmetic expansions `$(( … ))` are skipped whole: they expand
// variables but cannot execute commands. An unterminated span is an error,
// not an empty list — a scanner that lost track of where code ends must
// refuse rather than guess.
func liveSubstitutions(command string) ([]string, error) {
	var spans []string
	runes := []rune(command)
	n := len(runes)
	quote := rune(0)
	i := 0
	for i < n {
		if next, nq, handled := shellQuoteStep(runes, i, quote); handled {
			quote = nq
			i = next
			continue
		}
		// Only the unquoted state and the "…" state reach here, and both are
		// live: sh executes $( … ) and ` … ` inside double quotes.
		r := runes[i]
		switch {
		case r == '`':
			text, next, err := scanBacktickSpan(runes, i)
			if err != nil {
				return nil, err
			}
			spans = append(spans, text)
			i = next
		case r == '$' && i+2 < n && runes[i+1] == '(' && runes[i+2] == '(':
			next, ok := skipArithmetic(runes, i)
			if !ok {
				return nil, fmt.Errorf("unterminated arithmetic expansion")
			}
			i = next
		case r == '$' && i+1 < n && runes[i+1] == '(':
			text, next, err := scanParenSpan(runes, i+1)
			if err != nil {
				return nil, err
			}
			spans = append(spans, text)
			i = next
		case (r == '<' || r == '>') && i+1 < n && runes[i+1] == '(':
			text, next, err := scanParenSpan(runes, i+1)
			if err != nil {
				return nil, err
			}
			spans = append(spans, text)
			i = next
		default:
			i++
		}
	}
	return spans, nil
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

// scanBacktickSpan reads from the opening backtick at position open to its
// matching close, honouring the backslash escapes sh allows inside a legacy
// substitution. It returns the inner text and the index one past the closing
// backtick. An unterminated span is an error: a scanner that lost track of
// where code ends must refuse rather than guess.
func scanBacktickSpan(runes []rune, open int) (string, int, error) {
	for j := open + 1; j < len(runes); j++ {
		switch runes[j] {
		case '\\':
			j++
		case '`':
			return string(runes[open+1 : j]), j + 1, nil
		}
	}
	return "", 0, fmt.Errorf("unterminated backtick substitution")
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
		if next, nq, handled := shellQuoteStep(runes, i, quote); handled {
			quote = nq
			i = next
			continue
		}
		r := runes[i]
		if quote != 0 {
			// Only the "…" state reaches here; a << inside it is literal text.
			i++
			continue
		}
		switch {
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
// The second return value reports whether a target could not be resolved to a
// literal path — it carries an expansion ($HOME, ${VAR}, ~) whose value only
// the shell knows. A target the gate cannot name is a write it cannot judge,
// so the caller must treat that as a refusal rather than skip the check.
//
// Matching strips trailing redirections so patterns like "dev map *" stay
// single-clause, which is exactly why the executed form has to be re-read
// here: the string that matched is not the string that runs.
//
// Command substitutions are descended into rather than skipped. Their contents
// are recursed through the policy ladder, and that ladder has no redirect rung
// of its own — the rung lives above it, in the caller of this function — so a
// substitution the scanner stepped over was a write nothing ever judged:
// `echo $(git diff > ~/.ssh/authorized_keys)` was allowed while the same
// redirect on its own was a hard denial. Backticks and <( … ) happened to be
// caught before, by the scanner not recognising them at all and stumbling onto
// the `>` inside; they are now found the same principled way as $( … ), so the
// three cannot diverge again.
func RedirectTargets(command string) ([]string, bool, error) {
	return redirectTargets(command, 0)
}

func redirectTargets(command string, depth int) ([]string, bool, error) {
	if depth > maxSubstitutionDepth {
		return nil, false, fmt.Errorf(
			"command substitution nested beyond the analysis limit; redirection targets could not be resolved")
	}
	var targets []string
	opaque := false
	runes := []rune(command)
	n := len(runes)
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
	// descend judges the redirections inside a substitution as the writes they
	// are. Its findings merge into this command's, because sh performs them
	// whether or not the surrounding line has a redirect of its own.
	descend := func(inner string) error {
		innerTargets, innerOpaque, err := redirectTargets(inner, depth+1)
		if err != nil {
			return err
		}
		targets = append(targets, innerTargets...)
		opaque = opaque || innerOpaque
		return nil
	}
	for i < n {
		if next, nq, handled := shellQuoteStep(runes, i, quote); handled {
			quote = nq
			i = next
			continue
		}
		r := runes[i]
		switch {
		case r == '`':
			text, next, err := scanBacktickSpan(runes, i)
			if err != nil {
				return nil, false, err
			}
			if err := descend(text); err != nil {
				return nil, false, err
			}
			i = next
		case r == '$' && i+2 < n && runes[i+1] == '(' && runes[i+2] == '(':
			next, ok := skipArithmetic(runes, i)
			if !ok {
				return nil, false, fmt.Errorf("unterminated arithmetic expansion")
			}
			i = next
		case r == '$' && i+1 < n && runes[i+1] == '(':
			text, next, err := scanParenSpan(runes, i+1)
			if err != nil {
				return nil, false, err
			}
			if err := descend(text); err != nil {
				return nil, false, err
			}
			i = next
		case (r == '<' || r == '>') && i+1 < n && runes[i+1] == '(':
			// Process substitution: the code inside runs, and its redirections
			// write files, exactly like $( … ).
			text, next, err := scanParenSpan(runes, i+1)
			if err != nil {
				return nil, false, err
			}
			if err := descend(text); err != nil {
				return nil, false, err
			}
			i = next
		case quote != 0:
			// Only the "…" state reaches here. The substitutions above still
			// execute inside it; a bare > does not — it is literal text.
			i++
		case r == '<' && i+1 < n && runes[i+1] == '<':
			// Heredoc introducer or herestring; not an output path.
			i += 2
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
			if strings.ContainsAny(target, "$~") {
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
	return targets, opaque, nil
}

// devToolDirs are the directory names a repo-local dev CLI is installed under.
// Matched exactly: a case-insensitive comparison let /tmp/x/BIN/dev through on
// a case-sensitive filesystem where that is a different directory entirely.
var devToolDirs = map[string]bool{"bin": true, "scripts": true, "Scripts": true}

// devVenvDirs are the virtualenv roots whose bin/ (Scripts/ on Windows) holds
// the installed `dev` console script.
var devVenvDirs = map[string]bool{".venv": true, "venv": true}

// NormalizeAllowlistCommand collapses the forms an allowlist would otherwise
// miss: path-prefixed dev binaries, `uv run --project X` wrappers, and trailing
// shell redirections.
//
// Normalisation is a claim that two spellings name the same program, and the
// executed string is the un-normalised one — so every spelling accepted here
// is a spelling that inherits `dev`'s allowlist entries. It used to accept any
// token whose basename folded to "dev" under a parent folding to "bin" or
// "scripts", which made `/tmp/attacker/bin/dev status`,
// `../../../../tmp/attacker/bin/dev status`, `attacker/scripts/dev run-cmd …`
// and `/tmp/x/bin/DEV status` all read as the repo's own CLI. What is accepted
// now is only what the harness can attribute to this working tree:
//
//   - a bare `dev`/`devcouncil` token resolved off PATH;
//   - a relative `bin/dev` or `scripts/dev` directly beneath the working
//     directory — the repo's own tool directories;
//   - a `…/.venv/bin/dev` (or venv/, or Scripts/ on Windows) layout, the shape
//     a project virtualenv install actually produces.
//
// Anything with a `..` component is refused outright: a path that can climb out
// of the tree cannot be attributed to it. A backslash is likewise not treated
// as a separator — the gate hands these lines to `sh`, where `\` escapes the
// next character rather than descending a directory, so reading `x\bin\dev` as
// a path laundered a token sh never resolves as `dev` at all.
//
// Known residual: an absolute `…/.venv/bin/dev` outside the repo is still
// accepted. Distinguishing it from the repo's own venv needs the repo root,
// which this function's signature does not carry (callers depend on it taking
// only the raw command), and testdata/command-parity.tsv pins
// `/abs/path/.venv/bin/dev status` as normalising. Closing it means threading a
// root through and regenerating that fixture.
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
		if isRepoDevBinary(token) {
			tokens[i] = "dev"
		}
	}
	return strings.Join(tokens, " ")
}

// isRepoDevBinary reports whether a token names this repo's own dev CLI in a
// spelling the gate can attribute to the working tree. See
// NormalizeAllowlistCommand for why the answer has to be this narrow.
func isRepoDevBinary(token string) bool {
	if strings.ContainsRune(token, '\\') {
		return false
	}
	parts := strings.Split(token, "/")
	absolute := parts[0] == "" && len(parts) > 1
	var comps []string
	for _, p := range parts {
		switch p {
		case "..":
			// The path can leave the working tree, so it is not this repo's.
			return false
		case "", ".":
			// Empty from a leading, trailing or doubled separator; "." is the
			// working directory itself.
		default:
			comps = append(comps, p)
		}
	}
	if len(comps) == 0 || !devBinaries[comps[len(comps)-1]] {
		return false
	}
	switch {
	case len(comps) == 1:
		// A bare name resolved off PATH — but "/dev" is the device directory,
		// not a program.
		return !absolute
	case len(comps) == 2:
		return !absolute && devToolDirs[comps[0]]
	default:
		return devVenvDirs[comps[len(comps)-3]] && devToolDirs[comps[len(comps)-2]]
	}
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
