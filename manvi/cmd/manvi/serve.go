package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"manvi/flags"
	"manvi/serve"
)

const serveUsage = `manvi serve — expose the harness's planes to a host process over stdio

Usage:
  manvi serve [--posture host|devcouncil]

Speaks NDJSON on stdin/stdout: one JSON request object per line in, one
response object per line out, correlated by the caller's "id". Diagnostics go
to stderr, so stdout carries nothing but protocol.

This is how a host that is not written in Go — an editor, an IDE, a desktop
app — uses the local-LLM and policy planes without a cgo boundary or a second
implementation of either.

Postures:
  host        Hard rules enforced; a denial that only says "no task authorises
              this" is demoted to an allow that records why. For a host with no
              DevCouncil task model. This is the default, because a process
              being driven over stdio by another program is embedded.
  devcouncil  The harness's own posture: a task is required and its absence is
              a denial.

The host should set MANVI_HARNESS_INIT_ENABLED=false before spawning this.
Every manvi command otherwise prepares the repository it stands in — creating
the state directory and adding managed .gitignore rules — which is right for a
command an operator ran and wrong for a sidecar that happens to have been
spawned inside someone else's project.

Closing stdin shuts the server down cleanly.
`

// serveCommand runs the stdio server until stdin closes or the process is
// signalled.
func serveCommand(out io.Writer, reg *flags.Registry, args []string) error {
	posture := serve.PostureHost
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help", "help":
			fmt.Fprint(out, serveUsage)
			return nil
		case "--posture":
			if i+1 >= len(args) {
				return fmt.Errorf("--posture needs a value (host or devcouncil)")
			}
			i++
			switch strings.ToLower(args[i]) {
			case string(serve.PostureHost):
				posture = serve.PostureHost
			case string(serve.PostureDevCouncil):
				posture = serve.PostureDevCouncil
			default:
				return fmt.Errorf("unknown posture %q (want host or devcouncil)", args[i])
			}
		default:
			return fmt.Errorf("unknown argument %q\n\n%s", args[i], serveUsage)
		}
	}

	// Read through the same accessor the gate uses, so `manvi serve` and
	// `manvi check` cannot disagree about what is enforced — including under
	// the yolo posture, which EffectiveHardRules folds in.
	hardRules, hardOrigin, err := flags.EffectiveHardRules(reg)
	if err != nil {
		return err
	}
	neighbors, _, err := reg.Bool(flags.PolicyNeighborScope)
	if err != nil {
		return err
	}

	// Announced on stderr, never stdout. A gate that was turned off must not be
	// silent about it, and a host reading protocol on stdout must not have to
	// skip a banner to find its first response.
	if !hardRules {
		fmt.Fprintf(os.Stderr,
			"manvi serve: WEAKENED — hard rules are off (%s). Credential paths, restricted "+
				"paths, the repository boundary and git safety are not enforced.\n", hardOrigin)
	}

	// SIGINT and SIGTERM end the server as cleanly as a closed stdin. Without
	// this a host that kills the sidecar leaves a half-written line on the
	// wire, which the host then reports as a protocol error against itself.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := serve.New(os.Stdout, serve.Options{
		HardRules:      hardRules,
		AllowNeighbors: neighbors,
		Posture:        posture,
	})
	return srv.Serve(ctx, os.Stdin)
}
