package main

import (
	"bytes"
	"strings"
	"testing"

	"manvi/flags"
)

// runFlags drives the real command dispatch, so what these tests assert is what
// an operator gets — including the boot sequence that seals the registry.
func runFlags(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out, notes bytes.Buffer
	err := run(&out, &notes, args)
	return out.String(), err
}

// TestFlagsSetMovesASetting is the whole point of the seam. The flag table's
// last column says "human" for forty-six settings, and until this command
// existed there was no human-authority path to any of them: Registry.Set had
// exactly one production caller, --yolo, at startup. A column that describes a
// capability the binary does not ship is the same defect as a check that could
// not run reporting success.
func TestFlagsSetMovesASetting(t *testing.T) {
	t.Chdir(t.TempDir())

	out, err := runFlags(t, "flags", "set", flags.PolicyFileMode, flags.ModeAdvisory)
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	for _, want := range []string{flags.PolicyFileMode, "enforce", "advisory", "override"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not mention %q:\n%s", want, out)
		}
	}
}

// TestFlagsSetReportsTheDirectionOfASafetyMove keeps the warning meaningful. A
// safety flag returned to its safest value that still printed "this run is no
// longer strict" would teach the operator to read the warning as noise.
func TestFlagsSetReportsTheDirectionOfASafetyMove(t *testing.T) {
	t.Chdir(t.TempDir())

	relaxed, err := runFlags(t, "flags", "set", flags.HarnessPosture, flags.PostureYolo)
	if err != nil {
		t.Fatalf("relax: %v", err)
	}
	if !strings.Contains(relaxed, "no longer at its safest value") {
		t.Errorf("relaxing a safety flag did not warn:\n%s", relaxed)
	}

	tightened, err := runFlags(t, "flags", "set", flags.HarnessPosture, flags.PostureStrict)
	if err != nil {
		t.Fatalf("tighten: %v", err)
	}
	if strings.Contains(tightened, "no longer at its safest value") {
		t.Errorf("tightening a safety flag warned as though it had been relaxed:\n%s", tightened)
	}
	if !strings.Contains(tightened, "safest value") {
		t.Errorf("tightening did not say the flag is now at its safest value:\n%s", tightened)
	}
}

// TestFlagsSetRefusesStartupFlagsAfterBoot is the seal, asserted through the
// command an operator would use to break it.
//
// Registry.Seal had no production caller. Startup mutability — "fixed once Boot
// completes" — was therefore enforced by nothing, and two of the four startup
// flags are safety flags. Switching policy.hard_rules.enabled off mid-run is
// exactly the move the design forbids, and before the seal it would have
// succeeded and reported success.
func TestFlagsSetRefusesStartupFlagsAfterBoot(t *testing.T) {
	t.Chdir(t.TempDir())

	for _, key := range []string{flags.PolicyHardRules, flags.LogModelVisibleAssert} {
		out, err := runFlags(t, "flags", "set", key, "false")
		if err == nil {
			t.Fatalf("%s was moved after boot; output:\n%s", key, out)
		}
		if !strings.Contains(err.Error(), "startup-only") || !strings.Contains(err.Error(), "sealed") {
			t.Errorf("%s: refusal = %v, want it to name the seal", key, err)
		}
	}
}

// TestFlagsSetRefusesBadInput covers the ways a set can be wrong. Each is an
// error rather than a best guess: a mistyped key set to a plausible value is
// indistinguishable, afterwards, from a key that was never set.
func TestFlagsSetRefusesBadInput(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no arguments", []string{"flags", "set"}, "usage"},
		{"key with no value", []string{"flags", "set", flags.PolicyFileMode}, "usage"},
		{"unknown key", []string{"flags", "set", "policy.file.mod", "off"}, "unknown setting"},
		{"illegal enum", []string{"flags", "set", flags.PolicyFileMode, "advisery"}, "expected one of"},
		{"illegal bool", []string{"flags", "set", flags.GrantsEnabled, "yesnt"}, "bool"},
		{"illegal duration", []string{"flags", "set", flags.GrantsAgentMaxTTL, "soon"}, "duration"},
		{"illegal int", []string{"flags", "set", flags.MaxSteps, "many"}, "int"},
		{"extra values", []string{"flags", "set", flags.PolicyFileMode, "off", "please"}, "exactly one value"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			out, err := runFlags(t, c.args...)
			if err == nil {
				t.Fatalf("accepted %v; output:\n%s", c.args, out)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("refusal = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

// TestFlagsSetSuggestsANeighbourWhenTheKeyIsMistyped. The suggestion decorates
// the refusal; it never becomes the thing that is set.
func TestFlagsSetSuggestsANeighbourWhenTheKeyIsMistyped(t *testing.T) {
	t.Chdir(t.TempDir())
	_, err := runFlags(t, "flags", "set", "policy.file.mod", "off")
	if err == nil {
		t.Fatal("a mistyped key was accepted")
	}
	if !strings.Contains(err.Error(), flags.PolicyFileMode) {
		t.Errorf("refusal = %v, want it to suggest %s", err, flags.PolicyFileMode)
	}

	out, err := runFlags(t, "flags", "--all")
	if err != nil {
		t.Fatal(err)
	}
	// The refused set must not have landed anywhere.
	if !strings.Contains(out, flags.PolicyFileMode+" ") || strings.Contains(out, "override") {
		t.Errorf("a refused set left state behind:\n%s", out)
	}
}

// TestFlagsSetSaysHowToMakeItDurable. The override layer dies with the process,
// so from a shell this command changes nothing that outlives the exit. An
// operator who does not know that believes the setting is applied.
func TestFlagsSetSaysHowToMakeItDurable(t *testing.T) {
	t.Chdir(t.TempDir())
	out, err := runFlags(t, "flags", "set", flags.PolicyFileMode, flags.ModeAdvisory)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "as long as this process") {
		t.Errorf("the report does not say the override is process-scoped:\n%s", out)
	}
	if !strings.Contains(out, flags.EnvKey(flags.PolicyFileMode)) {
		t.Errorf("the report does not name the environment variable that would be durable:\n%s", out)
	}
}

// TestFlagsSetReportsHowFarTheChangeReaches. A registry that accepts a change
// and prints the new value has done half a job; the other half is whether the
// code governed by the flag will read it.
//
// And the answer depends on who ran the command. A shell invocation is a
// process about to exit — nothing in it cached the old value, so nothing
// reloads, and saying otherwise would be the same untruth this command exists
// to take out of the flag table.
func TestFlagsSetReportsHowFarTheChangeReaches(t *testing.T) {
	cases := []struct {
		key   string
		value string
		want  string
	}{
		{flags.MCPEnabled, "false", "sessions opened from now on"},
		{flags.GrantsAgentEnabled, "false", "reloaded before the next turn"},
		{flags.LLMDefaultProvider, "local", "reloaded before the next turn"},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			var session, shell bytes.Buffer
			reg := newTestRegistry(t)
			if _, err := setFlag(&session, reg, []string{c.key, c.value}, surfaceSession); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(session.String(), c.want) {
				t.Errorf("the session report does not mention %q:\n%s", c.want, session.String())
			}
			if _, err := setFlag(&shell, newTestRegistry(t), []string{c.key, c.value}, surfaceShell); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(shell.String(), c.want) {
				t.Errorf("a shell invocation claimed a reload nothing did:\n%s", shell.String())
			}
		})
	}

	// A live flag claims no reload on either surface, because claiming one that
	// did not happen is the failure this whole classification exists to prevent.
	t.Run("live flag claims nothing", func(t *testing.T) {
		var out bytes.Buffer
		if _, err := setFlag(&out, newTestRegistry(t), []string{flags.PolicyFileMode, flags.ModeAdvisory}, surfaceSession); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "reloaded") || strings.Contains(out.String(), "sessions opened") {
			t.Errorf("a live flag reported a reload it did not need:\n%s", out.String())
		}
	})
}

// TestFlagsReportExplainsTheMutabilityColumn. The column was the original
// complaint: it says "human" and nothing told the reader what to do with that.
func TestFlagsReportExplainsTheMutabilityColumn(t *testing.T) {
	t.Chdir(t.TempDir())
	out, err := runFlags(t, "flags")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "flags set KEY VALUE") {
		t.Errorf("the flag table does not say how to move a setting:\n%s", out)
	}
}

// TestReloadPlanMatchesTheFlagCatalogue holds two tables in step.
//
// flags.ReachOf classifies how far a change to a setting gets; reloadPlanFor
// says which of this harness's snapshots a change invalidates. They are written
// in different packages for good reason — the registry knows nothing about
// gates and providers — but they describe the same fact, and if they drift the
// harness either reloads nothing while promising it did, or promises nothing
// while a stale copy goes on deciding.
func TestReloadPlanMatchesTheFlagCatalogue(t *testing.T) {
	reg, err := flags.NewHarnessRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	sawReload, sawLive := false, false
	for _, key := range reg.Keys() {
		def, _ := reg.Def(key)
		reach := flags.ReachOf(def)
		plan := reloadPlanFor(key)
		switch reach {
		case flags.ReachReload:
			sawReload = true
			if !plan.any() {
				t.Errorf("%s is classified %q but nothing here reloads for it", key, reach)
			}
		case flags.ReachLive, flags.ReachNewSession, flags.ReachBoot:
			if reach == flags.ReachLive {
				sawLive = true
			}
			if plan.any() {
				t.Errorf("%s is classified %q but this harness reloads %+v for it — "+
					"either the classification is wrong or the reload is unnecessary", key, reach, plan)
			}
		}
	}
	if !sawReload || !sawLive {
		t.Fatal("the catalogue has no reload-class or no live-class flag; this test asserts nothing")
	}
}

// TestFlagsSetIsRefusedWhileAnotherSessionIsMidTurn. The flags package states
// the rule in its own doc comment — safety flags move "only outside a running
// turn" — and had no way to enforce it. The registry is one object shared by
// every session, so a setting moved while another session is mid-turn changes
// the rules that turn is being judged by, halfway through.
func TestFlagsSetIsRefusedWhileAnotherSessionIsMidTurn(t *testing.T) {
	h := &harnessHost{reg: newTestRegistry(t), busyTurns: func(string) []string { return []string{"S2"} }}

	err := h.refuseFlagMoveDuringATurn("S1", []string{"set", flags.PolicyFileMode, flags.ModeOff})
	if err == nil {
		t.Fatal("a setting was moved while another session was mid-turn")
	}
	if !strings.Contains(err.Error(), "S2") {
		t.Errorf("refusal = %v, want it to name the busy session", err)
	}

	// Reading is always allowed: a report changes nothing anyone is being
	// judged by, and refusing it would make the busy case unobservable.
	if err := h.refuseFlagMoveDuringATurn("S1", []string{"--all"}); err != nil {
		t.Errorf("reading the flag table during a turn was refused: %v", err)
	}
	if err := h.refuseFlagMoveDuringATurn("S1", nil); err != nil {
		t.Errorf("reading the flag table during a turn was refused: %v", err)
	}

	// The session running this command is not itself an obstacle — a slash
	// command is run as that session's turn, so counting it would refuse every
	// set there has ever been.
	// Busy answers the caller's own exclusion, so a runner with only this
	// session mid-turn reports nothing to this caller.
	alone := &harnessHost{reg: newTestRegistry(t), busyTurns: func(string) []string { return nil }}
	if err := alone.refuseFlagMoveDuringATurn("S1", []string{"set", flags.PolicyFileMode, flags.ModeOff}); err != nil {
		t.Errorf("a set was refused because of the command's own turn: %v", err)
	}
}

// TestFlagsSetDistinguishesANoOpFromAMove. Three outcomes look identical on the
// value line and lead to different next actions: the value moved, only the
// layer it comes from moved, or nothing happened at all. An operator who
// believes they moved something stops looking for why the behaviour did not
// change.
func TestFlagsSetDistinguishesANoOpFromAMove(t *testing.T) {
	t.Chdir(t.TempDir())

	moved, err := runFlags(t, "flags", "set", flags.PolicyFileMode, flags.ModeAdvisory)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(moved, "nothing changed") || strings.Contains(moved, "did not change") {
		t.Errorf("a real move reported as a no-op:\n%s", moved)
	}

	// Same value, different layer: default → override.
	sameValue, err := runFlags(t, "flags", "set", flags.PolicyFileMode, flags.ModeEnforce)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sameValue, "only the layer") {
		t.Errorf("setting a flag to the value it already had did not say so:\n%s", sameValue)
	}
}
