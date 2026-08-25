package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"manvi/credentials"
	"manvi/internal/proc"
	"manvi/ui"
)

const (
	quickInspectTimeout = 10 * time.Second
	quickNetworkTimeout = 2 * time.Minute
	quickIssuesTimeout  = 30 * time.Second
	quickIssueLimit     = 100
	maxQuickOutputBytes = 256 << 10
)

// quickCommandRunner is the one process seam behind the CLI and TUI actions.
// Keeping it injectable makes the exact argv, directory, and deadline part of
// the tests without letting those tests touch a remote.
type quickCommandRunner interface {
	Run(ctx context.Context, timeout time.Duration, dir, name string, args ...string) (quickCommandResult, error)
}

type quickCommandResult struct {
	Output    string
	Truncated bool
}

type osQuickCommandRunner struct{}

// cappedBuffer retains a bounded prefix while continuing to drain the child.
// Returning a short write would stop the copy goroutine and could leave a
// verbose child blocked on its pipe; accepting and discarding the tail bounds
// memory without changing the subprocess lifecycle.
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		_, _ = b.buf.Write(p[:remaining])
	}
	if remaining < len(p) {
		b.truncated = true
	}
	return n, nil
}

func (osQuickCommandRunner) Run(
	ctx context.Context,
	timeout time.Duration,
	dir, name string,
	args ...string,
) (quickCommandResult, error) {
	bound, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(bound, name, args...)
	cmd.Dir = dir
	cmd.WaitDelay = 2 * time.Second
	out := &cappedBuffer{limit: maxQuickOutputBytes}
	cmd.Stdout, cmd.Stderr = out, out

	runErr, timedOut := proc.RunBounded(bound, cmd.Run)
	if timedOut || errors.Is(bound.Err(), context.DeadlineExceeded) {
		// proc.RunBounded deliberately abandons a Start blocked in the kernel.
		// Do not inspect out on this path: that goroutine may still write it.
		if ctx.Err() != nil {
			return quickCommandResult{}, ctx.Err()
		}
		return quickCommandResult{}, fmt.Errorf("%s timed out after %s", name, timeout)
	}
	if ctx.Err() != nil {
		return quickCommandResult{}, ctx.Err()
	}
	return quickCommandResult{Output: out.buf.String(), Truncated: out.truncated}, runErr
}

// runQuickAction is the canonical owner shared by `manvi pull|push|issues`
// and the same-named slash commands. These stay on the human-operated command
// surface; they are deliberately not registered as model-callable tools.
func runQuickAction(
	ctx context.Context,
	out io.Writer,
	root, name string,
	args []string,
	runner quickCommandRunner,
) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: manvi %s", name)
	}
	switch name {
	case "pull":
		return quickPull(ctx, out, root, runner)
	case "push":
		return quickPush(ctx, out, root, runner)
	case "issues":
		return quickIssues(ctx, out, root, runner)
	default:
		return fmt.Errorf("unknown quick action %q", name)
	}
}

func quickPull(ctx context.Context, out io.Writer, root string, runner quickCommandRunner) error {
	status, err := runner.Run(ctx, quickInspectTimeout, root, "git",
		"status", "--porcelain=v1", "-z", "--untracked-files=normal")
	if err != nil {
		return quickFailure("pull preflight", status, err)
	}
	if status.Truncated || status.Output != "" {
		return errors.New("pull refused: the working tree has staged, modified, or untracked files; commit or stash them first")
	}

	result, err := runner.Run(ctx, quickNetworkTimeout, root, "git", "pull", "--ff-only")
	if err != nil {
		return quickFailure("pull", result, err)
	}
	return writeQuickResult(out, "pull completed", result)
}

func quickPush(ctx context.Context, out io.Writer, root string, runner quickCommandRunner) error {
	// No arguments means the configured upstream is the authority. In
	// particular, this cannot force, rewrite a refspec, or silently create a
	// tracking branch.
	result, err := runner.Run(ctx, quickNetworkTimeout, root, "git", "push")
	if err != nil {
		return quickFailure("push", result, err)
	}
	return writeQuickResult(out, "push completed", result)
}

type issueReportItem struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	UpdatedAt string `json:"updatedAt"`
	URL       string `json:"url"`
	Labels    []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func quickIssues(ctx context.Context, out io.Writer, root string, runner quickCommandRunner) error {
	result, err := runner.Run(ctx, quickIssuesTimeout, root, "gh",
		"issue", "list", "--state", "open", "--limit", fmt.Sprint(quickIssueLimit),
		"--search", "sort:updated-desc", "--json", "number,title,updatedAt,url,labels")
	if err != nil {
		return quickFailure("issue report", result, err)
	}
	if result.Truncated {
		return fmt.Errorf("issue report exceeded the %d-byte output bound; no partial report was shown", maxQuickOutputBytes)
	}

	var issues []issueReportItem
	if err := json.Unmarshal([]byte(result.Output), &issues); err != nil {
		return fmt.Errorf("issue report returned malformed JSON: %w", err)
	}
	if len(issues) == 0 {
		_, err := fmt.Fprintln(out, "issues: no open issues")
		return err
	}

	var report strings.Builder
	if len(issues) >= quickIssueLimit {
		fmt.Fprintf(&report, "open issues shown: %d (report capped at %d; more may exist)\n", len(issues), quickIssueLimit)
	} else {
		fmt.Fprintf(&report, "open issues: %d\n", len(issues))
	}
	seen := make(map[int]bool, len(issues))
	for _, issue := range issues {
		if issue.Number <= 0 || strings.TrimSpace(issue.Title) == "" || strings.TrimSpace(issue.URL) == "" {
			return errors.New("issue report contained an issue without a positive number, title, or URL")
		}
		if seen[issue.Number] {
			return fmt.Errorf("issue report repeated issue #%d", issue.Number)
		}
		seen[issue.Number] = true

		updated, err := time.Parse(time.RFC3339, issue.UpdatedAt)
		if err != nil {
			return fmt.Errorf("issue #%d has an invalid updatedAt timestamp", issue.Number)
		}
		labels := make([]string, 0, len(issue.Labels))
		for _, label := range issue.Labels {
			if name := strings.TrimSpace(label.Name); name != "" {
				labels = append(labels, cleanQuickOutput(name))
			}
		}
		sort.Strings(labels)
		meta := "updated " + updated.UTC().Format("2006-01-02")
		if len(labels) > 0 {
			meta += "  labels: " + strings.Join(labels, ", ")
		}
		fmt.Fprintf(&report, "  #%d %s\n      %s\n      %s\n",
			issue.Number, cleanQuickOutput(strings.TrimSpace(issue.Title)), meta,
			cleanQuickOutput(strings.TrimSpace(issue.URL)))
	}
	_, err = fmt.Fprint(out, report.String())
	return err
}

func writeQuickResult(out io.Writer, fallback string, result quickCommandResult) error {
	text := strings.TrimSpace(cleanQuickOutput(result.Output))
	if text == "" {
		text = fallback
	}
	if result.Truncated {
		text += fmt.Sprintf("\n[output truncated at %d bytes]", maxQuickOutputBytes)
	}
	_, err := fmt.Fprintln(out, text)
	return err
}

func quickFailure(action string, result quickCommandResult, err error) error {
	detail := strings.TrimSpace(cleanQuickOutput(result.Output))
	if result.Truncated {
		detail += fmt.Sprintf("\n[output truncated at %d bytes]", maxQuickOutputBytes)
	}
	if detail == "" {
		return fmt.Errorf("%s failed: %w", action, err)
	}
	return fmt.Errorf("%s failed: %w\n%s", action, err, detail)
}

var (
	remoteUserinfoPattern = regexp.MustCompile(`(?i)(https?://)([^/@\s]+)@`)
	githubTokenPattern    = regexp.MustCompile(`(?i)\b(?:gh[pousr]_[A-Za-z0-9_]{8,}|github_pat_[A-Za-z0-9_]{8,})\b`)
)

// cleanQuickOutput is the CLI backstop. The TUI cleans again at its own event
// boundary, but the shell surface has no renderer between this text and a log
// or terminal. Besides provider credentials, watch GitHub's documented token
// variables and redact userinfo from an accidentally credentialed remote URL.
func cleanQuickOutput(text string) string {
	scrubber := credentials.NewScrubber()
	scrubber.WatchAll(credentials.NewResolver())
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			scrubber.Watch(credentials.NewSecret(value, key))
		}
	}
	text = scrubber.Clean(text)
	text = remoteUserinfoPattern.ReplaceAllString(text, `${1}[redacted]@`)
	text = githubTokenPattern.ReplaceAllString(text, credentials.Redacted)
	return ui.Sanitize(text)
}
