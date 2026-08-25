package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"manvi/core/bus"
	"manvi/credentials"
	"manvi/flags"
	"manvi/llm"
	"manvi/policy"
	"manvi/tools"
)

// zfixKey is the value these tests watch. It is not a credential and never was.
const zfixKey = "sk-ant-FAKE-TESTVALUE-1234567890abcdef"

// echoPipeline is a registry holding one tool that returns whatever text the
// test wants, which is what a real tool does: file contents, grep hits, the
// stdout of a command.
func echoPipeline(t *testing.T, text string, isError bool) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry(bus.New())
	err := reg.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "echo", Description: "echoes text back", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Handler: func(context.Context, tools.Call) tools.Result {
			return tools.Result{Text: text, IsError: isError}
		},
		ReadOnly: true,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	return reg
}

// TestManviToolNeverPrintsRawTerminalControlBytes is defect 6.
//
// `manvi tool` printed result.Text with fmt.Fprintln and nothing else. A tool
// result is file contents, grep hits, or a command's stdout — bytes this harness
// did not write, going to a terminal that executes control sequences rather than
// displaying them. A file holding a screen clear and a cursor home therefore
// wiped the operator's terminal and could redraw a prompt over it, which is the
// exact scenario ui/sanitize.go's doc names.
func TestManviToolNeverPrintsRawTerminalControlBytes(t *testing.T) {
	payloads := map[string]string{
		"clear and home": "\x1b[2J\x1b[1;1HApprove this write? [y/N] ",
		"osc 52":         "\x1b]52;c;bWFsaWNl\x07",
		"osc 8":          "\x1b]8;;https://evil.example\x07docs\x1b]8;;\x07",
		"dcs":            "\x1bPq\x1b\\",
		"bare cr":        "harmless\rrewritten",
		"c1 csi":         "before \u009b2J after",
		"nul":            "a\x00b",
		"bidi":           "start \u202e dne",
	}
	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			for _, isError := range []bool{false, true} {
				var out, notes bytes.Buffer
				err := callTool(&out, &notes, credentials.NewScrubber(),
					echoPipeline(t, payload, isError), []string{"echo"})
				if isError && err == nil {
					t.Fatal("a tool that reported an error returned no error")
				}
				if !isError && err != nil {
					t.Fatalf("callTool: %v", err)
				}
				for _, r := range payload {
					if r < 0x20 || (r >= 0x7f && r < 0xa0) || r == 0x202e {
						if strings.ContainsRune(out.String(), r) {
							t.Fatalf("is_error=%v: raw control %q reached stdout: %q",
								isError, r, out.String())
						}
					}
				}
			}
		})
	}
}

// TestManviToolRedactsACredentialEchoedByATool: the same write is the backstop
// for a key that came back out of a tool — a subprocess printing its own
// environment, or a file the tool was asked to read.
func TestManviToolRedactsACredentialEchoedByATool(t *testing.T) {
	scrubber := credentials.NewScrubber()
	scrubber.Watch(credentials.NewSecret(zfixKey, "ANTHROPIC_API_KEY"))

	var out, notes bytes.Buffer
	if err := callTool(&out, &notes, scrubber,
		echoPipeline(t, "ANTHROPIC_API_KEY="+zfixKey, false), []string{"echo"}); err != nil {
		t.Fatalf("callTool: %v", err)
	}
	if strings.Contains(out.String(), zfixKey) {
		t.Fatalf("the credential was printed: %q", out.String())
	}
}

// TestManviToolStillPrintsOrdinaryOutputUnchanged: the fix must not damage the
// common case, which is a tool returning plain text that gets piped somewhere.
func TestManviToolStillPrintsOrdinaryOutputUnchanged(t *testing.T) {
	const body = "package main\n\nfunc main() {}\n"
	var out, notes bytes.Buffer
	if err := callTool(&out, &notes, nil, echoPipeline(t, body, false), []string{"echo"}); err != nil {
		t.Fatalf("callTool: %v", err)
	}
	if out.String() != body+"\n" {
		t.Fatalf("ordinary output was altered:\n got %q\nwant %q", out.String(), body+"\n")
	}
}

// TestTheFullScreenFaceArmsACredentialScrubber is the host half of defect 3.
//
// manvi run, manvi watch and manvi probe each built a scrubber and armed it from
// a resolver. The TUI host built neither, so sinkFor handed the runner an
// unwrapped sink and there was nothing anywhere on that face to remove a key
// from a provider's error body. This asserts the host now has one and that it is
// armed — a scrubber watching nothing is a backstop that reports success without
// having checked anything.
func TestTheFullScreenFaceArmsACredentialScrubber(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", zfixKey)

	h := &harnessHost{reg: newTestRegistry(t)}
	resolver, scrubber := h.creds()
	if resolver == nil || scrubber == nil {
		t.Fatal("the host has no credential resolver or no scrubber")
	}
	if scrubber.Count() == 0 {
		t.Fatal("the scrubber was built but never armed from the resolver")
	}
	if got := scrubber.Clean("401: " + zfixKey); strings.Contains(got, zfixKey) {
		t.Fatalf("the host's scrubber does not remove the credential: %q", got)
	}
	// Called twice, because attachProvider calls it again after buildProvider
	// registers a requirement the resolver did not have before.
	if _, again := h.creds(); again != scrubber {
		t.Fatal("a second call built a different scrubber, so the two disagree about what is watched")
	}
}

// TestCheckReportsABlockInItsExitStatus is defect 7.
//
// Both branches of check printed the decision and returned nil, so every refusal
// this command exists to produce exited 0. A CI pre-flight written the obvious
// way — `manvi check "$f" && git commit` — therefore committed through every
// block it was added to catch.
func TestCheckReportsABlockInItsExitStatus(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)

	// A file inside the repository that policy has no reason to refuse.
	ordinary := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(ordinary, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args []string
		// enforce turns the command gate up, because the shipped dev posture
		// runs it advisory — which demotes a soft denial to an allow, so the
		// only soft block reachable from the CLI needs the gate enforcing.
		enforce bool
		want    error
	}{
		{name: "an ordinary file passes", args: []string{"check", "notes.txt"}},
		{name: "an orientation command passes", args: []string{"check", "--cmd", "git status"}},
		{name: "a credential path is refused by a hard rule",
			args: []string{"check", ".env"}, want: errCheckHardBlocked},
		{name: "the git directory is refused by a hard rule",
			args: []string{"check", ".git/config"}, want: errCheckHardBlocked},
		{name: "a path outside the repository is refused by a hard rule",
			args: []string{"check", "/etc/passwd"}, want: errCheckHardBlocked},
		{name: "a force push is refused by a hard rule",
			args: []string{"check", "--cmd", "git push -f origin main"}, want: errCheckHardBlocked},
		{name: "a command off the allowlist is refused by a soft rule",
			args:    []string{"check", "--cmd", "curl https://example.com"},
			enforce: true, want: errCheckBlocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.enforce {
				t.Setenv(flags.EnvKey(flags.PolicyCommandMode), flags.ModeEnforce)
			}
			var out, notes bytes.Buffer
			err := run(&out, &notes, tc.args)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("got %v, want a clean pass\n%s", err, out.String())
				}
				if strings.Contains(out.String(), "DENY") {
					t.Fatalf("the decision says DENY but the status says pass:\n%s", out.String())
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v\n%s", err, tc.want, out.String())
			}
			// The status and the printed decision must agree. A block reported
			// only in prose is the defect one level down.
			if !strings.Contains(out.String(), "DENY") {
				t.Fatalf("the status says blocked and the decision does not:\n%s", out.String())
			}
		})
	}
}

// TestTheTwoBlockedStatusesAreDistinctAndNeitherCollidesWithARunStatus.
//
// Six and seven ask the caller for different things: one is cleared by a grant,
// the other by nothing. And main's switch is shared with `manvi run`, so a
// sentinel that matched one of its four would give a block the meaning of a
// truncated turn.
func TestTheTwoBlockedStatusesAreDistinctAndNeitherCollidesWithARunStatus(t *testing.T) {
	if errors.Is(errCheckBlocked, errCheckHardBlocked) || errors.Is(errCheckHardBlocked, errCheckBlocked) {
		t.Fatal("the two blocked statuses are not distinguishable")
	}
	for _, other := range []error{errTruncated, errOutputCap, errNoAnswer, errUnfinished, errUsage} {
		for _, blocked := range []error{errCheckBlocked, errCheckHardBlocked} {
			if errors.Is(blocked, other) || errors.Is(other, blocked) {
				t.Fatalf("%v and %v are not distinguishable in main's switch", blocked, other)
			}
		}
	}
}

// TestCheckStatusMapsEveryDecisionShape covers the mapping directly, including
// the one that must stay 0: a warn is not a block, and turning it into a
// non-zero status would break every pipeline that legitimately proceeds through
// a qualified allow.
func TestCheckStatusMapsEveryDecisionShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   policy.Decision
		want error
	}{
		{"allow", policy.Decision{Action: policy.Allow}, nil},
		{"warn", policy.Decision{Action: policy.Warn, Severity: policy.Soft}, nil},
		{"soft deny", policy.Decision{Action: policy.Deny, Severity: policy.Soft}, errCheckBlocked},
		{"hard deny", policy.Decision{Action: policy.Deny, Severity: policy.Hard}, errCheckHardBlocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := checkStatus(tc.in); !errors.Is(got, tc.want) && !(got == nil && tc.want == nil) {
				t.Fatalf("checkStatus = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTheUsageTextDocumentsTheBlockedStatuses. An exit status nobody is told
// about is one no script will check for.
func TestTheUsageTextDocumentsTheBlockedStatuses(t *testing.T) {
	for _, want := range []string{"6", "7", "manvi check"} {
		if !strings.Contains(usage, want) {
			t.Fatalf("the usage text does not mention %q", want)
		}
	}
}
