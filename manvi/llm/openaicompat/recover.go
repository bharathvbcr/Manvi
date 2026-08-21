package openaicompat

import (
	"strings"

	"manvi/llm"
)

// The stream path owns tool-call recovery and reasoning separation, and both
// are unexported because nothing outside this package used to need them. A
// host driving its own HTTP client does: it has the finished assistant text
// and the same two problems the stream has, and reimplementing either against
// the same models would produce a second answer to a question that already has
// one. These are thin delegations, never copies — the parsers below are the
// ones the streaming path runs.

// RecoveredCall is one tool call read out of plain assistant text.
type RecoveredCall struct {
	// Name is the tool, already validated against the offered schemas.
	Name string `json:"name"`
	// Arguments is a JSON object. Types come from the declared schema rather
	// than from the text, which matters most for the XML spelling: it carries
	// no types at all, and guessing turns a mode string of "0755" into the
	// number 755.
	Arguments string `json:"arguments"`
}

// Recovery is what could be read out of a response the server did not parse.
type Recovery struct {
	// Text is the assistant text with any recovered call markup removed. When
	// nothing was recovered it is the input unchanged.
	Text string `json:"text"`
	// Reasoning is thinking separated out of the text.
	Reasoning string `json:"reasoning,omitempty"`
	// Calls are the recovered tool calls, in the order the model emitted them.
	Calls []RecoveredCall `json:"calls,omitempty"`
	// Format names the spelling that was recognised: "" when the server
	// parsed the calls itself (or there were none), otherwise "hermes-json",
	// "qwen-xml" or "fenced-json".
	//
	// A host should surface a non-empty value rather than swallow it. It means
	// the server is running without a tool parser for the model it serves —
	// recovery works, but the same gap costs correctness elsewhere, and a
	// silent compensation is one nobody ever fixes.
	Format string `json:"format,omitempty"`
	// Reclassified reports that an unmatched closing think tag proved the
	// block had been open from the first byte, so text already treated as the
	// answer was in fact reasoning.
	Reclassified bool `json:"reclassified,omitempty"`
}

// RecoverFromText reads reasoning and tool calls out of finished assistant
// text.
//
// Order matters and is not interchangeable: reasoning is separated first,
// because a model that thinks out loud about calling a tool will write
// something that looks like a call inside its think block, and extracting that
// would dispatch a call the model was only considering.
//
// assumePrefill declares that the chat template ends the prompt with an open
// thinking tag, as Qwen3's does. Leaving it false still recovers the case, via
// the unmatched-closing-tag rule; setting it makes the classification right
// from the first byte instead of retroactively.
func RecoverFromText(text string, tools []llm.ToolSchema, assumePrefill bool) Recovery {
	visible, reasoning, reclassified := SplitReasoning(text, assumePrefill)

	cleaned, blocks, format := extractFallbackToolCalls(visible, tools)
	out := Recovery{
		Text:         cleaned,
		Reasoning:    reasoning,
		Format:       string(format),
		Reclassified: reclassified,
	}
	for _, b := range blocks {
		out.Calls = append(out.Calls, RecoveredCall{
			Name:      b.Name,
			Arguments: string(b.Arguments),
		})
	}
	return out
}

// SplitReasoning separates inline <think>…</think> reasoning from visible text
// in a finished response, using the same filter the streaming path uses.
//
// The third return reports that an unmatched closing tag was seen, which is
// what a server prefilling the opening tag looks like from here. It is a
// report, not a transformation: the text stays text. Acting on it used to move
// everything before the tag into reasoning, which deleted the answer whenever
// the model was merely writing *about* think tags — routine in a harness whose
// own source is full of them. Nothing in the byte stream separates the two
// cases, so the caller is told and AssumeReasoningPrefill is how an operator
// answers for a server that really does prefill.
func SplitReasoning(text string, assumePrefill bool) (visible, reasoning string, reclassified bool) {
	if text == "" {
		return "", "", false
	}

	f := &tagFilter{assumePrefill: assumePrefill}
	var vis, think strings.Builder

	apply := func(c filteredChunk) {
		vis.WriteString(c.text)
		think.WriteString(c.reasoning)
	}

	for _, c := range f.feed(text) {
		apply(c)
	}
	// Whatever is still held as a possible partial tag. A response truncated
	// mid-thought ends here, and dropping the carry would silently lose the
	// tail of the answer.
	apply(f.flush())

	return vis.String(), think.String(), f.prefillSuspected
}
