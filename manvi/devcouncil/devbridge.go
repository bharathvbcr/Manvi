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

	"manvi/tools"
)

// The dev-CLI bridge is the one place this harness shells out to the Python
// incumbent, and it is read-only on purpose. Everything the harness owns
// natively — the lease, the gate, the verifier — is reached through the
// native tools; what only the incumbent has is its project-level view:
// requirement coverage, cost accounting, the live review cards, the evidence
// gate over the whole working tree. This tool surfaces that view without
// making the Python process an authority over anything: it can inform a
// decision, never enforce one.
//
// If no CLI is installed the tool says so and names the environment variable
// that fixes it. It does not fall back to reading .devcouncil/state.sqlite
// itself: two readers of one database with different assumptions about its
// schema are how "compatible" drifts apart.

const (
	// devInspectEnvBinary overrides binary discovery, so a test or an
	// operator can pin the integration to a known copy of the CLI.
	devInspectEnvBinary = "MANVI_DEVCOUNCIL_BINARY"

	// devInspectTimeout bounds every invocation. `check --verify` runs the
	// incumbent's deterministic gates and can legitimately take minutes;
	// status and gaps answer in seconds. One generous bound covers all three,
	// and the bound is what stops a wedged Python startup from hanging a turn.
	devInspectTimeout = 5 * time.Minute
)

func (r *Registry) devTools() []tools.Tool {
	return []tools.Tool{
		{
			Schema: schema("devcouncil_dev_inspect",
				"Query the DevCouncil project CLI (the external `dev`/`devcouncil` command) for "+
					"project-level state the native tools do not carry: section=status for phase, "+
					"coverage summary and task counts; section=gaps for verification gaps (optionally "+
					"scoped with task_id); section=check for the working-tree audit, run in its "+
					"deterministic mode. Read-only; needs no lease.",
				`{"type":"object","properties":{"section":{"type":"string","enum":["status","gaps","check"],"description":"which project view to query (default: status)"},"task_id":{"type":"string","description":"with section=gaps, scope the gaps to one task"}}}`),
			ReadOnly: true,
			Group:    tools.GroupCore,
			Extended: true,
			Handler:  r.devInspect,
		},
	}
}

// resolveDevCLI finds the incumbent's command-line entry point: an explicit
// override first, then the canonical name, then the short alias the allowlists
// already normalize to the same family.
func resolveDevCLI() (string, error) {
	if path := strings.TrimSpace(os.Getenv(devInspectEnvBinary)); path != "" {
		if _, err := exec.LookPath(path); err != nil {
			return "", fmt.Errorf("%s=%q is not executable: %w", devInspectEnvBinary, path, err)
		}
		return path, nil
	}
	for _, name := range []string{"devcouncil", "dev"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no %q or %q on PATH; install the DevCouncil CLI or set %s",
		"devcouncil", "dev", devInspectEnvBinary)
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
	cmd.Dir = r.deps.Root
	cmd.WaitDelay = 5 * time.Second

	var stdout, stderr bytes.Buffer
	outCap := &limitWriter{w: &stdout, limit: maxGitOutputBytes}
	errCap := &limitWriter{w: &stderr, limit: 32 * 1024}
	cmd.Stdout = outCap
	cmd.Stderr = errCap

	runErr := cmd.Run()

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
