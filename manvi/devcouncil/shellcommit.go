package devcouncil

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

const shellCommitPointer = "Use the devcouncil git commit tool (devcouncil_git_stage, then devcouncil_git_commit) " +
	"instead of the shell: it checks exactly what it commits."

// shellCommitScan is what reading one command line found.
type shellCommitScan struct {
	raw     string
	commits int
	others  int
	reasons []string
	aliases func() (map[string]string, error)
	loaded  map[string]string
}

func (s *shellCommitScan) refuse(format string, a ...any) {
	s.reasons = append(s.reasons, fmt.Sprintf(format, a...))
}

// The shell's `git commit` and the secret content scan.
//
// devcouncil_exec_command is allowed to run `git commit` (the owner's policy,
// hooks and --no-verify included), and a commit made that way used to skip
// every check the commit tool makes: a staged copy of a credential under an
// innocent name went into history through the shell although the tool would
// have refused it. The content scan is Manvi's, not a repository hook, so the
// owner's waiver of hooks does not cover it.
//
// shellCommitRefusal runs between the final policy decision and runShell. It
// applies checkStagedForCommit, the commit tool's own sequence, to the index
// the shell commit is about to record. That is only honest when the commit's
// input IS the current index, so a command is vouched for only in one shape: a
// single `git commit` and nothing else in the command line, with no option,
// environment variable or global git option that changes what is committed.
// Everything the scan cannot vouch for is refused, fail closed, with a pointer
// to the commit tool (an unparseable command that mentions `commit` included).
//
// Refused forms (each is the same refusal; the reason names the form):
//   - the command line has any other command, before or after the commit, or
//     two commits (an earlier `git add`/`rm`/`mv`/`apply --cached`/
//     `update-index`/`read-tree`/`restore --staged` changes the index after it
//     was scanned; a later or backgrounded one races the commit);
//   - a command substitution or backtick inside the commit command;
//   - commit -a/--all, -i/--include, -o/--only, -p/--patch, --interactive,
//     --pathspec-from-file, a pathspec, any word after `--`, and any commit
//     option not on the allow-list below (git accepts abbreviated long
//     options, so only exact spellings are vouched for);
//   - GIT_* environment other than author/committer identity and GIT_EDITOR
//     (GIT_INDEX_FILE, GIT_DIR, GIT_WORK_TREE, GIT_CONFIG_*, ...);
//   - global git options -C, --git-dir, --work-tree, --namespace, --bare,
//     --exec-path, --config-env, --attr-source and --super-prefix, and `-c`
//     for any key outside a short list of identity/signing settings (an alias,
//     include.path or core.worktree override changes what runs or where);
//   - a git subcommand name that is a configured alias for a commit, or any
//     `!` alias mentioning one (aliases are resolved from `git config`);
//   - the same command reached through sh/bash/zsh/dash/ksh -c, eval, env,
//     command, exec, nohup, time, sudo, timeout, xargs, find -exec, and the
//     other wrappers in shellWrappers, which are unwrapped and judged as above;
//     a shell reading its program from stdin or a pipe in a command that
//     mentions `commit`.
//
// Residuals, which the scan does NOT cover and which are deliberately not
// denied (denying them would change the owner's command policy):
//   - subcommands that create commits whose content is not in the index before
//     they run: am, cherry-pick, rebase, revert, merge (including --commit),
//     commit-tree, pull (a merge commit), and stash (its commits are not on a
//     branch, and its content is the working tree plus the index);
//   - any program that commits on its own: a script file, make target,
//     interpreter (`python -c`), or git hook, a hook that rewrites the index
//     between the scan and the commit (the tool re-reads the tree after the
//     commit; the shell form cannot), and shell functions or aliases defined
//     in an earlier command.
//
// shellCommitRefusal returns a refusal when command would create a commit the
// scan cannot vouch for or finds a leak in, and nil when it may run.
func (r *Registry) shellCommitRefusal(ctx context.Context, command string) *tools.Result {
	scan := &shellCommitScan{raw: command}
	scan.aliases = func() (map[string]string, error) { return r.gitAliases(ctx) }
	scan.inspect(command, 0)
	if scan.commits == 0 && len(scan.reasons) == 0 {
		return nil
	}
	if scan.commits > 1 || (scan.commits > 0 && scan.others > 0) {
		scan.refuse("the command line holds a commit together with other commands, so the index the scan read " +
			"is not necessarily the index the commit records")
	}
	if len(scan.reasons) > 0 {
		res := shellCommitBlocked(strings.Join(dedupe(scan.reasons), "; "))
		return &res
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	refusal, _, _ := r.checkStagedForCommit(ctx)
	if refusal == nil {
		return nil
	}
	res := *refusal
	res.Text += "\n" + shellCommitPointer
	return &res
}

func shellCommitBlocked(reason string) tools.Result {
	payload := map[string]any{
		"operation": "commit",
		"rule":      string(policy.RuleSecretPath),
		"severity":  string(policy.Hard),
		"reason": "this shell command would create a commit that the staged-content secret check cannot vouch for: " +
			reason + ". " + shellCommitPointer,
	}
	data, _ := json.Marshal(payload)
	return tools.Result{
		Text:     string(data),
		IsError:  true,
		Blocked:  true,
		Rule:     string(policy.RuleSecretPath),
		Severity: string(policy.Hard),
	}
}

// gitAliases reads the configured aliases, name to value, from every scope.
func (r *Registry) gitAliases(ctx context.Context) (map[string]string, error) {
	out, err := runGit(ctx, r.deps.Root, "config", "-z", "--list")
	if err != nil {
		return nil, err
	}
	aliases := map[string]string{}
	for _, rec := range strings.Split(out, "\x00") {
		key, value, _ := strings.Cut(rec, "\n")
		if name, ok := strings.CutPrefix(strings.ToLower(key), "alias."); ok {
			aliases[name] = value
		}
	}
	return aliases, nil
}

const maxShellCommitDepth = 6

// inspect reads one command line: its segments, and through wrappers the
// command lines nested inside them.
func (s *shellCommitScan) inspect(command string, depth int) {
	if depth > maxShellCommitDepth {
		s.refuse("the command nests wrappers deeper than %d levels", maxShellCommitDepth)
		return
	}
	for _, seg := range policy.SplitCommandChain(command) {
		words, ok := shellTokens(seg)
		if !ok {
			if strings.Contains(seg, "commit") {
				s.refuse("a command mentioning `commit` could not be parsed (unbalanced quoting)")
			} else {
				s.others++
			}
			continue
		}
		s.inspectWords(words, nil, depth)
	}
}

type shellTok struct {
	text    string
	expands bool // an unquoted or double-quoted $ or backtick: the real text is chosen at run time
	quoted  bool
}

var assignmentWord = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
var redirectWord = regexp.MustCompile(`^(?:[0-9]*(?:>>?|<|>&|<&)|&>>?)[^ ]*$`)
var redirectOperator = regexp.MustCompile(`^(?:[0-9]*(?:>>?|<|>&|<&)|&>>?)$`)

// shellTokens splits one segment into words with sh's quoting. It reports false
// for an unterminated quote. Redirections are dropped (a redirect target is not
// an argument), but a command substitution inside any word marks it expands.
func shellTokens(seg string) ([]shellTok, bool) {
	var words []shellTok
	var cur strings.Builder
	var curExpands, curQuoted, started bool
	flush := func() {
		if started {
			words = append(words, shellTok{text: cur.String(), expands: curExpands, quoted: curQuoted})
		}
		cur.Reset()
		curExpands, curQuoted, started = false, false, false
	}
	rs := []rune(seg)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
		case c == '\\':
			started = true
			if i+1 < len(rs) {
				i++
				cur.WriteRune(rs[i])
			}
		case c == '\'':
			started, curQuoted = true, true
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				j++
			}
			if j >= len(rs) {
				return nil, false
			}
			cur.WriteString(string(rs[i+1 : j]))
			i = j
		case c == '"':
			started, curQuoted = true, true
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				if rs[j] == '\\' && j+1 < len(rs) {
					if strings.ContainsRune("\"\\$`\n", rs[j+1]) {
						j++
					}
				} else if rs[j] == '$' || rs[j] == '`' {
					curExpands = true
				}
				cur.WriteRune(rs[j])
				j++
			}
			if j >= len(rs) {
				return nil, false
			}
			i = j
		case c == '$' || c == '`':
			started, curExpands = true, true
			cur.WriteRune(c)
		default:
			started = true
			cur.WriteRune(c)
		}
	}
	flush()
	var out []shellTok
	for i := 0; i < len(words); i++ {
		w := words[i]
		if w.quoted || w.expands || !redirectWord.MatchString(w.text) {
			out = append(out, w)
			continue
		}
		if redirectOperator.MatchString(w.text) {
			i++ // the target is the next word, not an argument
		}
	}
	return out, true
}

var shellKeywords = map[string]bool{
	"then": true, "do": true, "else": true, "elif": true, "if": true, "while": true, "until": true,
	"!": true, "{": true, "(": true, "time": true, "function": true,
}

var shellInterpreters = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true}

// shellWrappers run another command given later on their own line. The wrapped
// command is found by its name rather than by parsing each wrapper's options,
// which is why this errs towards finding a `git` that is only an operand.
var shellWrappers = map[string]bool{
	"env": true, "command": true, "builtin": true, "exec": true, "nohup": true, "sudo": true, "doas": true,
	"nice": true, "ionice": true, "timeout": true, "stdbuf": true, "xargs": true, "setsid": true, "find": true,
	"watch": true, "flock": true, "parallel": true, "caffeinate": true, "chronic": true, "unbuffer": true,
}

// shellInert never runs the git word they are given.
var shellInert = map[string]bool{
	"echo": true, "printf": true, "grep": true, "rg": true, "egrep": true, "fgrep": true, "cat": true,
	"man": true, "which": true, "type": true, "head": true, "tail": true, "less": true, "sed": true, "awk": true,
}

func (s *shellCommitScan) inspectWords(words []shellTok, assigns []string, depth int) {
	for len(words) > 0 {
		t := words[0].text
		if shellKeywords[t] {
			words = words[1:]
			continue
		}
		if trimmed := strings.TrimLeft(t, "({"); trimmed != t && !words[0].quoted {
			if trimmed == "" {
				words = words[1:]
				continue
			}
			words = append([]shellTok{{text: trimmed, expands: words[0].expands}}, words[1:]...)
			continue
		}
		break
	}
	for len(words) > 0 && !words[0].quoted && assignmentWord.MatchString(words[0].text) {
		assigns = append(assigns, words[0].text)
		words = words[1:]
	}
	if len(words) == 0 {
		s.others++
		return
	}
	cmd := words[0]
	if cmd.expands {
		for _, w := range words {
			if w.text == "commit" {
				s.refuse("a command word chosen by expansion is followed by `commit`")
				return
			}
		}
		s.others++
		return
	}
	base := path.Base(cmd.text)
	switch {
	case base == "git":
		s.inspectGit(words[1:], assigns, depth)
	case base == "git-commit":
		s.inspectGit(append([]shellTok{{text: "commit"}}, words[1:]...), assigns, depth)
	case shellInterpreters[base]:
		s.inspectShell(words[1:], depth)
	case base == "eval":
		parts := make([]string, 0, len(words)-1)
		for _, w := range words[1:] {
			parts = append(parts, w.text)
		}
		s.inspect(strings.Join(parts, " "), depth+1)
	case shellWrappers[base]:
		for j := 1; j < len(words); j++ {
			b := path.Base(words[j].text)
			if words[j].expands || (b != "git" && !shellInterpreters[b]) {
				continue
			}
			inner := append([]string(nil), assigns...)
			for _, w := range words[1:j] {
				if !w.quoted && assignmentWord.MatchString(w.text) {
					inner = append(inner, w.text)
				}
			}
			s.inspectWords(words[j:], inner, depth+1)
			return
		}
		s.others++
	case !shellInert[base]:
		for j, w := range words {
			if path.Base(w.text) != "git" || w.expands {
				continue
			}
			for _, later := range words[j+1:] {
				if later.text == "commit" {
					s.inspectGit(words[j+1:], assigns, depth)
					return
				}
			}
		}
		s.others++
	default:
		s.others++
	}
}

// inspectShell reads `sh -c script`: the script is a command line of its own.
func (s *shellCommitScan) inspectShell(args []shellTok, depth int) {
	for i := 0; i < len(args); i++ {
		a := args[i].text
		if !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "+") {
			break // a script file: its content is not visible here
		}
		if a == "-o" || a == "+o" || a == "-O" || a == "+O" {
			i++
			continue
		}
		if !strings.HasPrefix(a, "--") && strings.Contains(a, "c") {
			if i+1 < len(args) {
				s.inspect(args[i+1].text, depth+1)
				return
			}
			break
		}
	}
	if strings.Contains(s.raw, "commit") {
		s.refuse("a shell reading its program from a file or standard input appears in a command mentioning `commit`")
		return
	}
	s.others++
}

// builtinGit are subcommand names git itself defines, which no alias can
// override, so they need no alias lookup.
var builtinGit = map[string]bool{
	"status": true, "log": true, "diff": true, "add": true, "rm": true, "mv": true, "show": true, "branch": true,
	"checkout": true, "switch": true, "restore": true, "reset": true, "fetch": true, "pull": true, "push": true,
	"remote": true, "tag": true, "config": true, "stash": true, "merge": true, "rebase": true, "cherry-pick": true,
	"revert": true, "am": true, "apply": true, "clean": true, "grep": true, "ls-files": true, "rev-parse": true,
	"describe": true, "blame": true, "bisect": true, "submodule": true, "worktree": true, "init": true,
	"clone": true, "reflog": true, "format-patch": true, "show-ref": true, "cat-file": true, "rev-list": true,
	"ls-tree": true, "ls-remote": true, "commit-tree": true, "update-index": true, "read-tree": true,
	"write-tree": true, "help": true, "version": true, "gc": true, "fsck": true, "archive": true, "shortlog": true,
	"notes": true, "mergetool": true, "difftool": true, "range-diff": true, "sparse-checkout": true,
	"update-ref": true, "symbolic-ref": true, "hash-object": true, "for-each-ref": true, "name-rev": true,
}

var gitGlobalFlags = map[string]bool{
	"--no-pager": true, "-p": true, "--paginate": true, "-P": true, "--no-optional-locks": true,
	"--no-replace-objects": true, "--literal-pathspecs": true, "--glob-pathspecs": true,
	"--noglob-pathspecs": true, "--icase-pathspecs": true, "--no-lazy-fetch": true, "--no-advice": true,
	"--version": true, "--help": true, "-h": true,
}

var gitGlobalValued = map[string]bool{
	"-C": true, "--git-dir": true, "--work-tree": true, "--namespace": true, "--exec-path": true,
	"--config-env": true, "--attr-source": true, "--super-prefix": true,
}

// safeConfigKey are the -c settings that do not change what is committed.
var safeConfigKey = regexp.MustCompile(`^(?:user\.[a-z.]+|commit\.(?:gpgsign|cleanup|verbose)|gpg\.[a-z.]+|core\.(?:editor|commentchar|abbrev))$`)

var safeGitEnv = map[string]bool{
	"GIT_AUTHOR_NAME": true, "GIT_AUTHOR_EMAIL": true, "GIT_AUTHOR_DATE": true,
	"GIT_COMMITTER_NAME": true, "GIT_COMMITTER_EMAIL": true, "GIT_COMMITTER_DATE": true, "GIT_EDITOR": true,
}

// inspectGit reads the words after `git`: its global options, the subcommand,
// and, for a commit, the commit's options.
func (s *shellCommitScan) inspectGit(args []shellTok, assigns []string, depth int) {
	var globalReasons []string
	sub := -1
	for i := 0; i < len(args) && sub < 0; i++ {
		a := args[i].text
		if args[i].expands {
			for _, w := range args {
				if w.text == "commit" {
					s.refuse("a git word chosen by expansion is followed by `commit`")
					return
				}
			}
			s.others++
			return
		}
		if !strings.HasPrefix(a, "-") {
			sub = i
			break
		}
		name, _, hasValue := strings.Cut(a, "=")
		switch {
		case a == "-c":
			if i+1 >= len(args) {
				globalReasons = append(globalReasons, "`git -c` has no setting")
				break
			}
			i++
			key, _, _ := strings.Cut(args[i].text, "=")
			if !safeConfigKey.MatchString(strings.ToLower(key)) {
				globalReasons = append(globalReasons, fmt.Sprintf("`git -c %s` may change what is committed or what runs", key))
			}
		case gitGlobalValued[name]:
			globalReasons = append(globalReasons, fmt.Sprintf("the global git option %s changes which repository or tree is committed", name))
			if !hasValue {
				i++
			}
		case gitGlobalFlags[a]:
		default:
			globalReasons = append(globalReasons, fmt.Sprintf("the global git option %s is not one the scan recognises", a))
		}
	}
	if sub < 0 {
		s.others++
		return
	}
	name := args[sub].text
	rest := args[sub+1:]

	if !builtinGit[name] && name != "commit" {
		for _, g := range globalReasons {
			if strings.Contains(g, "alias.") {
				s.refuse("%s: a command-line alias could stand for a commit", g)
				return
			}
		}
		aliases, err := s.loadAliases()
		if err != nil {
			s.refuse("git aliases could not be read to resolve `git %s` (%v)", name, err)
			return
		}
		if value, ok := aliases[strings.ToLower(name)]; ok {
			if depth > maxShellCommitDepth {
				s.refuse("git aliases nest deeper than %d levels", maxShellCommitDepth)
				return
			}
			if script, shell := strings.CutPrefix(strings.TrimSpace(value), "!"); shell {
				s.inspect(script, depth+1)
				return
			}
			expanded, ok := shellTokens(value)
			if !ok {
				s.refuse("the git alias %q could not be parsed", name)
				return
			}
			s.inspectGit(append(expanded, rest...), assigns, depth+1)
			return
		}
	}

	if name != "commit" {
		s.others++
		return
	}
	s.commits++
	for _, g := range globalReasons {
		s.refuse("%s", g)
	}
	for _, a := range assigns {
		key, _, _ := strings.Cut(a, "=")
		if strings.HasPrefix(strings.ToUpper(key), "GIT_") && !safeGitEnv[key] {
			s.refuse("%s in the environment changes which index or tree is committed", key)
		}
	}
	for _, w := range rest {
		if w.expands && (strings.Contains(w.text, "$(") || strings.Contains(w.text, "`")) {
			s.refuse("a command substitution inside the commit command can change the index before it is read")
			break
		}
	}
	s.inspectCommitOptions(rest)
}

func (s *shellCommitScan) loadAliases() (map[string]string, error) {
	if s.loaded != nil {
		return s.loaded, nil
	}
	a, err := s.aliases()
	if err != nil {
		return nil, err
	}
	s.loaded = a
	return a, nil
}

var commitLongFlags = map[string]bool{
	"amend": true, "no-verify": true, "verify": true, "allow-empty": true, "allow-empty-message": true,
	"no-edit": true, "edit": true, "signoff": true, "no-signoff": true, "verbose": true, "quiet": true,
	"no-gpg-sign": true, "gpg-sign": true, "reset-author": true, "no-post-rewrite": true, "status": true,
	"no-status": true, "dry-run": true, "short": true, "porcelain": true, "long": true, "branch": true,
	"null": true, "untracked-files": true,
}

var commitLongValued = map[string]bool{
	"message": true, "file": true, "author": true, "date": true, "reuse-message": true,
	"reedit-message": true, "fixup": true, "squash": true, "template": true, "cleanup": true, "trailer": true,
}

var commitRefusedLong = map[string]string{
	"all": "-a/--all commits the working tree, not the index", "include": "--include stages paths itself",
	"only": "--only commits the named paths, not the index", "patch": "--patch stages interactively",
	"interactive": "--interactive stages interactively", "pathspec-from-file": "--pathspec-from-file names paths to commit",
	"pathspec-file-nul": "--pathspec-file-nul names paths to commit",
}

// inspectCommitOptions refuses every option and operand that makes the commit's
// input something other than the current index. It allows only spellings it
// knows: git accepts abbreviated long options, and an abbreviation of --all is
// still --all.
func (s *shellCommitScan) inspectCommitOptions(args []shellTok) {
	for i := 0; i < len(args); i++ {
		a := args[i].text
		switch {
		case a == "--":
			if i+1 < len(args) {
				s.refuse("a pathspec after `--` commits those paths from the working tree, not the index")
			}
			return
		case strings.HasPrefix(a, "--"):
			name, _, hasValue := strings.Cut(strings.TrimPrefix(a, "--"), "=")
			switch {
			case commitRefusedLong[name] != "":
				s.refuse("%s", commitRefusedLong[name])
			case commitLongValued[name]:
				if !hasValue {
					i++
				}
			case commitLongFlags[name]:
			default:
				s.refuse("the commit option --%s is not one the scan recognises", name)
			}
		case strings.HasPrefix(a, "-") && a != "-":
			cluster := a[1:]
			for k := 0; k < len(cluster); k++ {
				c := cluster[k]
				switch {
				case strings.IndexByte("mFCct", c) >= 0:
					if k+1 == len(cluster) {
						i++
					}
					k = len(cluster)
				case c == 'S' || c == 'u':
					k = len(cluster) // optional value attached, never the next word
				case strings.IndexByte("svqnehz", c) >= 0:
				case c == 'a', c == 'i', c == 'o', c == 'p':
					s.refuse("-%c commits something other than the index as it stands", c)
				default:
					s.refuse("the commit option -%c is not one the scan recognises", c)
				}
			}
		default:
			s.refuse("the pathspec %q commits those paths from the working tree, not the index", a)
		}
	}
}
