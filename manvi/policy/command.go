package policy

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode"

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
	redirectTailRe = regexp.MustCompile(`(?: \d*(?:>>|>\||>|<|&>|&>>) ?\S+| \d*>&\d+)$`)
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
	// Root is the absolute path of the working tree this gate judges for.
	//
	// It exists so that an absolute `…/.venv/bin/dev` can be told apart from
	// the repository's own — see NormalizeAllowlistCommand. Empty is a
	// supported value and it fails closed: with no root to compare against,
	// no absolute path is attributed to this tree, so none is normalised into
	// a bare `dev` and none inherits `dev`'s allowlist entries. A gate that
	// leaves this unset is therefore stricter, never looser.
	Root string
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
	// Length is checked once, here, before any rung reads the line. Every rung
	// below is a scan and several recurse, so an unbounded line is an unbounded
	// amount of work handed to the gate by whoever composed the string — and
	// the string is composed by a model. Measured on this machine, a 1.8 MB
	// line of chained substitutions took seven seconds to refuse.
	//
	// It is deliberately not a rung inside the ladder: a rung would be reached
	// per clause, after the splitter had already walked the whole input, which
	// is most of the cost it exists to avoid.
	if len(command) > maxCommandBytes {
		taskID := ""
		if task != nil {
			taskID = task.ID
		}
		return g.noteHardRules(deny(RuleCommandTooLong,
			fmt.Sprintf("Command is %d bytes; the limit is %d. A line this long is not a command "+
				"someone wrote — put the work in a script file and run that.",
				len(command), maxCommandBytes),
			// The line itself is not echoed back into the decision: it is
			// megabytes of model-authored text and Target travels into logs,
			// transcripts and the TUI.
			fmt.Sprintf("<%d-byte command>", len(command)), taskID))
	}
	return g.evaluate(command, task, 0)
}

// maxCommandBytes bounds the command line the ladder will read.
//
// Generous by two orders of magnitude against anything a person or an agent
// legitimately writes — real command lines are tens of bytes — and small enough
// that the worst measured input costs well under a second to refuse. It is a
// backstop on cost, not a style rule.
const maxCommandBytes = 128 << 10

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
	normalized := NormalizeAllowlistCommandInRoot(command, g.Root)
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
	// Checked on the raw line, beside the heredoc rung, because it is the same
	// refusal: a construct whose meaning is not in the text being judged. It
	// runs before the substitution rung so that `eval $(...)` is named as the
	// re-parse it is rather than as the substitution it also contains.
	if word, isReparse := reparsingCommandWord(raw); isReparse {
		return g.noteHardRules(deny(RuleCommandReparse,
			"`"+word+"` re-parses its argument as shell code after expansion, so nothing in this "+
				"line — the allowlist match, the git-safety rules, or the redirection targets — "+
				"describes what would actually run; write the commands out directly instead.",
			normalized, taskID))
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

// reparsingCommandWord reports whether this single command's command word is a
// builtin that re-parses its arguments as shell code, and names it.
//
// Only the command word counts. `grep eval file` and `echo eval` mention the
// word without invoking it, and refusing those would make the rung fire on
// text rather than on behaviour — the exact defect the git-safety rules avoid
// by reading a dequoted variant rather than by matching substrings.
//
// The word is dequoted before comparison, because sh removes quotes before it
// decides what to run: `\eval`, `"eval"` and `'ev'al` all invoke the builtin.
// Leading VAR=value assignments are stepped over for the same reason — they
// precede the command word without being it.
func reparsingCommandWord(command string) (string, bool) {
	words := shellWords(command)
	for i := 0; i < len(words); i++ {
		word := words[i]
		if word.quotedHead {
			// The word begins inside quotes, so its first character is data
			// rather than syntax: `">"` is a filename and `"FOO=1"` is a
			// command named FOO=1, neither an operator nor an assignment. It
			// can still *be* the command word — sh runs `"eval"` and `\eval`
			// alike — so it is matched here before the scan stops.
			if reparsingBuiltins[word.text] {
				return word.text, true
			}
			return "", false
		}
		if isAssignmentWord(word.text) {
			continue
		}
		if operand, isRedirect := redirectionPrefix(word.text); isRedirect {
			// `> out eval …` puts the target in the next word; `2>out eval …`
			// carries it in this one. Stepping over the wrong number of words
			// is how `> out eval` read `>` as the command and stopped looking.
			if operand {
				i++
			}
			continue
		}
		// `command eval …` and `builtin eval …` reach the builtin through a
		// wrapper, so the search continues past them rather than stopping on a
		// word that is not itself the thing being run.
		if word.text == "command" || word.text == "builtin" {
			continue
		}
		if reparsingBuiltins[word.text] {
			return word.text, true
		}
		return "", false
	}
	return "", false
}

// redirectionPrefix reports whether a word is a redirection operator standing
// before the command word, and whether its target is a separate word.
func redirectionPrefix(word string) (targetIsNextWord, isRedirect bool) {
	i := 0
	for i < len(word) && word[i] >= '0' && word[i] <= '9' {
		i++
	}
	if i >= len(word) || (word[i] != '<' && word[i] != '>') {
		return false, false
	}
	for i < len(word) && (word[i] == '<' || word[i] == '>' || word[i] == '&' || word[i] == '|') {
		i++
	}
	return i == len(word), true
}

// shellWord is one word of a command line, with its quoting recorded.
type shellWord struct {
	text string
	// quotedHead reports whether the word's *first* character was produced by
	// quoting or by a backslash escape, which is the distinction sh itself
	// draws when deciding whether a word is syntax or data.
	//
	// It is the head specifically, not "any part quoted". `FOO="a b"` is an
	// assignment — the quoting is in the value — while `"FOO=1"` is a command
	// named FOO=1; `>` is an operator while `">"` is a filename. A flag set by
	// quoting anywhere in the word conflates those, and did: it read
	// `FOO="a b" eval …` as a quoted literal and stopped looking for the
	// command word.
	quotedHead bool
}

// shellWords splits a command line into the words sh would produce, honouring
// quotes and backslash escapes and performing no expansion.
//
// strings.Fields is not a substitute, and using it here was a defect this
// function exists to fix: it split `FOO="a b" eval "…"` into four pieces, the
// second of which (`b"`) is neither an assignment nor a command, so the scan
// concluded the command word was not eval and stopped. Quoting is precisely how
// a shell word holds a space, so a word splitter that does not read quotes is
// answering a different question from the one sh asks.
func shellWords(command string) []shellWord {
	var words []shellWord
	var cur strings.Builder
	started, quotedHead := false, false
	quote := rune(0)
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case quote == 0 && r == '\\' && i+1 < len(runes):
			quotedHead = quotedHead || !started
			cur.WriteRune(runes[i+1])
			i++
			started = true
		case quote == 0 && (r == '\'' || r == '"'):
			quotedHead = quotedHead || !started
			quote = r
			started = true
		case quote != 0 && r == quote:
			quote = 0
		case quote == '"' && r == '\\' && i+1 < len(runes) &&
			(runes[i+1] == '"' || runes[i+1] == '\\' || runes[i+1] == '$' || runes[i+1] == '`'):
			cur.WriteRune(runes[i+1])
			i++
		case quote != 0:
			cur.WriteRune(r)
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if started {
				words = append(words, shellWord{text: cur.String(), quotedHead: quotedHead})
				cur.Reset()
				started, quotedHead = false, false
			}
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	if started {
		words = append(words, shellWord{text: cur.String(), quotedHead: quotedHead})
	}
	return words
}

// reparsingBuiltins are the command words whose arguments become shell code
// only at run time.
//
// `eval` is the whole set, and the boundary is deliberate. This rung refuses
// code that is *inline in the line being judged* — text the gate holds and
// cannot interpret. Running a script that lives on disk (`sh x.sh`, `make`,
// `pytest`) is a different problem and is not addressed here: the code is not
// in this string at all, so no reading of this string could catch it, and
// refusing the command words that do it would deny every test runner while
// leaving the capability one rename away. That boundary is documented in
// docs/POLICY_AND_SAFETY.md rather than left implicit here.
var reparsingBuiltins = map[string]bool{"eval": true}

// isAssignmentWord reports whether a word is a NAME=value prefix rather than
// the command word.
func isAssignmentWord(word string) bool {
	eq := strings.Index(word, "=")
	if eq <= 0 {
		return false
	}
	for i, r := range word[:eq] {
		if r == '_' || unicode.IsLetter(r) {
			continue
		}
		if i > 0 && unicode.IsDigit(r) {
			continue
		}
		return false
	}
	return true
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
// would write: the targets of >, >>, >|, &>, >& and their fd-prefixed forms.
// Input redirections (<) are reads, which this ladder does not gate, and a
// heredoc introducer (<<) is not a path at all; neither is returned.
//
// The second return value reports whether a target could not be resolved to a
// literal path — it carries an expansion ($HOME, ${VAR}, ~) whose value only
// the shell knows, or it sits inside a construct this scanner could not read
// to its end. A target the gate cannot name is a write it cannot judge, so the
// caller must treat that as a refusal rather than skip the check.
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
	targets, opaque, err := redirectTargets(command, 0)
	if err != nil {
		return nil, false, err
	}
	// Deduplicated, and bounded. Both are about what the caller does with this
	// list: it puts every entry through the write gate, so the length of the
	// list is a multiplier on the cost of judging one command line.
	//
	// `a > f && a > f && …` names one file however many times it is repeated,
	// and re-judging it once per clause turned a 360 KiB command line into
	// eight seconds of gate time — the same verdict, twenty thousand times.
	// Distinctness is the honest unit anyway: the question the gate answers is
	// "may this file be written", and it has one answer per file.
	seen := make(map[string]struct{}, len(targets))
	distinct := make([]string, 0, len(targets))
	for _, target := range targets {
		if _, dup := seen[target]; dup {
			continue
		}
		if len(distinct) >= maxRedirectTargets {
			// Past the cap the enumeration is incomplete, and incomplete is
			// exactly what opacity means — the caller refuses rather than
			// judging a prefix of the writes and calling it the whole set.
			return distinct, true, nil
		}
		seen[target] = struct{}{}
		distinct = append(distinct, target)
	}
	return distinct, opaque, nil
}

// maxRedirectTargets bounds how many distinct files one command line may be
// judged to write.
//
// It is a backstop rather than a tuning knob. A real command redirects into a
// handful of files; a line naming more than this is not a command someone
// wrote, and enumerating it without limit hands an unbounded amount of gate
// work to whoever composed the string. Exceeding it is reported as an
// incomplete enumeration, so the caller fails closed instead of judging the
// first sixty-four writes and ignoring the rest.
const maxRedirectTargets = 64

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
			// A backtick closes a legacy substitution; it is never part of the
			// path. Without it `echo `cat > f`` yielded the target "f`", and
			// the gate then judged a filename the shell never opens — which
			// took a write to .env past the secret rung as ".env`".
			if q == 0 && (r == ' ' || r == '\n' || r == '\t' || r == ';' || r == '|' || r == '&' ||
				r == '`' || r == '(' || r == ')') {
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
			} else if i < n && runes[i] == '|' {
				// >| is > with noclobber overridden. It names a path exactly
				// as > does; read as an unresolvable target it reported
				// opacity, which refused the command for the wrong reason and
				// — because the refusal never reached the write gate — let the
				// path itself go unjudged.
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
// An ABSOLUTE `…/.venv/bin/dev` is the one spelling that cannot be judged from
// the command alone: `/repo/.venv/bin/dev` and `/tmp/attacker/.venv/bin/dev`
// have the same shape, and only one of them is this tree's. That is what
// NormalizeAllowlistCommandInRoot's root argument decides. This entry point
// keeps the old signature for callers with no root to give, and answers the
// question the only way an unknown root permits: no absolute path is this
// repository's, so none is laundered into a bare `dev`. Fail closed — a
// spelling that is refused here is a spelling that must be spelled out in the
// allowlist, which is a nuisance; a spelling wrongly accepted here is a
// foreign binary inheriting `dev`'s entries, which is not.
func NormalizeAllowlistCommand(command string) string {
	return NormalizeAllowlistCommandInRoot(command, "")
}

// NormalizeAllowlistCommandInRoot is NormalizeAllowlistCommand with the working
// tree named, so an absolute path to this repository's own virtualenv `dev` is
// distinguishable from a foreign one at the same layout.
//
// root must be absolute; anything else is treated as no root at all, because a
// relative root cannot decide containment for an absolute token and guessing
// from the process's cwd would make the answer depend on where the harness
// happened to be started.
func NormalizeAllowlistCommandInRoot(command, root string) string {
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
		if isRepoDevBinary(token, root) {
			tokens[i] = "dev"
		}
	}
	return strings.Join(tokens, " ")
}

// insideRoot reports whether an absolute token names a path strictly beneath an
// absolute root.
//
// Slash semantics rather than filepath's, deliberately: these strings are
// handed to `sh`, which resolves "/" and nothing else, so judging them by the
// host platform's separator would judge a path the shell will never walk. A
// token equal to the root is not inside it — the root is a directory, not a
// program.
func insideRoot(root, token string) bool {
	if !strings.HasPrefix(root, "/") || !strings.HasPrefix(token, "/") {
		return false
	}
	// path.Clean cannot climb out here: isRepoDevBinary has already refused
	// any token carrying a ".." component.
	cleanRoot := path.Clean(root)
	cleanToken := path.Clean(token)
	if cleanRoot == "/" {
		return cleanToken != "/"
	}
	return strings.HasPrefix(cleanToken, cleanRoot+"/")
}

// isRepoDevBinary reports whether a token names this repo's own dev CLI in a
// spelling the gate can attribute to the working tree. See
// NormalizeAllowlistCommand for why the answer has to be this narrow.
func isRepoDevBinary(token, root string) bool {
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
		if !devVenvDirs[comps[len(comps)-3]] || !devToolDirs[comps[len(comps)-2]] {
			return false
		}
		if !absolute {
			// Relative, no "..": it resolves beneath the working directory,
			// which is this tree.
			return true
		}
		// Absolute, so the layout alone proves nothing — /tmp/attacker/.venv/
		// bin/dev has exactly the shape a project virtualenv produces. Only
		// containment in the tree this gate was built for makes it this
		// repository's, and an unknown root cannot establish that.
		return insideRoot(root, token)
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
