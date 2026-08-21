package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"manvi/flags"
	"manvi/llm/openaicompat"
)

// newTestRegistry builds a harness registry at its shipped defaults.
//
// It lives here because prompt_test.go referenced it without it existing
// anywhere, which left every test in this package failing to compile — and a
// package that does not build reports as a test failure rather than as the
// missing helper it is.
func newTestRegistry(t *testing.T) *flags.Registry {
	t.Helper()
	return registryWith(t, nil)
}

func registryWith(t *testing.T, settings map[string]string) *flags.Registry {
	t.Helper()
	reg := flags.New()
	if err := flags.DefineHarnessFlags(reg); err != nil {
		t.Fatal(err)
	}
	if settings != nil {
		if err := reg.LoadConfig(settings); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

// TestStallTimeoutOffActuallyDisablesTheWatchdog is the regression test for a
// setting that documented the opposite of what it did.
//
// The flag said "Empty or 0 disables it". The generic duration parser turned
// both empty and "0" into zero, and the adapter reads zero as "use the default"
// — so the documented way to switch the watchdog off armed it for five minutes,
// while the value that genuinely disables it (a negative, in the adapter's own
// vocabulary) was rejected outright by the parser. The escape hatch was
// unreachable from configuration, and the setting lied about it.
func TestStallTimeoutOffActuallyDisablesTheWatchdog(t *testing.T) {
	for _, raw := range []string{"0", "0s", "off", "OFF", "none"} {
		t.Run(raw, func(t *testing.T) {
			cfg, err := localConfig(registryWith(t, map[string]string{
				flags.LLMLocalStallTimeout: raw,
			}))
			if err != nil {
				t.Fatalf("localConfig: %v", err)
			}
			if cfg.StallTimeout >= 0 {
				t.Fatalf("StallTimeout = %v; %q must disable the watchdog, and the adapter "+
					"spells disabled as a negative", cfg.StallTimeout, raw)
			}
		})
	}
}

// TestStallTimeoutUnsetArmsTheDefault. An unconfigured harness must still not
// wait forever on a wedged server, so unset is the armed direction.
func TestStallTimeoutUnsetArmsTheDefault(t *testing.T) {
	cfg, err := localConfig(registryWith(t, map[string]string{
		flags.LLMLocalStallTimeout: "",
	}))
	if err != nil {
		t.Fatalf("localConfig: %v", err)
	}
	if cfg.StallTimeout != 0 {
		t.Fatalf("StallTimeout = %v; unset must defer to the adapter default", cfg.StallTimeout)
	}
	// And the adapter's default must actually be a bound, not "forever".
	if openaicompat.DefaultStallTimeout <= 0 {
		t.Fatalf("DefaultStallTimeout = %v; unset must not mean unbounded",
			openaicompat.DefaultStallTimeout)
	}
}

// TestStallTimeoutExplicitDurationSurvives.
func TestStallTimeoutExplicitDurationSurvives(t *testing.T) {
	cfg, err := localConfig(registryWith(t, map[string]string{
		flags.LLMLocalStallTimeout: "90s",
	}))
	if err != nil {
		t.Fatalf("localConfig: %v", err)
	}
	if cfg.StallTimeout != 90*time.Second {
		t.Fatalf("StallTimeout = %v, want 90s", cfg.StallTimeout)
	}
}

// TestStallTimeoutNegativeIsRefusedAndNamesTheWayToDisable. The adapter uses a
// negative internally, but an operator typing one is guessing at an internal
// contract; the refusal must point at the supported spelling.
func TestStallTimeoutNegativeIsRefusedAndNamesTheWayToDisable(t *testing.T) {
	_, err := localConfig(registryWith(t, map[string]string{
		flags.LLMLocalStallTimeout: "-5m",
	}))
	if err == nil {
		t.Fatal("a negative stall timeout must be refused")
	}
	if got := err.Error(); !strings.Contains(got, "use 0 to disable") {
		t.Fatalf("the refusal must name the supported spelling, got %q", got)
	}
}

// TestStallTimeoutGarbageIsRefusedNotIgnored. A setting that does not take
// effect and does not complain is worse than one that refuses.
func TestStallTimeoutGarbageIsRefusedNotIgnored(t *testing.T) {
	_, err := localConfig(registryWith(t, map[string]string{
		flags.LLMLocalStallTimeout: "5 minutes",
	}))
	if err == nil {
		t.Fatal("an unparseable duration must be refused, not silently treated as unset")
	}
}

// TestSamplingValueThatCannotBeEncodedIsRefused is the regression test for a
// misconfiguration that reported itself as a network fault.
//
// strconv.ParseFloat accepts "NaN", "Inf", "+Inf", "-Inf" and "infinity", so
// every one of these passed validation, was stored in local.Config, and then
// failed at json.Marshal on every single request for the life of the run —
// surfacing as "local: transport failure: encoding request: json: unsupported
// value: NaN". That message sends an operator to debug their server or their
// network. The setting they mistyped is never mentioned.
func TestSamplingValueThatCannotBeEncodedIsRefused(t *testing.T) {
	keys := []string{
		flags.LLMLocalTemperature,
		flags.LLMLocalTopP,
		flags.LLMLocalMinP,
		flags.LLMLocalRepetitionPenalty,
		flags.LLMLocalPresencePenalty,
		flags.LLMLocalFrequencyPenalty,
	}
	values := []string{"NaN", "nan", "Inf", "+Inf", "-Inf", "infinity"}
	for _, key := range keys {
		for _, raw := range values {
			t.Run(key+"="+raw, func(t *testing.T) {
				_, err := localConfig(registryWith(t, map[string]string{key: raw}))
				if err == nil {
					t.Fatalf("%s=%q was accepted; it cannot be encoded into a request "+
						"and would fail as a transport error on every turn", key, raw)
				}
				if !strings.Contains(err.Error(), flags.EnvKey(key)) {
					t.Fatalf("the refusal must name the setting the operator has to fix, got %q", err)
				}
			})
		}
	}
}

// TestSamplingValueMustBePlainDecimal.
//
// strconv.ParseFloat accepts Go literal syntax that no operator writing a
// config file intends: "0x1p-2" silently becomes 0.25 and "1_0" silently
// becomes 10. A coding agent running at temperature 10 is unusable, and
// nothing anywhere would have said so — the value parsed, so it was applied.
func TestSamplingValueMustBePlainDecimal(t *testing.T) {
	for _, raw := range []string{"0x1p-2", "1_0", "0x1P+3", "1_000"} {
		t.Run(raw, func(t *testing.T) {
			_, err := localConfig(registryWith(t, map[string]string{
				flags.LLMLocalTemperature: raw,
			}))
			if err == nil {
				t.Fatalf("temperature=%q was accepted as a Go literal; only plain "+
					"decimal is what an operator means", raw)
			}
		})
	}
}

// TestSamplingValueOutOfRangeIsRefused.
//
// localConfig already refuses max_output_tokens above the context window, and
// says why: the error names the two settings that disagree instead of arriving
// as a refused request. The sampling knobs had no such check, so top_p=2.5 and
// top_k=-5 went out on the wire. mlx_lm's server answers those with a 400
// (validate_model_parameters), while mlx_vlm and Ollama accept them and
// silently sample as if the setting were not there — which is worse, because
// the operator's declaration has no effect and nothing reports it.
func TestSamplingValueOutOfRangeIsRefused(t *testing.T) {
	cases := []struct{ key, raw string }{
		{flags.LLMLocalTemperature, "-1"},
		{flags.LLMLocalTemperature, "999"},
		{flags.LLMLocalTemperature, "10"},
		{flags.LLMLocalTopP, "2.5"},
		{flags.LLMLocalTopP, "-0.5"},
		{flags.LLMLocalMinP, "1.5"},
		{flags.LLMLocalMinP, "-0.1"},
		{flags.LLMLocalRepetitionPenalty, "-1"},
		{flags.LLMLocalPresencePenalty, "-3"},
		{flags.LLMLocalPresencePenalty, "2.5"},
		{flags.LLMLocalFrequencyPenalty, "-2.1"},
		{flags.LLMLocalFrequencyPenalty, "3"},
		{flags.LLMLocalTopK, "-5"},
	}
	for _, tc := range cases {
		t.Run(tc.key+"="+tc.raw, func(t *testing.T) {
			_, err := localConfig(registryWith(t, map[string]string{tc.key: tc.raw}))
			if err == nil {
				t.Fatalf("%s=%q is outside the range local servers accept and was "+
					"applied anyway", tc.key, tc.raw)
			}
			got := err.Error()
			if !strings.Contains(got, flags.EnvKey(tc.key)) {
				t.Fatalf("the refusal must name the setting, got %q", got)
			}
			// The rationale localConfig already states for the token caps: an
			// error that does not say what would have been acceptable leaves
			// the operator guessing at the next value to try.
			if !strings.Contains(got, "must be") {
				t.Fatalf("the refusal must state the accepted range, got %q", got)
			}
		})
	}
}

// TestSamplingValuesAtTheEdgesAreAccepted guards the other direction. A range
// check that is too tight is the same defect wearing the opposite sign: it
// refuses a configuration the server would have honoured.
func TestSamplingValuesAtTheEdgesAreAccepted(t *testing.T) {
	cases := []struct{ key, raw string }{
		{flags.LLMLocalTemperature, "0"},
		{flags.LLMLocalTemperature, "2"},
		{flags.LLMLocalTemperature, "1e0"},
		{flags.LLMLocalTopP, "0"},
		{flags.LLMLocalTopP, "1"},
		{flags.LLMLocalMinP, "0.05"},
		{flags.LLMLocalRepetitionPenalty, "1.05"},
		{flags.LLMLocalPresencePenalty, "-2"},
		{flags.LLMLocalPresencePenalty, "2"},
		{flags.LLMLocalFrequencyPenalty, "0"},
		{flags.LLMLocalTopK, "0"},
		{flags.LLMLocalTopK, "20"},
	}
	for _, tc := range cases {
		t.Run(tc.key+"="+tc.raw, func(t *testing.T) {
			if _, err := localConfig(registryWith(t, map[string]string{tc.key: tc.raw})); err != nil {
				t.Fatalf("%s=%q is inside the accepted range and was refused: %v", tc.key, tc.raw, err)
			}
		})
	}
}

// TestShippedSamplingDefaultsProduceAnEncodableRequest ties the catalogue to
// the wire. Every default must survive the same journey a set value does,
// because a default that cannot be encoded is a harness that cannot start.
func TestShippedSamplingDefaultsProduceAnEncodableRequest(t *testing.T) {
	cfg, err := localConfig(newTestRegistry(t))
	if err != nil {
		t.Fatalf("the shipped defaults do not assemble: %v", err)
	}
	if _, err := json.Marshal(cfg); err != nil {
		t.Fatalf("the shipped defaults do not encode: %v", err)
	}
}
