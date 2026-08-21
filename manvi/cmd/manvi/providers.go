package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"manvi/credentials"
	"manvi/flags"
	"manvi/llm"
	"manvi/llm/anthropic"
	"manvi/llm/gemini"
	"manvi/llm/local"
	"manvi/llm/xai"
)

// This file is the one place an adapter is constructed.
//
// It exists because there were two: `manvi probe` had a switch and the TUI had
// another, and a provider was wired into the harness only when someone
// remembered both. The local adapter is the case that proved it — its settings
// and its credential entry were added, `manvi providers` listed it, and it
// still answered "unknown provider" everywhere that mattered, because neither
// switch had a case for it. Adding headless mode would have made a third.

// buildProvider constructs the adapter for a provider name.
//
// The credential is resolved here rather than inside each adapter so that a
// missing one is reported before a request is assembled, and so that the one
// provider whose credential is optional does not need every caller to know
// that it is.
func buildProvider(name string, reg *flags.Registry, resolver *credentials.Resolver, notes io.Writer) (llm.Provider, error) {
	// The name is checked before the credential, because resolving first makes a
	// typo report itself as a credentials fault. `manvi probe not-a-provider`
	// answered `credentials: no requirement registered for provider
	// "not-a-provider"`, which sends an operator to look for an API key for a
	// provider that does not exist. The first question is whether this harness
	// has such an adapter at all, and the answer names the ones it does.
	if !knownProvider(name) {
		return nil, fmt.Errorf("unknown provider %q (%s)", name, strings.Join(providerNames(), ", "))
	}
	if _, err := resolver.Resolve(name); err != nil {
		return nil, err
	}
	resolve := func() (credentials.Secret, error) { return resolver.Resolve(name) }

	switch name {
	case anthropic.Name:
		return anthropic.New(env("ANTHROPIC_BASE_URL", ""), resolve), nil
	case xai.Name:
		base, _, err := reg.String(flags.LLMXAIBaseURL)
		if err != nil {
			return nil, err
		}
		return xai.New(base, resolve), nil
	case gemini.Name:
		base, _, err := reg.String(flags.LLMGeminiBaseURL)
		if err != nil {
			return nil, err
		}
		return gemini.New(base, resolve), nil
	case local.Name:
		cfg, err := localConfig(reg)
		if err != nil {
			return nil, err
		}
		cfg.BaseURL = resolveLocalEndpoint(reg, cfg.BaseURL, resolve, notes)
		return local.New(cfg, resolve), nil
	default:
		return nil, fmt.Errorf("unknown provider %q (%s)", name, strings.Join(providerNames(), ", "))
	}
}

// localConfig reads the operator's declarations about their own server.
// localConfig assembles the local adapter's declarations from settings.
func localConfig(reg *flags.Registry) (local.Config, error) {
	base, _, err := reg.String(flags.LLMLocalBaseURL)
	if err != nil {
		return local.Config{}, err
	}
	window, _, err := reg.Int(flags.LLMLocalContextWindow)
	if err != nil {
		return local.Config{}, err
	}
	maxOut, _, err := reg.Int(flags.LLMLocalMaxOutputTokens)
	if err != nil {
		return local.Config{}, err
	}
	supportsTools, _, err := reg.Bool(flags.LLMLocalSupportsTools)
	if err != nil {
		return local.Config{}, err
	}
	reasoning, _, err := reg.Bool(flags.LLMLocalSupportsReasoning)
	if err != nil {
		return local.Config{}, err
	}
	assume, _, err := reg.Bool(flags.LLMLocalAssumeModelServed)
	if err != nil {
		return local.Config{}, err
	}
	trustDeclared, _, err := reg.Bool(flags.LLMLocalTrustDeclared)
	if err != nil {
		return local.Config{}, err
	}
	assumePrefill, _, err := reg.Bool(flags.LLMLocalAssumePrefill)
	if err != nil {
		return local.Config{}, err
	}

	// A declared output cap above the declared window is not a cap. Caught here
	// rather than at request time so the error names the two settings that
	// disagree instead of arriving as a refused request.
	if window > 0 && maxOut > window {
		return local.Config{}, fmt.Errorf(
			"%s (%d) exceeds %s (%d): a response cannot be larger than the context it is generated in",
			flags.LLMLocalMaxOutputTokens, maxOut, flags.LLMLocalContextWindow, window)
	}

	// A malformed sampling value is an error, not a shrug.
	//
	// These used to be parsed with the parse error discarded, so "0.7x"
	// silently meant "unset": the operator got the server's default while their
	// setting sat in the config looking applied. A setting that does not take
	// effect and does not complain is worse than one that refuses.
	tempPtr, err := optionalFloat(reg, flags.LLMLocalTemperature, between(0, 2))
	if err != nil {
		return local.Config{}, err
	}
	topPPtr, err := optionalFloat(reg, flags.LLMLocalTopP, between(0, 1))
	if err != nil {
		return local.Config{}, err
	}
	minPPtr, err := optionalFloat(reg, flags.LLMLocalMinP, between(0, 1))
	if err != nil {
		return local.Config{}, err
	}
	repPenPtr, err := optionalFloat(reg, flags.LLMLocalRepetitionPenalty, atLeast(0))
	if err != nil {
		return local.Config{}, err
	}
	presencePtr, err := optionalFloat(reg, flags.LLMLocalPresencePenalty, between(-2, 2))
	if err != nil {
		return local.Config{}, err
	}
	frequencyPtr, err := optionalFloat(reg, flags.LLMLocalFrequencyPenalty, between(-2, 2))
	if err != nil {
		return local.Config{}, err
	}
	topKPtr, err := optionalInt(reg, flags.LLMLocalTopK, atLeast(0))
	if err != nil {
		return local.Config{}, err
	}
	seedPtr, err := optionalInt(reg, flags.LLMLocalSeed, unbounded())
	if err != nil {
		return local.Config{}, err
	}
	stall, err := stallTimeoutSetting(reg)
	if err != nil {
		return local.Config{}, err
	}

	var stopSeqs []string
	stopStr, _, err := reg.String(flags.LLMLocalStop)
	if err != nil {
		return local.Config{}, err
	}
	for _, s := range strings.Split(stopStr, ",") {
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			stopSeqs = append(stopSeqs, trimmed)
		}
	}

	return local.Config{
		BaseURL:                base,
		ContextWindow:          window,
		MaxOutputTokens:        maxOut,
		SupportsTools:          supportsTools,
		SupportsReasoning:      reasoning,
		AssumeModelServed:      assume,
		TrustDeclaredContext:   trustDeclared,
		AssumeReasoningPrefill: assumePrefill,
		Temperature:            tempPtr,
		TopP:                   topPPtr,
		TopK:                   topKPtr,
		MinP:                   minPPtr,
		RepetitionPenalty:      repPenPtr,
		PresencePenalty:        presencePtr,
		FrequencyPenalty:       frequencyPtr,
		Seed:                   seedPtr,
		Stop:                   stopSeqs,
		StallTimeout:           stall,
	}, nil
}

// resolveLocalEndpoint decides which local server this run will talk to.
//
// The shipped llm.local.base_url points at vLLM's port, and most operators
// running a local model are running Ollama on a different one. That default is
// a guess this harness made, not a statement anyone made about their machine,
// so when it is still in force the well-known addresses are scanned and an
// unambiguous answer is used. An address the operator set is used as set: the
// scan does not run at all, because probing alternatives to a value someone
// typed is the harness arguing with its operator about their own machine.
//
// Whatever it decides is written to notes, because an address that was found
// and one that was configured produce the same requests and warrant very
// different confidence — the same reason a discovered context window reports
// its provenance.
func resolveLocalEndpoint(reg *flags.Registry, declared string, resolve func() (credentials.Secret, error), notes io.Writer) string {
	_, origin, err := reg.String(flags.LLMLocalBaseURL)
	if err != nil {
		// Unreadable settings are not this function's failure to report; the
		// caller already read the same key and would have returned the error.
		return declared
	}
	res := local.ResolveEndpoint(context.Background(), local.ResolveOptions{
		Declared:           declared,
		DeclaredByOperator: origin != flags.OriginDefault,
		Model:              namedLocalModel(reg),
		Credential:         resolve,
	})
	if note := res.Note(); note != "" && notes != nil {
		fmt.Fprintf(notes, "manvi: %s\n", note)
	}
	return res.BaseURL
}

// namedLocalModel is the model id the operator wrote down, or empty.
//
// Only the explicit sources, in the same order resolveModelFor reads them.
// Discovery is deliberately not consulted: a discovered model is one a server
// offered, and this is called to decide which server to ask.
func namedLocalModel(reg *flags.Registry) string {
	if m := strings.TrimSpace(os.Getenv("MANVI_MODEL")); m != "" {
		return m
	}
	if m, _, err := reg.String(flags.LLMLocalModel); err == nil {
		return strings.TrimSpace(m)
	}
	return ""
}

// knownProvider reports whether this binary has an adapter for a name.
func knownProvider(name string) bool {
	for _, n := range providerNames() {
		if n == name {
			return true
		}
	}
	return false
}

// providerNames lists every provider this binary can construct.
func providerNames() []string {
	return []string{anthropic.Name, gemini.Name, local.Name, xai.Name}
}

// probeModel names a model to send `manvi probe` at when the operator did not
// choose one.
//
// The hosted providers have a documented cheap model. The local provider has no
// such thing — the served set is whatever the operator pulled — so it returns
// empty and probe resolves a model the same way a session does.
func probeModel(name string) string {
	switch name {
	case anthropic.Name:
		return "claude-haiku-4-5"
	case xai.Name:
		return "grok-4.6"
	case gemini.Name:
		return "gemini-3.5-flash-lite"
	default:
		return ""
	}
}

// ModelSource names where a run's model id came from.
//
// It exists for the same reason flags.Origin does: a model someone typed and a
// model the harness worked out lead to the same request and warrant different
// confidence. Once discovery can supply one, a report that cannot say which
// happened is a report that hides its own reasoning.
type ModelSource string

const (
	// SourceEnv is MANVI_MODEL, named for this run.
	SourceEnv ModelSource = "MANVI_MODEL"
	// SourceSetting is the provider's own standing configuration.
	SourceSetting ModelSource = "llm.local.model"
	// SourceDiscovered is the server's own answer: it serves exactly one model
	// that could drive a coding turn, so there was nothing to choose.
	SourceDiscovered ModelSource = "discovered"
	// SourceNone accompanies an error, where no model was resolved at all.
	SourceNone ModelSource = ""
)

// resolveModelFor decides which model a run will send.
//
// Precedence is explicit-beats-ambient: MANVI_MODEL is what an operator typed
// for this run, the provider's own setting is standing configuration, and
// neither being present is an error that names what the provider serves rather
// than a default this harness invented. No model id is defaulted anywhere in
// this binary, for the reason the flag catalogue gives: they must be read from
// what actually exists, not from memory.
func resolveModelFor(ctx context.Context, name string, provider llm.Provider, reg *flags.Registry) (string, ModelSource, error) {
	if m := strings.TrimSpace(os.Getenv("MANVI_MODEL")); m != "" {
		return m, SourceEnv, nil
	}
	if name == local.Name {
		if m, _, err := reg.String(flags.LLMLocalModel); err == nil && strings.TrimSpace(m) != "" {
			return strings.TrimSpace(m), SourceSetting, nil
		}
		// A local server that serves exactly one model an agent could run
		// leaves nothing to choose, and refusing to start until the operator
		// types that one name back is ceremony. Where there is a genuine
		// choice this still refuses — picking which weights someone's work
		// runs against is not a default worth holding — but the refusal now
		// says which models were rejected and why, instead of listing every id
		// including the embedding models that were never candidates.
		if adapter, ok := provider.(*local.Adapter); ok {
			survey, err := adapter.Survey(ctx, true)
			if err != nil {
				return "", SourceNone, err
			}
			model, why := local.SoleUsableModel(survey)
			if model != "" {
				return model, SourceDiscovered, nil
			}
			return "", SourceNone, fmt.Errorf("set %s or MANVI_MODEL — %s",
				flags.EnvKey(flags.LLMLocalModel), why)
		}
	}
	models := modelsFor(ctx, name, provider)
	if len(models) == 0 {
		return "", SourceNone, fmt.Errorf("set MANVI_MODEL: provider %q has no catalogued models", name)
	}
	const limit = 12
	shown := models
	suffix := ""
	if len(shown) > limit {
		shown, suffix = shown[:limit], fmt.Sprintf(" and %d more", len(models)-limit)
	}
	return "", SourceNone, fmt.Errorf("set MANVI_MODEL — %s serves: %s%s",
		name, strings.Join(shown, ", "), suffix)
}

// modelsFor lists what a provider serves, for an error that can name the
// alternatives.
//
// For the hosted adapters this is a transcribed catalogue. For the local one it
// is a live question put to the server, which is why this takes a context and
// a provider rather than just a name: there is no catalogue to read.
func modelsFor(ctx context.Context, name string, provider llm.Provider) []string {
	var models []string
	switch name {
	case anthropic.Name:
		models = anthropic.Models()
	case xai.Name:
		models = xai.Models()
	case gemini.Name:
		models = gemini.Models()
	case local.Name:
		if adapter, ok := provider.(*local.Adapter); ok {
			served, err := adapter.Models(ctx)
			if err != nil {
				// Deliberately dropped: the caller is already building an error
				// about a missing model, and a discovery failure is reported by
				// the adapter itself when the request is assembled. Returning
				// nil here yields "has no catalogued models", which is true.
				return nil
			}
			models = served
		}
	}
	sort.Strings(models)
	return models
}

// samplingRange is the interval one sampling setting is accepted in.
//
// Both bounds are optional because the settings genuinely differ: some have a
// documented floor and no documented ceiling, and inventing a ceiling here
// would refuse a configuration the server would have honoured — the same
// defect as accepting one it would not, wearing the opposite sign.
//
// Where the numbers come from, read rather than recalled:
//
//   - temperature [0, 2] and top_p [0, 1] are the bounds OpenAI's own request
//     schema declares (openai/openai-openapi openapi.yaml,
//     ModelResponseProperties, which CreateChatCompletionRequest composes),
//     and presence_penalty and frequency_penalty [-2, 2] are declared on
//     CreateChatCompletionRequest itself. That schema is the contract every
//     "OpenAI-compatible" local server is emulating, so it is the right thing
//     to hold an operator's value to.
//
//   - top_k, min_p and repetition_penalty are not OpenAI fields at all, so
//     there is no schema to cite. The bounds are the ones a server that
//     implements them enforces: mlx_lm's validate_model_parameters
//     (mlx_lm/server.py) checks top_k >= 0, min_p in [0, 1] and
//     repetition_penalty >= 0, and answers anything else with a 400.
//
// Not every server checks, and that is the reason this is worth doing rather
// than leaving to the server. Measured here: mlx_vlm.server and Ollama 0.32.13
// both accepted top_p=2.5, top_k=-5 and temperature=-1 and generated normally,
// sampling as though the field had never been sent. That is the worse of the
// two failures — the operator's declaration has no effect, the run looks fine,
// and nothing anywhere reports it. Refusing here is what makes an unreachable
// value fail the same way on every server.
type samplingRange struct {
	Min, Max       float64
	HasMin, HasMax bool
}

// between bounds a setting on both sides.
func between(min, max float64) samplingRange {
	return samplingRange{Min: min, Max: max, HasMin: true, HasMax: true}
}

// atLeast bounds a setting below only, for the knobs whose implementations
// document a floor and no ceiling.
func atLeast(min float64) samplingRange {
	return samplingRange{Min: min, HasMin: true}
}

// unbounded accepts any finite value. The seed is the case: any int64 is a
// legitimate seed, and there is nothing to check beyond it being a number.
func unbounded() samplingRange { return samplingRange{} }

// check refuses a value outside the range, naming the setting and what would
// have been accepted.
//
// Both facts are load-bearing, and localConfig already states why a few lines
// up for the token caps: an error that arrives as a refused request does not
// name the setting, and an error that names the setting without naming the
// range leaves the operator guessing at the next value to try.
func (r samplingRange) check(envKey, raw string, val float64) error {
	if (r.HasMin && val < r.Min) || (r.HasMax && val > r.Max) {
		return fmt.Errorf("%s=%q must be %s", envKey, raw, r.describe())
	}
	return nil
}

// describe spells the range the way the refusal reads it back.
func (r samplingRange) describe() string {
	switch {
	case r.HasMin && r.HasMax:
		return fmt.Sprintf("between %s and %s", formatBound(r.Min), formatBound(r.Max))
	case r.HasMin:
		return fmt.Sprintf("at least %s", formatBound(r.Min))
	case r.HasMax:
		return fmt.Sprintf("at most %s", formatBound(r.Max))
	}
	return "a finite number"
}

// formatBound prints a bound without the trailing zeros %v would add.
func formatBound(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// plainDecimal reports whether raw is a decimal number and nothing else.
//
// strconv.ParseFloat accepts the whole of Go's float literal syntax, which is
// considerably more than anyone writing a config file means. "0x1p-2" is a
// hexadecimal float that parses cleanly to 0.25 and "1_0" parses cleanly to
// 10 — so an operator who fat-fingered an underscore got a coding agent
// running at temperature 10, applied silently, with no error anywhere and
// nothing in the run pointing at the setting.
//
// Restricting the alphabet to the characters a decimal number is spelled with
// rejects those, and rejects "NaN", "nan", "Inf", "+Inf", "-Inf" and
// "infinity" along with them, before ParseFloat is ever consulted. ParseFloat
// then rejects what is left that uses only these characters and is still not a
// number: "1.2.3", "+-1", "1e".
func plainDecimal(raw string) bool {
	if raw == "" {
		return false
	}
	for i := 0; i < len(raw); i++ {
		switch c := raw[i]; {
		case c >= '0' && c <= '9', c == '.', c == '+', c == '-', c == 'e', c == 'E':
		default:
			return false
		}
	}
	return true
}

// optionalFloat reads a sampling setting that may be empty, refusing a value
// that is present but unreadable, not a plain decimal, not finite, or outside
// the range the servers accept.
//
// The finiteness check is what stops a misconfiguration from reporting itself
// as a network fault. "NaN" and the infinities parse without error, land in
// local.Config, and then fail at json.Marshal on every request for the life of
// the run — reaching the operator as "local: transport failure: encoding
// request: json: unsupported value: NaN", which sends them to debug their
// server or their network. The setting they mistyped is never mentioned.
func optionalFloat(reg *flags.Registry, key string, r samplingRange) (*float64, error) {
	raw, _, err := reg.String(key)
	if err != nil {
		return nil, err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if !plainDecimal(raw) {
		return nil, fmt.Errorf("%s=%q is not a plain decimal number", flags.EnvKey(key), raw)
	}
	val, parseErr := strconv.ParseFloat(raw, 64)
	if parseErr != nil {
		return nil, fmt.Errorf("%s=%q is not a number: %w", flags.EnvKey(key), raw, parseErr)
	}
	// plainDecimal already rules out every value spelled as a name, but not
	// "1e400", which overflows to +Inf. Checked on the value rather than the
	// spelling so the guarantee does not rest on having enumerated the
	// spellings correctly.
	if math.IsNaN(val) || math.IsInf(val, 0) {
		return nil, fmt.Errorf("%s=%q is not a finite number and cannot be encoded into a request",
			flags.EnvKey(key), raw)
	}
	if err := r.check(flags.EnvKey(key), raw, val); err != nil {
		return nil, err
	}
	return &val, nil
}

// optionalInt reads an integer sampling setting that may be empty.
//
// strconv.Atoi is already strict in the way ParseFloat is not: it refuses
// "1_0", "0x10", "1e3" and "12x" outright, so there is no alphabet to police
// here. Only the range needed adding.
func optionalInt(reg *flags.Registry, key string, r samplingRange) (*int, error) {
	raw, _, err := reg.String(key)
	if err != nil {
		return nil, err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	val, parseErr := strconv.Atoi(raw)
	if parseErr != nil {
		return nil, fmt.Errorf("%s=%q is not a whole number: %w", flags.EnvKey(key), raw, parseErr)
	}
	if err := r.check(flags.EnvKey(key), raw, float64(val)); err != nil {
		return nil, err
	}
	return &val, nil
}

// stallTimeoutSetting reads llm.local.stall_timeout and translates the
// operator's vocabulary into the adapter's.
//
// The two disagreed, and the setting described itself wrongly as a result. The
// adapter reads zero as "use the default" and a negative as "disable", which is
// a reasonable internal contract and is what its tests pin. The flag told the
// operator "Empty or 0 disables it" — and the generic duration parser turned
// empty *and* "0" into zero, which armed the five-minute watchdog, while
// rejecting the negative that would actually have disabled it. So the documented
// way to switch the watchdog off armed it, and the working way was unreachable
// from configuration at all. A setting that misreports itself is worse than one
// that refuses: an operator reads it, believes it, and does not look again.
//
// Translating here rather than changing the adapter keeps that internal contract
// and its tests intact, and puts the vocabulary an operator types where the
// operator's other settings are read.
func stallTimeoutSetting(reg *flags.Registry) (time.Duration, error) {
	raw, _, err := reg.String(flags.LLMLocalStallTimeout)
	if err != nil {
		return 0, err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		// Unset: the adapter's own default applies, which is the safe direction
		// — an unconfigured harness should still not wait forever on a wedged
		// server.
		return 0, nil
	}
	if raw == "0" || strings.EqualFold(raw, "off") || strings.EqualFold(raw, "none") {
		// Deliberately off. Carried to the adapter as a negative because that
		// is how it spells "disabled"; zero would mean the opposite.
		return -1, nil
	}
	d, parseErr := time.ParseDuration(raw)
	if parseErr != nil {
		return 0, fmt.Errorf("%s=%q is not a duration like 90s or 5m, nor 0/off to disable: %w",
			flags.EnvKey(flags.LLMLocalStallTimeout), raw, parseErr)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s=%q is negative; use 0 to disable the stall watchdog",
			flags.EnvKey(flags.LLMLocalStallTimeout), raw)
	}
	if d == 0 {
		// "0s" and friends mean the same as "0".
		return -1, nil
	}
	return d, nil
}
