package devcouncil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/proc"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// The inspect bridge used to shell out to DevCouncil's Python `dev` CLI for
// project-level views (status / gaps / check). That package is deleted. The
// Go `devcouncil` binary still exists, but it does not implement those
// subcommands (`mcp`, `integrate`, `skills`, `verify`, `map`). Looking them
// up on PATH would exec the wrong program with the old argv and look like a
// working inspect.
//
// Native tools own those views now. This handler runs an external binary only
// when MANVI_DEVCOUNCIL_BINARY is set — an explicit pin, never a PATH guess.
// Missing or unpinned is unavailable, never an empty success.

const (
	// devInspectEnvBinary overrides binary discovery, so a test or an
	// operator can pin the integration to a known copy of the CLI.
	devInspectEnvBinary = "MANVI_DEVCOUNCIL_BINARY"

	// devInspectTimeout bounds every invocation when an override binary is
	// pinned. status and gaps answer in seconds; check can take longer. The
	// bound stops a wedged child from hanging a turn.
	devInspectTimeout = 5 * time.Minute
)

func (r *Registry) devTools() []tools.Tool {
	return []tools.Tool{
		{
			Schema: schema("devcouncil_dev_inspect",
				"Query a pinned DevCouncil inspect binary for project-level status/gaps/check JSON. "+
					"The Python `dev` CLI is deleted; Go `devcouncil` does not implement those sections. "+
					"Requires MANVI_DEVCOUNCIL_BINARY. Prefer native tools: devcouncil_get_gaps, "+
					"devcouncil_verify_task, manvi map. Read-only; needs no lease.",
				`{"type":"object","properties":{"section":{"type":"string","enum":["status","gaps","check"],"description":"which project view to query (default: status)"},"task_id":{"type":"string","description":"with section=gaps, scope the gaps to one task"}}}`),
			ReadOnly: true,
			Group:    tools.GroupCore,
			Extended: true,
			Handler:  r.devInspect,
		},
	}
}

// resolveDevCLI returns the inspect binary only when the operator pinned one.
// PATH `dev` / `devcouncil` are not consulted: `dev` is a common unrelated
// name, and Go `devcouncil` does not accept status/gaps/check.
func resolveDevCLI() (string, error) {
	path := strings.TrimSpace(os.Getenv(devInspectEnvBinary))
	if path == "" {
		return "", fmt.Errorf(
			"the Python DevCouncil CLI that answered status/gaps/check is deleted; "+
				"Go `devcouncil` does not implement those subcommands. "+
				"Use native tools (devcouncil_get_gaps, devcouncil_verify_task, manvi map) "+
				"or set %s to a binary you own",
			devInspectEnvBinary)
	}
	if _, err := exec.LookPath(path); err != nil {
		return "", fmt.Errorf("%s=%q is not executable: %w", devInspectEnvBinary, path, err)
	}
	return path, nil
}

func (r *Registry) devInspect(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Section string `json:"section"`
		TaskID  string `json:"task_id"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	section := args.Section
	if section == "" {
		section = "status"
	}
	switch section {
	case "status", "gaps", "check":
	default:
		return tools.Errorf("unknown section %q; use status, gaps, or check", args.Section)
	}

	bin, err := resolveDevCLI()
	if err != nil {
		return unavailable("the devcouncil CLI", err)
	}

	argv := []string{bin, section, "--json", "--project-root", r.deps.Root}
	switch section {
	case "gaps":
		// A scoped gap list is the payload MCP consumers get; passing the id
		// through rather than filtering client-side means one owner of what
		// "the gaps for task X" means.
		if strings.TrimSpace(args.TaskID) != "" {
			argv = append(argv, "--task-id", args.TaskID)
		}
	case "check":
		// Deterministic evidence gate, not the LLM audit: this tool runs from
		// an agent turn, where a surprise model call is a surprise bill.
		argv = append(argv, "--verify")
	}

	cmdCtx, cancel := context.WithTimeout(ctx, devInspectTimeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, argv[0], argv[1:]...)
	// A pinned inspect binary may spawn children. Killing only the direct
	// process leaves those children holding the inherited stdout pipe, and
	// os/exec then waits on an EOF that never comes.
	proc.ConfigureGroup(cmd)
	cmd.Dir = r.deps.Root
	cmd.WaitDelay = 5 * time.Second

	var stdout, stderr bytes.Buffer
	outCap := &limitWriter{w: &stdout, limit: maxGitOutputBytes}
	errCap := &limitWriter{w: &stderr, limit: 32 * 1024}
	cmd.Stdout = outCap
	cmd.Stderr = errCap

	runErr, timedOut := proc.RunBounded(cmdCtx, cmd.Run)
	if timedOut {
		return unavailable(fmt.Sprintf("the devcouncil CLI (%s)", bin),
			fmt.Errorf("timed out after %s", devInspectTimeout))
	}

	exitCode := 0
	if runErr != nil {
		exitErr, ok := runErr.(*exec.ExitError)
		if !ok {
			return unavailable(fmt.Sprintf("the devcouncil CLI (%s)", bin), runErr)
		}
		exitCode = exitErr.ExitCode()
	}

	envelope := map[string]any{
		"binary":    bin,
		"section":   section,
		"exit_code": exitCode,
	}
	var notes []string
	// Truncation is a degradation, not a detail: a JSON document cut off at the
	// cap will not parse, and the raw_output fallback below would otherwise
	// present the surviving prefix as though it were all the CLI said.
	if note := outCap.truncationNote(); note != "" {
		notes = append(notes, "dev_inspect: "+note)
	}
	if note := errCap.truncationNote(); note != "" {
		notes = append(notes, "dev_inspect stderr: "+note)
	}
	out := stdout.Bytes()
	var parsed any
	if json.Unmarshal(out, &parsed) == nil && parsed != nil {
		envelope["devcouncil"] = parsed
	} else if len(bytes.TrimSpace(out)) > 0 {
		// Non-JSON output is still evidence — version mismatches produce
		// prose tracebacks — but it must be labelled, because a consumer that
		// assumes the parsed shape would read garbage as structure.
		envelope["raw_output"] = string(out)
		notes = append(notes, "dev_inspect: output was not JSON; returned as raw_output")
	}
	if tail := strings.TrimSpace(stderr.String()); tail != "" {
		envelope["stderr_tail"] = tail
	}
	res := ok(envelope)
	if len(notes) > 0 {
		envelope["degraded"] = notes
		res.Degraded = notes
	}
	if exitCode != 0 {
		return failure(envelope, fmt.Sprintf("devcouncil %s exited with code %d", section, exitCode))
	}
	return res
}
