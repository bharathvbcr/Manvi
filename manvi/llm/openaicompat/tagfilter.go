package openaicompat

import (
	"strings"
)

// tagFilter separates inline reasoning from visible text.
//
// Two shapes have to work. The ordinary one is a matched pair: the model emits
// <think>…</think> inside the content stream and the reasoning is what lies
// between them.
//
// The other is a *prefilled* opening tag. Qwen3's chat template ends the
// generation prompt with a bare "<think>", so the model begins generating
// inside the block and the only tag it ever emits is the closing one. Read as
// ordinary text that put the whole chain of thought into the answer, left a
// literal "</think>" in the output, and — because it was stored as text rather
// than reasoning — replayed all of it to the model on every later step.
//
// An unmatched closing tag is therefore read as evidence that the block was
// open from the start, and the content before it is *retroactively* reclassified
// as reasoning. Retroactively, rather than by holding text back until the
// question is settled: buffering would delay every visible token on the servers
// that never prefill, which is most of them. The chunks already emitted are
// spent, but the settled message is what gets logged and replayed, and that is
// what this corrects. An operator who wants the live view right from the first
// byte declares the prefill instead.
type tagFilter struct {
	// depth counts open think blocks. One would do if models never nested
	// them, and the previous shape assumed so: an inner </think> closed the
	// outer block, so "<think>outer <think>inner</think> still-thinking
	// </think>ANSWER" put " still-thinking" and the answer on the same side and
	// left the outer closing tag to be dropped as stray framing. The settled
	// message is what gets logged and replayed, so the model's private
	// reasoning became part of its answer and was fed back on every later step.
	//
	// Counting costs the opposite error — a model that writes a literal
	// "<think>" *inside* its reasoning now needs two closing tags — but that
	// direction fails safe: the excess is classified as reasoning, and an
	// unclosed block is flushed as reasoning at end of stream. Leaking
	// reasoning into an answer is the failure that cannot be undone.
	depth   int
	inThink bool
	carry   string
	// resolved records that the prefill question is settled — an opening tag
	// arrived, or an unmatched closing tag was seen.
	resolved bool
	// prefillSuspected records that an unmatched closing tag arrived, which is
	// what a server that prefills the opening tag looks like from here. It is
	// reported, never acted on: acting on it deleted answers.
	prefillSuspected bool
	// assumePrefill starts the filter inside a think block, for a server the
	// operator has declared prefills the tag.
	assumePrefill bool
	// started records that assumePrefill has been applied.
	started bool
	// assumed records that the filter is inside a think block it was told
	// about rather than one it saw opened. Only that state may be cancelled by
	// outOfBandReasoning; a block opened by a real tag is not in question.
	assumed bool
	// visibleTail is the last few bytes handed to the visible channel, kept so
	// framing removal can be checked against everything already emitted rather
	// than only against the run immediately before it.
	//
	// A pairwise check is not enough: "<t</think>hink</think>>" deletes two
	// tags, and neither deletion fuses a marker on its own, but the three
	// surviving runs concatenate to "<think>". The consumer sees one stream, so
	// the comparison has to be against that stream.
	visibleTail string
	// prefillDisproved records that the declared prefill was contradicted by
	// the server itself. Reported, so an operator can correct the setting
	// rather than discovering it as missing answers.
	prefillDisproved bool
	// inToolCall suspends tag interpretation for the body of a <tool_call>
	// block, because those bytes are a payload the model was asked to produce
	// and not prose it is narrating.
	//
	// Without it, a tool argument whose *value* contains "</think>" had the tag
	// deleted from the value on its way past: write(s="please strip </think>
	// markers") reached the tool as "please strip  markers", the file landed on
	// disk with content nobody asked for, and nothing anywhere reported the
	// edit. The same bytes also set prefillSuspected, so the harness told the
	// operator their server might be prefilling <think> on the strength of a
	// closing tag the model had been *instructed* to write.
	//
	// The filter runs over the raw byte stream and extractFallbackToolCalls
	// runs after it, so the stream is the only place that can know the
	// difference — by the time the parser sees the text the bytes are already
	// gone. Hence the flag here rather than a repair downstream.
	//
	// Suspension preserves the current classification rather than forcing
	// text: a block opened inside a think block stays reasoning. That keeps
	// both truncation directions fail-safe. A <tool_call> opened in the answer
	// and never closed leaves the tail as text, so no answer is deleted; one
	// opened inside reasoning and never closed leaves the tail as reasoning, so
	// no private chain of thought leaks into the answer. Nothing is buffered
	// while waiting for the closer — protected bytes are emitted as they
	// arrive — so an unclosed block costs no memory and cannot defeat
	// MaxDecodedResponseBytes.
	inToolCall bool
}

var (
	openTags  = []string{"<think>", "<thought>"}
	closeTags = []string{"</think>", "</thought>"}
	allTags   = []string{"<think>", "<thought>", "</think>", "</thought>"}
)

// The tool-call markers the filter has to recognise. Only <tool_call>…
// </tool_call> is protected, which is exactly what extractTaggedCalls treats as
// a payload — the filter and the parser must agree on where a call begins and
// ends or one of them is protecting bytes the other is not reading.
//
// That is why the inner Qwen spellings, <function=…> and <parameter=…>, are not
// listed. They are not protected in their own right because they are never
// recognised in their own right: parseCallPayload only ever sees them inside a
// <tool_call> body, so protecting the outer block already covers every
// parameter value inside it, and protecting a bare <function= would suspend
// reasoning separation for text that can never become a call.
//
// The fenced-JSON spelling (```json {…} ```) is deliberately left unprotected,
// and it keeps the residual: a fenced call whose argument string contains a
// literal "</think>" still loses it. Protecting fences would cost far more than
// it buys. A fence is ordinary prose in a coding harness — models write code
// blocks constantly, inside their reasoning most of all — whereas a literal
// "<tool_call>" is machinery a model only emits when it means it. Protecting
// every fence would therefore suspend reasoning separation across a large
// fraction of real reasoning, and a fence opened *inside* a think block would
// swallow the closing </think> and turn the entire remaining answer into
// reasoning. Worse, a fence cannot be recognised as a call until its body is
// complete and parsed, which in a stream means buffering the whole fence before
// deciding — unbounded, against a cap this package exists to respect. The
// tagged spellings are decidable from the opening marker alone; the fenced one
// is not.
const (
	toolCallOpen  = "<tool_call>"
	toolCallClose = "</tool_call>"
)

var (
	toolCallCloseTags = []string{toolCallClose}
	// carryTags is what partialSuffix must consider while tag interpretation is
	// live, so a <tool_call> opener split across two SSE frames is held back
	// exactly as a split <think> is. A server that happened to break the frame
	// after "<tool" would otherwise strip the payload anyway, which makes the
	// protection decorative rather than real.
	//
	// It is a separate list from allTags on purpose: allTags is the set of tags
	// that must never survive into content, and <tool_call> must survive —
	// extractTaggedCalls is what reads it.
	carryTags = []string{"<think>", "<thought>", "</think>", "</thought>", toolCallOpen}
)

// classify labels a run of bytes with whichever side of the filter is currently
// open, so protected tool-call payload keeps the classification it was opened
// in instead of being forced onto one side.
// emit records a chunk and keeps visibleTail current. Every visible emission
// goes through here so the tail cannot silently fall behind.
func (f *tagFilter) emit(out []filteredChunk, c filteredChunk) []filteredChunk {
	if c.text != "" {
		tail := f.visibleTail + c.text
		if n := longestCarryTag() - 1; n > 0 && len(tail) > n {
			tail = tail[len(tail)-n:]
		}
		f.visibleTail = tail
	}
	return append(out, c)
}

func longestCarryTag() int {
	longest := 0
	for _, t := range carryTags {
		if len(t) > longest {
			longest = len(t)
		}
	}
	return longest
}

func (f *tagFilter) classify(s string) filteredChunk {
	if f.inThink {
		return filteredChunk{reasoning: s}
	}
	return filteredChunk{text: s}
}

type filteredChunk struct {
	text      string
	reasoning string
}

// findFirst returns the earliest occurrence of any tag, and its length.
func findFirst(data string, tags []string) (int, int) {
	idx, tagLen := -1, 0
	for _, tag := range tags {
		at := strings.Index(data, tag)
		if at < 0 {
			continue
		}
		if idx < 0 || at < idx {
			idx, tagLen = at, len(tag)
		}
	}
	return idx, tagLen
}

// partialSuffix returns how many trailing bytes could be the start of a tag, so
// a tag split across two deltas is not mistaken for content.
func partialSuffix(data string, tags []string) int {
	longest := 0
	for _, tag := range tags {
		for l := len(tag) - 1; l >= 1; l-- {
			if strings.HasSuffix(data, tag[:l]) && l > longest {
				longest = l
			}
		}
	}
	return longest
}

// deltaReasoning picks whichever of the two out-of-band reasoning spellings a
// server used. A server sending both would be sending the same text twice, so
// the first non-empty one wins rather than being concatenated.
func deltaReasoning(reasoningContent, reasoning *string) string {
	if reasoningContent != nil && *reasoningContent != "" {
		return *reasoningContent
	}
	if reasoning != nil && *reasoning != "" {
		return *reasoning
	}
	return ""
}

// outOfBandReasoning tells the filter that the server delivered reasoning on
// its own channel, and reports whether that contradicted a declared prefill.
//
// assumePrefill exists because some servers begin generation inside a thinking
// tag and never send the opening one, so the content stream arrives already
// inside a block. A server that puts reasoning in its own field is doing the
// opposite: it has separated the two itself, and what arrives on the content
// channel is the answer, outside any block.
//
// Believing the declaration over the wire deletes answers. Measured against
// omlx 0.6.2 serving Qwen3.8-27B on 2026-08-19, with
// llm.local.assume_reasoning_prefill true: the server streamed its thinking as
// reasoning_content deltas and the answer as content "\n\nDONE", with no tag
// anywhere. The filter, told it had begun inside a block, classified the whole
// answer as reasoning and flushed it as reasoning at end of stream. Every text
// answer in a four-task benchmark was lost this way — three of the four turns
// also made no edits, because the model could no longer see its own prose in
// the history it was replayed.
//
// The setting is not ignored, and this is not a guess: it is the server
// contradicting the operator, in the one way that cannot be mistaken for
// anything else. The contradiction is reported rather than silently absorbed,
// so the setting gets corrected instead of quietly compensated for forever.
func (f *tagFilter) outOfBandReasoning() bool {
	if !f.assumePrefill {
		return false
	}
	// Never applied yet: disarm it before feed() ever acts on it.
	if !f.started {
		f.assumePrefill = false
		f.prefillDisproved = true
		return true
	}
	// Applied, and still inside the block it invented. Step out. A block that
	// a real tag opened is not touched — f.assumed is false for those.
	if f.assumed {
		f.inThink = false
		f.depth = 0
		f.assumed = false
		f.assumePrefill = false
		f.prefillDisproved = true
		return true
	}
	return false
}

// wouldFuse reports whether deleting framing between these two runs would splice
// a marker into existence.
//
// Deleting framing makes the bytes either side of it adjacent. They were never
// adjacent in the model's output, and their join can form a marker nothing
// downstream can tell from a real one. Found by fuzzing:
//
//	"<t</think>hink>"  ->  visible text "<think>"
//
// which contradicts this file's own invariant that no think tag survives into
// content. The consequential form manufactures a tool call:
//
//	"<tool</think>_call>{\"name\":\"write_file\",…}</tool_call>"
//	 -> "<tool_call>{\"name\":\"write_file\",…}</tool_call>"
//	 -> RecoverFromText returns a write_file call the model never made
//
// extractFallbackToolCalls only recovers names that were offered, so a
// fabricated call always names a real tool.
//
// When this fires the tag is kept as literal text instead of dropped. That is
// the trade this file already made once, in the other direction and for the
// same reason: "Leaking reasoning into a visible answer is a cosmetic fault the
// operator can see and fix; deleting the answer is neither." A stray "</think>"
// in an answer is visible and harmless. A tool call the model never wrote is
// neither.
func wouldFuse(before, after string) bool {
	longest := 0
	for _, t := range carryTags {
		if len(t) > longest {
			longest = len(t)
		}
	}
	if longest < 2 {
		return false
	}
	if len(before) > longest-1 {
		before = before[len(before)-(longest-1):]
	}
	if len(after) > longest-1 {
		after = after[:longest-1]
	}
	if before == "" || after == "" {
		return false
	}
	joined := before + after
	for _, t := range carryTags {
		for i := 0; i+len(t) <= len(joined); i++ {
			if joined[i:i+len(t)] != t {
				continue
			}
			// Only a match that actually spans the seam is fabricated; one
			// wholly inside either side was already there.
			if i < len(before) && i+len(t) > len(before) {
				return true
			}
		}
	}
	return false
}

func (f *tagFilter) feed(raw string) []filteredChunk {
	if !f.started {
		f.started = true
		if f.assumePrefill {
			f.inThink = true
			f.depth = 1
			f.resolved = true
			f.assumed = true
		}
	}

	data := f.carry + raw
	f.carry = ""
	var out []filteredChunk

	for len(data) > 0 {
		if f.inToolCall {
			// Inside a tool-call block nothing is a tag. These bytes are a
			// payload — a path, a patch, a file the model was asked to write —
			// and a "</think>" among them is a character the tool is supposed
			// to receive, not framing to drop and not evidence about the
			// server's chat template. prefillSuspected is unreachable from
			// here, which is the point: it must describe the template, never
			// the file the model happens to be writing.
			//
			// The first "</tool_call>" ends the block, matching
			// extractTaggedCalls byte for byte. A value containing a literal
			// "</tool_call>" ends the call early for the parser too, so filter
			// and parser agree about where the payload stopped rather than
			// disagreeing silently.
			closeIdx := strings.Index(data, toolCallClose)
			if closeIdx >= 0 {
				end := closeIdx + len(toolCallClose)
				out = f.emit(out, f.classify(data[:end]))
				data = data[end:]
				f.inToolCall = false
				continue
			}
			// Only the closing marker can matter now, so only its prefixes are
			// held back — at most len("</tool_call>")-1 bytes. Everything else
			// is emitted as it arrives: a block that never closes must not grow
			// a buffer, which is what keeps this inside MaxDecodedResponseBytes
			// instead of defeating it.
			if partial := partialSuffix(data, toolCallCloseTags); partial > 0 {
				if safe := data[:len(data)-partial]; safe != "" {
					out = f.emit(out, f.classify(safe))
				}
				f.carry = data[len(data)-partial:]
				break
			}
			out = f.emit(out, f.classify(data))
			break
		}

		if !f.inThink {
			toolIdx := strings.Index(data, toolCallOpen)
			closeIdx, closeLen := findFirst(data, closeTags)
			openIdx, openLen := findFirst(data, openTags)

			// A tool-call opener arriving before any think tag suspends tag
			// interpretation for what follows. The marker itself is emitted
			// rather than swallowed as framing, because unlike a think tag it
			// is not framing: extractFallbackToolCalls reads it, and a filter
			// that ate it would leave the parser nothing to recognise.
			//
			// Matching is exact, as it is for the think tags — see
			// TestTagMatchingIsDeliberatelyCaseAndSpaceExact. A loose match
			// here would let prose that merely mentions the marker switch the
			// reasoning filter off for the rest of the response.
			if toolIdx >= 0 && (openIdx < 0 || toolIdx < openIdx) && (closeIdx < 0 || toolIdx < closeIdx) {
				if toolIdx > 0 {
					out = f.emit(out, f.classify(data[:toolIdx]))
				}
				end := toolIdx + len(toolCallOpen)
				out = f.emit(out, f.classify(data[toolIdx:end]))
				data = data[end:]
				f.inToolCall = true
				continue
			}

			if closeIdx >= 0 && (openIdx < 0 || closeIdx < openIdx) {
				if !f.resolved {
					// A closing tag with nothing open *might* mean the opening
					// tag was prefilled into the prompt — or it might just be a
					// model writing about think tags, which in a harness whose
					// own source is full of them is the common case, not the
					// exotic one.
					//
					// This used to resolve that ambiguity by guessing prefill
					// and retroactively moving everything before the tag into
					// reasoning. When the guess was wrong it deleted the answer
					// from the settled message, not merely from the live view:
					// "To strip reasoning you remove everything up to the
					// closing tag </think> in the buffer" came back as " in the
					// buffer." A wrong guess here is unrecoverable and silent.
					//
					// Nothing in the byte stream can tell the two apart, so the
					// stream is no longer asked to. The text stays text, the tag
					// is dropped as framing, and the suspicion is reported
					// instead of acted on — see prefillSuspected, which tells
					// the operator to set AssumeReasoningPrefill if their server
					// really does prefill. Leaking reasoning into a visible
					// answer is a cosmetic fault the operator can see and fix;
					// deleting the answer is neither.
					//
					// Reaching this at all means the tag was outside tool-call
					// markup. One inside a payload is a character the model was
					// asked to write and says nothing about the template, so the
					// protected branch above never gets here.
					f.resolved = true
					f.prefillSuspected = true
				}
				// A stray closing tag once the shape is already known. It is
				// template framing, not content, and emitting it as text is
				// how "</think>" ends up in an answer.
				//
				// The bytes either side of it are about to become adjacent, so
				// a tag-length tail is withheld and re-scanned rather than
				// emitted blind. See seamHold.
				rest := data[closeIdx+closeLen:]
				if wouldFuse(f.visibleTail+data[:closeIdx], rest) {
					// Keep the tag rather than splice a marker into being.
					out = f.emit(out, filteredChunk{text: data[:closeIdx+closeLen]})
					data = rest
					continue
				}
				if closeIdx > 0 {
					out = f.emit(out, filteredChunk{text: data[:closeIdx]})
				}
				data = rest
				continue
			}

			if openIdx >= 0 {
				f.resolved = true
				f.assumed = false
				if openIdx > 0 {
					out = f.emit(out, filteredChunk{text: data[:openIdx]})
				}
				data = data[openIdx+openLen:]
				f.inThink = true
				f.depth = 1
				continue
			}

			if partial := partialSuffix(data, carryTags); partial > 0 {
				if safe := data[:len(data)-partial]; safe != "" {
					out = f.emit(out, filteredChunk{text: safe})
				}
				f.carry = data[len(data)-partial:]
				break
			}

			out = f.emit(out, filteredChunk{text: data})
			break
		}

		toolIdx := strings.Index(data, toolCallOpen)
		closeIdx, closeLen := findFirst(data, closeTags)
		// A nested opening tag deepens the block rather than being ignored.
		// The tag itself is still framing and is dropped, so no think tag ever
		// survives into content, but the matching closing tag now closes the
		// nesting instead of the outer block.
		nestIdx, nestLen := findFirst(data, openTags)

		// A tool-call block opened inside reasoning is protected on the same
		// terms and stays classified as reasoning. A model that thinks about
		// calling a tool writes the whole block out inside its think block, and
		// leaving that unprotected let the payload's own "</think>" close the
		// real block early: the rest of the deliberation, the stray closing
		// tag and the parameter markup all landed in the answer. The considered
		// call is still not dispatched — it is reasoning, and
		// extractFallbackToolCalls only ever sees visible text.
		if toolIdx >= 0 && (nestIdx < 0 || toolIdx < nestIdx) && (closeIdx < 0 || toolIdx < closeIdx) {
			if toolIdx > 0 {
				out = f.emit(out, f.classify(data[:toolIdx]))
			}
			end := toolIdx + len(toolCallOpen)
			out = f.emit(out, f.classify(data[toolIdx:end]))
			data = data[end:]
			f.inToolCall = true
			continue
		}

		if nestIdx >= 0 && (closeIdx < 0 || nestIdx < closeIdx) {
			if nestIdx > 0 {
				out = f.emit(out, filteredChunk{reasoning: data[:nestIdx]})
			}
			data = data[nestIdx+nestLen:]
			f.depth++
			continue
		}
		if closeIdx >= 0 {
			if closeIdx > 0 {
				out = f.emit(out, filteredChunk{reasoning: data[:closeIdx]})
			}
			data = data[closeIdx+closeLen:]
			f.depth--
			if f.depth <= 0 {
				f.depth = 0
				f.inThink = false
				f.assumed = false
			}
			f.resolved = true
			continue
		}
		if partial := partialSuffix(data, carryTags); partial > 0 {
			if safe := data[:len(data)-partial]; safe != "" {
				out = f.emit(out, filteredChunk{reasoning: safe})
			}
			f.carry = data[len(data)-partial:]
			break
		}
		out = f.emit(out, filteredChunk{reasoning: data})
		break
	}
	return out
}

// flush releases whatever is still held when the stream ends. Content inside an
// open block is reasoning even though the model never closed it, which is what
// a response truncated mid-thought looks like.
//
// A stream that ends inside a <tool_call> block is the same shape one step
// along: a response cut off at max_tokens can open the block and never close
// it. The held bytes are released on whichever side the block was opened on and
// are never dropped — they exist nowhere else. Because the marker never closed,
// extractTaggedCalls writes the whole unterminated block back into the text as
// prose and recovers no call from it, which is the rule that has to keep
// holding: half a payload is not a request.
func (f *tagFilter) flush() filteredChunk {
	if f.carry == "" {
		return filteredChunk{}
	}
	pending := f.carry
	f.carry = ""
	return f.classify(pending)
}
