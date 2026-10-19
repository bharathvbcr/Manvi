package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/devmap"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/credentials"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/llm/local"
	"github.com/bharathvbcr/Manvi/manvi/ui"
)

// --- doctor ---

// doctor reports what the harness would do, and refuses to imply health it has
// not established. A check that could not run is printed as such rather than
// omitted, because an omitted check reads as a passing one.
func doctor(out io.Writer, reg *flags.Registry) error {
	_, _ = fmt.Fprintln(out, "manvi doctor")
	_, _ = fmt.Fprintln(out)

	// First, because every other line below describes the configuration of a
	// binary this one names. A report that does not say which build produced it
	// cannot be compared against another run's.
	_, _ = fmt.Fprintf(out, "  build           %s\n", currentBuild().Summary())

	posture, postureOrigin, err := reg.String(flags.HarnessPosture)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "  posture         %s (%s)\n", posture, postureOrigin)
	for _, line := range wrapNotice(flags.DescribePosture(posture).Notice, 60) {
		_, _ = fmt.Fprintf(out, "                  %s\n", line)
	}

	// The *effective* mode, resolved exactly as the gate resolves it. Printing
	// the raw flag while the posture overrides it would be a report that
	// misleads, which is worse than no report.
	mode, origin, err := flags.EffectiveGateMode(reg, flags.PolicyFileMode)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "  file gate       %s (%s)\n", mode, origin)

	cmdMode, cmdOrigin, err := flags.EffectiveGateMode(reg, flags.PolicyCommandMode)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "  command gate    %s (%s)\n", cmdMode, cmdOrigin)

	// Resolved, not raw, for the same reason the gate modes are: the yolo
	// posture turns these off without moving the flag, and a doctor that read
	// the flag would print "on" for rules that are not running.
	hard, hardOrigin, err := flags.EffectiveHardRules(reg)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "  hard rules      %s (%s)\n", ui.OnOff(hard), hardOrigin)
	if !hard {
		_, _ = fmt.Fprintln(out, "                  credential paths, restricted paths, git safety, and the")
		_, _ = fmt.Fprintln(out, "                  repository boundary are not enforced; writes can land")
		_, _ = fmt.Fprintln(out, "                  anywhere, and every decision is recorded as unchecked")
	}

	agentGrants, _, err := reg.Bool(flags.GrantsAgentEnabled)
	if err != nil {
		return err
	}
	ttl, _, err := reg.Duration(flags.GrantsAgentMaxTTL)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "  agent overrides %s (max %s)\n", ui.OnOff(agentGrants), ttl)

	// Reported as what will actually be sent, for the same reason the gate
	// modes are reported resolved rather than raw. supports_reasoning only
	// permits the reasoning_effort field; llm.effort is what fills it. Set one
	// without the other and both settings read back exactly as written while no
	// reasoning is requested at all — a configuration that looks on and behaves
	// off, which is the one thing doctor exists to catch.
	reasoning, _, err := reg.Bool(flags.LLMLocalSupportsReasoning)
	if err != nil {
		return err
	}
	effort, _, err := reg.String(flags.LLMEffort)
	if err != nil {
		return err
	}
	switch {
	case !reasoning:
		_, _ = fmt.Fprintf(out, "  reasoning       off (%s=false)\n", flags.LLMLocalSupportsReasoning)
	case effort == "":
		_, _ = fmt.Fprintln(out, "  reasoning       not requested")
		_, _ = fmt.Fprintf(out, "                  %s is on, but %s is empty, so no\n",
			flags.LLMLocalSupportsReasoning, flags.LLMEffort)
		_, _ = fmt.Fprintln(out, "                  reasoning_effort is sent and the model reasons at its own")
		_, _ = fmt.Fprintf(out, "                  default; set %s to ask for it\n", flags.LLMEffort)
	default:
		_, _ = fmt.Fprintf(out, "  reasoning       requested at effort %q\n", effort)
		// The ceiling belongs beside the base, not in a separate line item: a
		// turn that goes in circles may climb from one to the other, and an
		// operator reading only the base would be told half the answer.
		ceiling, _, err := reg.String(flags.LLMEffortCeiling)
		if err != nil {
			return err
		}
		if ceiling == "" {
			_, _ = fmt.Fprintf(out, "                  fixed for the whole turn; set %s to let a\n",
				flags.LLMEffortCeiling)
			_, _ = fmt.Fprintln(out, "                  turn that goes in circles raise it")
		} else {
			_, _ = fmt.Fprintf(out, "                  rising to at most %q if calls are refused for\n", ceiling)
			_, _ = fmt.Fprintf(out, "                  going in circles (%s)\n", flags.LLMEffortCeiling)
		}
	}

	// Named before the settings that came out of it. "Why is this value what it
	// is" is the question doctor exists to answer, and until now the config
	// layer was never one of the possible answers.
	_, _ = fmt.Fprintf(out, "  config          %s\n", configPath())
	if keys := reg.ConfiguredKeys(); len(keys) > 0 {
		_, _ = fmt.Fprintf(out, "                  read, %d setting(s): %s\n", len(keys), strings.Join(keys, ", "))
	} else {
		_, _ = fmt.Fprintln(out, "                  absent — every setting comes from the environment or its default")
	}

	verifier := toolBinary("MANVI_VERIFY_BINARY", "dcverify")
	_, _ = fmt.Fprintf(out, "  verifier        %s\n", verifier)
	if err := (rigorProbe{verifier}).check(); err != nil {
		_, _ = fmt.Fprintf(out, "                  UNAVAILABLE: %v\n", err)
		_, _ = fmt.Fprintln(out, "                  secret_scan, stub_detection, and diff_coverage will report as degraded")
	} else {
		_, _ = fmt.Fprintln(out, "                  reachable — secret_scan, stub_detection, diff_coverage will run")
	}

	searcher := grepClient(projectRoot())
	_, _ = fmt.Fprintf(out, "  searcher        %s\n", searcher.Binary)
	if err := searcher.Available(context.Background()); err != nil {
		_, _ = fmt.Fprintf(out, "                  UNAVAILABLE: %v\n", err)
		_, _ = fmt.Fprintln(out, "                  devcouncil_grep will refuse rather than report zero matches")
	} else {
		_, _ = fmt.Fprintln(out, "                  reachable — devcouncil_grep will search, honouring ignore rules")
	}

	client := storeClient()
	_, _ = fmt.Fprintf(out, "  store           %s -> %s\n", client.Binary, client.DB)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Available(ctx); err != nil {
		_, _ = fmt.Fprintf(out, "                  UNAVAILABLE: %v\n", err)
		_, _ = fmt.Fprintln(out, "                  lease checks cannot run; writes that need one will be refused")
	} else {
		leases, _ := client.ActiveLeases(ctx)
		_, _ = fmt.Fprintf(out, "                  reachable, %d active lease(s)\n", len(leases))
	}

	reportDevMap(out, ctx, mapClient(projectRoot()))

	_, _ = fmt.Fprintln(out)
	statuses := credentials.NewResolver().Statuses()
	for _, status := range statuses {
		// Reported below instead, under its own heading. A loopback server
		// needs no key, so "local — no credential" is a true statement about
		// nothing, printed directly above the line that answers the question
		// an operator was actually asking.
		if status.Provider == local.Name {
			continue
		}
		// The second Anthropic key is a fact about the anthropic provider, so
		// it is printed beneath that line rather than as a provider of its own.
		if status.Provider == credentials.AnthropicFallback {
			continue
		}
		if status.Present {
			_, _ = fmt.Fprintf(out, "  %-15s credential from %s (%d chars)\n",
				status.Provider, status.Source, status.Length)
		} else {
			_, _ = fmt.Fprintf(out, "  %-15s no credential — %s\n", status.Provider, status.Detail)
		}
		if status.Provider == anthropic.Name {
			facts, fault := anthropicFallbackFacts(statuses, status.Present)
			if fault != "" {
				_, _ = fmt.Fprintf(out, "  %-15s MISCONFIGURED: %s\n", "", fault)
			}
			for _, fact := range facts {
				_, _ = fmt.Fprintf(out, "  %-15s %s\n", "", fact)
			}
		}
	}

	// Named separately from the credential list above, because the credential
	// list answers a question this provider does not have: a loopback server
	// needs no key, so "no credential" said nothing about whether a local model
	// was available to run against.
	_, _ = fmt.Fprintf(out, "  %-15s %s\n", "local models", describeLocalReadiness(reg))

	// A green run assembled under relaxed settings is not a green run.
	if weak := reg.Weakened(); len(weak) > 0 {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, "  WEAKENED — results produced under these settings are not strict:")
		for _, v := range weak {
			_, _ = fmt.Fprintf(out, "    %s = %s (%s)\n", v.Key, v.Raw, v.Origin)
		}
	}
	return nil
}

// rigorProbe asks the verifier to identify itself, so doctor reports a fact
// rather than the absence of an error.
type rigorProbe struct{ binary string }

func (p rigorProbe) check() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return devcouncil.VerifierAvailable(ctx, p.binary)
}

// wrapNotice breaks a sentence into lines of at most width columns. An empty
// notice wraps to nothing, so a posture that relaxes nothing prints nothing
// rather than a blank continuation line.
func wrapNotice(notice string, width int) []string {
	var lines []string
	var line string
	for _, word := range strings.Fields(notice) {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) <= width:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// reportDevMap states the index and the code graph as the two separate things
// they are.
//
// It was one line: the index's symbol and edge counts printed beside the
// artifact's path. That asserts of one file a number that came from another,
// and the two do diverge — while this repository's index stood at generation 4
// holding 4,249 symbols, the artifact carried generation 2 and 2,713, and
// doctor reported the artifact as holding 4,249.
//
// The line was also printed only when the index answered and held symbols, so a
// repository whose index would not open said nothing about navigation at all.
// The row an operator opens doctor to read is the row that disappeared exactly
// when there was something to read. Both halves now always report, including
// when what they have to report is that they could not look.
func reportDevMap(out io.Writer, ctx context.Context, mc *devmap.Client) {
	status, err := mc.Status(ctx)
	switch {
	case err != nil:
		_, _ = fmt.Fprintf(out, "  devmap          index UNAVAILABLE: %v\n", err)
		_, _ = fmt.Fprintln(out, "                  navigation tools will report unavailable; the scope rung falls back to same-directory")
	case status.NodeCount <= 0:
		_, _ = fmt.Fprintln(out, "  devmap          index holds no symbols — run `manvi map build`")
	default:
		_, _ = fmt.Fprintf(out, "  devmap          index generation %d (%d symbols, %d edges)%s\n",
			status.GenerationID, status.NodeCount, status.EdgeCount, staleSuffix(status))
	}

	graph := graphArtifactPath()
	m, loadErr := loadGraph(graph)
	switch {
	case loadErr != nil:
		_, _ = fmt.Fprintf(out, "                  %s UNREADABLE: %v\n", graph, loadErr)
	case m == nil:
		_, _ = fmt.Fprintf(out, "                  no %s — the scope rung will record repo_map.unavailable\n", graph)
	default:
		p := m.Provenance()
		generation := "unstamped"
		if p.Stamped {
			generation = fmt.Sprintf("generation %d", p.GenerationID)
		}
		_, _ = fmt.Fprintf(out, "                  %s (%d symbols over %d files, %s)\n",
			graph, p.Nodes, m.Stats().Files, generation)
		if status != nil {
			for _, note := range m.DisagreementsWith(status.GenerationID, status.NodeCount) {
				_, _ = fmt.Fprintf(out, "                  DIVERGED: %s\n", note)
			}
		}
	}
}

// staleSuffix marks an index older than the tree it describes.
func staleSuffix(status *devmap.Status) string {
	if status.IsFresh {
		return ""
	}
	return " — STALE, built before the current working tree"
}
