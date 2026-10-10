package devcouncil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/dcgrep"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/fnmatch"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// resolvePath turns a tool argument into a filesystem path, and decides whether
// an escaping one may be used.
//
// It owns that decision rather than reading the gate's verdict: a tool that
// touches the filesystem must not depend on another component having run. What
// it does share with the gate is the setting, so an operator has one switch to
// reason about instead of two that can disagree — and the containment it used
// to enforce unconditionally is now exactly as strong as the hard rules are.
//
// With the hard rules on, which is every posture but yolo, nothing resolves
// outside the repository root. With them off, an escaping path resolves to what
// it names and the write lands there. That is a deliberate retirement of an
// invariant, requested explicitly: yolo means the harness is not containing the
// agent, and a containment that survived it would have made the posture a
// half-truth. Two refusals survive regardless, because neither is containment:
// a path whose text and whose meaning to the kernel differ (NUL, control
// characters) and the repository root itself, both of which normalize to no
// usable target at all.
func (r *Registry) resolvePath(path string) (string, error) {
	if rel, ok := r.containedRel(path); ok {
		return filepath.Join(r.deps.Root, filepath.FromSlash(rel)), nil
	}

	normalized, _ := policy.NormalizeRepoPath(r.deps.Root, path)
	hardRules, _, err := flags.EffectiveHardRules(r.deps.Gate.Flags)
	if err != nil {
		// An unreadable setting is not permission. The containment stands.
		return "", fmt.Errorf("path %q is outside the repository, and the hard-rules setting could not be read: %w", path, err)
	}
	if hardRules {
		return "", fmt.Errorf("path %q is outside the repository", path)
	}
	// NormalizeRepoPath reports an escaping path as its resolved absolute form.
	// Anything else it calls uncontained — a malformed path, or the root itself
	// — carries no target to write to, and is refused whatever the setting says.
	target := filepath.FromSlash(normalized)
	if !filepath.IsAbs(target) {
		return "", fmt.Errorf("path %q does not name a usable location", path)
	}
	return target, nil
}

// containedRel answers the one question every path-taking tool has to ask
// before it touches the filesystem: which repository-relative path does this
// string name, and does it name one at all?
//
// It is the single owner of that answer. resolvePath is built on it, and so is
// readContained — which is the point: read_file refused a symlink pointing out
// of the repository while grep read the very same link through os.ReadFile and
// reported the contents under the in-repo name, because the two had separate
// opinions about containment and only one of them was enforcing.
//
// NormalizeRepoPath resolves symlinks before judging, so a link whose target
// escapes is reported uncontained here rather than followed later.
func (r *Registry) containedRel(p string) (string, bool) {
	normalized, outside := policy.NormalizeRepoPath(r.deps.Root, p)
	if outside {
		return "", false
	}
	// If the file exists directly under root, use it. If it does not exist
	// under root, but cwd is inside root and the file exists at cwd, prefer cwd.
	if !filepath.IsAbs(p) {
		if _, err := os.Stat(filepath.Join(r.deps.Root, normalized)); os.IsNotExist(err) {
			if cwd, err := os.Getwd(); err == nil {
				candidate := filepath.Join(cwd, p)
				if normCwd, outCwd := policy.NormalizeRepoPath(r.deps.Root, candidate); !outCwd {
					if _, statErr := os.Stat(candidate); statErr == nil {
						return filepath.ToSlash(normCwd), true
					}
				}
			}
		}
	}
	return filepath.ToSlash(normalized), true
}

// containedRelOf is containedRel's form for a path the tool already holds as an
// absolute location — a walk hit, say — where no normalization is wanted, only
// the containment verdict and the relative name.
func containedRelOf(root, abs string) (string, bool) {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	// "." is the root itself: a legitimate search root, and never a file the
	// walk can hand to the reader — pinWriteTarget refuses it, so the read side
	// fails closed even if one ever arrived.
	return rel, true
}

// maxToolReadBytes bounds every file the ungated read tools will pull into
// memory. One number, because read_file and grep face the same repository and
// two different ceilings would only mean one of them was wrong.
const maxToolReadBytes = 2 * 1024 * 1024

// readContained is the canonical reader for the ungated read tools.
//
// It delegates to the pinned reader in safefs.go, which is the only code here
// that gets containment right: it opens with O_NOFOLLOW so a symlink cannot
// redirect the read, checks the opened descriptor is a regular file so a FIFO
// or a device node cannot block forever or stream without end, re-verifies
// every directory identity captured at pin time, and measures the size with
// fstat on the descriptor rather than an lstat on the name — an lstat measures
// the link, so a 182-byte symlink to a 5 MiB file passed a 2 MiB guard.
//
// The read runs on its own goroutine so ctx actually bounds it. It used to be
// accepted and ignored; a FIFO planted in the repository then held the tool
// open seconds past its own deadline, wedging the turn. The channel is
// buffered, so the goroutine finishes and exits even when nobody is left to
// receive.
func readContained(ctx context.Context, root, rel string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", rel, err)
	}
	type outcome struct {
		data []byte
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		data, err := ReadPinned(root, rel, limit)
		done <- outcome{data: data, err: err}
	}()
	select {
	case res := <-done:
		return res.data, res.err
	case <-ctx.Done():
		return nil, fmt.Errorf("reading %s: %w", rel, ctx.Err())
	}
}

func (r *Registry) resolveDirPath(path string) (string, error) {
	if path == "" || path == "." || path == "./" {
		return r.deps.Root, nil
	}
	return r.resolvePath(path)
}

// ungatedReadTools are the tools that reach the repository without consulting
// task scope at all. They are listed on a command refusal because a blocked
// command is usually an attempt to look at something, and looking has never
// needed permission here — only writing and executing do.
var ungatedReadTools = []string{
	"devcouncil_read_file", "devcouncil_grep",
	"devcouncil_list_dir", "devcouncil_find_files",
}

const ungatedToolSentence = "devcouncil_read_file, devcouncil_grep, " +
	"devcouncil_list_dir and devcouncil_find_files are not gated by task scope."

func (r *Registry) readFile(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Path string `json:"path"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	rel, contained := r.containedRel(args.Path)
	if !contained {
		return tools.Errorf("path %q is outside the repository", args.Path)
	}
	// Reads go through the gate, exactly as writes do. Containment answers
	// "is this file in the repository", which is a different question from
	// "may this agent see it": .env is inside the repository. Without this the
	// write gate refused .env as path.secret while this handler returned its
	// contents, and the credential scrubber downstream only replaces values
	// this process was handed — it has never seen the repository's own.
	// Deliberately not authorisingTask: a read does not require a lease, and
	// the rung does not consult the task for anything but the ID it records.
	// A session holding none still has its reads judged.
	task, _ := r.currentTask(ctx)
	decision, err := r.deps.Gate.EvaluateRead(args.Path, task)
	if err != nil {
		return unavailable("read policy decision", err)
	}
	if decision.Blocked() {
		return r.refusal(decision)
	}
	data, err := readContained(ctx, r.deps.Root, rel, maxToolReadBytes)
	if err != nil {
		return tools.Errorf("reading %s: %v", args.Path, err)
	}
	return annotate(tools.Result{Text: string(data)}, decision)
}

func (r *Registry) writeFile(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}

	// The lease is checked against the store on every write, not once at
	// checkout. A lease can expire mid-turn, and a write authorised by a lease
	// that has since lapsed is a write two agents could both believe they own.
	task, refusal := r.authorisingTask(ctx, "writing")
	if refusal != nil {
		return *refusal
	}
	// Asked before the write, because afterwards every path exists. See
	// reuse.go: this is what turns "extend existing seams rather than
	// duplicating" from a line in the system prompt into something that
	// actually looks.
	reusePath, creating := r.createdPath(args.Path)
	// The target is pinned BEFORE the ladder runs and before any approval
	// prompt: the identity snapshot has to predate human-scale delays, or a
	// concurrent actor could swap a directory for a symlink while the prompt
	// sat open and the kernel would follow it at open time. See safefs.go.
	normalized, outside := policy.NormalizeRepoPath(r.deps.Root, args.Path)
	var pinned *pinnedTarget
	if !outside {
		var err error
		pinned, err = pinWriteTarget(r.deps.Root, normalized)
		if err != nil {
			return tools.Errorf("%v", err)
		}
	}
	decision, err := r.deps.Gate.EvaluateWrite(args.Path, task, dc.OpWrite)
	if err != nil {
		return unavailable("policy decision", err)
	}
	if decision.Blocked() {
		escalated, ok := r.escalate(ctx, decision, decision.Target)
		if !ok {
			return r.refusal(decision)
		}
		decision = escalated
	}

	full, err := r.resolvePath(args.Path)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	if pinned != nil {
		if err := pinned.Write([]byte(args.Content), 0o644); err != nil {
			return tools.Errorf("writing %s: %v", args.Path, err)
		}
	} else {
		// Outside-root targets are reachable only with hard rules off — an
		// explicit operator decision. The legacy path stands there.
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return tools.Errorf("creating directory for %s: %v", args.Path, err)
		}
		if err := os.WriteFile(full, []byte(args.Content), 0o644); err != nil {
			return tools.Errorf("writing %s: %v", args.Path, err)
		}
	}

	// The path is claimed only here, after the bytes have landed. A refusal, a
	// failed open or a pin that could not be taken all return above without it,
	// because none of them changed a file and a checker handed a path that was
	// never written would report on the wrong thing — or, worse, report a pass
	// for a write the gate refused.
	result := annotate(
		tools.Result{
			Text:  fmt.Sprintf("wrote %s (%d bytes)", decision.Target, len(args.Content)),
			Wrote: []string{decision.Target},
		},
		decision)
	if creating {
		result = annotateReuse(result, r.checkReuse(ctx, reusePath))
	}
	return result
}

func (r *Registry) deleteFile(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Path string `json:"path"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if strings.TrimSpace(args.Path) == "" {
		return tools.Errorf("path is required")
	}

	task, refusal := r.authorisingTask(ctx, "deleting")
	if refusal != nil {
		return *refusal
	}
	decision, err := r.deps.Gate.EvaluateWrite(args.Path, task, dc.OpDelete)
	if err != nil {
		return unavailable("policy decision", err)
	}
	if decision.Blocked() {
		escalated, ok := r.escalate(ctx, decision, decision.Target)
		if !ok {
			return r.refusal(decision)
		}
		decision = escalated
	}

	// Delete is pinned like write: a directory swapped to an escaping symlink
	// between evaluation and unlink would otherwise delete wherever the link
	// points. The pin happens after escalation here — unlink has no
	// deliberation payload — so the residual race is only the microsecond
	// between pin and syscall, not the whole approval window.
	normalizedDelete, outside := policy.NormalizeRepoPath(r.deps.Root, args.Path)
	if !outside {
		if err := RemovePinned(r.deps.Root, normalizedDelete); err != nil {
			return tools.Errorf("%v", err)
		}
		return annotate(
			tools.Result{
				Text:  fmt.Sprintf("deleted %s", decision.Target),
				Wrote: []string{decision.Target},
			},
			decision)
	}

	full, err := r.resolvePath(args.Path)
	if err != nil {
		return tools.Errorf("%v", err)
	}

	if _, statErr := os.Stat(full); os.IsNotExist(statErr) {
		return tools.Errorf("file %s does not exist", args.Path)
	}

	if err := os.Remove(full); err != nil {
		return tools.Errorf("deleting %s: %v", args.Path, err)
	}

	return annotate(
		tools.Result{
			Text:  fmt.Sprintf("deleted %s", decision.Target),
			Wrote: []string{decision.Target},
		},
		decision)
}

func (r *Registry) listDir(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
		// IncludeIgnored applies to the recursive branch, which enumerates the
		// repository. The flat branch always reports the literal directory.
		IncludeIgnored bool `json:"include_ignored"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	targetPath := args.Path
	if targetPath == "" {
		targetPath = "."
	}
	full, err := r.resolveDirPath(targetPath)
	if err != nil {
		return tools.Errorf("%v", err)
	}

	type entry struct {
		Name  string `json:"name"`
		IsDir bool   `json:"is_dir"`
		Size  int64  `json:"size_bytes,omitempty"`
	}
	var entries []entry

	// One ceiling for both branches. The recursive walk capped at 500 and said
	// so; the flat listing had no cap at all, so a directory with a hundred
	// thousand entries was rendered whole into one tool result.
	const maxEntries = 500

	if !args.Recursive {
		dirEntries, err := os.ReadDir(full)
		if err != nil {
			return tools.Errorf("reading directory %s: %v", targetPath, err)
		}
		truncated := len(dirEntries) > maxEntries
		if truncated {
			dirEntries = dirEntries[:maxEntries]
		}
		for _, de := range dirEntries {
			var size int64
			if info, err := de.Info(); err == nil && !de.IsDir() {
				size = info.Size()
			}
			entries = append(entries, entry{
				Name:  de.Name(),
				IsDir: de.IsDir(),
				Size:  size,
			})
		}
		payload := map[string]any{
			"path":    targetPath,
			"count":   len(entries),
			"entries": entries,
		}
		// A capped sample is never handed back looking like the whole
		// directory: a caller that concluded "the file is not here" from a
		// silently trimmed listing would be concluding it from nothing.
		if truncated {
			payload["truncated"] = true
			payload["limit"] = maxEntries
		}
		return ok(payload)
	}

	// The recursive branch enumerates the repository under a path, which is the
	// same question devcouncil_find_files and devcouncil_grep answer, so it
	// goes through the same walk. It used to be a third filepath.WalkDir with a
	// four-name skip list of its own, and once grep moved onto ripgrep's ignore
	// rules that walk described a different repository than either of them.
	//
	// The flat branch above is deliberately not routed here. "What is literally
	// in this directory" is a different question from "what files does this
	// repository have", and an operator checking whether a build output
	// directory exists is asking the first one.
	if r.deps.Grep == nil {
		return tools.Errorf("no searcher is configured, so no listing ran — this is not an " +
			"empty directory. Build the analysis plane with `cargo build --release " +
			"--manifest-path crates/Cargo.toml`, or set " + dcgrep.BinaryEnv + " to a dcgrep binary")
	}
	rel, contained := containedRelOf(r.deps.Root, full)
	if !contained {
		return tools.Errorf("path %q is outside the repository and this tool reads only inside it — "+
			"this is not an empty result, no listing ran", targetPath)
	}
	listing, listErr := r.deps.Grep.List(ctx, dcgrep.ListRequest{
		Path:           rel,
		MaxResults:     maxEntries,
		IncludeIgnored: args.IncludeIgnored,
	})
	if listErr != nil {
		return tools.Errorf("%v — this is not an empty result, no listing ran", listErr)
	}

	// Directories are reconstructed from the paths rather than reported by the
	// walk. The set that results is every directory holding at least one
	// visible file, which is the set a caller can act on: a directory whose
	// entire contents are excluded is not somewhere a search will look, and
	// listing it as present invites exactly the "but I can see it there"
	// confusion this change exists to remove.
	seenDir := map[string]bool{}
	for _, path := range listing.Paths {
		var size int64
		if info, statErr := os.Lstat(filepath.Join(r.deps.Root, filepath.FromSlash(path))); statErr == nil {
			size = info.Size()
		}
		entries = append(entries, entry{Name: path, IsDir: false, Size: size})
		// Every prefix ending at a separator is an ancestor directory.
		//
		// Written as a forward scan rather than as a loop that advances an
		// index by a search result. The first attempt did the latter — and
		// `strings.Index` returns -1 when there is no further separator, so
		// `idx = -1 + idx + 1` left the index exactly where it was and the loop
		// never terminated. `src/a.go` was enough to hang it. A tool handler is
		// pure computation between the gate and the reply, so nothing in the
		// harness bounds it: the turn simply stops.
		for idx := range len(path) {
			if path[idx] != '/' {
				continue
			}
			parent := path[:idx]
			if parent != "" && !seenDir[parent] {
				seenDir[parent] = true
				entries = append(entries, entry{Name: parent, IsDir: true})
			}
		}
	}
	payload := map[string]any{
		"path":                 targetPath,
		"count":                len(entries),
		"entries":              entries,
		"ignore_rules_applied": listing.IgnoreRulesApplied,
	}
	// A capped sample is never handed back looking like the whole tree: a
	// caller that concluded "the file is not here" from a silently trimmed
	// listing would be concluding it from nothing. The cap is on files, so the
	// reconstructed directories can push the entry count past it — the limit
	// reported is the one that actually applied.
	if listing.Truncated {
		payload["truncated"] = true
		payload["limit"] = listing.Limit
		payload["files_listed"] = len(listing.Paths)
	}
	if len(listing.Paths) == 0 {
		payload["searched_nothing"] = true
		payload["note"] = "no file was listed, so this result is not evidence about the " +
			"repository's contents — the path may be empty, or every file under it may be " +
			"excluded by ignore rules. Re-run with include_ignored to find out which."
	}
	return ok(payload)
}

// grepSearch answers "where in this repository does this appear".
//
// The search itself runs in the analysis plane, on ripgrep's engine — see
// manvi/dc/dcgrep and crates/dc-grep. What stays here is the contract this
// tool has always had with the model, and every clause of it is a lesson:
//
//   - An unparseable pattern is an error naming the fault, never an empty match
//     set. It used to be a substring search, and that is not a smaller feature,
//     it is a wrong answer: the alternation `sys|time` matched nothing and came
//     back as {"count":0}, which is exactly the shape of "this file does not use
//     sys or time". A model asked to remove unused imports read that as proof,
//     then read `subprocess|re|os` as proof of the same thing about imports the
//     file plainly used, and spent its entire step budget trying to reconcile
//     two contradictory facts it had been handed. Nothing in the result said
//     the pattern had not been understood.
//
//   - An uncontained search root is refused outright rather than walked to an
//     empty result, for the same reason.
//
//   - A searcher that could not be reached is an error naming what to build.
//     This is the cost of putting the engine behind the process boundary, and
//     paying it in an error beats paying it in a zero.
//
// What is new is that ignore rules now apply. `.gitignore`, `.ignore`,
// `.git/info/exclude` and hidden files are honoured, so a search of a
// repository with a build tree in it no longer spends its match budget inside
// target/ before reaching the source. That is a real change in what comes back,
// so the tool says which mode it ran in on every reply, and include_ignored
// turns the rules off for the case where the build output is the question.
func (r *Registry) grepSearch(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Pattern         string `json:"pattern"`
		Path            string `json:"path"`
		MaxResults      int    `json:"max_results"`
		IncludeIgnored  bool   `json:"include_ignored"`
		CaseInsensitive bool   `json:"case_insensitive"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if strings.TrimSpace(args.Pattern) == "" {
		return tools.Errorf("pattern is required")
	}
	if r.deps.Grep == nil {
		return tools.Errorf("no searcher is configured, so no search ran — this is not a " +
			"negative result. Build the analysis plane with `cargo build --release " +
			"--manifest-path crates/Cargo.toml`, or set " + dcgrep.BinaryEnv + " to a dcgrep binary")
	}

	// Containment is checked here as well as in the searcher. The searcher
	// enforces it because it is the process doing the reading; this check
	// exists so the refusal names the path the model actually typed, before a
	// child is spawned to reject it.
	targetPath := args.Path
	if targetPath == "" {
		targetPath = "."
	}
	full, resolveErr := r.resolveDirPath(targetPath)
	if resolveErr != nil {
		return tools.Errorf("%v", resolveErr)
	}
	rel, contained := containedRelOf(r.deps.Root, full)
	if !contained {
		return tools.Errorf("path %q is outside the repository and grep reads only inside it — "+
			"this is not a negative result, no search ran", targetPath)
	}

	result, err := r.deps.Grep.Search(ctx, dcgrep.Request{
		Pattern:         args.Pattern,
		Path:            rel,
		MaxResults:      args.MaxResults,
		IncludeIgnored:  args.IncludeIgnored,
		CaseInsensitive: args.CaseInsensitive,
	})
	if err != nil {
		// Every failure the searcher reports is a fault it named — a bad
		// pattern, an unreadable root, a child that died. None of them may
		// reach the model shaped like a result.
		return tools.Errorf("%v — this is not a negative result, no search ran", err)
	}

	matches := result.Matches
	if matches == nil {
		// A nil slice renders as JSON null, and null is not the same statement
		// as "no matches" to anything reading this.
		matches = []dcgrep.Match{}
	}
	// A search is a read, and it reads every file under its path. Refusing the
	// whole search because the tree contains a .env would make the tool useless;
	// returning the lines out of one would make the read gate on read_file
	// decorative, since `grep -r . ` reaches the same bytes. So the credential
	// files are dropped from the result — and counted, because a filtered
	// result presented as a complete one is the failure this file's other
	// notes exist to prevent.
	withheld := 0
	withheldFiles := map[string]struct{}{}
	kept := matches[:0]
	// A file's matches arrive together, and the verdict is per file: it
	// resolves the path through every symlink on the way, so it is asked once
	// per file rather than once per matching line.
	refused := map[string]bool{}
	for _, m := range matches {
		verdict, seen := refused[m.Path]
		if !seen {
			verdict = policy.ReadRefused(r.deps.Root, m.Path)
			refused[m.Path] = verdict
		}
		if verdict {
			withheld++
			withheldFiles[m.Path] = struct{}{}
			continue
		}
		kept = append(kept, m)
	}
	matches = kept
	payload := map[string]any{
		"pattern":              args.Pattern,
		"count":                result.Count,
		"matches":              matches,
		"files_searched":       result.FilesSearched,
		"ignore_rules_applied": result.IgnoreRulesApplied,
	}
	if result.Truncated {
		payload["truncated"] = true
		payload["limit"] = result.Limit
	}
	if withheld > 0 {
		payload["count"] = result.Count - withheld
		names := make([]string, 0, len(withheldFiles))
		for name := range withheldFiles {
			names = append(names, name)
		}
		sort.Strings(names)
		payload["withheld"] = map[string]any{
			"matches": withheld,
			"files":   names,
			"note": fmt.Sprintf("%d match(es) in %d credential file(s) were withheld by the read "+
				"gate, so this result does not cover them", withheld, len(names)),
		}
	}
	// Reported only when there is something to report, but never inferred from
	// silence: a search that skipped nothing carries no note, and one that
	// skipped something says exactly what and why. A capped sample must not be
	// presented as complete coverage.
	if skipped := result.Skipped; skipped.Total() > 0 {
		payload["skipped"] = map[string]any{
			"too_large":            skipped.TooLarge,
			"binary":               skipped.Binary,
			"unreadable":           skipped.Unreadable,
			"unrepresentable_name": skipped.UnrepresentableName,
			"note": fmt.Sprintf("%d file(s) were not searched, so this result does not cover them",
				skipped.Total()),
		}
	}
	// A search that opened no files is the one zero that says nothing about the
	// repository, and it is otherwise byte-identical to the zero that says a
	// great deal. It is reachable without anything being broken: a `.gitignore`
	// containing `*`, a path naming an empty directory, a tree whose every file
	// is hidden. Left unmarked, a model reads "not present here" from a search
	// that never looked at a single line.
	if result.FilesSearched == 0 {
		payload["searched_nothing"] = true
		payload["note"] = "no file was opened, so this result is not evidence about the " +
			"repository's contents — the path may be empty, or every file under it may be " +
			"excluded by ignore rules. Re-run with include_ignored to find out which."
	}
	return ok(payload)
}

func (r *Registry) patchFile(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Path               string `json:"path"`
		TargetContent      string `json:"target_content"`
		ReplacementContent string `json:"replacement_content"`
		StartLine          int    `json:"start_line,omitempty"`
		EndLine            int    `json:"end_line,omitempty"`
		AllowMultiple      bool   `json:"allow_multiple,omitempty"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if strings.TrimSpace(args.Path) == "" {
		return tools.Errorf("path is required")
	}
	if args.TargetContent == "" {
		return tools.Errorf("target_content is required")
	}

	task, refusal := r.authorisingTask(ctx, "patching")
	if refusal != nil {
		return *refusal
	}
	// Pinned before evaluation and before any approval prompt: patch is
	// read-modify-write, so both halves must address the same verified file,
	// and a directory swapped mid-deliberation must void the whole operation.
	normalizedPatch, outside := policy.NormalizeRepoPath(r.deps.Root, args.Path)
	var pinned *pinnedTarget
	if !outside {
		var err error
		pinned, err = pinWriteTarget(r.deps.Root, normalizedPatch)
		if err != nil {
			return tools.Errorf("%v", err)
		}
	}
	decision, err := r.deps.Gate.EvaluateWrite(args.Path, task, dc.OpModify)
	if err != nil {
		return unavailable("policy decision", err)
	}
	if decision.Blocked() {
		escalated, ok := r.escalate(ctx, decision, decision.Target)
		if !ok {
			return r.refusal(decision)
		}
		decision = escalated
	}

	full, err := r.resolvePath(args.Path)
	if err != nil {
		return tools.Errorf("%v", err)
	}

	var content string
	if pinned != nil {
		data, err := pinned.Read(maxPatchReadBytes)
		if err != nil {
			return tools.Errorf("%v", err)
		}
		content = string(data)
	} else {
		data, err := os.ReadFile(full)
		if err != nil {
			if os.IsNotExist(err) {
				return tools.Errorf("file %s does not exist; use devcouncil_write_file to create new files", args.Path)
			}
			return tools.Errorf("reading %s: %v", args.Path, err)
		}
		content = string(data)
	}

	if args.StartLine > 0 || args.EndLine > 0 {
		lines := strings.Split(content, "\n")
		totalLines := len(lines)

		start := args.StartLine
		if start < 1 {
			start = 1
		}
		end := args.EndLine
		if end < 1 || end > totalLines {
			end = totalLines
		}
		if start > end || start > totalLines {
			return tools.Errorf("invalid line range [%d, %d] for file %s with %d lines",
				args.StartLine, args.EndLine, args.Path, totalLines)
		}

		chunkLines := lines[start-1 : end]
		chunk := strings.Join(chunkLines, "\n")

		occurrences := strings.Count(chunk, args.TargetContent)
		if occurrences == 0 {
			return tools.Errorf("target_content not found in lines %d-%d of %s", start, end, args.Path)
		}
		if occurrences > 1 && !args.AllowMultiple {
			return tools.Errorf("target_content found %d times in lines %d-%d of %s; narrow the line range or set allow_multiple: true",
				occurrences, start, end, args.Path)
		}

		var replacedChunk string
		if args.AllowMultiple {
			replacedChunk = strings.ReplaceAll(chunk, args.TargetContent, args.ReplacementContent)
		} else {
			replacedChunk = strings.Replace(chunk, args.TargetContent, args.ReplacementContent, 1)
		}

		var newLines []string
		if start > 1 {
			newLines = append(newLines, lines[:start-1]...)
		}
		newLines = append(newLines, replacedChunk)
		if end < totalLines {
			newLines = append(newLines, lines[end:]...)
		}
		content = strings.Join(newLines, "\n")
	} else {
		occurrences := strings.Count(content, args.TargetContent)
		if occurrences == 0 {
			return tools.Errorf("target_content not found in %s", args.Path)
		}
		if occurrences > 1 && !args.AllowMultiple {
			return tools.Errorf("target_content found %d times in %s; specify start_line and end_line or set allow_multiple: true",
				occurrences, args.Path)
		}

		if args.AllowMultiple {
			content = strings.ReplaceAll(content, args.TargetContent, args.ReplacementContent)
		} else {
			content = strings.Replace(content, args.TargetContent, args.ReplacementContent, 1)
		}
	}

	if pinned != nil {
		if err := pinned.Write([]byte(content), 0o644); err != nil {
			return tools.Errorf("writing patched %s: %v", args.Path, err)
		}
	} else {
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return tools.Errorf("writing patched %s: %v", args.Path, err)
		}
	}

	return annotate(
		tools.Result{
			Text:  fmt.Sprintf("patched %s (%d bytes)", decision.Target, len(content)),
			Wrote: []string{decision.Target},
		},
		decision)
}

// matchCandidates keeps the paths that match pattern, either whole or by their
// last component, stopping once limit have matched.
//
// A path matches when fnmatch.Match accepts the path or its base name. That
// rule is unchanged; what this skips is a second call whose answer is already
// known. For a path pattern that is every base-name call, about a third of the
// loop over a full 200,000-path listing; for a base-name pattern such as
// "*.go" it is only the files at the top of the listed directory:
//
//   - A path with no '/' is its own base name, so the second call would repeat
//     the first with the same arguments.
//   - A pattern with a '/' before its first '[' can never match a base name.
//     Every unit before the first '[' is a '*', a '?' or a literal — no
//     bracket expression can have begun yet to swallow the '/' — so that '/'
//     is a literal the matcher must consume against a '/' in the name, and a
//     base name has none. A '/' after a '[' may sit inside a class, where a
//     negated set matches non-'/' characters too, so that case is left to the
//     matcher rather than reasoned about here. Note that "no '/' in the
//     pattern" proves nothing the other way: in Python's fnmatch '*' crosses
//     a separator.
//
// TestMatchCandidatesSkipsOnlyWhatCannotMatch pins this against the unskipped
// rule over generated patterns, brackets and all.
func matchCandidates(pattern string, paths []string, limit int) ([]string, bool) {
	baseCanMatch := !slashBeforeFirstBracket(pattern)
	matches := []string{}
	for _, path := range paths {
		if len(matches) >= limit {
			return matches, true
		}
		if fnmatch.Match(pattern, path) {
			matches = append(matches, path)
			continue
		}
		if !baseCanMatch {
			continue
		}
		if idx := strings.LastIndex(path, "/"); idx >= 0 && fnmatch.Match(pattern, path[idx+1:]) {
			matches = append(matches, path)
		}
	}
	return matches, false
}

// slashBeforeFirstBracket reports a literal '/' that no bracket expression can
// contain: one that comes before any '[' at all. Bytes suffice — neither '/'
// nor '[' can occur inside a multi-byte UTF-8 sequence.
func slashBeforeFirstBracket(pattern string) bool {
	head := pattern
	if i := strings.IndexByte(pattern, '['); i >= 0 {
		head = pattern[:i]
	}
	return strings.IndexByte(head, '/') >= 0
}

func (r *Registry) findFiles(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Pattern        string `json:"pattern"`
		Path           string `json:"path,omitempty"`
		MaxResults     int    `json:"max_results,omitempty"`
		IncludeIgnored bool   `json:"include_ignored,omitempty"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if strings.TrimSpace(args.Pattern) == "" {
		return tools.Errorf("pattern is required")
	}
	if args.MaxResults <= 0 {
		args.MaxResults = 100
	}
	if r.deps.Grep == nil {
		return tools.Errorf("no searcher is configured, so no listing ran — this is not an " +
			"empty repository. Build the analysis plane with `cargo build --release " +
			"--manifest-path crates/Cargo.toml`, or set " + dcgrep.BinaryEnv + " to a dcgrep binary")
	}

	targetPath := args.Path
	if targetPath == "" {
		targetPath = "."
	}
	full, err := r.resolveDirPath(targetPath)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	rel, contained := containedRelOf(r.deps.Root, full)
	if !contained {
		return tools.Errorf("path %q is outside the repository and this tool reads only inside it — "+
			"this is not an empty result, no listing ran", targetPath)
	}

	// The file list comes from the same walk devcouncil_grep searches, which is
	// the whole point of routing this through the analysis plane.
	//
	// It used to be a second filepath.WalkDir with its own five-name skip list,
	// and once grep moved onto ripgrep's ignore rules the two disagreed: this
	// tool reported `dist/generated.go` in a repository whose .gitignore
	// excluded `dist/`, and grep would never open it. An agent handed both
	// answers had no way to tell which described the tree it was editing.
	//
	// The listing is over-fetched deliberately. Matching happens below, so the
	// searcher's own limit would otherwise truncate the *candidates* rather
	// than the results, and a glob matching one file deep in a large tree would
	// come back empty because the first hundred paths did not match it.
	listing, err := r.deps.Grep.List(ctx, dcgrep.ListRequest{
		Path:           rel,
		MaxResults:     dcgrep.MaxListResults,
		IncludeIgnored: args.IncludeIgnored,
	})
	if err != nil {
		return tools.Errorf("%v — this is not an empty result, no listing ran", err)
	}

	// Matched here rather than in the analysis plane, and with the harness's
	// own fnmatch. That engine is pinned to a 775-case CPython parity fixture
	// shared with crates/dc-glob; moving the matching across the boundary would
	// fork the glob semantics to gain nothing. What was wrong was never the
	// matching — it was that there were two walks.
	matches, truncated := matchCandidates(args.Pattern, listing.Paths, args.MaxResults)

	payload := map[string]any{
		"pattern":              args.Pattern,
		"path":                 targetPath,
		"count":                len(matches),
		"files":                matches,
		"files_considered":     len(listing.Paths),
		"ignore_rules_applied": listing.IgnoreRulesApplied,
	}
	if truncated {
		payload["truncated"] = true
		payload["limit"] = args.MaxResults
	}
	// The candidate list itself was capped, so even an untruncated match list
	// does not cover the repository. Two different truncations, reported
	// separately, because they mean different things: one says "more matched",
	// the other says "more was never looked at".
	if listing.Truncated {
		payload["candidates_truncated"] = true
		payload["note"] = fmt.Sprintf(
			"only the first %d files were considered, so this list may be incomplete; "+
				"narrow it with the path argument", listing.Limit)
	}
	if listing.Skipped.Total() > 0 {
		payload["skipped"] = map[string]any{
			"unreadable":           listing.Skipped.Unreadable,
			"unrepresentable_name": listing.Skipped.UnrepresentableName,
		}
	}
	if len(listing.Paths) == 0 {
		payload["searched_nothing"] = true
		payload["note"] = "no file was listed, so this result is not evidence about the " +
			"repository's contents — the path may be empty, or every file under it may be " +
			"excluded by ignore rules. Re-run with include_ignored to find out which."
	}
	return ok(payload)
}
