package agents

import (
	"strings"
	"testing"

	"manvi/flags"
)

// registryWith builds a flag registry carrying the given settings.
func registryWith(t *testing.T, values map[string]string) *flags.Registry {
	t.Helper()
	reg := flags.New()
	if err := flags.DefineHarnessFlags(reg); err != nil {
		t.Fatal(err)
	}
	if len(values) > 0 {
		if err := reg.LoadConfig(values); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

// TestLocalSessionsRunSolo is the plane rule.
//
// It fails against the behaviour it replaced, where a local session got the
// catalogue's default depth of two and a fan-out cap of two: delegation was
// narrowed and never switched off, so a local model could still activate the
// sub-agent group and dispatch a team onto the one GPU it was already using.
func TestLocalSessionsRunSolo(t *testing.T) {
	reg := registryWith(t, map[string]string{flags.LLMDefaultProvider: "local"})
	got := ResolveBounds(reg)
	if got.MaxDepth != 0 {
		t.Fatalf("MaxDepth = %d on a local session, want 0 — narrowing delegation is not "+
			"switching it off, and one child at a time is still a team", got.MaxDepth)
	}
	if got.DepthReason == "" {
		t.Fatal("a depth this harness imposed must say why, or the refusal blames the flag")
	}
	if strings.Contains(got.DepthReason, flags.AgentsMaxSpawnDepth) {
		t.Fatalf("DepthReason = %q; it must name the cause, not the setting that did not decide it",
			got.DepthReason)
	}
}

// The hardware bound wins over the setting. An operator who raised the depth on
// a local session gets a refusal that explains the machine, not one that
// misreports their own configuration back at them.
func TestLocalSoloOverridesAnExplicitDepth(t *testing.T) {
	reg := registryWith(t, map[string]string{
		flags.LLMDefaultProvider:  "local",
		flags.AgentsMaxSpawnDepth: "2",
	})
	if got := ResolveBounds(reg); got.MaxDepth != 0 {
		t.Fatalf("MaxDepth = %d, want 0: the plane rule is not advisory", got.MaxDepth)
	}
}

// The rule is about where the weights are, not about how big the window is.
// Every non-local provider keeps its configured depth.
func TestNonLocalProvidersKeepTheirDepth(t *testing.T) {
	for _, provider := range []string{"anthropic", "gemini", "xai", "openaicompat"} {
		t.Run(provider, func(t *testing.T) {
			reg := registryWith(t, map[string]string{
				flags.LLMDefaultProvider:  provider,
				flags.AgentsMaxSpawnDepth: "1",
			})
			got := ResolveBounds(reg)
			if got.MaxDepth != 1 {
				t.Fatalf("MaxDepth = %d for %s, want the configured 1", got.MaxDepth, provider)
			}
			if got.DepthReason != "" {
				t.Fatalf("DepthReason = %q for %s, want none: nothing overrode the setting",
					got.DepthReason, provider)
			}
		})
	}
}

// A depth of zero that the operator asked for is still reported as their
// setting, not dressed up as a hardware decision.
func TestExplicitZeroDepthIsNotAttributedToTheProvider(t *testing.T) {
	reg := registryWith(t, map[string]string{
		flags.LLMDefaultProvider:  "anthropic",
		flags.AgentsMaxSpawnDepth: "0",
	})
	got := ResolveBounds(reg)
	if got.MaxDepth != 0 {
		t.Fatalf("MaxDepth = %d, want the configured 0", got.MaxDepth)
	}
	if got.DepthReason != "" {
		t.Fatalf("DepthReason = %q, want none — the operator's own setting decided this",
			got.DepthReason)
	}
}

// An unreadable registry must not widen anything. The fallback is the
// catalogue's default, and a local session that could not read its settings
// still does not get to delegate on the strength of a failed read.
func TestUnreadableRegistryDoesNotWiden(t *testing.T) {
	if got := ResolveBounds(nil); got.MaxDepth > flags.DefaultAgentsMaxSpawnDepth ||
		got.MaxFanout > flags.DefaultAgentsMaxFanout {
		t.Fatalf("ResolveBounds(nil) = %+v, wider than the catalogue defaults", got)
	}
}

// The fan-out bound and the placement decision are made in different packages
// at different times, and until FanoutFor existed they never met: a frontier
// session resolved the frontier width and then placed its children on the one
// device the local cap exists to protect.
func TestFanoutFollowsWhereTheChildrenActuallyRun(t *testing.T) {
	reg := registryWith(t, map[string]string{
		flags.LLMDefaultProvider: "anthropic",
		flags.AgentsMaxFanout:    "8",
	})
	bounds := ResolveBounds(reg)
	if bounds.MaxFanout != 8 {
		t.Fatalf("a frontier session resolved MaxFanout=%d, want 8", bounds.MaxFanout)
	}

	// One local child among seven inheriting ones still narrows the pool: the
	// pool has a single concurrency limit, and the device does not care that
	// most of the tasks were going somewhere else.
	got, reason := FanoutFor(bounds, []string{"inherit", "local/qwen3.8:27b", "inherit"})
	if got != 2 {
		t.Fatalf("fan-out = %d with a local child, want the local width of 2 — eight "+
			"concurrent children on one GPU is what the local cap exists to prevent", got)
	}
	if reason == "" {
		t.Fatal("the fan-out was narrowed silently; an operator who set 8 and got 2 " +
			"cannot tell which setting decided it")
	}
	if !strings.Contains(reason, "local") {
		t.Fatalf("reason = %q, want it to name the cause", reason)
	}
}

// A fan-out that stays on the session's provider is not narrowed twice.
// ResolveBounds already accounted for it, and reporting a narrowing that did
// not happen is its own kind of wrong answer.
func TestFanoutIsNotNarrowedForInheritingChildren(t *testing.T) {
	bounds := Bounds{MaxFanout: 8, MaxDepth: 1}
	for _, specs := range [][]string{
		nil,
		{"inherit", "inherit"},
		{"", ""},
		{"anthropic/claude", "gemini/pro"},
		{"claude-opus-5"},
	} {
		got, reason := FanoutFor(bounds, specs)
		if got != 8 {
			t.Errorf("FanoutFor(%v) = %d, want the resolved 8", specs, got)
		}
		if reason != "" {
			t.Errorf("FanoutFor(%v) reported a narrowing that did not happen: %q", specs, reason)
		}
	}
}

// A local session is already capped at 2, so a local child narrows nothing
// further and must not claim to.
func TestFanoutOnALocalSessionReportsNoFurtherNarrowing(t *testing.T) {
	reg := registryWith(t, map[string]string{flags.LLMDefaultProvider: "local"})
	bounds := ResolveBounds(reg)
	got, reason := FanoutFor(bounds, []string{"local/model"})
	if got != bounds.MaxFanout {
		t.Fatalf("fan-out = %d, want the already-narrowed %d", got, bounds.MaxFanout)
	}
	if reason != "" {
		t.Fatalf("reported a second narrowing: %q", reason)
	}
}

// The spec reader has to be exact. A provider whose name merely starts with
// "local" is a different provider, and treating it as the local plane would
// throttle a fan-out for no reason.
func TestPlacesLocallyIsExact(t *testing.T) {
	local := []string{"local", "LOCAL", " local ", "local/qwen", "Local/Qwen3"}
	for _, spec := range local {
		if !PlacesLocally(spec) {
			t.Errorf("PlacesLocally(%q) = false, want true", spec)
		}
	}
	notLocal := []string{
		"", "inherit", "INHERIT",
		"localhost", "local-gpu", "locality/model",
		"anthropic", "anthropic/claude",
		"/local", // a spec with no provider half
	}
	for _, spec := range notLocal {
		if PlacesLocally(spec) {
			t.Errorf("PlacesLocally(%q) = true, want false", spec)
		}
	}
}
