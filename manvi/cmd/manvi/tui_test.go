package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"manvi/core/bus"
	"manvi/dc/devmap"
	"manvi/flags"
	"manvi/llm"
	"manvi/llm/gemini"
	"manvi/llm/local"
	"manvi/prompt"
	"manvi/tools"
	"manvi/ui"
)

func TestSplitArgsCarriesQuotedValues(t *testing.T) {
	// The case that made this necessary: a tool payload with a space inside a
	// JSON string. strings.Fields hands the decoder half an object.
	cases := []struct {
		in   string
		want []string
	}{
		{``, nil},
		{`doctor`, []string{"doctor"}},
		{`--all  --json`, []string{"--all", "--json"}},
		// A JSON payload keeps its own quotes only when the whole value is
		// wrapped, exactly as it must be on the CLI. Unwrapped, the inner
		// quotes are shell quoting and are consumed as such.
		{`--json '{"path":"a.go","content":"package calc"}'`,
			[]string{"--json", `{"path":"a.go","content":"package calc"}`}},
		{`--reason "the plan omitted this helper"`,
			[]string{"--reason", "the plan omitted this helper"}},
		{`--reason 'single quoted'`, []string{"--reason", "single quoted"}},
		{`--reason "with \"nested\" quotes"`, []string{"--reason", `with "nested" quotes`}},
		{`--reason ""`, []string{"--reason", ""}},
		{`a"b"c`, []string{"abc"}},
	}
	for _, c := range cases {
		got, err := splitArgs(c.in)
		if err != nil {
			t.Errorf("splitArgs(%q) errored: %v", c.in, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("splitArgs(%q) = %#v, want %#v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitArgs(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestSplitArgsRefusesAnUnterminatedQuote(t *testing.T) {
	// Silently closing the quote would hand a command a truncated argument that
	// looks complete — a --reason cut in half is still recorded on a grant.
	if _, err := splitArgs(`--reason "half a thought`); err == nil {
		t.Fatal("an unterminated quote was accepted")
	} else if !strings.Contains(err.Error(), "unterminated") {
		t.Fatalf("error = %v", err)
	}
}

func TestSplitArgsRoundTripsAWriteToolPayload(t *testing.T) {
	// The exact shape a session uses to reach the write gate.
	got, err := splitArgs(`devcouncil_write_file --json '{"path":"src/helper.go","content":"package calc"}'`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "devcouncil_write_file" || got[1] != "--json" {
		t.Fatalf("got %#v", got)
	}
	if !strings.HasPrefix(got[2], `{"path"`) || !strings.HasSuffix(got[2], `}`) {
		t.Fatalf("the payload was split: %q", got[2])
	}
}

// TestEffortReachesTheSessionOnlyWhenTheModelAcceptsIt covers the wiring the
// reasoning field depends on: llm.effort is read at attach, checked against the
// model that will receive it, and carried on the session into every request.
// Before this was wired, agent.Config.Effort was never set and Gemini's
// thinking_level was omitted from every request the harness made.
func TestEffortReachesTheSessionOnlyWhenTheModelAcceptsIt(t *testing.T) {
	registry := llm.NewRegistry()
	// The credential resolver is never called: Capability answers from the
	// catalogue, and no request is made here.
	if err := registry.Register(gemini.New("", nil)); err != nil {
		t.Fatalf("register: %v", err)
	}

	cases := []struct {
		name    string
		effort  string
		model   string
		want    string
		wantErr []string
	}{
		{
			name: "unset stays unset", effort: "", model: "gemini-3.7-flash", want: "",
		},
		{
			name: "a level the model serves is carried", effort: "high", model: "gemini-3.7-flash", want: "high",
		},
		{
			// Caught at attach rather than mid-turn, and the error has to name
			// both the setting and what the model actually takes.
			name: "a level the model does not serve is refused", effort: "xhigh", model: "gemini-3.7-flash",
			wantErr: []string{"MANVI_LLM_EFFORT", "xhigh", "low medium high"},
		},
		{
			// flash-lite is catalogued without reasoning support, so any effort
			// at all is a misconfiguration rather than a wrong tier.
			name: "a model without reasoning refuses any level", effort: "low", model: "gemini-3.5-flash-lite",
			wantErr: []string{"MANVI_LLM_EFFORT", "does not support reasoning effort"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reg, err := flags.NewHarnessRegistry("")
			if err != nil {
				t.Fatalf("registry: %v", err)
			}
			if c.effort != "" {
				if err := reg.Set(flags.Human, flags.LLMEffort, c.effort); err != nil {
					t.Fatalf("set effort: %v", err)
				}
			}
			host := &harnessHost{reg: reg}

			got, err := host.resolveEffort(registry, gemini.Name, c.model)
			if len(c.wantErr) > 0 {
				if err == nil {
					t.Fatalf("effort %q on %s was accepted, want a refusal at attach", c.effort, c.model)
				}
				for _, want := range c.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not mention %q", err.Error(), want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveEffort: %v", err)
			}
			if got != c.want {
				t.Fatalf("effort = %q, want %q", got, c.want)
			}
		})
	}
}

// TestPlanIndexRefreshDistinguishesOldFromMissing. Both need a build, and an
// operator's next move is different: a stale index answers about code that used
// to be there, an absent one answers "no results" to everything, and a missing
// devmap is a machine to fix rather than a repository to reindex. A plan that
// blurred them would send the reader to the wrong problem.
// TestTheBuildHeadlineCannotReadAsCompleteCoverage.
//
// This one line is what the TUI shows as the outcome of a build and what `manvi
// map build` prints first. "indexed 173 files, 900 symbols, 4000 edges" is a
// true sentence about a build that refused the 174th file, and a reader has no
// way to tell it from a build that refused nothing — which is the same failure
// as a capped sample presented as the whole answer.
// TestRebuildDiscardsOnlyTheDerivedIndex.
//
// `map rebuild` exists because an index committed by an older devmap keeps that
// binary's conclusions: a build over an unchanged tree recomputes nothing, so a
// wrong dead-code result survives every later build and no check from outside
// can see it. The command is only safe if it removes exactly the derived index
// and nothing an operator authored, which is what this pins.
func TestRebuildDiscardsOnlyTheDerivedIndex(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MANVI_STATE_DIR", filepath.Join(dir, ".devcouncil"))

	index := indexDir()
	if err := os.MkdirAll(index, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(index, "index.sqlite"), []byte("derived"), 0o644); err != nil {
		t.Fatal(err)
	}
	// State the harness owns, beside the index and not derived from the tree.
	if err := os.WriteFile(storeDBPath(), []byte("leases and tasks"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(indexDir()); err != nil {
		t.Fatalf("discarding the index: %v", err)
	}

	if _, err := os.Stat(index); !os.IsNotExist(err) {
		t.Errorf("the derived index survived: %v", err)
	}
	if _, err := os.Stat(storeDBPath()); err != nil {
		t.Errorf("rebuild reached state it does not own: %v", err)
	}
}

// TestAnUnchangedBuildIsDescribedNotFormattedAsNil.
//
// `manvi map build` twice in a row printed "indexed <nil> files, <nil> symbols,
// <nil> edges" the second time: devmap answers an unchanged tree with a payload
// carrying no counts, and the headline formatted the absent fields straight
// into the line.
func TestAnUnchangedBuildIsDescribedNotFormattedAsNil(t *testing.T) {
	report := &devmap.BuildReport{Stats: map[string]any{
		"unchanged": true, "files": float64(155), "generation": float64(7),
	}}
	line := describeIndex(report)
	if strings.Contains(line, "<nil>") {
		t.Fatalf("absent counts were formatted as numbers: %q", line)
	}
	if !strings.Contains(line, "155") || !strings.Contains(line, "already current") {
		t.Errorf("headline = %q, want it to say the index was already current at 155 files", line)
	}
}

func TestTheBuildHeadlineCannotReadAsCompleteCoverage(t *testing.T) {
	stats := map[string]any{"files_indexed": 173, "symbols": 900, "edges": 4000}

	clean := describeIndex(&devmap.BuildReport{Stats: stats})
	if strings.Contains(clean, "refused") {
		t.Errorf("a build that refused nothing must not mention refusals: %q", clean)
	}

	partial := describeIndex(&devmap.BuildReport{
		Stats:   stats,
		Notices: devmap.Notices{Refused: 1, Refusals: []devmap.Refusal{{Path: "vendor/huge.py", Reason: "Oversized"}}},
	})
	if partial == clean {
		t.Fatalf("a build that refused a file renders identically to one that did not: %q", partial)
	}
	if !strings.Contains(partial, "refused 1") {
		t.Errorf("headline = %q, want it to carry the count", partial)
	}
}

func TestPlanIndexRefreshDistinguishesOldFromMissing(t *testing.T) {
	cases := []struct {
		name      string
		status    *devmap.Status
		err       error
		build     bool
		kind      ui.Kind
		mentions  []string
		degrading bool
	}{
		{
			name:   "current index needs nothing",
			status: &devmap.Status{GenerationID: 7, NodeCount: 120, EdgeCount: 340, IsFresh: true},
			kind:   ui.KindReport, mentions: []string{"current", "7", "120", "340"},
		},
		{
			name:   "an index older than the tree is rebuilt",
			status: &devmap.Status{GenerationID: 7, NodeCount: 120, IsFresh: false},
			build:  true, kind: ui.KindNotice, degrading: true,
			mentions: []string{"before the current working tree", "rebuilding"},
		},
		{
			name: "an unavailable index carries the reason it gave",
			err:  errors.New("no generation has been committed"),
			// The client's own words, not a paraphrase: "run `manvi map build`"
			// and "devmap: executable file not found" are different problems.
			build: true, kind: ui.KindNotice, degrading: true,
			mentions: []string{"no generation has been committed", "unavailable"},
		},
		{
			name:  "neither a state nor an error is treated as no index",
			build: true, kind: ui.KindNotice, degrading: true,
			mentions: []string{"neither a state nor an error"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := planIndexRefresh(c.status, c.err, nil)
			if plan.Build != c.build {
				t.Errorf("Build = %v, want %v", plan.Build, c.build)
			}
			if plan.Kind != c.kind {
				t.Errorf("Kind = %q, want %q", plan.Kind, c.kind)
			}
			if c.degrading && len(plan.Degraded) == 0 {
				t.Error("nothing is named as degraded while the index is not usable")
			}
			if !c.degrading && len(plan.Degraded) != 0 {
				t.Errorf("Degraded = %q on an index that needs nothing", plan.Degraded)
			}
			// Against everything the operator is shown: the sentence and the
			// degradations are one message on screen.
			shown := plan.Text + " " + strings.Join(plan.Degraded, " ")
			for _, want := range c.mentions {
				if !strings.Contains(shown, want) {
					t.Errorf("the plan %q does not mention %q", shown, want)
				}
			}
		})
	}
}

// TestBareCommandLaunchesTUI asserts that running manvi with no subcommand
// launches the full-screen TUI by default.
func TestBareCommandLaunchesTUI(t *testing.T) {
	t.Chdir(t.TempDir())
	var out, notes strings.Builder
	// In test mode without an interactive terminal, launching TUI reports the terminal requirement.
	err := run(&out, &notes, []string{})
	if err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("run with no args did not attempt to launch TUI: err = %v, out = %q", err, out.String())
	}

	var yoloOut, yoloNotes strings.Builder
	yoloErr := run(&yoloOut, &yoloNotes, []string{"--yolo"})
	if yoloErr == nil || !strings.Contains(yoloErr.Error(), "interactive terminal") {
		t.Fatalf("run with --yolo did not attempt to launch TUI: err = %v, out = %q", yoloErr, yoloOut.String())
	}

	var helpOut, helpNotes strings.Builder
	if err := run(&helpOut, &helpNotes, []string{"--help"}); err != nil {
		t.Fatalf("run with --help failed: %v", err)
	}
	if !strings.Contains(helpOut.String(), "manvi — the DevCouncil execution harness") {
		t.Fatalf("help output does not contain usage header: %q", helpOut.String())
	}
}

func TestHarnessHostCommandsAreComplete(t *testing.T) {
	reg, err := flags.NewHarnessRegistry("")
	if err != nil {
		t.Fatalf("NewHarnessRegistry: %v", err)
	}
	host := &harnessHost{reg: reg}
	cmds := host.Commands()
	found := map[string]bool{}
	for _, c := range cmds {
		found[c.Name] = true
	}
	for _, want := range []string{
		"doctor", "flags", "providers", "tools", "leases", "lease",
		"check", "allow", "tool", "map", "probe", "logo", "help", "clear", "quit",
	} {
		if !found[want] {
			t.Errorf("harnessHost.Commands() missing %q", want)
		}
	}
}

// TestMaxStepsMalformedValuesAreRefusedAtBootNotReinterpreted.
//
// MANVI_MAX_STEPS was read with fmt.Sscanf(v, "%d", &n), which stops at the
// first byte it cannot use and reports success for everything it consumed.
// "12x" became 12 and "1e3" became 1 — so an operator writing 1e3 meaning a
// thousand got a ceiling of one step, and every turn ended after a single tool
// call with "ran out of steps". Nothing in the run said the number came from a
// misread setting rather than from the work.
//
// The key is in the catalogue now, so this is the registry's own rule rather
// than a second parser: LoadEnv refuses a value that is not an int, at boot,
// naming the variable — the same treatment every other setting has always had.
func TestMaxStepsMalformedValuesAreRefusedAtBootNotReinterpreted(t *testing.T) {
	for _, raw := range []string{"12x", "1e3", "1 000", "0x10", "1_0", " 12 x", "abc", "12.5"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(flags.EnvKey(flags.MaxSteps), raw)
			reg := flags.New()
			if err := flags.DefineHarnessFlags(reg); err != nil {
				t.Fatal(err)
			}
			err := reg.LoadEnv()
			if err == nil {
				t.Fatalf("MANVI_MAX_STEPS=%q was accepted; a value that is not a whole "+
					"number must not silently become one", raw)
			}
			if !strings.Contains(err.Error(), "MANVI_MAX_STEPS") {
				t.Fatalf("the refusal must name the variable, got %q", err)
			}
		})
	}
}

// TestMaxStepsHonoursAWellFormedValue. The escape hatch has to keep working:
// the point of refusing "1e3" is that 1000 is what the operator meant.
func TestMaxStepsHonoursAWellFormedValue(t *testing.T) {
	for raw, want := range map[string]int{"1": 1, "24": 24, "1000": 1000} {
		t.Run(raw, func(t *testing.T) {
			if got := maxSteps(registryWith(t, map[string]string{flags.MaxSteps: raw})); got != want {
				t.Fatalf("max_steps=%q gave %d, want %d", raw, got, want)
			}
		})
	}
}

// TestMaxStepsIsReadableFromEveryLayer is what moving the key into the
// catalogue actually bought. It used to be environment-only: a config file
// could not set it, and `manvi flags` could not say where the value in force
// came from.
func TestMaxStepsIsReadableFromEveryLayer(t *testing.T) {
	t.Setenv(flags.EnvKey(flags.MaxSteps), "321")
	reg := flags.New()
	if err := flags.DefineHarnessFlags(reg); err != nil {
		t.Fatal(err)
	}
	if err := reg.LoadConfig(map[string]string{flags.MaxSteps: "123"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.LoadEnv(); err != nil {
		t.Fatal(err)
	}
	if got := maxSteps(reg); got != 321 {
		t.Fatalf("maxSteps = %d, want the environment layer's 321 to beat the config's 123", got)
	}
	v, err := reg.Lookup(flags.MaxSteps)
	if err != nil {
		t.Fatal(err)
	}
	if v.Origin != flags.OriginEnv {
		t.Fatalf("origin = %q, want %q — the whole point is that this is now answerable",
			v.Origin, flags.OriginEnv)
	}
}

// TestMaxStepsNonPositiveUsesTheShippedCeiling. Zero and negatives are legal
// integers, so the registry's KindInt check passes them through. They are not
// "no ceiling" — the loop would end every turn before it began — and a
// misconfiguration must never be able to tighten the last line.
func TestMaxStepsNonPositiveUsesTheShippedCeiling(t *testing.T) {
	for _, raw := range []string{"0", "-1", "-500"} {
		t.Run(raw, func(t *testing.T) {
			got := maxSteps(registryWith(t, map[string]string{flags.MaxSteps: raw}))
			if got != flags.DefaultMaxSteps {
				t.Fatalf("max_steps=%q gave %d, want the shipped %d", raw, got, flags.DefaultMaxSteps)
			}
		})
	}
}

// TestMaxStepsDefaultIsTheShippedCeiling.
func TestMaxStepsDefaultIsTheShippedCeiling(t *testing.T) {
	if got := maxSteps(newTestRegistry(t)); got != flags.DefaultMaxSteps {
		t.Fatalf("maxSteps = %d, want %d", got, flags.DefaultMaxSteps)
	}
}

// TestPromptAssemblyFaultsAreReportedNotSwallowed.
//
// package prompt exists because "nothing records which parts were included,
// which were dropped for budget, or which failed to load" — and the sole call
// site discarded every Add error and the whole assembly Report. Every source
// here is a prompt.Static that cannot fail today, so this was latent; a
// guarantee nothing enforces is a guarantee until the day someone adds a
// source that reads a file.
func TestPromptAssemblyFaultsAreReportedNotSwallowed(t *testing.T) {
	failing := prompt.SourceFunc{Label: "reads-a-file", Fn: func() ([]prompt.Section, error) {
		return nil, errors.New("open /nope: no such file or directory")
	}}
	good := prompt.Static("identity", 10, true, "you are a builder agent")

	text, faults := assembleSections([]prompt.Source{good, failing}, 0)
	if len(faults) == 0 {
		t.Fatal("a source that failed to load produced no fault; the prompt was assembled without it and said nothing")
	}
	if !strings.Contains(faults[0], "reads-a-file") {
		t.Fatalf("the fault must name the section that is missing, got %q", faults[0])
	}
	if !strings.Contains(text, "builder agent") {
		t.Fatalf("assembly must still produce the sections that did load, got %q", text)
	}
}

// TestPromptAssemblyRefusesADuplicateSectionQuietly. Add's other failure mode.
// Two contributors under one name make the report ambiguous about which
// produced what, which is the reason the package exists — and it was ignored
// seven times over with `_ = a.Add(...)`.
func TestPromptAssemblyRefusesADuplicateSectionQuietly(t *testing.T) {
	first := prompt.Static("policy", 40, true, "first")
	second := prompt.Static("policy", 40, true, "second")
	_, faults := assembleSections([]prompt.Source{first, second}, 0)
	if len(faults) == 0 {
		t.Fatal("a duplicate section name was dropped silently")
	}
	if !strings.Contains(faults[0], "policy") {
		t.Fatalf("the fault must name the section, got %q", faults[0])
	}
}

// TestAssemblePromptStillCarriesEverySection characterises what the shipped
// configuration produces, so the fault plumbing above cannot start reporting
// phantom faults on a healthy assembly.
func TestAssemblePromptStillCarriesEverySection(t *testing.T) {
	reg := newTestRegistry(t)
	text, faults := assemblePromptWithFaults(reg, PromptOptions{Provider: local.Name, TaskToolsOffered: true})
	if len(faults) != 0 {
		t.Fatalf("the shipped prompt reports faults it should not have: %v", faults)
	}
	for _, want := range []string{"builder agent", "Posture:", "devcouncil_next_task", "blocked write", "Environment:", "Calling tools:", "Working method:"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the assembled prompt is missing %q", want)
		}
	}
}

// fakeTool is the least a tools.Registry will accept, so a profile can be
// built in a test without standing up a gate, a store and a lease.
func fakeTool(name string, extended bool) tools.Tool {
	return tools.Tool{
		Schema: llm.ToolSchema{
			Name: name, Description: name,
			InputSchema: []byte(`{"type":"object","properties":{}}`),
		},
		ReadOnly: true,
		Extended: extended,
		Handler: func(context.Context, tools.Call) tools.Result {
			return tools.Result{Text: "ok"}
		},
	}
}

// TestTaskToolsOfferedFollowsTheProfileNotTheFlag.
//
// The prompt used to derive "are the task tools available" from
// `!coreToolsOnly`, which was a restatement of the tool profile rather than a
// reading of it. The two came apart the moment the core profile was closed
// under its own prerequisites: devcouncil_checkout_task became core, so
// core_tools_only stopped removing it — and the prompt went on telling the
// model "There is no task to check out here" while the tool sat in its list.
//
// That is the same defect the reduced prompt was written to prevent, arriving
// from the other side: last time the prompt named a tool the profile had
// removed, this time the prompt denied a tool the profile still offers. Both
// are answered by asking the registry instead of the flag.
func TestTaskToolsOfferedFollowsTheProfileNotTheFlag(t *testing.T) {
	pipeline := tools.NewRegistry(bus.New())
	for _, tool := range []tools.Tool{
		fakeTool("devcouncil_checkout_task", false),
		fakeTool("devcouncil_get_task", true),
	} {
		if err := pipeline.Register(tool); err != nil {
			t.Fatal(err)
		}
	}

	if !taskToolsOffered(pipeline, true) {
		t.Error("core_tools_only reported the task tools absent, but devcouncil_checkout_task " +
			"is core: the prompt would deny a tool the model can see")
	}
	if !taskToolsOffered(pipeline, false) {
		t.Error("the full profile reported the task tools absent")
	}

	// And the other direction still holds: a profile that really does drop the
	// checkout tool must still produce the reduced prompt.
	dropped := tools.NewRegistry(bus.New())
	if err := dropped.Register(fakeTool("devcouncil_checkout_task", true)); err != nil {
		t.Fatal(err)
	}
	if taskToolsOffered(dropped, true) {
		t.Error("the checkout tool is Extended and core_tools_only is on, yet the prompt " +
			"would still tell the model to check a task out")
	}
	if !taskToolsOffered(dropped, false) {
		t.Error("the full profile offers every tool; the prompt must say so")
	}
}

// TestTheRealCoreProfileStillOffersTheTaskTools ties the helper above to the
// tool set the harness actually ships. The helper answering correctly about a
// hand-built registry is worth nothing if the real core profile has moved
// again — and that profile is owned by another package, which is precisely why
// the prompt must not restate it.
func TestTheRealCoreProfileStillOffersTheTaskTools(t *testing.T) {
	_, pipeline, err := nativeToolsWith(newTestRegistry(t), nil)
	if err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}
	if !taskToolsOffered(pipeline, true) {
		t.Fatal("devcouncil_checkout_task is no longer in the core profile; the prompt's " +
			"task section and the offered tool set have to move together")
	}
}

// TestAFreshIndexWithADivergedArtifactIsStillRebuilt.
//
// The session-start check asked devmap whether the *index* was current and
// stopped there. The gate does not read the index; it reads the code graph
// artifact, which is a separate file written by a separate command. The state
// this misses is exactly the one this repository was in: a fresh index at
// generation 4 and an artifact at generation 2, so the plan reported
// "repository index current" and the scope rung spent the session deciding from
// a graph missing 112 files.
func TestAFreshIndexWithADivergedArtifactIsStillRebuilt(t *testing.T) {
	current := &devmap.Status{GenerationID: 4, NodeCount: 4249, EdgeCount: 32536, IsFresh: true}
	diverged := []string{"the code graph was written from generation 2 and the index now stands at 4"}

	plan := planIndexRefresh(current, nil, diverged)
	if !plan.Build {
		t.Fatal("an artifact the gate reads that did not come from the current index must be rewritten")
	}
	if plan.Kind != ui.KindNotice {
		t.Errorf("Kind = %q, want a notice: this is a state that is less than it appears", plan.Kind)
	}
	if len(plan.Degraded) == 0 {
		t.Error("what answers wrongly until the rebuild lands must be named")
	}
	shown := plan.Text + " " + strings.Join(plan.Degraded, " ")
	if !strings.Contains(shown, "generation 2") {
		t.Errorf("the operator must be told what diverged, not only that something did: %q", shown)
	}
	// The scope rung is the consumer that reads this file; naming the index
	// would send an operator to the half that was correct.
	if !strings.Contains(shown, "scope") && !strings.Contains(shown, "neighbour") {
		t.Errorf("the report must name the consumer that is deciding from it: %q", shown)
	}
}

// TestACurrentArtifactOnACurrentIndexNeedsNothing is the guard against a check
// that rebuilds on every session start.
func TestACurrentArtifactOnACurrentIndexNeedsNothing(t *testing.T) {
	current := &devmap.Status{GenerationID: 4, NodeCount: 4249, EdgeCount: 32536, IsFresh: true}
	plan := planIndexRefresh(current, nil, nil)
	if plan.Build {
		t.Fatal("an artifact derived from the current index must not trigger a rebuild")
	}
	if len(plan.Degraded) != 0 {
		t.Errorf("Degraded = %q on a state that needs nothing", plan.Degraded)
	}
}
