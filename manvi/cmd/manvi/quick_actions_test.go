package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

type quickCall struct {
	Timeout time.Duration
	Dir     string
	Name    string
	Args    []string
}

type fakeQuickRunner struct {
	Calls   []quickCall
	Results []quickCommandResult
	Errors  []error
}

func (f *fakeQuickRunner) Run(
	_ context.Context,
	timeout time.Duration,
	dir, name string,
	args ...string,
) (quickCommandResult, error) {
	f.Calls = append(f.Calls, quickCall{Timeout: timeout, Dir: dir, Name: name, Args: append([]string(nil), args...)})
	i := len(f.Calls) - 1
	var result quickCommandResult
	if i < len(f.Results) {
		result = f.Results[i]
	}
	if i < len(f.Errors) {
		return result, f.Errors[i]
	}
	return result, nil
}

func TestQuickPullRefusesDirtyTreesBeforeTheNetwork(t *testing.T) {
	runner := &fakeQuickRunner{Results: []quickCommandResult{{Output: " M notes.txt\x00"}}}
	var out strings.Builder
	err := runQuickAction(context.Background(), &out, "/repo", "pull", nil, runner)
	if err == nil || !strings.Contains(err.Error(), "working tree") {
		t.Fatalf("dirty pull = %v, want an explicit refusal", err)
	}
	if len(runner.Calls) != 1 || runner.Calls[0].Name != "git" {
		t.Fatalf("dirty pull ran past its one preflight: %#v", runner.Calls)
	}
	if out.Len() != 0 {
		t.Fatalf("a refused pull printed a success answer: %q", out.String())
	}
}

func TestQuickPullIsFastForwardOnlyAndBounded(t *testing.T) {
	runner := &fakeQuickRunner{Results: []quickCommandResult{{}, {Output: "Already up to date.\n"}}}
	var out strings.Builder
	if err := runQuickAction(context.Background(), &out, "/repo", "pull", nil, runner); err != nil {
		t.Fatal(err)
	}
	if len(runner.Calls) != 2 {
		t.Fatalf("calls = %#v", runner.Calls)
	}
	wantStatus := []string{"status", "--porcelain=v1", "-z", "--untracked-files=normal"}
	if got := runner.Calls[0]; got.Dir != "/repo" || got.Timeout != quickInspectTimeout ||
		got.Name != "git" || !reflect.DeepEqual(got.Args, wantStatus) {
		t.Fatalf("preflight = %#v", got)
	}
	wantPull := []string{"pull", "--ff-only"}
	if got := runner.Calls[1]; got.Dir != "/repo" || got.Timeout != quickNetworkTimeout ||
		got.Name != "git" || !reflect.DeepEqual(got.Args, wantPull) {
		t.Fatalf("pull = %#v", got)
	}
	if got := out.String(); got != "Already up to date.\n" {
		t.Fatalf("output = %q", got)
	}
}

func TestQuickPushCannotForceOrOverrideTheUpstream(t *testing.T) {
	runner := &fakeQuickRunner{Results: []quickCommandResult{{}}}
	var out strings.Builder
	if err := runQuickAction(context.Background(), &out, "/repo", "push", nil, runner); err != nil {
		t.Fatal(err)
	}
	if len(runner.Calls) != 1 {
		t.Fatalf("calls = %#v", runner.Calls)
	}
	got := runner.Calls[0]
	if got.Dir != "/repo" || got.Timeout != quickNetworkTimeout || got.Name != "git" ||
		!reflect.DeepEqual(got.Args, []string{"push"}) {
		t.Fatalf("push = %#v; only bare `git push` is permitted", got)
	}
	if out.String() != "push completed\n" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestQuickActionsRejectArgumentsInsteadOfBecomingGitPassthroughs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "pull", args: []string{"--rebase"}},
		{name: "push", args: []string{"--force"}},
		{name: "issues", args: []string{"--limit", "10000"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeQuickRunner{}
			err := runQuickAction(context.Background(), io.Discard, "/repo", tc.name, tc.args, runner)
			if err == nil || !strings.Contains(err.Error(), "usage") {
				t.Fatalf("%s %v = %v, want usage refusal", tc.name, tc.args, err)
			}
			if len(runner.Calls) != 0 {
				t.Fatalf("refused arguments reached a subprocess: %#v", runner.Calls)
			}
		})
	}
}

func TestIssueReportIsStructuredBoundedAndTerminalSafe(t *testing.T) {
	items := []issueReportItem{
		{Number: 12, Title: "Fix \x1b[31mrender reset", UpdatedAt: "2026-08-25T12:30:00Z", URL: "https://github.com/o/r/issues/12"},
	}
	items[0].Labels = append(items[0].Labels, struct {
		Name string `json:"name"`
	}{Name: "urgent"}, struct {
		Name string `json:"name"`
	}{Name: "bug"})
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeQuickRunner{Results: []quickCommandResult{{Output: string(raw)}}}
	var out strings.Builder
	if err := runQuickAction(context.Background(), &out, "/repo", "issues", nil, runner); err != nil {
		t.Fatal(err)
	}
	if len(runner.Calls) != 1 {
		t.Fatalf("calls = %#v", runner.Calls)
	}
	call := runner.Calls[0]
	if call.Name != "gh" || call.Timeout != quickIssuesTimeout || call.Dir != "/repo" {
		t.Fatalf("issue command = %#v", call)
	}
	joined := strings.Join(call.Args, " ")
	for _, want := range []string{"issue list", "--state open", "--limit 100", "sort:updated-desc"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q does not contain %q", joined, want)
		}
	}
	text := out.String()
	for _, want := range []string{"open issues: 1", "#12", `Fix \e[31mrender reset`, "updated 2026-08-25", "labels: bug, urgent"} {
		if !strings.Contains(text, want) {
			t.Errorf("report does not contain %q:\n%s", want, text)
		}
	}
	if strings.ContainsRune(text, '\x1b') {
		t.Fatalf("an issue title injected a terminal escape: %q", text)
	}
}

func TestIssueReportNeverPresentsItsCapAsCompleteCoverage(t *testing.T) {
	items := make([]issueReportItem, quickIssueLimit)
	for i := range items {
		items[i] = issueReportItem{
			Number: i + 1, Title: fmt.Sprintf("issue %d", i+1),
			UpdatedAt: "2026-08-25T12:30:00Z", URL: fmt.Sprintf("https://example.test/issues/%d", i+1),
		}
	}
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeQuickRunner{Results: []quickCommandResult{{Output: string(raw)}}}
	var out strings.Builder
	if err := quickIssues(context.Background(), &out, "/repo", runner); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "capped at 100; more may exist") {
		t.Fatalf("capped report claimed completeness:\n%s", got)
	}
}

func TestQuickActionFailuresRedactCredentialsAndRemoteUserinfo(t *testing.T) {
	token := "ghp_" + "FAKEQUICKACTIONTOKEN1234"
	t.Setenv("GH_TOKEN", token)
	runner := &fakeQuickRunner{
		Results: []quickCommandResult{{Output: "remote https://user:password@example.test failed with " + token}},
		Errors:  []error{errors.New("exit status 1")},
	}
	err := quickPush(context.Background(), io.Discard, "/repo", runner)
	if err == nil {
		t.Fatal("failed push returned nil")
	}
	text := err.Error()
	if strings.Contains(text, token) || strings.Contains(text, "user:password") {
		t.Fatalf("credential reached the error: %q", text)
	}
	if !strings.Contains(text, "[redacted]") {
		t.Fatalf("redaction was invisible: %q", text)
	}
}

func TestIssueReportRefusesMalformedPartialAndDuplicateAnswers(t *testing.T) {
	cases := []struct {
		name   string
		result quickCommandResult
		want   string
	}{
		{name: "malformed", result: quickCommandResult{Output: "{"}, want: "malformed JSON"},
		{name: "truncated", result: quickCommandResult{Output: "[]", Truncated: true}, want: "no partial report"},
		{name: "duplicate", result: quickCommandResult{Output: `[
			{"number":1,"title":"a","updatedAt":"2026-08-25T00:00:00Z","url":"https://e/1"},
			{"number":1,"title":"b","updatedAt":"2026-08-25T00:00:00Z","url":"https://e/1"}]`}, want: "repeated issue #1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeQuickRunner{Results: []quickCommandResult{tc.result}}
			var out strings.Builder
			err := quickIssues(context.Background(), &out, "/repo", runner)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestOSQuickCommandRunnerBoundsTimeAndOutput(t *testing.T) {
	if os.Getenv("MANVI_QUICK_HELPER") != "" {
		return
	}
	runner := osQuickCommandRunner{}

	t.Run("output", func(t *testing.T) {
		t.Setenv("MANVI_QUICK_HELPER", "output")
		result, err := runner.Run(context.Background(), 5*time.Second, t.TempDir(),
			os.Args[0], "-test.run=^TestQuickCommandHelper$")
		if err != nil {
			t.Fatal(err)
		}
		if !result.Truncated || len(result.Output) != maxQuickOutputBytes {
			t.Fatalf("output len=%d truncated=%v", len(result.Output), result.Truncated)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		t.Setenv("MANVI_QUICK_HELPER", "sleep")
		start := time.Now()
		_, err := runner.Run(context.Background(), 50*time.Millisecond, t.TempDir(),
			os.Args[0], "-test.run=^TestQuickCommandHelper$")
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("timeout = %v", err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("50ms timeout took %s", elapsed)
		}
	})
}

func TestQuickCommandHelper(t *testing.T) {
	switch os.Getenv("MANVI_QUICK_HELPER") {
	case "output":
		_, _ = io.WriteString(os.Stdout, strings.Repeat("x", maxQuickOutputBytes+1024))
		os.Exit(0)
	case "sleep":
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
}

type quickFailWriter struct{}

func (quickFailWriter) Write([]byte) (int, error) { return 0, errors.New("output closed") }

func TestQuickActionsPropagateLostOutput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result quickCommandResult
	}{
		{name: "push", result: quickCommandResult{}},
		{name: "issues", result: quickCommandResult{Output: "[]"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeQuickRunner{Results: []quickCommandResult{tc.result}}
			err := runQuickAction(context.Background(), quickFailWriter{}, "/repo", tc.name, nil, runner)
			if err == nil || !strings.Contains(err.Error(), "output closed") {
				t.Fatalf("lost output = %v", err)
			}
		})
	}
}
