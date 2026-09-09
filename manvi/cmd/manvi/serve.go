package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/bharathvbcr/Manvi/manvi/codingagent"
	"github.com/bharathvbcr/Manvi/manvi/credentials"
	"github.com/bharathvbcr/Manvi/manvi/dc/store"
	"github.com/bharathvbcr/Manvi/manvi/flags"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/serve"
)

const serveUsage = `manvi serve — expose the harness's planes to a host process over stdio

Usage:
  manvi serve [--posture host|devcouncil] [--workbench-db /absolute/profile.sqlite]

Speaks NDJSON on stdin/stdout: one JSON request object per line in, one
response object per line out, correlated by the caller's "id". Diagnostics go
to stderr, so stdout carries nothing but protocol.

This is how a host that is not written in Go — an editor, an IDE, a desktop
app — uses the local-LLM, policy, and deep code-intelligence planes without a
cgo boundary or a second implementation of any of them.

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

--workbench-db explicitly enables work.* repository groups and task storage in
a profile database. Its parent directory must already exist. Use a separate
profile file from repository execution/lease databases. No worker is started
until the host sends its first work.* request. work.enhancements.generate starts
the explicitly selected provider/model asynchronously after a durable claim;
board reads and cancellation remain responsive. No model call starts on CRUD.

Closing stdin shuts the server and its store processes down cleanly.
`

// serveCommand runs the stdio server until stdin closes or the process is
// signalled.
func serveCommand(out io.Writer, reg *flags.Registry, args []string) error {
	posture := serve.PostureHost
	workbenchDB := ""
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
		case "--workbench-db":
			if workbenchDB != "" {
				return fmt.Errorf("--workbench-db must be supplied only once")
			}
			if i+1 >= len(args) {
				return fmt.Errorf("--workbench-db needs an absolute profile database path")
			}
			i++
			if !filepath.IsAbs(args[i]) || strings.ContainsRune(args[i], '\x00') {
				return fmt.Errorf("--workbench-db needs an absolute profile database path")
			}
			workbenchDB = filepath.Clean(args[i])
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

	sameDir, _, err := reg.Bool(flags.PolicyScopeSameDir)
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

	modules := []serve.Module{serve.DevmapModule{Client: mapClient(projectRoot())}}
	var runner *serve.EnhancementRunner
	var managed *serve.ManagedRunner
	if workbenchDB != "" {
		client := store.New(toolBinary("MANVI_STORE_BINARY", "dcstore"), workbenchDB)
		defer client.Close()
		scrubber := credentials.NewScrubber()
		runner, err = serve.NewEnhancementRunner(client, func(ctx context.Context, name, _ string) (llm.Provider, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			resolver := credentials.NewResolver()
			scrubber.WatchAll(resolver)
			provider, err := buildProvider(name, reg, resolver, io.Discard)
			scrubber.WatchAll(resolver)
			if err != nil {
				return nil, err
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return provider, nil
		}, func(err error) string { return scrubber.Clean(err.Error()) })
		if err != nil {
			return err
		}
		configuration, err := workbenchEnhancementConfiguration(reg)
		if err != nil {
			return err
		}
		managed, err = serve.NewManagedRunner(client, func(ctx context.Context, options codingagent.Options) (serve.ManagedSession, error) {
			options.Program = toolBinary("MANVI_CODEX_BINARY", "codex")
			session, err := codingagent.StartCodex(ctx, options)
			if err != nil {
				return nil, err
			}
			return session, nil
		}, func(err error) string { return scrubber.Clean(err.Error()) })
		if err != nil {
			return err
		}
		modules = append(modules, serve.WorkbenchModule{Client: client, Runner: runner, Configuration: &configuration, Managed: managed})
	}
	srv := serve.New(os.Stdout, serve.Options{
		HardRules:      hardRules,
		AllowNeighbors: neighbors,
		AllowSameDir:   sameDir,
		Posture:        posture,
		Modules:        modules,
	})
	err = srv.Serve(ctx, os.Stdin)
	if managed != nil {
		err = errors.Join(err, managed.Close())
	}
	if runner != nil {
		err = errors.Join(err, runner.Close())
	}
	return err
}

func workbenchEnhancementConfiguration(reg *flags.Registry) (serve.EnhancementConfiguration, error) {
	provider, _, err := reg.String(flags.LLMDefaultProvider)
	if err != nil {
		return serve.EnhancementConfiguration{}, err
	}
	configuration := serve.EnhancementConfiguration{OK: true, Provider: provider, Providers: providerNames(), ModelSource: "none"}
	if model := strings.TrimSpace(os.Getenv("MANVI_MODEL")); model != "" {
		configuration.Model, configuration.ModelSource = model, string(SourceEnv)
	} else if provider == "local" {
		model, _, err := reg.String(flags.LLMLocalModel)
		if err != nil {
			return serve.EnhancementConfiguration{}, err
		}
		if model = strings.TrimSpace(model); model != "" {
			configuration.Model, configuration.ModelSource = model, string(SourceSetting)
		}
	}
	return configuration, nil
}
