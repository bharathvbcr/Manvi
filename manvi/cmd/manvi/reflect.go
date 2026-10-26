package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/artifacts"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/lessons"
	"github.com/bharathvbcr/Manvi/manvi/session"
	"github.com/bharathvbcr/Manvi/manvi/tools"
	"github.com/bharathvbcr/Manvi/manvi/ui"
)

const reflectUsage = `manvi reflect — turn one finished session into at most one lesson

Usage:
  manvi reflect --session ID [--library NAME] [--timeout DURATION]

Options:
  --session ID         The session to reflect on, by full id or unique prefix.
                       Required. See 'manvi run' for where sessions are kept.
  --library NAME       The lesson library to file under: the host application or
                       repository the lesson is about. One path component of
                       a-z, 0-9, _ and -. Defaults to the repository's directory
                       name, taken from the main repository in a linked worktree.
  --timeout DURATION   Wall-clock bound on the reflection, e.g. 5m (default 10m).

What it does:
  Builds an evidence packet from the session log alone — verify reports, how each
  system prompt was assembled and what the budget dropped, per-step usage, and
  counts of overflows, compactions and malformed calls — and asks the reflector
  role to attribute each harness change as helped, hurt, no observed effect or
  confounded, and then to write one lesson or the single line NO_ENTRY. The
  packet never includes message text or the checkpoint critic's verdict, so a
  lesson is a reading of the evidence and not a restatement of a reviewer.

  The answer is read strictly. An unknown verdict, two lessons, a missing field
  or anything unreadable is refused: nothing is stored, the reason is printed,
  and the status is 1. NO_ENTRY stores nothing and exits 0. An accepted lesson
  is written as Markdown to .devcouncil/artifacts/lessons/<library>/, and its
  path is printed. The text is scrubbed of credential-shaped values first (said
  in the output when anything was redacted), and a cue seen before revises its
  entry, keeping the earlier revision as a History row.

  Lessons are never put in a prompt automatically. A person reads them and
  promotes the ones worth keeping into AGENTS.md, a skill or a spec.
`

const defaultReflectTimeout = 10 * time.Minute

// reflectMaxTokens is the output bound the reflector's requests name. An answer
// is a few edit rows and four short fields, well under a thousand tokens; the
// headroom is for a reasoning model's thinking, and the bound is what stops a
// runaway generation being read to the end.
const reflectMaxTokens = 8192

type reflectOptions struct {
	session string
	library string
	timeout time.Duration
}

func parseReflectArgs(args []string) (opts reflectOptions, help bool, err error) {
	opts.timeout = defaultReflectTimeout
	for i := 0; i < len(args); i++ {
		arg := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", arg)
			}
			i++
			return args[i], nil
		}
		switch arg {
		case "-h", "--help":
			return reflectOptions{}, true, nil
		case "--session":
			opts.session, err = next()
		case "--library":
			opts.library, err = next()
		case "--timeout":
			var raw string
			if raw, err = next(); err == nil {
				d, convErr := time.ParseDuration(raw)
				if convErr != nil || d <= 0 {
					return reflectOptions{}, false, fmt.Errorf("--timeout must be a positive duration such as 5m, got %q", raw)
				}
				opts.timeout = d
			}
		default:
			return reflectOptions{}, false, fmt.Errorf("reflect: unknown argument %q\n\n%s", arg, reflectUsage)
		}
		if err != nil {
			return reflectOptions{}, false, err
		}
	}
	if strings.TrimSpace(opts.session) == "" {
		return reflectOptions{}, false, fmt.Errorf("reflect: --session is required\n\n%s", reflectUsage)
	}
	if opts.library != "" {
		if err := lessons.ValidateLibrary(opts.library); err != nil {
			return reflectOptions{}, false, err
		}
	}
	return opts, false, nil
}

// reflectCommand runs `manvi reflect`.
func reflectCommand(out, notes io.Writer, reg *flags.Registry, args []string) error {
	opts, help, err := parseReflectArgs(args)
	if err != nil {
		return err
	}
	if help {
		_, err := io.WriteString(out, reflectUsage)
		return err
	}
	library := opts.library
	if library == "" {
		if library, err = lessons.DefaultLibrary(projectRoot()); err != nil {
			return err
		}
	}

	// Everything that can be refused is refused before a model is contacted:
	// the session, the packet and the library are all local, and a reflection
	// that cannot be filed would have been paid for to no purpose.
	sessions := session.NewStore(sessionsDir())
	id, err := sessions.Resolve(opts.session)
	if err != nil {
		var ambiguous *session.AmbiguousError
		var missing *session.NotFoundError
		switch {
		case errors.As(err, &ambiguous):
			return fmt.Errorf("--session %s matches %d sessions — name one of them: %s",
				ambiguous.Ref, len(ambiguous.Candidates), strings.Join(ambiguous.Candidates, " "))
		case errors.As(err, &missing):
			return fmt.Errorf("--session %s: no session in %s has that id or prefix", missing.Ref, sessions.Dir())
		}
		return err
	}
	log, report, err := sessions.Load(id)
	if err != nil {
		return err
	}
	packet, err := lessons.BuildPacket(id, log.Events())
	if err != nil {
		return err
	}
	if report.TrimmedTurns > 0 {
		// Said to the reflector as well as to the operator: a packet built from
		// what retention left is not the whole session, and a lesson that does
		// not know that will claim more than it saw.
		note := fmt.Sprintf("The session store dropped this session's %d oldest turn(s) to stay within its size bound "+
			"before it was loaded, so everything above describes only what remained.", report.TrimmedTurns)
		packet += "\nNOTE: " + note + "\n"
		_, _ = fmt.Fprintf(notes, "manvi: %s\n", note)
	}

	store, err := artifacts.NewStore(artifactsDir())
	if err != nil {
		return err
	}

	native, pipeline, err := nativeToolsWith(reg, nil)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()

	answer, scrub, err := runReflector(ctx, notes, reg, native, pipeline, lessons.Prompt(packet))
	if err != nil {
		return err
	}
	return applyReflection(out, notes, store, library, id, answer, scrub)
}

// applyReflection reads the reflector's answer and files what it may. It is
// the one place an answer becomes a stored lesson, and the only caller of
// lessons.Save, so "stored" and "passed the parser" are the same fact.
func applyReflection(out, notes io.Writer, store *artifacts.Store, library, sessionID, answer string, clean func(string) string) error {
	if clean == nil {
		clean = func(s string) string { return s }
	}
	result, err := lessons.Parse(answer)
	if err != nil {
		// The reason, and enough of the answer to see why; never the stored
		// state, because there is none.
		_, _ = fmt.Fprintf(notes, "manvi: reflect: nothing stored: %s\nmanvi: reflect: the refused answer began:\n%s\n",
			clean(err.Error()), indentLines(clean(truncateForNotes(answer))))
		return fmt.Errorf("reflect: the reflector's answer was refused, nothing was stored: %s", clean(err.Error()))
	}
	if result.NoEntry {
		_, err := fmt.Fprintf(out, "%s: the reflector found nothing worth keeping from session %s; nothing was stored\n",
			lessons.NoEntryLine, sessionID)
		return err
	}
	// Scrubbed before anything is stored or printed, and the scrubbed text is
	// all that goes further: the raw answer is not kept anywhere.
	result, redacted, err := lessons.Scrub(result, clean)
	if err != nil {
		return fmt.Errorf("reflect: nothing was stored: %w", err)
	}
	name, revision, err := lessons.Save(store, library, sessionID, result)
	unsynced := errors.Is(err, artifacts.ErrPublishedUnsynced)
	if err != nil && !unsynced {
		return fmt.Errorf("reflect: the answer was accepted but could not be stored: %w", err)
	}
	return reportStored(out, name, revision, library, sessionID, result.Experience.Cue, redacted, unsynced)
}

// reportStored says what was stored. A lesson whose directory sync was not
// confirmed is stored all the same, so it is reported as stored with a warning
// and not as an error: an error exit invites a re-run, which would file a
// second revision of a lesson already filed.
func reportStored(out io.Writer, name string, revision int, library, sessionID, cue string, redacted, unsynced bool) error {
	_, err := fmt.Fprintf(out, "stored %s (revision %d) in library %s from session %s\n  cue: %s\n",
		name, revision, library, sessionID, cue)
	if err == nil && unsynced {
		_, err = fmt.Fprintf(out, "  warning: the lesson is saved, but syncing its directory failed, so it may not survive a crash; do not re-run to retry it\n")
	}
	if err == nil && redacted {
		_, err = fmt.Fprintf(out, "  credential-shaped text was redacted from the lesson before it was stored\n")
	}
	return err
}

func truncateForNotes(s string) string {
	const limit = 1500
	// A refused answer is by definition unchecked, so what is echoed of it has
	// its control characters removed: an escape sequence in it would otherwise
	// be run by the terminal this is printed to.
	s = strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) {
			return -1
		}
		return r
	}, s)
	if r := []rune(s); len(r) > limit {
		return string(r[:limit]) + "… (truncated)"
	}
	return s
}

// runReflector asks the reflector role one question and returns its answer.
//
// It goes through subAgentRunner, the same machinery the checkpoint critic and
// the model's own fan-outs run on, so the child gets the role's tool surface,
// its tier placement, its spend accounting and the sub-agent step ceiling
// without any of it being reimplemented here. The provider, model and runner
// setup is buildRunEnv's, shared with `manvi run`.
//
// native and pipeline are built by the caller before its context exists, as
// runHeadless does: building them is not cancellable work.
func runReflector(ctx context.Context, notes io.Writer, reg *flags.Registry,
	native *devcouncil.Registry, pipeline *tools.Registry, prompt string) (string, func(string) string, error) {
	env, err := buildRunEnv(ctx, notes, reg, pipeline)
	if err != nil {
		return "", nil, err
	}
	scrubber := env.scrubber

	runner, _ := harnessFor(pipeline)
	if runner == nil {
		return "", nil, errors.New("reflect: this build has no sub-agent runner to run the reflector on")
	}
	meter := &subAgentMeter{}
	cfg := env.subAgentConfig(reg, pipeline, native, meter, coreToolsOnly(reg, env.provider.Name()))
	cfg.sink = ui.SinkFunc(func(ui.Event) {})
	runner.attach(cfg)

	def, ok := native.Roles().Get(reflectorRoleName)
	if !ok {
		def, ok = agents.NewRegistry().Get(reflectorRoleName)
	}
	if !ok {
		return "", nil, fmt.Errorf("reflect: the %q role is not in the role catalogue", reflectorRoleName)
	}
	req := devcouncil.SubAgentRequest{Label: reflectorRoleName, Prompt: prompt, MaxTokens: reflectMaxTokens}
	// Read-only whatever the role says, as the checkpoint critic's is.
	devcouncil.ApplyRole(&req, def, true)
	req.Tier = agents.Route(def, true, false, 1)

	result, err := runner.RunSubAgent(ctx, req)
	if err != nil {
		return "", nil, fmt.Errorf("reflect: the reflector did not complete: %s", scrubber.Clean(err.Error()))
	}
	if total := meter.Total(); total.Any() {
		_, _ = fmt.Fprintf(notes, "manvi: reflection spent %d in / %d out\n", total.InputTokens, total.OutputTokens)
	}
	return result.Summary, scrubber.Clean, nil
}

// reflectorRoleName is the shipped role `manvi reflect` runs.
const reflectorRoleName = "reflector"
