package devcouncil

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"manvi/internal/fnmatch"
	"manvi/internal/proc"
	"manvi/policy"
	"manvi/tools"
)

// The git integration gives an agent structured access to version control
// without handing it a shell. The reads answer orientation questions — where
// am I, what changed, what landed recently — and the writes go through the
// same command gate devcouncil_exec_command uses, because staging and
// committing *are* shell commands semantically and one canonical owner of
// that judgement beats a second, softer one beside it.
//
// What these tools deliberately do not offer: push, reset, rebase, checkout,
// stash, config, hooks, plumbing. Everything that moves or rewrites history
// stays behind the allowlists of devcouncil_exec_command, whose refusals are
// recorded, grantable by exactly the authorities that should be granting
// them, and visible in the run report. Convenience that skips the ladder is
// not convenience; it is the workaround the override seam exists to make
// unnecessary.

const (
	// defaultGitLogCommits balances context against context: a log is pulled
	// to orient, not to read, and ten subjects answer "what happened lately"
	// without burying the turn.
	defaultGitLogCommits = 10
	maxGitLogCommits     = 50

	// maxShowPatchBytes caps the patch half of devcouncil_git_show. A merge
	// commit over a wide tree renders megabytes; the metadata half carries
	// the identity either way, and the cap is reported rather than silent.
	maxShowPatchBytes = 128 << 10

	// maxGitOutputBytes caps every read-only invocation's captured output,
	// matching what exec_command allows its children.
	maxGitOutputBytes = 1024 * 1024
)

// showObjectRe admits only what can name a git object. Anything that could
// begin an option ("-"), carry a NUL, or smuggle a revision syntax this tool
// has no business interpreting is refused before git ever sees it.
var showObjectRe = regexp.MustCompile(`^[0-9A-Za-z._/~^:{}-]+$`)

func (r *Registry) gitTools() []tools.Tool {
	return []tools.Tool{
		{
			Schema: schema("devcouncil_git_status",
				"Report the working tree's version-control state: current branch, HEAD commit, "+
					"and which files are staged, modified, or untracked. Read-only; needs no lease.",
				`{"type":"object","properties":{}}`),
			ReadOnly: true,
			Group:    tools.GroupCore,
			Handler:  r.gitStatus,
		},
		{
			Schema: schema("devcouncil_git_log",
				"List recent commits: hash, author, date, subject. Use this to orient before "+
					"changing anything, not to reconstruct history.",
				`{"type":"object","properties":{"max":{"type":"integer","description":"how many commits (default 10, capped at 50)"}}}`),
			ReadOnly: true,
			Group:    tools.GroupCore,
			Handler:  r.gitLog,
		},
		{
			Schema: schema("devcouncil_git_branches",
				"List local branches with the current one marked.",
				`{"type":"object","properties":{}}`),
			ReadOnly: true,
			Group:    tools.GroupCore,
			Handler:  r.gitBranches,
		},
		{
			Schema: schema("devcouncil_git_show",
				"Show one commit's metadata and patch. Defaults to HEAD. The patch is size-capped "+
					"and reports when it was cut short.",
				`{"type":"object","properties":{"object":{"type":"string","description":"a commit hash, short hash, branch, tag, or HEAD~N (default: HEAD)"}}}`),
			ReadOnly: true,
			Group:    tools.GroupCore,
			Handler:  r.gitShow,
		},
		{
			Schema: schema("devcouncil_git_stage",
				"Stage named paths for the next commit. Passes the command policy gate — the same "+
					"ladder as devcouncil_exec_command — and refuses outright to stage credential and "+
					"secret paths under any posture.",
				`{"type":"object","properties":{"paths":{"type":"array","items":{"type":"string"},"description":"repository-relative paths to stage (as devcouncil_git_status reports them)"}},"required":["paths"]}`),
			Group:   tools.GroupCore,
			Handler: r.gitStage,
		},
		{
			Schema: schema("devcouncil_git_commit",
				"Commit everything currently staged with the given message. Passes the command "+
					"policy gate and refuses if the staged set contains a secret path, whatever "+
					"staged it.",
				`{"type":"object","properties":{"message":{"type":"string"},"allow_empty":{"type":"boolean","description":"permit a commit with no staged changes (default false)"}},"required":["message"]}`),
			Group:   tools.GroupCore,
			Handler: r.gitCommit,
		},
	}
}

// --- read-only handlers ---

// gitStatus answers "where am I and what has changed". The porcelain -z form
// is parsed rather than prose because prose is locale-dependent and -z makes
// path boundaries unambiguous, which matters for exactly the awkward filenames
// the diff path already learned to handle.
func (r *Registry) gitStatus(ctx context.Context, call tools.Call) tools.Result {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	// -uall keeps untracked files enumerated individually: porcelain's
	// default collapses an untracked directory to its name ("notes/"), which
	// is useless to a caller staging from this list — it would have to guess
	// what was inside. One token, deliberately: as separate argv elements
	// "all" parses as a pathspec and silently empties the report.
	raw, err := runGit(ctx, r.deps.Root, "status", "--porcelain=v1", "-z", "-b", "-uall")
	if err != nil {
		return unavailable("working-tree status", err)
	}
	status, err := parsePorcelain(raw)
	if err != nil {
		return unavailable("status parse", err)
	}

	payload := map[string]any{
		"branch":    status.Branch,
		"detached":  status.Detached,
		"staged":    status.Staged,
		"unstaged":  status.Unstaged,
		"untracked": status.Untracked,
	}
	if status.Ahead != nil {
		payload["ahead"] = *status.Ahead
	}
	if status.Behind != nil {
		payload["behind"] = *status.Behind
	}

	// An unborn repository has no HEAD. That is a state worth reporting, not
	// an error: "no commits yet" is a usable answer.
	head, headErr := runGit(ctx, r.deps.Root, "log", "-1", "--format=%H%x00%s")
	if headErr == nil {
		if sha, subject, ok := strings.Cut(strings.TrimRight(head, "\n"), "\x00"); ok {
			payload["head"] = map[string]string{"sha": sha, "subject": subject}
		}
	} else {
		payload["head"] = nil
	}
	return ok(payload)
}

type gitStatusResult struct {
	Branch    string
	Detached  bool
	Ahead     *int
	Behind    *int
	Staged    []map[string]any
	Unstaged  []map[string]any
	Untracked []string
}

// parsePorcelain reads `git status --porcelain=v1 -z -b`.
//
// With -z every record is NUL-terminated, and rename and copy records carry a
// second NUL-separated field: the *other* side of the rename. In v1 the first
// field is the path the file has now and the second is where it came from.
func parsePorcelain(raw string) (*gitStatusResult, error) {
	out := &gitStatusResult{}
	fields := strings.Split(raw, "\x00")

	for i := 0; i < len(fields); i++ {
		field := fields[i]
		switch {
		case field == "":
			continue
		case strings.HasPrefix(field, "## "):
			branch, ahead, behind, detached := parseBranchHeader(field)
			out.Branch, out.Ahead, out.Behind, out.Detached = branch, ahead, behind, detached
		case len(field) < 4:
			return nil, fmt.Errorf("malformed porcelain entry %q", field)
		default:
			xy, path := field[:2], field[3:]
			entry := map[string]any{"path": path, "index": xy[:1], "worktree": xy[1:]}
			if xy == "??" || xy == "!!" {
				if xy == "??" {
					out.Untracked = append(out.Untracked, path)
				}
				continue
			}
			// A rename consumes the original path as the following field.
			if xy[0] == 'R' || xy[0] == 'C' || xy[1] == 'R' || xy[1] == 'C' {
				i++
				if i >= len(fields) {
					return nil, fmt.Errorf("rename entry %q has no origin path", field)
				}
				entry["orig_path"] = fields[i]
			}
			if xy[0] != ' ' {
				out.Staged = append(out.Staged, entry)
			}
			if xy[1] != ' ' {
				out.Unstaged = append(out.Unstaged, entry)
			}
		}
	}
	return out, nil
}

// parseBranchHeader reads the `## ` record: "main...origin/main [ahead 1,
// behind 2]", "main", "HEAD (no branch)", or "No commits yet on main".
func parseBranchHeader(field string) (branch string, ahead, behind *int, detached bool) {
	header := strings.TrimSpace(strings.TrimPrefix(field, "## "))
	if header == "" {
		return "", nil, nil, false
	}
	if strings.HasPrefix(header, "HEAD (no branch)") || strings.HasPrefix(header, "HEAD (no commit)") {
		return "HEAD", nil, nil, true
	}
	// An unborn repository spells its state out in prose rather than emitting
	// a tracking spec; read past it to the branch it names.
	if rest, found := strings.CutPrefix(header, "No commits yet on "); found {
		return strings.TrimSpace(rest), nil, nil, false
	}
	branch = header
	if idx := strings.Index(header, "..."); idx >= 0 {
		branch = header[:idx]
		header = header[idx+3:]
	} else if idx := strings.IndexByte(header, ' '); idx >= 0 {
		branch = header[:idx]
		header = header[idx+1:]
	} else {
		header = ""
	}
	// Whatever follows the tracking spec is the "[ahead n, behind m]" note;
	// "No commits yet" arrives here too and simply parses as nothing.
	for _, label := range []struct {
		name  string
		value **int
	}{
		{"ahead", &ahead},
		{"behind", &behind},
	} {
		pattern := label.name + " "
		idx := strings.Index(header, pattern)
		if idx < 0 {
			continue
		}
		rest := header[idx+len(pattern):]
		end := strings.IndexAny(rest, ",]")
		if end < 0 {
			end = len(rest)
		}
		if n, err := strconv.Atoi(strings.TrimSpace(rest[:end])); err == nil {
			*label.value = &n
		}
	}
	return branch, ahead, behind, detached
}

// gitLog returns recent commits. Fields are NUL-separated and records end
// with RS, because a subject may contain any byte except NUL and the ones a
// tab or space joiner would break include tabs.
func (r *Registry) gitLog(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Max int `json:"max"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	max := args.Max
	if max <= 0 {
		max = defaultGitLogCommits
	}
	if max > maxGitLogCommits {
		max = maxGitLogCommits
	}

	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	raw, err := runGit(ctx, r.deps.Root, "log",
		"-n", strconv.Itoa(max),
		"--format=%H%x00%h%x00%an%x00%aI%x00%s%x1e")
	if err != nil {
		// Unborn repository: there is no history to list, and that is an
		// answer rather than a failure — but it is stated, not implied.
		if unborn(err) {
			return ok(map[string]any{"commits": []any{}, "note": "repository has no commits yet"})
		}
		return unavailable("commit log", err)
	}

	var commits []map[string]string
	for _, record := range strings.Split(raw, "\x1e") {
		record = strings.TrimLeft(record, "\n")
		if strings.TrimSpace(record) == "" {
			continue
		}
		parts := strings.Split(record, "\x00")
		if len(parts) != 5 {
			return unavailable("commit log", fmt.Errorf("malformed record with %d fields", len(parts)))
		}
		commits = append(commits, map[string]string{
			"sha":     parts[0],
			"short":   parts[1],
			"author":  parts[2],
			"date":    parts[3],
			"subject": parts[4],
		})
	}
	payload := map[string]any{"commits": commits}
	res := ok(payload)
	if args.Max > maxGitLogCommits {
		note := fmt.Sprintf("git_log: requested %d commits, capped at %d", args.Max, maxGitLogCommits)
		payload["degraded"] = []string{note}
		res.Degraded = []string{note}
	}
	return res
}

// unborn reports whether a git failure is the ordinary "no commits yet" case.
func unborn(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 128
}

func (r *Registry) gitBranches(ctx context.Context, call tools.Call) tools.Result {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	// Branch names cannot contain tabs or newlines (control characters are
	// barred by check-ref-format), so this format round-trips safely.
	raw, err := runGit(ctx, r.deps.Root, "branch",
		"--format=%(refname:short)%09%(objectname:short)%09%(HEAD)")
	if err != nil {
		return unavailable("branch list", err)
	}

	type branch struct {
		Name    string `json:"name"`
		Short   string `json:"sha"`
		Current bool   `json:"current"`
	}
	var branches []branch
	for _, line := range strings.Split(strings.TrimRight(raw, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			return unavailable("branch list", fmt.Errorf("malformed branch line %q", line))
		}
		branches = append(branches, branch{Name: parts[0], Short: parts[1], Current: parts[2] == "*"})
	}
	return ok(map[string]any{"branches": branches})
}

func (r *Registry) gitShow(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Object string `json:"object"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	object := strings.TrimSpace(args.Object)
	if object == "" {
		object = "HEAD"
	}
	if len(object) > 200 || !showObjectRe.MatchString(object) || strings.HasPrefix(object, "-") {
		return tools.Errorf("object %q does not name a git object this tool accepts", args.Object)
	}

	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	meta, err := runGit(ctx, r.deps.Root, "show", "-s",
		"--format=%H%x00%h%x00%an%x00%aI%x00%s", object)
	if err != nil {
		return unavailable(fmt.Sprintf("commit %s", object), err)
	}
	parts := strings.SplitN(strings.TrimRight(meta, "\n"), "\x00", 5)
	if len(parts) != 5 {
		return unavailable("commit metadata", fmt.Errorf("unexpected field count"))
	}

	patchRaw, err := runGit(ctx, r.deps.Root, "show", "--format=", object)
	if err != nil {
		return unavailable(fmt.Sprintf("patch for %s", object), err)
	}
	patch := patchRaw
	var degraded []string
	if len(patch) > maxShowPatchBytes {
		patch = patch[:maxShowPatchBytes]
		degraded = append(degraded, fmt.Sprintf(
			"git_show: patch truncated at %d of %d bytes; pipe through devcouncil_exec_command "+
				"(e.g. `git show --stat %s`) for the rest", maxShowPatchBytes, len(patchRaw), parts[1]))
	}

	payload := map[string]any{
		"object":  object,
		"sha":     parts[0],
		"short":   parts[1],
		"author":  parts[2],
		"date":    parts[3],
		"subject": parts[4],
		"patch":   patch,
	}
	res := ok(payload)
	if len(degraded) > 0 {
		res.Degraded = degraded
	}
	return res
}

// --- gated writes ---

// gitStage stages explicit paths. There is no "stage everything" switch on
// purpose: the caller enumerates what it wants committed, usually straight
// from devcouncil_git_status, and every enumerated path is checked against the
// secret-path patterns before git sees it. Staging a credential is how a
// credential reaches remote history, and no later gate undoes a push.
//
// Authorisation is the command gate's, through the exact command line the
// equivalent shell invocation would have used, so a task allowlist, a global
// allowlist, a grant, or a posture answers once — for both routes.
func (r *Registry) gitStage(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Paths []string `json:"paths"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if len(args.Paths) == 0 {
		return tools.Errorf("paths is required")
	}

	rel := make([]string, 0, len(args.Paths))
	for _, p := range args.Paths {
		normalized, outside := policy.NormalizeRepoPath(r.deps.Root, p)
		if outside {
			return tools.Errorf("path %q does not resolve inside the repository", p)
		}
		rel = append(rel, normalized)
	}
	if leaked := secretPaths(rel); len(leaked) > 0 {
		return secretRefusal("stage", leaked)
	}
	if aliased := aliasedPaths(r.deps.Root, rel); len(aliased) > 0 {
		return aliasedRefusal("stage", aliased)
	}

	argv := append([]string{"add", "--"}, rel...)
	commandLine := renderCommandLine(append([]string{"git"}, argv...))
	return r.gatedGit(ctx, "staging", commandLine, argv, map[string]any{
		"staged": rel,
	})
}

// gitCommit commits the staged set. The staged paths are re-checked against
// the secret patterns at commit time, not trusted to whatever staged them:
// the index is writable by anything with repository access, and the commit is
// the moment contents become history.
func (r *Registry) gitCommit(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Message    string `json:"message"`
		AllowEmpty bool   `json:"allow_empty"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if strings.TrimSpace(args.Message) == "" {
		return tools.Errorf("message is required")
	}

	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	staged, err := runGit(ctx, r.deps.Root, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return unavailable("staged file list", err)
	}
	var stagedPaths []string
	for _, p := range strings.Split(staged, "\x00") {
		if p != "" {
			stagedPaths = append(stagedPaths, p)
		}
	}
	if leaked := secretPaths(stagedPaths); len(leaked) > 0 {
		return secretRefusal("commit", leaked)
	}
	if aliased := aliasedPaths(r.deps.Root, stagedPaths); len(aliased) > 0 {
		return aliasedRefusal("commit", aliased)
	}

	argv := []string{"commit", "-m", args.Message}
	if args.AllowEmpty {
		argv = append(argv, "--allow-empty")
	}
	commandLine := renderCommandLine(append([]string{"git"}, argv...))

	extra := map[string]any{"message": args.Message}
	if len(stagedPaths) > 0 {
		extra["committed_paths"] = stagedPaths
	}
	res := r.gatedGit(ctx, "committing", commandLine, argv, extra)

	// On success, hand back the new HEAD so the caller can cite what it made
	// without a second round trip.
	if !res.IsError {
		if head, headErr := runGit(ctx, r.deps.Root, "log", "-1", "--format=%H%x00%s"); headErr == nil {
			if sha, subject, ok := strings.Cut(strings.TrimRight(head, "\n"), "\x00"); ok {
				if payload := decodePayload(res.Text); payload != nil {
					payload["head"] = map[string]string{"sha": sha, "subject": subject}
					if data, merr := json.Marshal(payload); merr == nil {
						res.Text = string(data)
					}
				}
			}
		}
	}
	return res
}

// gatedGit is the write path every mutating git tool takes: resolve the lease,
// ask the command gate about the exact command line this work represents, run
// the escalation seam on a block, execute git directly with argv (never a
// shell), and annotate the outcome with the decision that authorised it.
//
// The executed argv and the evaluated command line are built from the same
// slice, so the policy judged what actually ran.
func (r *Registry) gatedGit(ctx context.Context, verb, commandLine string, argv []string, extra map[string]any) tools.Result {
	task, refusal := r.authorisingTask(ctx, verb)
	if refusal != nil {
		return *refusal
	}

	decision, err := r.deps.Gate.EvaluateCommand(commandLine, task)
	if err != nil {
		return unavailable("command policy decision", err)
	}
	if decision.Blocked() {
		escalated, ok := r.escalate(ctx, decision, commandLine)
		if !ok {
			return r.refusal(decision)
		}
		decision = escalated
	}

	cmdCtx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "git", argv...)
	cmd.Dir = r.deps.Root
	cmd.WaitDelay = 5 * time.Second

	var stdout, stderr bytes.Buffer
	outCap := &limitWriter{w: &stdout, limit: maxGitOutputBytes}
	errCap := &limitWriter{w: &stderr, limit: 64 * 1024}
	cmd.Stdout = outCap
	cmd.Stderr = errCap

	runErr, timedOut := proc.RunBounded(cmdCtx, cmd.Run)
	if timedOut {
		return unavailable("git execution", fmt.Errorf("timed out after %s", gitTimeout))
	}

	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return unavailable("git execution", runErr)
		}
		exitCode = exitErr.ExitCode()
	}

	payload := map[string]any{}
	for k, v := range extra {
		payload[k] = v
	}
	payload["exit_code"] = exitCode
	if out := strings.TrimRight(stdout.String(), "\n"); out != "" {
		payload["output"] = out
	}
	if errMsg := strings.TrimSpace(stderr.String()); errMsg != "" {
		payload["git_stderr"] = errMsg
	}
	// A capped capture says it was capped. Reading a trimmed diff as the whole
	// diff is how "nothing else changed" gets asserted from a partial sample.
	if note := outCap.truncationNote(); note != "" {
		payload["output_truncated"] = note
	}
	if note := errCap.truncationNote(); note != "" {
		payload["git_stderr_truncated"] = note
	}

	if exitCode != 0 {
		return annotate(failure(payload, fmt.Sprintf(
			"git %s failed with exit code %d", argv[0], exitCode)), decision)
	}
	return annotate(ok(payload), decision)
}

// secretPaths reports which of paths match the harness's secret-path
// patterns. One owner of the list: policy.SecretPathPatterns is what the
// write gate blocks with, so the git tools refuse on exactly the same set.
//
// Folded, like the write ladder's own secret rung: the two consult one list
// and must read it the same way. They did not — this side matched
// case-sensitively — so ".ENV", "server.KEY" and "ID_RSA" reported clean here
// while the ladder refused them, and on a case-insensitive filesystem those
// are the same files. git's pathspec matching turned out to block the
// staging of a case variant on APFS, so the divergence never became a leak
// through this door; a divergence between two readings of one list is a defect
// on its own, and the door it does not open today is an implementation detail
// of git.
func secretPaths(paths []string) []string {
	var leaked []string
	for _, p := range paths {
		if fnmatch.MatchAnyFold(policy.SecretPathPatterns, p) {
			leaked = append(leaked, p)
		}
	}
	return leaked
}

// aliasedPaths reports which of paths carry more than one name in the
// filesystem.
//
// secretPaths is a statement about names, and a hard link gives one inode two
// of them: `ln .env notes.txt` produces a path matching no secret pattern
// whose contents are the credential, and `git add notes.txt && git commit`
// puts them in history past this check and past .gitignore, which is the one
// git act that cannot be undone locally. The link cannot be created through
// any tool here — git does not store hard links either — but `ln`, `cp -al`
// and the package managers that hardlink into a store are ordinary allowed
// commands.
//
// Which other names an inode has cannot be read back from the inode, so this
// refuses on the count rather than on what the other names are. That is the
// honest rule for the same reason the pinned write uses it: a path whose
// contents are reachable under a name the secret check did not examine is a
// path this check cannot vouch for. Lstat, not Stat — for a symbolic link git
// commits the link, not the file at the far end, so the far end's link count
// is not this operation's business.
func aliasedPaths(root string, paths []string) []string {
	var aliased []string
	for _, p := range paths {
		fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if links, ok := linksOf(fi); ok && links > 1 {
			aliased = append(aliased, p)
		}
	}
	return aliased
}

// aliasedRefusal refuses a stage or commit of a file that has more than one
// name. It carries the secret rung's rule id because that is the rung a hard
// link defeats, and because the payload contract callers match on is the one
// secretRefusal established; the reason says what actually happened.
func aliasedRefusal(op string, aliased []string) tools.Result {
	payload := map[string]any{
		"operation": op,
		"paths":     aliased,
		"rule":      string(policy.RuleSecretPath),
		"severity":  string(policy.Hard),
		"reason": "these paths are hard links: their contents are reachable under at least one " +
			"other name, which the secret-path check never examined, so committing them can put " +
			"a credential into history under an innocent name. Break the link — copy the file and " +
			"replace the original with the copy — and retry, or stage the other name instead so " +
			"the check can see it.",
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

// secretRefusal refuses a stage or commit that would put a secret into
// history. This is checked before and independently of the command gate, and
// it is hard under every posture: committing credentials is the one git act
// that cannot be undone locally, and demoting the check under yolo would make
// the posture a way to leak rather than a way to proceed.
func secretRefusal(op string, leaked []string) tools.Result {
	payload := map[string]any{
		"operation": op,
		"paths":     leaked,
		"rule":      string(policy.RuleSecretPath),
		"severity":  string(policy.Hard),
		"reason": "these paths match the harness's secret-path patterns; committing them " +
			"would put them in history. Move the values into configuration the repository " +
			"does not track, add the paths to .gitignore, and retry without them.",
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

// renderCommandLine renders argv the way the shell would have written it, so
// the command gate evaluates and the audit trail records the command this
// work represents. Quoting is display-only — execution never passes through a
// shell — but the rendered form is what allowlists match against, and it must
// therefore be faithful: a path containing a space is quoted exactly as the
// agent would have had to quote it in devcouncil_exec_command.
func renderCommandLine(argv []string) string {
	parts := make([]string, len(argv))
	for i, arg := range argv {
		if arg != "" && !strings.ContainsAny(arg, " \t\n\"'\\") {
			parts[i] = arg
			continue
		}
		parts[i] = strconv.Quote(arg)
	}
	return strings.Join(parts, " ")
}

// decodePayload re-reads a result this package produced, for the one handler
// that enriches its payload after executing. Nil when the text is not a JSON
// object, which for results built by ok() cannot happen — and if it somehow
// did, the enrichment is skipped rather than the original answer replaced.
func decodePayload(text string) map[string]any {
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return nil
	}
	return payload
}
