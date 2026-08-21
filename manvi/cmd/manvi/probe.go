package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"manvi/credentials"
	"manvi/flags"
	"manvi/llm"
	"manvi/llm/local"
)

// probe makes one real request against a provider.
//
// It exists because of a gap that no amount of local testing closes. The three
// adapters are tested against scripted servers that replay captured event
// sequences, which proves the decoders handle the shapes they were written
// against — and proves nothing about whether those shapes are what the provider
// currently sends. Every constant here was transcribed from documentation, and
// documentation is a claim about an API, not the API.
//
// So this is deliberately not a unit test. It costs money, needs a credential,
// and reaches the public internet, none of which belong in `go test`. It is a
// command an operator runs, and it is the only thing in this repository that
// can honestly report that an adapter works.
func probe(out io.Writer, reg *flags.Registry, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: manvi probe PROVIDER [--model NAME] [--effort LEVEL]\n" +
			"  providers: " + strings.Join(providerNames(), ", "))
	}
	name := args[0]
	model := flagValue(args[1:], "--model")
	// Named on the command line rather than read from llm.effort, for the same
	// reason the model is: a probe reports on the request it was asked to make.
	// Silently folding in ambient configuration would make one command's result
	// depend on an environment the operator is not looking at, and this is the
	// command whose whole value is that its result is unambiguous.
	effort := flagValue(args[1:], "--effort")

	if !knownProvider(name) {
		// Ahead of the credential lookup, so a typo reads as a typo rather than
		// as a missing key for a provider that does not exist.
		return fmt.Errorf("unknown provider %q (%s)", name, strings.Join(providerNames(), ", "))
	}

	resolver := credentials.NewResolver()
	secret, err := resolver.Resolve(name)
	if err != nil {
		// The credential is missing, which is a configuration answer rather
		// than a verdict on the adapter. Saying "probe failed" here would let
		// an unconfigured machine read as a broken adapter.
		return fmt.Errorf("cannot probe %s: %w", name, err)
	}

	provider, defaultModel, err := adapterFor(name, reg, resolver, out)
	if err != nil {
		return err
	}
	if model == "" {
		model = defaultModel
	}
	if model == "" {
		// The local provider has no documented cheap default, because its
		// served set is whatever the operator pulled. Resolve it the way a
		// session does, and if that cannot answer either, say what the server
		// actually offers rather than probing an empty model name.
		model, _, err = resolveModelFor(context.Background(), name, provider, reg)
		if err != nil {
			return err
		}
	}

	// The probe offers a tool, and this is the point of it.
	//
	// It used to send text only, so it validated the text path and returned a
	// green verdict while the tool-calling path went untested — and tool calling
	// is the path that actually stops a harness working. A server without a
	// parser for the model it serves leaves the call in the message content,
	// where the loop sees no tool calls and reads a turn that did nothing as a
	// turn that finished.
	probeTool := llm.ToolSchema{
		Name:        "manvi_probe_echo",
		Description: "Echo a word back. Used only to verify tool calling works.",
		InputSchema: json.RawMessage(
			`{"type":"object","properties":{"word":{"type":"string"}},"required":["word"]}`),
	}

	req := llm.Request{
		Model:  model,
		Effort: effort,
		// Deliberately trivial and deterministic. The probe is asking whether
		// the wire contract holds, not whether the model is any good, and a
		// long generation makes it slower and dearer for no extra signal.
		System: "You verify tool calling. Call manvi_probe_echo with the word \"ok\". Call the tool; do not answer in prose.",
		Messages: []llm.Message{{
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{llm.TextBlock{Text: `Call manvi_probe_echo with word="ok".`}},
		}},
		Tools:     []llm.ToolSchema{probeTool},
		MaxTokens: 256,
	}
	if cap, ok := provider.Capability(model); ok && !cap.SupportsTools {
		// A model declared without tools cannot be asked to prove it has them,
		// and Resolve would refuse the request before it left the process.
		req.Tools = nil
		req.System = "Reply with exactly the word: ok"
	}

	if effort != "" {
		fmt.Fprintf(out, "probing %s/%s at effort %s %s\n\n", name, model, effort, credentialNote(secret))
	} else {
		fmt.Fprintf(out, "probing %s/%s %s\n\n", name, model, credentialNote(secret))
	}

	// Cold prefill of a large quantised model is measured in minutes — 120s for
	// a 14.7k-token prompt on a 4-bit 27B — so a one-minute ceiling failed on
	// models that were merely cold rather than broken. The probe's own prompt is
	// tiny; what this has to cover is loading the weights.
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout(name))
	defer cancel()

	started := time.Now()
	stream, err := provider.Stream(ctx, req)
	if err != nil {
		return fmt.Errorf("the request was refused before any content arrived: %w", err)
	}
	defer stream.Close()

	// A scrubber armed with this run's credential, because a provider that
	// echoes the key back inside an error body would otherwise print it to the
	// terminal — which is where an operator copies from when filing a bug.
	scrubber := credentials.NewScrubber()
	scrubber.Watch(secret)

	var text strings.Builder
	kinds := map[llm.ChunkKind]int{}
	for {
		chunk, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("the stream failed after %d chunk(s): %s",
				total(kinds), scrubber.Clean(err.Error()))
		}
		kinds[chunk.Kind]++
		if chunk.Kind == llm.ChunkText {
			text.WriteString(chunk.Text)
		}
	}

	response, err := stream.Response()
	if err != nil {
		return fmt.Errorf("the stream did not settle: %s", scrubber.Clean(err.Error()))
	}
	elapsed := time.Since(started)

	calls := response.Message.ToolCalls()
	fmt.Fprintf(out, "  reply        %q\n", scrubber.Clean(strings.TrimSpace(text.String())))
	if len(req.Tools) > 0 {
		switch {
		case len(calls) == 0:
			fmt.Fprintf(out, "  tool call    none\n")
		case response.Decoding.FallbackFormat != "":
			// The distinction an operator needs: the model did ask for the
			// tool, and the *server* did not parse it, so the harness read it
			// out of the message text. That works, but it is a misconfigured
			// server and it will keep costing correctness elsewhere.
			fmt.Fprintf(out, "  tool call    %s (recovered by the harness from %s text — "+
				"the server did not parse it)\n", calls[0].Name, response.Decoding.FallbackFormat)
		default:
			fmt.Fprintf(out, "  tool call    %s (parsed by the server)\n", calls[0].Name)
		}
	}
	if response.Usage.CacheReuse() >= 0 {
		fmt.Fprintf(out, "  prefix cache %.0f%% of the prompt reused\n", 100*response.Usage.CacheReuse())
	}
	if response.Usage.OutputTokensPerSecond > 0 {
		fmt.Fprintf(out, "  throughput   %.1f tok/s\n", response.Usage.OutputTokensPerSecond)
	}
	if local, ok := provider.(*local.Adapter); ok {
		fmt.Fprintf(out, "  context      %s\n", local.Dimensions(context.Background(), model).Describe())
	}
	fmt.Fprintf(out, "  stop reason  %s\n", response.StopReason)
	fmt.Fprintf(out, "  usage        %d in / %d out", response.Usage.InputTokens, response.Usage.OutputTokens)
	if response.Usage.ReasoningTokens > 0 {
		fmt.Fprintf(out, " / %d reasoning", response.Usage.ReasoningTokens)
	}
	fmt.Fprintf(out, "\n  chunks       %s\n  elapsed      %s\n\n", describe(kinds), elapsed.Round(time.Millisecond))

	// The checks that make this a verdict rather than a transcript.
	var complaints []string
	if response.Decoding.PrefillDisproved {
		// The one this command exists to catch. Nothing in a transcript shows
		// it: the answer simply is not there, and the turn reads as a model
		// that had nothing to say.
		complaints = append(complaints, fmt.Sprintf(
			"%s is set, and this server contradicts it: it delivers reasoning on its own channel, "+
				"so what arrives on the content channel is already the answer. The declaration was "+
				"dropped for this probe. Left set, the filter starts inside a thinking block that "+
				"never closes and files every answer as reasoning — turns then end with no answer "+
				"at all. Unset it for this server",
			flags.LLMLocalAssumePrefill))
	}
	if len(req.Tools) > 0 {
		if len(calls) == 0 {
			complaints = append(complaints,
				"the model was given one tool and asked to call it, and no tool call arrived. "+
					"Either the server does not implement tool calling for this model, or it is "+
					"emitting calls in a shape neither it nor this harness parses — in which case "+
					"an agent turn will end reporting success with no work done")
		}
		if response.Decoding.FallbackFormat != "" {
			complaints = append(complaints,
				"the tool call had to be recovered from the message text: the server is not "+
					"parsing tool calls for this model. Start it with the tool parser its chat "+
					"template needs; the harness can compensate but should not have to")
		}
	} else if strings.TrimSpace(text.String()) == "" {
		complaints = append(complaints,
			"no text arrived; the decoder saw a stream but produced nothing, which is what a shape change looks like")
	}
	if len(response.Malformed) > 0 {
		complaints = append(complaints, fmt.Sprintf(
			"%d tool call(s) could not be reconstructed: %s",
			len(response.Malformed), response.Malformed[0].Reason))
	}
	if response.Usage.InputTokens == 0 && response.Usage.OutputTokens == 0 {
		if name == local.Name {
			fmt.Fprintln(out, "  NOTICE       no token usage was reported by the local server; token estimation will be used")
		} else {
			complaints = append(complaints,
				"no token usage was reported; a turn that cannot be costed cannot be budgeted")
		}
	}
	if response.Message.Provenance == nil || response.Message.Provenance.Provider != name {
		complaints = append(complaints,
			"the settled message carries no provenance; the next turn cannot tell whether its reasoning is replayable")
	}
	if response.StopReason == llm.StopOther {
		complaints = append(complaints,
			"the stop reason did not map to a known value; this adapter's mapping is out of date")
	}
	if len(complaints) > 0 {
		for _, c := range complaints {
			fmt.Fprintf(out, "  PROBLEM      %s\n", c)
		}
		return fmt.Errorf("%s answered, but %d contract check(s) failed", name, len(complaints))
	}

	fmt.Fprintf(out, "  OK — %s's live wire contract matches what this adapter was built against\n", name)
	return nil
}

// adapterFor builds one provider adapter and names a default model to probe.
//
// Both halves are delegated to the single provider owner in providers.go, so a
// newly wired adapter is probeable without anyone remembering to edit this
// switch as well — which is exactly how the local adapter came to be listed
// everywhere and constructible nowhere.
func adapterFor(name string, reg *flags.Registry, resolver *credentials.Resolver, notes io.Writer) (llm.Provider, string, error) {
	provider, err := buildProvider(name, reg, resolver, notes)
	if err != nil {
		return nil, "", err
	}
	return provider, probeModel(name), nil
}

func describe(kinds map[llm.ChunkKind]int) string {
	if len(kinds) == 0 {
		return "none"
	}
	var parts []string
	for _, kind := range []llm.ChunkKind{
		llm.ChunkText, llm.ChunkReasoning, llm.ChunkToolCallStart, llm.ChunkToolCallDelta,
	} {
		if n := kinds[kind]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, kind))
		}
	}
	return strings.Join(parts, ", ")
}

func total(kinds map[llm.ChunkKind]int) int {
	n := 0
	for _, count := range kinds {
		n += count
	}
	return n
}

// credentialNote describes which credential a probe used, including the case
// where there is none because the provider does not require one. Printing
// "with the credential from " and then nothing would read as a bug in the
// resolver rather than the normal state of a loopback server.
func credentialNote(secret credentials.Secret) string {
	if !secret.Present() {
		return "with no credential (this provider does not require one)"
	}
	return "with the credential from " + secret.Source()
}

// probeTimeout bounds one probe request.
//
// A hosted provider answers a trivial prompt in seconds. A local server may
// have to load tens of gigabytes of weights and prefill before the first token,
// which is measured in minutes on a large quantised model — so the same ceiling
// for both would either fail on cold local models or wait absurdly long on a
// hosted outage.
func probeTimeout(provider string) time.Duration {
	if provider == local.Name {
		return 10 * time.Minute
	}
	return 60 * time.Second
}
