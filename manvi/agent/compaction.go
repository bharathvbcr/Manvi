package agent

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/prompt"
	"github.com/bharathvbcr/Manvi/manvi/session"
)

// Compaction shortens tool results to keep a turn inside the model's context
// window. Three properties make it safe, and each one is a defect this file
// was written to remove.
//
// It is *durable*. A compaction is appended to the session log and applied by
// the projection, so the history the model receives is the history the log
// describes. The previous implementation rewrote messages on the way out of the
// pre-step waterfall, which meant the log recorded a request that was never
// sent — and the model-visible-means-logged assertion, which is on by default,
// failed the moment compaction touched a multi-line tool result.
//
// It is *one-way*. A result compacted once keeps that exact text for the rest
// of the session. Recomputing it every step, as the old tiers did, changed the
// prompt prefix on every step.
//
// And it aims *past* the threshold rather than at it. A local server's KV cache
// is keyed on an unchanged token prefix: editing history invalidates it and
// costs a full re-prefill, measured at 120s for a 14.7k-token prompt on a 27B
// at 4-bit. Compaction that lands exactly on the threshold re-triggers almost
// immediately and pays that cost again. Landing well under it makes compaction
// a rare event, which is the only way it is affordable at all.

// On a provider that binds thinking to history, none of this runs.
//
// Anthropic signs each thinking block to the system prompt, the tools and every
// message before it, so rewriting an earlier tool result is an edit that
// invalidates every block after it (a 400 on an enforced account, the silent
// loss of that reasoning otherwise). The planner below is the client-side
// rewrite; for that provider the same job is done by the API's server-side
// context editing, which the history-editing check does not count as an edit
// because it compares the conversation as the client sent it (claude-api skill,
// shared/model-migration.md, "Breaking change 3": "server-side compaction and
// context editing ... don't count as edits"). The Anthropic adapter sends
// context_management with the clear_tool_uses_20250919 strategy under beta
// context-management-2025-06-27 (llm/anthropic.ContextEditsFor).
//
// What each operation of this file becomes there:
//
//	head-and-tail text cut of an old tool result   clear_tool_uses_20250919: the
//	    server replaces the whole result with placeholder text and keeps the
//	    most recent tool uses. Coarser, because the head and tail are not kept.
//	image removal with a notice (ImageRemovalMarker)   the same strategy: images
//	    are part of the cleared result and the placeholder stands in for the notice.
//	protected tail (ProtectedTail messages)   the strategy's keep, in tool uses.
//	headroom (CompactionHeadroomNum/Den)   the strategy's clear_at_least, so a
//	    clear is skipped unless it buys that much and the cache write is worth it.
//	summarisation   Manvi has none; threshold compaction (compact_20260112) is a
//	    different feature that the docs say not to combine with this one.
//
// A client-side edit that is still present on such a conversation is made safe
// by stripping, not by avoiding it: session.Log.StripStaleReasoning removes
// every thinking and redacted_thinking block from the first edited position
// onward (a leading run is the only removal that keeps later blocks valid), and
// keeps the blocks before it. Two cases remain.
//
// A log written before this change replays its ToolResultCompacted records.
//
// And a request that offers no tools (a ChatOnly run) has nothing for the
// server's clear_tool_uses strategy to act on, and the adapter does not send
// context_management without tools, because that combination is not documented.
// So ServerSideClearing is false for that request, the planner below runs, and
// its edit is followed by the strip. That edit lands only at a turn boundary,
// never while the last message is a tool result (see AtTurnBoundary): the turn
// in progress must go back with its reasoning intact.
//
// Providers that do not bind thinking (OpenAI-compatible, Gemini, local) keep
// everything below unchanged.
func ServerSideEditing(p llm.Provider) bool {
	return p != nil && llm.BindsThinkingToHistory(p.Name())
}

// ServerSideClearing reports whether the server will clear tool results for the
// request that offers tools: the provider edits server-side, and there is a tool
// set for the strategy to apply to.
func ServerSideClearing(p llm.Provider, tools []llm.ToolSchema) bool {
	return ServerSideEditing(p) && len(tools) > 0
}

// AtTurnBoundary reports whether history ends where a client-side edit may
// land: it does not end in tool results. A history whose last message carries a
// tool result is mid tool round, with an assistant turn whose tool_use has just
// been answered and whose signed reasoning must go back untouched.
func AtTurnBoundary(messages []llm.Message) bool {
	if len(messages) == 0 {
		return true
	}
	for _, b := range messages[len(messages)-1].Content {
		if _, ok := b.(llm.ToolResultBlock); ok {
			return false
		}
	}
	return true
}

// serverKeepToolUses is how many of the most recent tool uses the server keeps
// when it clears, which llm/anthropic.ContextEditsFor asks for. A test holds the
// two together.
const serverKeepToolUses = 3

// clearedPlaceholder stands for the text the server puts in a cleared result.
// Its exact wording is not documented; a short one is the honest estimate.
const clearedPlaceholder = "[tool result cleared]"

// EstimateAfterServerClearing estimates the request once the server has cleared
// every tool result but the most recent serverKeepToolUses, which is the most
// clearing can do. What is left is what only a smaller history or a bigger
// window can fix.
func EstimateAfterServerClearing(systemPrompt string, tools []llm.ToolSchema, messages []llm.Message) int {
	results := 0
	for _, m := range messages {
		for _, b := range m.Content {
			if _, ok := b.(llm.ToolResultBlock); ok {
				results++
			}
		}
	}
	clear := results - serverKeepToolUses
	out := make([]llm.Message, len(messages))
	for i, m := range messages {
		out[i] = m
		if clear <= 0 {
			continue
		}
		content := make([]llm.ContentBlock, len(m.Content))
		for j, b := range m.Content {
			content[j] = b
			if tr, ok := b.(llm.ToolResultBlock); ok && clear > 0 {
				clear--
				tr.Content = []llm.ContentBlock{llm.TextBlock{Text: clearedPlaceholder}}
				content[j] = tr
			}
		}
		out[i].Content = content
	}
	return CountRequestTokens(systemPrompt, tools, out)
}

const (
	// ProtectedTail is how many trailing messages are never compacted. A tool
	// loop alternates assistant and results, so this is roughly three
	// exchanges — the working set the model is actively reasoning over.
	ProtectedTail = 6

	// CompactionHeadroom is the fraction of the threshold compaction aims to
	// land at. Compacting to exactly the threshold means the next tool result
	// crosses it again and the prefix moves again.
	CompactionHeadroomNum = 7
	CompactionHeadroomDen = 10

	// The floors a tool result may be compacted to, tried widest-first so the
	// least information is destroyed that still buys the needed headroom.
	compactFloorWide   = 1200
	compactFloorMedium = 400
	compactFloorTight  = 120
)

var compactFloors = []int{compactFloorWide, compactFloorMedium, compactFloorTight}

// Budget is what a turn has to fit inside.
type Budget struct {
	// ContextWindow is the model's total token capacity.
	ContextWindow int
	// ReservedOutput is held back for the response.
	ReservedOutput int
	// Overhead covers chat-template scaffolding the harness does not model.
	Overhead int
}

// minThreshold is the floor a budget cannot fall below, and maxThreshold the
// ceiling it cannot rise above.
//
// The ceiling exists for arithmetic rather than for policy. Threshold feeds a
// multiplication in Target, and `int` is 64-bit on the platforms this ships on
// and 32-bit on others — so a budget near the top of the range wrapped to a
// *negative* target, and a negative target is a compaction goal the planner
// treats as already met. A declared context window of MaxInt64-4096 produced
// `threshold=9223372036854771711 target=922337203685474712`: a tenth of the
// threshold rather than seven tenths, which is the wrap wearing a plausible
// number's clothes.
//
// 1<<28 tokens is two orders of magnitude past any model that exists, and
// 1<<28 × 7 still fits in a 32-bit int, so the clamp makes the multiplication
// safe everywhere rather than only where it happened to be tested.
const (
	minThreshold = 4096
	maxThreshold = 1 << 28
)

// Threshold is the point at which history must be shortened.
//
// The subtraction is done in int64 because its operands are three independent
// caller-supplied numbers: a large ContextWindow against a negative
// ReservedOutput wraps in int, and the floor below would then read the wrapped
// value as a small budget rather than as nonsense.
func (b Budget) Threshold() int {
	t := int64(b.ContextWindow) - int64(b.ReservedOutput) - int64(b.Overhead)
	if t < minThreshold {
		t = minThreshold
	}
	if t > maxThreshold {
		t = maxThreshold
	}
	return int(t)
}

// Target is what compaction aims for once it runs.
func (b Budget) Target() int {
	return int(int64(b.Threshold()) * CompactionHeadroomNum / CompactionHeadroomDen)
}

// CompactionStep is one planned change: a tool result and the text that
// replaces it.
type CompactionStep struct {
	ToolCallID llm.CallID
	// Occurrence says which of the results answering ToolCallID this is,
	// counted from 1 in history order. A call id is not unique to a result, so
	// the step names the result and not only the id. Zero names every result
	// with the id, which is what a step built by hand without it always meant.
	Occurrence int
	Text       string
	FromBytes  int
	ToBytes    int
}

// CompactionPlan is the whole decision, including the case where it is not
// enough.
type CompactionPlan struct {
	Steps []CompactionStep
	// Before and After are estimated token totals.
	Before int
	After  int
	// Insufficient reports that every eligible result was compacted as far as
	// it goes and the history still exceeds the threshold. It is surfaced
	// rather than swallowed: at that point the turn is going to overflow the
	// server's window, and a harness that carried on silently would produce a
	// truncation the operator could not explain.
	Insufficient bool
}

// Empty reports whether the plan changes anything.
func (p CompactionPlan) Empty() bool { return len(p.Steps) == 0 }

// PlanCompactionCalibrated decides what to shorten, with the estimator
// corrected against what the server has been counting. A nil Calibrator leaves
// the estimate uncorrected.
//
// A result's text is cut to a floor, and its images, which are priced in the
// request like text, are removed whole and replaced by a notice that says how
// many (ImageRemovalMarker). A result of images alone is eligible: a history of
// screenshots can be reclaimed, and the model is told it once held them. A
// result without images is shortened exactly as before.
//
// already and done name tool results that carry a compaction from an earlier
// step; they are never revisited, which is what keeps the prompt prefix stable.
// already names call ids, and covers every result with the id: it is what a
// record that named only the id means. done names single results, by call id
// and place among the results with that id, because a provider may reuse an id
// and one result's compaction says nothing about the next.
func PlanCompactionCalibrated(
	messages []llm.Message,
	systemPrompt string,
	tools []llm.ToolSchema,
	budget Budget,
	already map[llm.CallID]struct{},
	cal *Calibrator,
	done ...llm.ResultKey,
) CompactionPlan {
	scale := cal.Ratio()
	estimate := func(text string) int { return int(float64(prompt.EstimateTokens(text)) * scale) }

	before := cal.Calibrated(CountRequestTokens(systemPrompt, tools, messages))
	plan := CompactionPlan{Before: before, After: before}
	if before <= budget.Threshold() {
		return plan
	}

	// A candidate is a result that may be shortened: its text, which the floors
	// cut, and its images, which compaction removes whole and says so. size is
	// the bytes the result holds, so the step's saving is audited in bytes as it
	// always was.
	type candidate struct {
		id         llm.CallID
		occurrence int
		text       string
		images     int
		size       int
		tokens     int
	}
	// shortened is the text the candidate is replaced with at a floor: its text
	// cut to the floor, then the notice for any images removed with it.
	shortened := func(c candidate, floor int) string {
		short := CompactToolResultText(c.text, floor)
		if c.images > 0 {
			short = withImageNotice(short, c.images)
		}
		return short
	}
	var eligible []candidate

	var compacted map[llm.ResultKey]struct{}
	if len(done) > 0 {
		compacted = make(map[llm.ResultKey]struct{}, len(done))
		for _, key := range done {
			compacted[key] = struct{}{}
		}
	}
	seen := make(map[llm.CallID]int)

	cutoff := len(messages) - ProtectedTail
	for i, msg := range messages {
		if i >= cutoff {
			break
		}
		for _, block := range msg.Content {
			tr, ok := block.(llm.ToolResultBlock)
			if !ok {
				continue
			}
			// Counted before any skip: the place of a result among those that
			// share its id does not depend on whether the earlier ones were
			// compacted.
			seen[tr.ToolCallID]++
			occurrence := seen[tr.ToolCallID]
			if _, skip := already[tr.ToolCallID]; skip {
				continue
			}
			if _, skip := compacted[llm.ResultKey{ID: tr.ToolCallID, Occurrence: occurrence}]; skip {
				continue
			}
			var body strings.Builder
			images, imageTokens, imageBytes := 0, 0, 0
			for _, inner := range tr.Content {
				switch b := inner.(type) {
				case llm.TextBlock:
					body.WriteString(b.Text)
				case llm.ImageBlock:
					// Counted in the request, so they have to be reclaimable: a
					// computer-use history is mostly screenshots, and a result
					// with nothing but images was never eligible, which left a
					// window full of them with no plan and Insufficient.
					images++
					imageTokens += ImageTokens(b)
					imageBytes += len(b.Data)
				}
			}
			text := body.String()
			if text == "" && images == 0 {
				continue
			}
			eligible = append(eligible, candidate{
				id: tr.ToolCallID, occurrence: occurrence, text: text, images: images,
				size:   len(text) + imageBytes,
				tokens: estimate(text) + int(float64(imageTokens)*scale),
			})
		}
	}
	if len(eligible) == 0 {
		// Over the threshold with nothing that may be shortened. That is the
		// most insufficient case there is, not an exception to it: the request
		// is going out oversized and the server will truncate it.
		//
		// It is easy to reach and was previously silent. ProtectedTail shields
		// the last six messages, so a turn shorter than that has no eligible
		// result at all — and one large Read early in a turn is enough to
		// exceed a small window before a sixth message exists.
		plan.Insufficient = true
		return plan
	}

	need := before - budget.Target()

	// Widest floor that could cover the need, so the least is destroyed. The
	// floors are tried in order rather than searched, because each candidate
	// must end this pass with exactly one final text — compacting a result
	// twice in one plan would defeat the point of the plan.
	chosen := compactFloors[len(compactFloors)-1]
	for _, floor := range compactFloors {
		total := 0
		for _, c := range eligible {
			total += c.tokens - estimate(shortened(c, floor))
		}
		if total >= need {
			chosen = floor
			break
		}
	}

	saved := 0
	for _, c := range eligible {
		if saved >= need {
			break
		}
		short := shortened(c, chosen)
		gain := c.tokens - estimate(short)
		if c.images == 0 {
			// Text alone: never longer than the input, as it always was.
			if len(short) >= len(c.text) {
				continue
			}
		} else if gain <= 0 {
			// An image smaller than its own notice is not worth removing.
			continue
		}
		saved += gain
		plan.Steps = append(plan.Steps, CompactionStep{
			ToolCallID: c.id, Occurrence: c.occurrence, Text: short,
			FromBytes: c.size, ToBytes: len(short),
		})
	}

	plan.After = before - saved
	plan.Insufficient = plan.After > budget.Threshold()
	return plan
}

// Apply writes the plan to the log. The projection picks it up from there.
//
// Each step records the sequence number of the result it shortens, resolved
// here from the step's call id and occurrence, so the record applies to that
// result alone. A step that names no occurrence is recorded by call id, which
// applies to every result with it. A step the log cannot resolve fails the
// whole Apply before anything is written.
func (p CompactionPlan) Apply(log *session.Log) error {
	var keys []llm.ResultKey
	for _, step := range p.Steps {
		if step.Occurrence > 0 {
			keys = append(keys, llm.ResultKey{ID: step.ToolCallID, Occurrence: step.Occurrence})
		}
	}
	seqs, err := log.ResultSeqs(keys)
	if err != nil {
		return err
	}
	next := 0
	for _, step := range p.Steps {
		data := session.CompactionData{
			ToolCallID: step.ToolCallID,
			Text:       step.Text,
			FromBytes:  step.FromBytes,
			ToBytes:    step.ToBytes,
		}
		if step.Occurrence > 0 {
			data.ResultSeq = seqs[next]
			next++
		}
		if _, err := log.Append(session.ToolResultCompacted, data); err != nil {
			return err
		}
	}
	return nil
}

// CompactionMarker is the sentinel a compacted result carries. It is a single
// constant because both the compactor and anything reading a transcript need to
// agree on what an elision looks like.
const CompactionMarker = "[%d line(s) omitted to fit the context window]"

// ImageRemovalMarker is the notice a compacted result carries for the images it
// held. Compaction replaces a result with text, so its images go with it, and
// the model must be told: left silent, a result that once showed a screenshot
// reads as one that never did.
const ImageRemovalMarker = "[%d image(s) removed to fit the context window]"

// withImageNotice appends the notice for n removed images to a result's text.
func withImageNotice(text string, n int) string {
	notice := fmt.Sprintf(ImageRemovalMarker, n)
	if text == "" {
		return notice
	}
	return text + "\n" + notice
}

// CompactToolResultText shortens one tool result to roughly maxChars, keeping
// the head and tail.
//
// Head and tail are kept rather than a plain prefix because tool output is
// shaped that way: a grep's first hits and last hits both matter, a compiler's
// first error and its summary line both matter, and a prefix cut keeps neither
// end of the answer. The elision is explicit so the model can tell the
// difference between a short result and a shortened one — an implicit cut reads
// as "the tool found this much", which is a fact that was never true.
func CompactToolResultText(text string, maxChars int) string {
	if maxChars <= 0 || len(text) <= maxChars {
		return text
	}

	lines := strings.Split(text, "\n")
	if len(lines) >= 8 {
		// The elision notice comes out of the budget rather than being added on
		// top of it. Splitting maxChars in half for the two ends and then
		// appending the marker made the result overshoot the floor it was
		// compacted to — measured at 8 bytes over the 1200 floor and 44 over
		// the 120 floor — and made this function non-idempotent, so compacting
		// an already-compacted result produced a third distinct string. Both
		// matter to the prefix cache: the floor is the number the planner
		// budgeted against, and any changed byte invalidates everything after
		// it. len(lines) is used for the marker's width because it is an upper
		// bound on the omitted count, so reserving it can never under-reserve.
		marker := fmt.Sprintf(CompactionMarker, len(lines))
		budget := maxChars - len(marker) - 1
		half := budget / 2
		var head []string
		used := 0
		for _, ln := range lines {
			if used+len(ln)+1 > half {
				break
			}
			head = append(head, ln)
			used += len(ln) + 1
		}
		var tail []string
		used = 0
		// From len(head), not len(head)+1: head occupies indices 0..len(head)-1,
		// so index len(head) is the first line the head did not take and there
		// is no overlap. Starting one later discarded a line that had budget.
		for i := len(lines) - 1; i >= len(head); i-- {
			if used+len(lines[i])+1 > half {
				break
			}
			tail = append([]string{lines[i]}, tail...)
			used += len(lines[i]) + 1
		}
		omitted := len(lines) - len(head) - len(tail)
		if omitted > 0 && len(head)+len(tail) > 0 {
			var b strings.Builder
			for _, ln := range head {
				b.WriteString(ln)
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, CompactionMarker+"\n", omitted)
			for i, ln := range tail {
				b.WriteString(ln)
				if i < len(tail)-1 {
					b.WriteString("\n")
				}
			}
			// Never longer than the input, and never over the floor it was
			// asked for. Failing either, fall through to the rune cut below
			// rather than return something that breaks the caller's budget.
			if out := b.String(); len(out) < len(text) && len(out) <= maxChars {
				return out
			}
		}
	}

	// Few lines, or the line-wise attempt did not help: cut on a rune boundary
	// so the result is still valid UTF-8, which a byte slice through a
	// multi-byte character would not be.
	runes := []rune(text)
	keep := maxChars
	if keep > len(runes) {
		keep = len(runes)
	}
	for keep > 0 && len(string(runes[:keep])) > maxChars {
		keep--
	}
	out := string(runes[:keep]) + "\n" + fmt.Sprintf(CompactionMarker, 0)

	// The elision notice has a length of its own, so on a short result it can
	// cost more than it saves. Compaction that makes the prompt bigger is
	// strictly worse than leaving it alone: it spends context and breaks the
	// prefix to achieve nothing.
	if len(out) >= len(text) {
		return text
	}
	return out
}

// CountRequestTokens estimates everything that goes out in one request.
//
// The tool schemas are included, which the previous accounting omitted. They
// are sent on every request and are not small: the shipped DevCouncil surface
// measures 1,755 tokens against a real Qwen tokenizer, so leaving them out
// understated every budget by that much in the direction that overflows.
func CountRequestTokens(systemPrompt string, tools []llm.ToolSchema, messages []llm.Message) int {
	total := prompt.EstimateTokens(systemPrompt)
	total += CountToolTokens(tools)
	for _, msg := range messages {
		total += countMessageBlocks(msg)
	}
	return total
}

// CountToolTokens estimates the serialized tool array.
func CountToolTokens(tools []llm.ToolSchema) int {
	total := 0
	for _, t := range tools {
		// Name, description and schema all cross the wire, plus the JSON
		// scaffolding around each entry.
		total += prompt.EstimateTokens(t.Name)
		total += prompt.EstimateTokens(t.Description)
		total += prompt.EstimateTokens(string(t.InputSchema))
		total += 8
	}
	return total
}

// maxImageTokens is the most visual tokens any image can cost: the
// high-resolution tier's limit (Claude 4.7 and later), which is also the
// price of an image whose size cannot be read.
const maxImageTokens = 4784

// imagePatch is the side, in pixels, of the block an image is priced in.
const imagePatch = 28

// ImageTokens estimates what one image costs in a request.
//
// Anthropic prices an image in 28x28-pixel patches: ceil(width/28) *
// ceil(height/28) visual tokens, with images beyond a model's resolution
// limits downscaled first, which caps the cost at 4,784 on the
// high-resolution tier and 1,568 on the standard tier
// (https://platform.claude.com/docs/en/build-with-claude/vision, "Resolution
// and token cost"). The estimate takes the high-resolution tier, the larger of
// the two, for every provider: it guards a context window, so it may only be
// wrong in the direction that compacts early. Other providers price images
// lower per image (Gemini tiles at 258 tokens), so the estimate stays an upper
// bound for them too, and the Calibrator corrects it from reported usage.
//
// Only the header is read, so the cost is the same on every step however many
// screenshots the history holds. An image whose size cannot be read (empty,
// truncated, or a format the standard library does not decode, such as WebP)
// costs the cap, never zero.
func ImageTokens(b llm.ImageBlock) int {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b.Data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return maxImageTokens
	}
	w := (cfg.Width + imagePatch - 1) / imagePatch
	h := (cfg.Height + imagePatch - 1) / imagePatch
	if w > maxImageTokens/h {
		return maxImageTokens
	}
	return min(w*h, maxImageTokens)
}

func countMessageBlocks(msg llm.Message) int {
	total := 0
	for _, b := range msg.Content {
		switch block := b.(type) {
		case llm.TextBlock:
			total += prompt.EstimateTokens(block.Text)
		case llm.ReasoningBlock:
			total += prompt.EstimateTokens(block.Text)
		case llm.ImageBlock:
			total += ImageTokens(block)
		case llm.ToolResultBlock:
			for _, inner := range block.Content {
				switch ib := inner.(type) {
				case llm.TextBlock:
					total += prompt.EstimateTokens(ib.Text)
				case llm.ImageBlock:
					total += ImageTokens(ib)
				}
			}
			total += 8
		case llm.ToolCallBlock:
			total += prompt.EstimateTokens(block.Name)
			total += prompt.EstimateTokens(string(block.Arguments))
			total += 12
		}
	}
	// Per-message chat-template scaffolding: role marker and delimiters.
	return total + 4
}

// Calibrator corrects the token estimate against what the server actually
// counted.
//
// The estimator is a heuristic over bytes; the server has the tokenizer.
// Measured against Qwen3.8-27B's own tokenizer the estimate runs about 25% high
// overall and 58% high on JSON — which is the shape of a tool result — so
// compaction fires earlier than it needs to and destroys context the model
// could have kept. The correction costs nothing: prompt_tokens already arrives
// on every response because the adapter asks for it.
//
// The ratio is smoothed rather than taken from the last sample alone. A single
// step is a noisy measurement — a cached prefix, a template change, a system
// prompt that grew — and letting one reading move the budget by a third would
// make compaction fire at a different point each step for no reason the
// operator could see.
type Calibrator struct {
	ratio   float64
	samples int
}

// CalibrationWeight is how many samples the running average is spread over.
const CalibrationWeight = 4

// CalibrationBounds keep a wild reading from making the budget nonsense.
//
// A ratio outside this range means the estimate and the count are not measuring
// the same thing — a request that was rejected, a server reporting a cumulative
// total, a proxy rewriting the prompt — and a budget derived from it would be
// worse than the uncalibrated one.
const (
	CalibrationMin = 0.4
	CalibrationMax = 2.5
)

// Observe records one (estimated, actual) pair.
func (c *Calibrator) Observe(estimated, actual int) {
	if estimated <= 0 || actual <= 0 {
		return
	}
	ratio := float64(actual) / float64(estimated)
	if ratio < CalibrationMin || ratio > CalibrationMax {
		return
	}
	if c.samples == 0 {
		c.ratio = ratio
	} else {
		weight := float64(CalibrationWeight)
		c.ratio = (c.ratio*(weight-1) + ratio) / weight
	}
	c.samples++
}

// Ratio is the multiplier to apply to an estimate, or 1 before any observation.
func (c *Calibrator) Ratio() float64 {
	if c == nil || c.samples == 0 || c.ratio <= 0 {
		return 1
	}
	return c.ratio
}

// Calibrated scales an estimate by the observed ratio.
func (c *Calibrator) Calibrated(estimate int) int {
	if c.Ratio() == 1 {
		return estimate
	}
	return int(float64(estimate) * c.Ratio())
}

// Samples reports how many observations back the ratio, so a caller can say
// whether a budget is measured or merely estimated.
func (c *Calibrator) Samples() int {
	if c == nil {
		return 0
	}
	return c.samples
}
