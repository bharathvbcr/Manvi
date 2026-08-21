package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"manvi/credentials"
	"manvi/flags"
	"manvi/llm/local"
)

// Every test here pins llm.local.base_url through the config layer, which makes
// the setting read as operator-declared and so stops the scan running. That is
// not only for hermeticity: a developer machine with Ollama up would otherwise
// have these tests assert against whatever it happens to be serving, and pass
// or fail for reasons that have nothing to do with the code.

// fakeOllama serves the three endpoints the adapter asks Ollama for.
func fakeOllama(t *testing.T, caps map[string][]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			entries := make([]map[string]any, 0, len(caps))
			for id := range caps {
				entries = append(entries, map[string]any{"id": id, "object": "model"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": entries})
		case "/api/version":
			_ = json.NewEncoder(w).Encode(map[string]any{"version": "0.32.13"})
		case "/api/show":
			var req struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			list, ok := caps[req.Model]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model_info":   map[string]any{"qwen3_5.context_length": 262144},
				"capabilities": list,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func pinnedTo(t *testing.T, baseURL string) *flags.Registry {
	t.Helper()
	return registryWith(t, map[string]string{flags.LLMLocalBaseURL: baseURL})
}

// TestLocalListsWhatAServerServesAndWhatCannotRunATurn is the command's whole
// purpose: an operator who does not know a model id gets the ids, and is not
// sent to try the one that cannot generate text.
func TestLocalListsWhatAServerServesAndWhatCannotRunATurn(t *testing.T) {
	srv := fakeOllama(t, map[string][]string{
		"qwen3.8:27b-mlx":         {"completion", "tools", "thinking"},
		"nomic-embed-text:latest": {"embedding"},
	})
	var out bytes.Buffer
	if err := showLocal(&out, pinnedTo(t, srv.URL+"/v1"), nil); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"qwen3.8:27b-mlx",
		"nomic-embed-text:latest",
		"ollama 0.32.13",
		"embedding model",
		"256k ctx",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output is missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "2 model(s), 1 usable") {
		t.Errorf("the count does not separate usable from served:\n%s", got)
	}
}

// TestLocalSaysTheCheckRanWhenNothingAnswered keeps the distinction the whole
// package is built around. "Nothing is running" and "I did not look" send an
// operator to different places.
func TestLocalSaysTheCheckRanWhenNothingAnswered(t *testing.T) {
	var out bytes.Buffer
	if err := showLocal(&out, pinnedTo(t, "http://127.0.0.1:1/v1"), nil); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "none answered") {
		t.Errorf("a scan that found nothing did not say so:\n%s", got)
	}
	if !strings.Contains(got, "http://127.0.0.1:1/v1") {
		t.Errorf("the address that was tried is not named:\n%s", got)
	}
	if !strings.Contains(got, flags.EnvKey(flags.LLMLocalBaseURL)) {
		t.Errorf("the output does not say how to point the harness elsewhere:\n%s", got)
	}
}

// TestLocalRefusesAnUnknownOption keeps a mistyped flag from being silently
// ignored, which would report on a scan the operator did not ask for.
func TestLocalRefusesAnUnknownOption(t *testing.T) {
	var out bytes.Buffer
	err := showLocal(&out, newTestRegistry(t), []string{"--all"})
	if err == nil {
		t.Fatal("an unknown option was accepted")
	}
	if !strings.Contains(err.Error(), "--all") {
		t.Errorf("the error does not name the option: %v", err)
	}
}

// TestLocalRefusesAMalformedTimeout is the same rule the sampling settings
// follow: a value that is present but unreadable is an error, never a shrug
// that silently applies the default.
func TestLocalRefusesAMalformedTimeout(t *testing.T) {
	var out bytes.Buffer
	for _, tc := range []struct{ name, arg, want string }{
		{"not a duration", "soon", "is not a duration"},
		{"negative", "-5s", "must be positive"},
		{"zero", "0s", "must be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Both spellings, because the bug this replaced accepted neither:
			// a prefix check rejected the value of --timeout as an unknown
			// option, so every one of these errored for the wrong reason.
			for _, args := range [][]string{
				{"--timeout", tc.arg},
				{"--timeout=" + tc.arg},
			} {
				err := showLocal(&out, newTestRegistry(t), args)
				if err == nil {
					t.Fatalf("%v was accepted", args)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("%v gave %q, want it to say %q", args, err, tc.want)
				}
			}
		})
	}
}

// TestLocalAcceptsAValidTimeout is the other half, and the one whose absence
// let a broken parser ship: every negative case passed on the wrong error.
func TestLocalAcceptsAValidTimeout(t *testing.T) {
	srv := fakeOllama(t, map[string][]string{"qwen3.8:27b-mlx": {"completion", "tools"}})
	reg := pinnedTo(t, srv.URL+"/v1")
	for _, args := range [][]string{{"--timeout", "5s"}, {"--timeout=5s"}} {
		var out bytes.Buffer
		if err := showLocal(&out, reg, args); err != nil {
			t.Fatalf("%v was refused: %v", args, err)
		}
		if !strings.Contains(out.String(), "qwen3.8:27b-mlx") {
			t.Errorf("%v did not produce a listing:\n%s", args, out.String())
		}
	}
}

// TestLocalRefusesATimeoutWithNoValue keeps a trailing flag from silently
// taking the default.
func TestLocalRefusesATimeoutWithNoValue(t *testing.T) {
	var out bytes.Buffer
	if err := showLocal(&out, newTestRegistry(t), []string{"--timeout"}); err == nil {
		t.Fatal("a --timeout with no value was accepted")
	}
}

// TestADeclaredEndpointIsNeverScannedPast is the guarantee that keeps this
// feature from overruling an operator about their own machine.
func TestADeclaredEndpointIsNeverScannedPast(t *testing.T) {
	const declared = "http://127.0.0.1:1/v1"
	var notes bytes.Buffer
	got := resolveLocalEndpoint(pinnedTo(t, declared), declared, nil, &notes)
	if got != declared {
		t.Fatalf("resolved to %q, want the declared address unchanged", got)
	}
	if notes.Len() != 0 {
		t.Errorf("a configured address produced a note: %q", notes.String())
	}
}

// TestTheShippedDefaultIsTreatedAsTheGuessItIs is the other half. The default
// points at vLLM's port and most operators are running something else, so it is
// the one value that gets second-guessed.
func TestTheShippedDefaultIsTreatedAsTheGuessItIs(t *testing.T) {
	srv := fakeOllama(t, map[string][]string{"qwen3.8:27b-mlx": {"completion", "tools"}})

	// Resolution over an injected endpoint list, so the assertion is about the
	// rule and not about what happens to be listening on this machine.
	res := local.ResolveEndpoint(context.Background(), local.ResolveOptions{
		Declared:  local.DefaultBaseURL,
		Endpoints: []local.Endpoint{{BaseURL: srv.URL + "/v1", Convention: "test"}},
	})
	if res.BaseURL != srv.URL+"/v1" {
		t.Fatalf("resolved to %q, want the server that answered", res.BaseURL)
	}
	if res.Note() == "" {
		t.Error("an address that was found reads identically to one that was configured")
	}
}

// TestASingleUsableModelNeedsNoSetting removes the step where an operator is
// refused until they type back the only name the server offers.
func TestASingleUsableModelNeedsNoSetting(t *testing.T) {
	t.Setenv("MANVI_MODEL", "")
	srv := fakeOllama(t, map[string][]string{
		"qwen3.8:27b-mlx":         {"completion", "tools", "thinking"},
		"nomic-embed-text:latest": {"embedding"},
	})
	reg := pinnedTo(t, srv.URL+"/v1")
	provider, err := buildProvider(local.Name, reg, credentials.NewResolver(), nil)
	if err != nil {
		t.Fatal(err)
	}
	model, source, err := resolveModelFor(context.Background(), local.Name, provider, reg)
	if err != nil {
		t.Fatalf("a server with one usable model still demanded a setting: %v", err)
	}
	if model != "qwen3.8:27b-mlx" {
		t.Fatalf("model = %q, want the only model that can run a turn", model)
	}
	if source != SourceDiscovered {
		t.Errorf("source = %q, want %q — a model the harness worked out must not "+
			"report as one someone chose", source, SourceDiscovered)
	}
}

// TestSeveralUsableModelsStillRefuses holds the line: choosing which weights an
// operator's work runs against is not a default this harness gets to hold.
func TestSeveralUsableModelsStillRefuses(t *testing.T) {
	t.Setenv("MANVI_MODEL", "")
	srv := fakeOllama(t, map[string][]string{
		"qwen3.8:27b-mlx": {"completion", "tools"},
		"gemma4:31b-mlx":  {"completion", "tools"},
	})
	reg := pinnedTo(t, srv.URL+"/v1")
	provider, err := buildProvider(local.Name, reg, credentials.NewResolver(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = resolveModelFor(context.Background(), local.Name, provider, reg)
	if err == nil {
		t.Fatal("the harness chose between two usable models")
	}
	msg := err.Error()
	if !strings.Contains(msg, flags.EnvKey(flags.LLMLocalModel)) {
		t.Errorf("the refusal does not name the setting that resolves it: %v", err)
	}
	for _, want := range []string{"qwen3.8:27b-mlx", "gemma4:31b-mlx"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not name %q as an alternative: %v", want, err)
		}
	}
}

// TestARefusalDoesNotOfferModelsThatCannotRun is the improvement over listing
// every served id: an operator picking from the refusal must not be able to
// pick something that was never a candidate.
func TestARefusalDoesNotOfferModelsThatCannotRun(t *testing.T) {
	t.Setenv("MANVI_MODEL", "")
	srv := fakeOllama(t, map[string][]string{
		"qwen3.8:27b-mlx":         {"completion", "tools"},
		"gemma4:31b-mlx":          {"completion", "tools"},
		"nomic-embed-text:latest": {"embedding"},
	})
	reg := pinnedTo(t, srv.URL+"/v1")
	provider, err := buildProvider(local.Name, reg, credentials.NewResolver(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = resolveModelFor(context.Background(), local.Name, provider, reg)
	if err == nil {
		t.Fatal("expected a refusal naming the alternatives")
	}
	if strings.Contains(err.Error(), "nomic-embed-text") {
		t.Errorf("the refusal offers an embedding model as something to run: %v", err)
	}
}

// TestAnExplicitModelSettingBeatsDiscovery keeps precedence where the rest of
// the harness puts it: what someone typed wins over what was found.
func TestAnExplicitModelSettingBeatsDiscovery(t *testing.T) {
	t.Setenv("MANVI_MODEL", "")
	srv := fakeOllama(t, map[string][]string{"qwen3.8:27b-mlx": {"completion", "tools"}})
	reg := registryWith(t, map[string]string{
		flags.LLMLocalBaseURL: srv.URL + "/v1",
		flags.LLMLocalModel:   "chosen-by-hand",
	})
	provider, err := buildProvider(local.Name, reg, credentials.NewResolver(), nil)
	if err != nil {
		t.Fatal(err)
	}
	model, source, err := resolveModelFor(context.Background(), local.Name, provider, reg)
	if err != nil {
		t.Fatal(err)
	}
	if model != "chosen-by-hand" {
		t.Fatalf("model = %q; discovery overruled the operator's own setting", model)
	}
	if source != SourceSetting {
		t.Errorf("source = %q, want %q", source, SourceSetting)
	}
}

// TestLocalReadinessIsNotACredentialQuestion is the reason this provider was
// reported as unavailable while it was working.
func TestLocalReadinessIsNotACredentialQuestion(t *testing.T) {
	srv := fakeOllama(t, map[string][]string{"qwen3.8:27b-mlx": {"completion", "tools"}})
	got := describeLocalReadiness(pinnedTo(t, srv.URL+"/v1"))
	if !strings.HasPrefix(got, "ready") {
		t.Fatalf("readiness = %q, want a reachable server reported as ready", got)
	}
	if !strings.Contains(got, "ollama") {
		t.Errorf("readiness = %q, want the runtime named", got)
	}

	down := describeLocalReadiness(pinnedTo(t, "http://127.0.0.1:1/v1"))
	if !strings.HasPrefix(down, "unavailable") {
		t.Fatalf("readiness = %q, want an unreachable server reported as unavailable", down)
	}
}

// TestLocalDoesNotSuggestSettingsThatCannotBeFollowed covers the server whose
// models are all the wrong kind. The guidance block used to print an export
// line with a placeholder in it, which reads as an instruction and is not one.
func TestLocalDoesNotSuggestSettingsThatCannotBeFollowed(t *testing.T) {
	srv := fakeOllama(t, map[string][]string{
		"nomic-embed-text:latest": {"embedding"},
		"textonly:7b":             {"completion"},
	})
	var out bytes.Buffer
	if err := showLocal(&out, pinnedTo(t, srv.URL+"/v1"), nil); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "=MODEL") {
		t.Errorf("a placeholder was printed as advice:\n%s", got)
	}
	if strings.Contains(got, "serves several usable models") {
		t.Errorf("a server with no usable model was described as having several:\n%s", got)
	}
	if !strings.Contains(got, "no model that can drive a coding turn") {
		t.Errorf("the output does not say why nothing here can be run:\n%s", got)
	}
}

// TestLocalPinnedToAServerReportsThatServer is the regression for guidance that
// re-resolved as though the address were unset, scanned the well-known ports,
// and then could not find the operator's own pinned server among them.
func TestLocalPinnedToAServerReportsThatServer(t *testing.T) {
	srv := fakeOllama(t, map[string][]string{
		"a:7b": {"completion", "tools"},
		"b:7b": {"completion", "tools"},
	})
	var out bytes.Buffer
	if err := showLocal(&out, pinnedTo(t, srv.URL+"/v1"), nil); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "Pin the one you want") {
		t.Errorf("an already-pinned server was reported as needing to be pinned:\n%s", got)
	}
	if !strings.Contains(got, srv.URL+"/v1 serves several usable models") {
		t.Errorf("the guidance does not name the pinned server:\n%s", got)
	}
}

// TestResolvePrintsAShellReadableDocument covers the contract verify.sh depends
// on. The gate parses these keys with awk, so a change to their names or shape
// breaks certification silently unless something asserts them here.
func TestResolvePrintsAShellReadableDocument(t *testing.T) {
	srv := fakeOllama(t, map[string][]string{
		"qwen3.8:27b-mlx":         {"completion", "tools"},
		"nomic-embed-text:latest": {"embedding"},
	})
	var out bytes.Buffer
	if err := showLocal(&out, pinnedTo(t, srv.URL+"/v1"), []string{"--resolve"}); err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("line %q is not key=value; a shell reading this with awk -F= cannot", line)
		}
		if strings.ContainsAny(v, " \t\"'") {
			t.Errorf("value %q for %q needs quoting; the document must be readable unquoted", v, k)
		}
		got[k] = v
	}
	want := map[string]string{
		"base_url":        srv.URL + "/v1",
		"base_url_source": string(flags.OriginConfig),
		"model":           "qwen3.8:27b-mlx",
		"model_source":    string(SourceDiscovered),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("document has %d keys, want %d: %v", len(got), len(want), got)
	}
}

// TestResolveEmitsNoPartialDocument is the property that lets the gate treat a
// zero exit as a complete answer. A half-written document parsed as a whole one
// would have the gate probe an empty model name.
func TestResolveEmitsNoPartialDocument(t *testing.T) {
	// Unreachable. This one takes a few seconds on purpose: a declared address
	// is retried by the transport, and --resolve reports what a run would do
	// rather than a cheaper approximation of it.
	var down bytes.Buffer
	if err := showLocal(&down, pinnedTo(t, "http://127.0.0.1:1/v1"), []string{"--resolve"}); err == nil {
		t.Fatal("resolution succeeded against a server that is not there")
	}
	if down.Len() != 0 {
		t.Errorf("a failed resolution still wrote to stdout:\n%s", down.String())
	}

	// The ambiguous case: several models could run, so nothing is chosen.
	srv := fakeOllama(t, map[string][]string{
		"a:7b": {"completion", "tools"},
		"b:7b": {"completion", "tools"},
	})
	var out bytes.Buffer
	err := showLocal(&out, pinnedTo(t, srv.URL+"/v1"), []string{"--resolve"})
	if err == nil {
		t.Fatal("resolution chose between two usable models")
	}
	if out.Len() != 0 {
		t.Errorf("an unresolvable selection still wrote a document:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "a:7b") {
		t.Errorf("the reason does not name the candidates: %v", err)
	}
}

// TestResolveReportsProvenanceNotJustValues keeps the gate able to say whether
// it certified a configured setup or a discovered one.
func TestResolveReportsProvenanceNotJustValues(t *testing.T) {
	// Two served models, so discovery would refuse and only MANVI_MODEL can
	// settle it. The named model has to be one the server actually serves —
	// naming one it does not is a different case, covered separately.
	t.Setenv("MANVI_MODEL", "named-for-this-run:7b")
	srv := fakeOllama(t, map[string][]string{
		"named-for-this-run:7b": {"completion", "tools"},
		"the-other-one:7b":      {"completion", "tools"},
	})
	var out bytes.Buffer
	if err := showLocal(&out, pinnedTo(t, srv.URL+"/v1"), []string{"--resolve"}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "model=named-for-this-run:7b") {
		t.Errorf("MANVI_MODEL did not win:\n%s", got)
	}
	if !strings.Contains(got, "model_source="+string(SourceEnv)) {
		t.Errorf("a model named for this run reported as something else:\n%s", got)
	}
}

// TestResolveRefusesAModelTheResolvedServerDoesNotServe is the regression for a
// gate that went red for the wrong reason.
//
// llm.local.model and llm.local.base_url are set independently, so each can be
// right while the pair is not. --resolve used to emit that pair as a usable
// selection; verify.sh probed it, the server refused a model it does not have,
// and the gate reported a broken wire contract on an adapter that was fine.
func TestResolveRefusesAModelTheResolvedServerDoesNotServe(t *testing.T) {
	srv := fakeOllama(t, map[string][]string{"served:7b": {"completion", "tools"}})
	reg := registryWith(t, map[string]string{
		flags.LLMLocalBaseURL: srv.URL + "/v1",
		flags.LLMLocalModel:   "on-some-other-server:7b",
	})
	var out bytes.Buffer
	err := showLocal(&out, reg, []string{"--resolve"})
	if err == nil {
		t.Fatal("a model the server does not serve was reported as a usable selection")
	}
	if out.Len() != 0 {
		t.Errorf("an unusable selection still wrote a document:\n%s", out.String())
	}
	for _, want := range []string{"on-some-other-server:7b", "served:7b"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// TestResolveStillAcceptsAServedNamedModel keeps the check above from being a
// blanket refusal of every model anyone sets by hand.
func TestResolveStillAcceptsAServedNamedModel(t *testing.T) {
	srv := fakeOllama(t, map[string][]string{
		"a:7b": {"completion", "tools"},
		"b:7b": {"completion", "tools"},
	})
	reg := registryWith(t, map[string]string{
		flags.LLMLocalBaseURL: srv.URL + "/v1",
		flags.LLMLocalModel:   "b:7b",
	})
	var out bytes.Buffer
	if err := showLocal(&out, reg, []string{"--resolve"}); err != nil {
		t.Fatalf("a model the server does serve was refused: %v", err)
	}
	if !strings.Contains(out.String(), "model=b:7b") {
		t.Errorf("the named model did not survive the check:\n%s", out.String())
	}
}
