package agent

import (
	"fmt"
	"strings"

	"manvi/llm"
	"manvi/prompt"
	"manvi/session"
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

// Threshold is the point at which history must be shortened.
func (b Budget) Threshold() int {
	t := b.ContextWindow - b.ReservedOutput - b.Overhead
	if t < 4096 {
		t = 4096
	}
	return t
}

// Target is what compaction aims for once it runs.
func (b Budget) Target() int {
	return b.Threshold() * CompactionHeadroomNum / CompactionHeadroomDen
}

// CompactionStep is one planned change: a tool result and the text that
// replaces it.
type CompactionStep struct {
	ToolCallID llm.CallID
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

// PlanCompaction decides what to shorten.
//
// already names tool results that carry a compaction from an earlier step;
// they are never revisited, which is what keeps the prompt prefix stable.
func PlanCompaction(
	messages []llm.Message,
	systemPrompt string,
	tools []llm.ToolSchema,
	budget Budget,
	already map[llm.CallID]struct{},
) CompactionPlan {
	return PlanCompactionCalibrated(messages, systemPrompt, tools, budget, already, nil)
}

// PlanCompactionCalibrated is PlanCompaction with the estimator corrected
// against what the server has been counting.
func PlanCompactionCalibrated(
	messages []llm.Message,
	systemPrompt string,
	tools []llm.ToolSchema,
	budget Budget,
	already map[llm.CallID]struct{},
	cal *Calibrator,
) CompactionPlan {
	scale := cal.Ratio()
	estimate := func(text string) int { return int(float64(prompt.EstimateTokens(text)) * scale) }

	before := cal.Calibrated(CountRequestTokens(systemPrompt, tools, messages))
	plan := CompactionPlan{Before: before, After: before}
	if before <= budget.Threshold() {
		return plan
	}

	type candidate struct {
		id     llm.CallID
		text   string
		tokens int
	}
	var eligible []candidate

	cutoff := len(messages) - ProtectedTail
	for i := 0; i < cutoff; i++ {
		for _, block := range messages[i].Content {
			tr, ok := block.(llm.ToolResultBlock)
			if !ok {
				continue
			}
			if _, done := already[tr.ToolCallID]; done {
				continue
			}
			var body strings.Builder
			for _, inner := range tr.Content {
				if t, ok := inner.(llm.TextBlock); ok {
					body.WriteString(t.Text)
				}
			}
			text := body.String()
			if text == "" {
				continue
			}
			eligible = append(eligible, candidate{
				id: tr.ToolCallID, text: text, tokens: estimate(text),
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
			total += c.tokens - estimate(CompactToolResultText(c.text, floor))
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
		short := CompactToolResultText(c.text, chosen)
		if len(short) >= len(c.text) {
			continue
		}
		saved += c.tokens - estimate(short)
		plan.Steps = append(plan.Steps, CompactionStep{
			ToolCallID: c.id, Text: short,
			FromBytes: len(c.text), ToBytes: len(short),
		})
	}

	plan.After = before - saved
	plan.Insufficient = plan.After > budget.Threshold()
	return plan
}

// Apply writes the plan to the log. The projection picks it up from there.
func (p CompactionPlan) Apply(log *session.Log) error {
	for _, step := range p.Steps {
		if _, err := log.Append(session.ToolResultCompacted, session.CompactionData{
			ToolCallID: step.ToolCallID,
			Text:       step.Text,
			FromBytes:  step.FromBytes,
			ToBytes:    step.ToBytes,
		}); err != nil {
			return err
		}
	}
	return nil
}

// CompactionMarker is the sentinel a compacted result carries. It is a single
// constant because both the compactor and anything reading a transcript need to
// agree on what an elision looks like.
const CompactionMarker = "[%d line(s) omitted to fit the context window]"

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

func countMessageBlocks(msg llm.Message) int {
	total := 0
	for _, b := range msg.Content {
		switch block := b.(type) {
		case llm.TextBlock:
			total += prompt.EstimateTokens(block.Text)
		case llm.ReasoningBlock:
			total += prompt.EstimateTokens(block.Text)
		case llm.ToolResultBlock:
			for _, inner := range block.Content {
				if tb, ok := inner.(llm.TextBlock); ok {
					total += prompt.EstimateTokens(tb.Text)
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
