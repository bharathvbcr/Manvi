package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"manvi/credentials"
	"manvi/flags"
	"manvi/llm/local"
)

// showLocal answers "what can I run on this machine, and how do I run it".
//
// It exists because every other route to that answer required already knowing
// it. `manvi providers` reports whether a credential is present, which for a
// loopback server is a question with no useful answer; `manvi probe local`
// needs a model id, which is the thing an operator is trying to find out; and
// the adapter's own refusal names the served set only once it has been pointed
// at the right port, which is the other half of what they do not know yet.
//
// So this asks the machine rather than the configuration: it scans the
// addresses local runtimes listen on, names what answered, says which models
// could actually drive a coding turn, and prints the lines that make one of
// them the default. Nothing here is a catalogue — every field is either
// something a server said or is marked as not having been said.
func showLocal(out io.Writer, reg *flags.Registry, args []string) error {
	// Parsed by walking rather than by scanning for a prefix, so that the value
	// of --timeout is consumed as a value. A prefix check rejected "5s" as an
	// unknown option, and the test that was supposed to cover a malformed
	// duration passed on that error instead of the one it named.
	timeout := 15 * time.Second
	resolveOnly := false
	for i := 0; i < len(args); i++ {
		raw := ""
		switch {
		case args[i] == "--resolve":
			resolveOnly = true
			continue
		case args[i] == "--timeout":
			if i+1 >= len(args) {
				return errors.New("--timeout needs a duration like 5s or 1m")
			}
			i++
			raw = args[i]
		case strings.HasPrefix(args[i], "--timeout="):
			raw = strings.TrimPrefix(args[i], "--timeout=")
		default:
			return fmt.Errorf("usage: manvi local [--resolve] [--timeout DURATION]\n"+
				"  unknown option %q", args[i])
		}
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("--timeout %q is not a duration like 5s or 1m: %w", raw, err)
		}
		if d <= 0 {
			return fmt.Errorf("--timeout %q must be positive", raw)
		}
		timeout = d
	}

	declared, origin, err := reg.String(flags.LLMLocalBaseURL)
	if err != nil {
		return err
	}
	pinned := origin != flags.OriginDefault

	if resolveOnly {
		return resolveLocalSelection(out, reg, timeout)
	}

	resolver := credentials.NewResolver()
	resolve := func() (credentials.Secret, error) { return resolver.Resolve(local.Name) }

	// A pinned address is surveyed and nothing else is probed, for the same
	// reason resolution does not scan past one: the operator has already
	// answered the question this command asks.
	endpoints := local.WellKnownEndpoints()
	if pinned {
		endpoints = []local.Endpoint{{BaseURL: declared}}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	servers := local.Scan(ctx, local.ScanOptions{
		Endpoints:    endpoints,
		Timeout:      timeout,
		Credential:   resolve,
		Capabilities: true,
	})

	fmt.Fprintln(out, "local model servers")
	fmt.Fprintln(out)
	if pinned {
		fmt.Fprintf(out, "  %s is set to %s (%s), so only that address was tried\n\n",
			flags.EnvKey(flags.LLMLocalBaseURL), declared, origin)
	}

	if len(servers) == 0 {
		reportNoLocalServers(out, endpoints, pinned)
		return nil
	}

	for _, srv := range servers {
		reportServer(out, srv)
	}
	reportHowToUse(out, reg, servers, pinned, resolve)
	return nil
}

// reportNoLocalServers says that the check ran and found nothing, which is a
// different statement from the check not having run — and names every address
// that was tried, so an operator serving on a port this harness does not guess
// can see that theirs was not among them.
func reportNoLocalServers(out io.Writer, endpoints []local.Endpoint, pinned bool) {
	fmt.Fprintln(out, "  none answered. Tried:")
	for _, ep := range endpoints {
		if pinned {
			fmt.Fprintf(out, "    %s\n", ep.BaseURL)
			continue
		}
		fmt.Fprintf(out, "    %-30s %s\n", ep.BaseURL, ep.Convention)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  Start a server, or point the harness at one it does not guess:")
	fmt.Fprintf(out, "    export %s=http://127.0.0.1:PORT/v1\n", flags.EnvKey(flags.LLMLocalBaseURL))
}

// reportServer renders one server and everything it said about its models.
func reportServer(out io.Writer, srv local.Server) {
	fmt.Fprintf(out, "  %s\n", srv.Describe())
	fmt.Fprintf(out, "    %s\n\n", summariseModels(srv))

	models := append([]local.ServedModel(nil), srv.Models...)
	sort.Slice(models, func(i, j int) bool {
		// Usable first, because the list exists to be chosen from. An
		// alphabetical list that opens with an embedding model buries its own
		// answer.
		if models[i].Usable() != models[j].Usable() {
			return models[i].Usable()
		}
		return models[i].ID < models[j].ID
	})
	for _, m := range models {
		mark := " "
		if !m.Usable() {
			mark = "-"
		}
		fmt.Fprintf(out, "    %s %-46s %s\n", mark, m.ID, describeModelDims(m))
		if why := m.Why(); why != "" {
			fmt.Fprintf(out, "      %s\n", why)
		}
	}
	fmt.Fprintln(out)
}

// summariseModels counts a server's models without overstating what was
// established.
//
// "10 usable" is a claim, and against a server that publishes no capabilities
// it is an assumption wearing a number's clothes: ServedModel.Usable treats
// silence as permission, which is right for not hiding models and wrong for
// counting them as checked. Where nothing was published, this says so.
func summariseModels(srv local.Server) string {
	known := 0
	for _, m := range srv.Models {
		if m.CapabilitiesKnown {
			known++
		}
	}
	switch {
	case len(srv.Models) == 0:
		return "no models"
	case known == 0:
		return fmt.Sprintf("%d model(s); this server publishes no capabilities, so none could be checked",
			len(srv.Models))
	case known < len(srv.Models):
		return fmt.Sprintf("%d model(s), %d usable for a coding turn, %d unchecked (no capabilities published)",
			len(srv.Models), len(srv.Usable()), len(srv.Models)-known)
	default:
		return fmt.Sprintf("%d model(s), %d usable for a coding turn", len(srv.Models), len(srv.Usable()))
	}
}

// describeModelDims renders a model's window and capabilities, distinguishing
// what the server published from what it did not.
//
// A server that says nothing gets "context unpublished" rather than the
// declared default printed as though it were fact. The declared number is what
// a run will use, and `manvi probe local` reports it as declared — but printing
// it here, in a column beside numbers that were read off servers, would make a
// setting look like a measurement.
func describeModelDims(m local.ServedModel) string {
	var parts []string
	if m.ContextWindow > 0 && m.Discovered() {
		parts = append(parts, fmt.Sprintf("%s ctx", humanTokens(m.ContextWindow)))
	} else {
		parts = append(parts, "ctx unpublished")
	}
	if !m.CapabilitiesKnown {
		parts = append(parts, "capabilities unpublished")
		return strings.Join(parts, ", ")
	}
	var caps []string
	for _, c := range []struct {
		on   bool
		name string
	}{
		{m.Embedding(), "embedding"},
		{m.SupportsCompletion, "completion"},
		{m.SupportsTools, "tools"},
		{m.SupportsVision, "vision"},
		{m.SupportsReasoning, "thinking"},
	} {
		if c.on {
			caps = append(caps, c.name)
		}
	}
	if len(caps) == 0 {
		// The server published a capability list and nothing in it was
		// recognised. Saying so names a real condition — a runtime this
		// harness has not been taught to read — rather than implying the
		// model has no abilities.
		caps = append(caps, "no capability this harness recognises")
	}
	parts = append(parts, strings.Join(caps, "+"))
	return strings.Join(parts, ", ")
}

// humanTokens renders a context window the way the model cards it came from do.
func humanTokens(n int) string {
	switch {
	case n >= 1000 && n%1024 == 0:
		return fmt.Sprintf("%dk", n/1024)
	case n >= 1000:
		return fmt.Sprintf("%.0fk", float64(n)/1024)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// reportHowToUse prints the settings that turn a found server into the one a
// run will use.
//
// It prints only what is not already true. An operator whose provider is
// already local and whose server was found does not need to be told to export
// what they exported, and a command that repeats settings back regardless is
// one whose output stops being read.
func reportHowToUse(out io.Writer, reg *flags.Registry, servers []local.Server, pinned bool, resolveCredential func() (credentials.Secret, error)) {
	provider, _, _ := reg.String(flags.LLMDefaultProvider)
	model, modelOrigin, _ := reg.String(flags.LLMLocalModel)

	var lines []string
	if provider != local.Name {
		lines = append(lines, fmt.Sprintf("export %s=%s",
			flags.EnvKey(flags.LLMDefaultProvider), local.Name))
	}

	// Which server a run resolves to, decided by the same function the run
	// uses rather than re-derived here — a "how to use it" that disagreed with
	// what actually happens would be worse than none. The pinned flag is passed
	// through for the same reason: resolving as though the address were unset
	// would re-scan the well-known ports and then fail to find the operator's
	// own pinned server among them.
	res := local.ResolveEndpoint(context.Background(), local.ResolveOptions{
		Declared:           servers[0].BaseURL,
		DeclaredByOperator: pinned,
		Model:              namedLocalModel(reg),
		Credential:         resolveCredential,
	})
	if note := res.Note(); note != "" {
		fmt.Fprintf(out, "  %s\n\n", note)
	}
	chosen := serverAt(servers, res.BaseURL)

	if chosen == nil {
		fmt.Fprintln(out, "  Pin the one you want, then run 'manvi probe local':")
		fmt.Fprintf(out, "    export %s=%s\n", flags.EnvKey(flags.LLMLocalBaseURL), servers[0].BaseURL)
		for _, l := range lines {
			fmt.Fprintf(out, "    %s\n", l)
		}
		return
	}

	switch sole, why := local.SoleUsableModel(*chosen); {
	case strings.TrimSpace(model) != "":
		fmt.Fprintf(out, "  %s is set to %s (%s)\n",
			flags.EnvKey(flags.LLMLocalModel), model, modelOrigin)
	case sole != "":
		fmt.Fprintf(out, "  %s serves one usable model, so a run needs no model set: %s\n",
			chosen.BaseURL, sole)
	case len(chosen.Usable()) == 0:
		// Nothing here can run a turn, so there is no setting to suggest —
		// printing an export line with a placeholder in it would be advice
		// that cannot be followed.
		fmt.Fprintf(out, "  %s\n", why)
		fmt.Fprintln(out, "  Pull a model that can generate text and call tools, then run 'manvi local' again.")
		return
	default:
		// The models are listed above; repeating them here would bury the one
		// line that says what to do. What this cannot do is choose — the
		// alphabetically first usable model on a machine is as likely to be an
		// audio editor as a coding model, so it is offered as the shape of the
		// setting, not as a recommendation.
		fmt.Fprintf(out, "  %s serves several usable models, so a run must name one from the list above:\n",
			chosen.BaseURL)
		if suggestion := firstUsable(*chosen); suggestion != "" {
			lines = append(lines, fmt.Sprintf("export %s=%s",
				flags.EnvKey(flags.LLMLocalModel), suggestion))
		}
	}

	if len(lines) == 0 {
		fmt.Fprintln(out, "\n  Nothing to set — 'manvi probe local' will exercise this server now.")
		return
	}
	fmt.Fprintln(out, "\n  To run against it:")
	for _, l := range lines {
		fmt.Fprintf(out, "    %s\n", l)
	}
	fmt.Fprintln(out, "    manvi probe local")
}

// serverAt finds the scanned server at an address, or nil.
func serverAt(servers []local.Server, baseURL string) *local.Server {
	for i := range servers {
		if servers[i].BaseURL == baseURL {
			return &servers[i]
		}
	}
	return nil
}

// firstUsable names a model worth suggesting, alphabetically so the suggestion
// does not change between runs against an unchanged server.
//
// It returns empty rather than a placeholder when there is nothing to suggest.
// Its caller has already established that there is, but a helper whose
// correctness rests on a condition checked somewhere else is one edit away from
// printing "export MANVI_LLM_LOCAL_MODEL=MODEL" as though it were advice.
func firstUsable(srv local.Server) string {
	usable := srv.Usable()
	names := make([]string, 0, len(usable))
	for _, m := range usable {
		names = append(names, m.ID)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// resolveLocalSelection prints exactly what a local run would use, in a form a
// shell can read.
//
// It exists for `./verify.sh`, which has to decide whether the live-wire gate
// can run before it runs it — and could not, because everything else this
// command prints is prose written for a person. Parsing that would make a
// certification gate depend on the wording of a report.
//
// It is not a verify.sh hook, though. "What will a run actually use, given
// everything currently set and running?" is a question an operator asks the
// moment any of it is discovered rather than declared, and the honest answer
// has to come from the same resolution the run performs. So this builds the
// provider the way a run builds it and asks the same two functions, rather than
// re-deriving an answer that could disagree with what happens next.
//
// Output is key=value, one per line, values unquoted: model ids contain '/',
// ':' and '.' but no whitespace, and a shell reading this with `read` or awk
// should not have to strip anything. A failure to resolve is a non-zero exit
// with the reason on the error, never a partial document.
func resolveLocalSelection(out io.Writer, reg *flags.Registry, timeout time.Duration) error {
	resolver := credentials.NewResolver()

	// io.Discard for the notes: the endpoint note is prose, and this document
	// carries the same fact in base_url_source.
	provider, err := buildProvider(local.Name, reg, resolver, io.Discard)
	if err != nil {
		return err
	}
	adapter, ok := provider.(*local.Adapter)
	if !ok {
		return fmt.Errorf("the local provider built as %T, which cannot report an address", provider)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	model, source, err := resolveModelFor(ctx, local.Name, provider, reg)
	if err != nil {
		return err
	}

	// A named model is checked against the address that was resolved, because
	// the two are set independently and nothing else pairs them.
	//
	// llm.local.model and llm.local.base_url can each be right and still not
	// belong together: on a machine running both Ollama and an MLX server,
	// naming an Ollama model while the address resolves to the MLX server
	// produces a selection that cannot work. Emitting it anyway would hand a
	// caller a document whose whole promise is that it describes a usable run,
	// and verify.sh acted on exactly that — probing a model the server does not
	// have and reporting the refusal as a broken wire contract. A gate that can
	// go red for a reason unrelated to what it certifies is worse than one that
	// says it did not run.
	//
	// Discovered models skip nothing here: Capability is how the adapter itself
	// answers, so the operator's assume_model_served escape hatch still applies.
	if _, ok := provider.Capability(model); !ok {
		served, listErr := adapter.Models(ctx)
		if listErr != nil {
			return listErr
		}
		return &local.ErrNotServed{BaseURL: adapter.BaseURL(), Model: model, Served: served}
	}

	_, origin, err := reg.String(flags.LLMLocalBaseURL)
	if err != nil {
		return err
	}
	baseSource := "discovered"
	if origin != flags.OriginDefault {
		baseSource = string(origin)
	}

	fmt.Fprintf(out, "base_url=%s\n", adapter.BaseURL())
	fmt.Fprintf(out, "base_url_source=%s\n", baseSource)
	fmt.Fprintf(out, "model=%s\n", model)
	fmt.Fprintf(out, "model_source=%s\n", source)
	return nil
}
