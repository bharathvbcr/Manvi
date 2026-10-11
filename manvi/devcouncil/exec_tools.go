package devcouncil

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/gate"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

func (r *Registry) execCommand(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Command string `json:"command"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if strings.TrimSpace(args.Command) == "" {
		return tools.Errorf("command is required")
	}

	task, refusal := r.authorisingTask(ctx, "running commands")
	if refusal != nil {
		return *refusal
	}

	decision, err := r.deps.Gate.EvaluateCommand(args.Command, task)
	if err != nil {
		return unavailable("command policy decision", err)
	}

	if decision.Blocked() {
		escalated, ok := r.escalate(ctx, decision, args.Command)
		if !ok {
			return r.refusal(decision)
		}
		decision = escalated

		// Clearing the command is not clearing the write.
		//
		// The gate runs its redirect rung only on a command it is about to
		// permit, so a command denied when EvaluateCommand ran had its
		// redirect targets skipped -- and an escalation issues the grant
		// afterwards. That left the write half judged by nobody: a human
		// cleared `cat seed.txt > unplanned.txt` and the write landed on a
		// path a direct write_file would have refused as scope.unplanned. The
		// human was asked once, about the command.
		//
		// Only the redirect rung is re-run, not the whole ladder. Re-running
		// EvaluateCommand would re-litigate the command half against its
		// normalized target, which the just-issued grant deliberately does not
		// match -- the grant is keyed to the raw command the human was shown.
		taskID := ""
		if task != nil {
			taskID = task.ID
		}
		refused, err := gate.EvaluateRedirects(args.Command, taskID, task != nil,
			func(target string) (policy.Decision, error) {
				return r.deps.Gate.EvaluateWrite(target, task, dc.OpWrite)
			})
		if err != nil {
			return unavailable("redirect target policy decision", err)
		}
		if refused.Refused {
			return r.refusal(refused.Decision)
		}
	}

	cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	start := time.Now()
	outText, exitCode, timedOut, runErr := runShell(cmdCtx, r.deps.Root, args.Command)
	elapsed := time.Since(start)

	if timedOut {
		return annotate(tools.Result{
			Text:    "command timed out after 5m:\n" + outText,
			IsError: true,
		}, decision)
	}
	if runErr != nil {
		return annotate(tools.Result{
			Text:    fmt.Sprintf("failed to execute command %q: %v", args.Command, runErr),
			IsError: true,
		}, decision)
	}

	resultText := fmt.Sprintf("exit code %d (took %s)\n%s", exitCode, elapsed.Round(time.Millisecond), outText)
	return annotate(tools.Result{
		Text:    strings.TrimRight(resultText, "\n"),
		IsError: exitCode != 0,
	}, decision)
}

// runShell executes command under `sh -c` in dir, bounded by the deadline on
// ctx, and returns the captured output, the exit code, and whether the
// deadline fired.
//
// The process group, the group reap on every exit path, the output cap and
// the wait delay are runProc's, shared with every other child this package
// starts. What is particular to an agent-authored command is two things.
//
// The child gets a built environment rather than the operator's. It used to
// inherit everything, so an agent-authored command could print the operator's
// API keys straight back into the transcript, and an interpreter preload
// variable in the operator's shell would apply to whatever the agent chose to
// run. See sanitizedEnv.
//
// And stdout and stderr share one capture, in the order the command wrote
// them, because the model reads them as one transcript. A command whose
// foreground work completed reports success even when leftover descendants
// were cut off holding the pipes.
func runShell(ctx context.Context, dir, command string) (string, int, bool, error) {
	res := runProc(ctx, procSpec{
		argv: []string{"sh", "-c", command}, dir: dir, env: sanitizedEnv(dir),
		stdoutLimit: 1024 * 1024, combined: true,
	})
	output := string(res.stdout)
	if res.stdoutNote != "" {
		output = strings.TrimRight(output, "\n") + "\n" + res.stdoutNote + "\n"
	}
	if res.timedOut {
		return output, 0, true, nil
	}
	if res.failedToRun() {
		return output, 0, false, res.err
	}
	return output, res.exitCode, false, nil
}

// sanitizedEnv builds the environment an agent-authored command runs under.
//
// An allowlist, never a denylist: the operator's environment is not an
// enumerable set, and every new secret exported into a shell would otherwise
// arrive here as a default-allow. The child used to inherit all of it, so
// `env` printed the operator's provider keys straight back into the model's
// transcript, and an interpreter preload variable — LD_PRELOAD on Linux,
// DYLD_INSERT_LIBRARIES where the platform does not strip it — applied to
// whatever the agent chose to run.
//
// What survives is what a build or a test genuinely needs: the interpreter
// search path, a home to find tool config under, locale, a temp directory, and
// the toolchain caches whose absence turns every command into a cold rebuild.
// Nothing matching a credential shape is on the list, and a name not on the
// list does not reach the child at all.
func sanitizedEnv(dir string) []string {
	env := make([]string, 0, len(childEnvAllowlist)+1)
	for _, name := range childEnvAllowlist {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	// PWD is the shell's own idea of where it is; leaving the operator's would
	// contradict cmd.Dir.
	env = append(env, "PWD="+dir)
	return env
}

// childEnvAllowlist is the complete set of variable names passed through to a
// child process. Adding one is a deliberate decision about what an agent may
// read; nothing is matched by prefix or pattern, because a pattern is how
// ANTHROPIC_API_KEY ends up covered by a rule written for ANTHROPIC_BASE_URL.
var childEnvAllowlist = []string{
	// Where to find programs, and who is running them.
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM",
	// Scratch space.
	"TMPDIR", "TMP", "TEMP",
	// Text handling and clock, so output is not silently mangled or misdated.
	"LANG", "LC_ALL", "LC_CTYPE", "TZ",
	// Toolchain caches and roots. Dropping these does not fail closed, it
	// fails slow: every command becomes a cold rebuild.
	"GOROOT", "GOPATH", "GOCACHE", "GOMODCACHE", "GOFLAGS", "GOTMPDIR",
	"CARGO_HOME", "RUSTUP_HOME", "CARGO_TARGET_DIR",
	"npm_config_cache", "NODE_PATH",
	"PYTHONHASHSEED", "VIRTUAL_ENV",
}
