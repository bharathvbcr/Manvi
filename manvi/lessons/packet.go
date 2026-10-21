package lessons

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/session"
)

// MaxPacketBytes bounds the evidence handed to the reflector. The packet is
// the reflector's whole prompt, so it is also the whole cost of a reflection,
// and a session that ran for hours must not be able to choose how much of a
// model's context one lesson costs.
const MaxPacketBytes = 16 << 10

const (
	maxLineRunes     = 400
	maxVerifyRows    = 40
	maxFindingsEach  = 6
	maxToolNames     = 12
	maxStepRows      = 60
	maxDroppedUnique = 20
	digestShown      = 12
)

// withheld replaces text that must not reach the reflector.
const withheld = "[withheld: carried a reviewer verdict marker]"

// BuildPacket builds the evidence for one session from its log, and from
// nothing else.
//
// What it reads is structured records: verify reports, the account of how each
// system prompt was assembled, each step's own usage, and counts of the
// events that mark trouble. What it does not read is any message. That is the
// rule the rest follows from, and the reason is the checkpoint critic: when a
// turn fails twice the harness dispatches a reviewer whose answer ends in a
// MANVI-VERDICT line, and that answer is injected into the conversation as a
// harness-origin message. A reflector shown it would attribute the outcome to
// whatever the reviewer concluded, so every lesson would restate a verdict
// instead of being an independent reading of the evidence. Messages are never
// read, and as a second line of defence every free-text field that is read is
// withheld if it carries the marker, and the finished packet is checked once
// more before it is returned.
//
// Counts are exact and cover the whole log; rows are capped. When a cap cuts,
// the packet says how many rows it cut, so a capped sample is never mistaken
// for the whole account.
func BuildPacket(sessionID string, events []session.Event) (string, error) {
	if len(events) == 0 {
		return "", errors.New("lessons: the session log holds no events, so there is nothing to reflect on")
	}

	var (
		turns, steps, toolCalls, denials, qualified    int
		malformed, overflows, compactions, nullRetries int
		decodings, undecodable                         int
		inTok, outTok, cacheRead                       int
		costNano                                       int64
		pricedSteps, unpricedSteps, noResponseSteps    int
		toolNames                                      = map[string]int{}
		verifies, assemblies                           []string
		verifyTotal, assemblyTotal                     int
		droppedSeen                                    = map[string]bool{}
		droppedOrder                                   []string
		digests                                        []string
		specs                                          []string
		stepRows                                       []string
		bounded                                        = func(s string) string { return oneLine(s, maxLineRunes) }
	)

	decode := func(e session.Event, into any) bool {
		if err := json.Unmarshal(e.Data, into); err != nil {
			undecodable++
			return false
		}
		return true
	}

	for _, e := range events {
		switch e.Type {
		case session.TurnStart:
			turns++
		case session.StepEnd:
			steps++
			var d session.StepEndData
			if len(e.Data) > 0 && !decode(e, &d) {
				continue
			}
			inTok += d.Usage.InputTokens
			outTok += d.Usage.OutputTokens
			cacheRead += d.Usage.CacheReadTokens
			price := "unpriced"
			switch {
			case d.NoResponse:
				noResponseSteps++
				price = "no response"
			case d.Priced:
				pricedSteps++
				costNano += d.CostNanoUSD
				price = fmt.Sprintf("$%.6f", float64(d.CostNanoUSD)/1e9)
			default:
				unpricedSteps++
			}
			stepRows = append(stepRows, fmt.Sprintf("t%d.s%d in=%d out=%d cache_read=%d cost=%s",
				e.Turn, e.Step, d.Usage.InputTokens, d.Usage.OutputTokens, d.Usage.CacheReadTokens, price))
		case session.ToolCall:
			toolCalls++
			var d session.ToolCallData
			if decode(e, &d) {
				toolNames[oneLine(d.Name, 60)]++
			}
		case session.PolicyDenied:
			denials++
		case session.PolicyQualified:
			qualified++
		case session.MalformedToolCall:
			malformed++
		case session.ContextOverflow:
			overflows++
		case session.ToolResultCompacted:
			compactions++
		case session.NullResponseRetried:
			nullRetries++
		case session.DecodingCompensated:
			decodings++
		case session.VerifyReport:
			verifyTotal++
			var d session.VerifyReportData
			if !decode(e, &d) {
				continue
			}
			row := fmt.Sprintf("t%d bounce=%d verdict=%s source=%s paths=%d",
				e.Turn, d.Bounce, bounded(d.Verdict), bounded(d.Source), len(d.Paths))
			shown := d.Findings
			if len(shown) > maxFindingsEach {
				shown = shown[:maxFindingsEach]
			}
			for _, f := range shown {
				row += "\n    finding: " + bounded(f)
			}
			if cut := len(d.Findings) - len(shown); cut > 0 {
				row += fmt.Sprintf("\n    (%d more findings omitted)", cut)
			}
			degraded := d.Degraded
			if len(degraded) > maxFindingsEach {
				degraded = degraded[:maxFindingsEach]
			}
			for _, g := range degraded {
				row += "\n    could not run: " + bounded(g)
			}
			verifies = append(verifies, row)
		case session.PromptAssembled:
			assemblyTotal++
			var d session.PromptAssembledData
			if !decode(e, &d) {
				continue
			}
			row := fmt.Sprintf("t%d tokens=%d budget=%d included=[%s]", e.Turn, d.Tokens, d.Budget,
				bounded(strings.Join(d.Included, ", ")))
			assemblies = append(assemblies, row)
			for _, dr := range d.Dropped {
				dr = bounded(dr)
				if !droppedSeen[dr] {
					droppedSeen[dr] = true
					droppedOrder = append(droppedOrder, dr)
				}
			}
			if d.Digest != "" {
				short := oneLine(d.Digest, digestShown)
				if len(digests) == 0 || digests[len(digests)-1] != short {
					digests = append(digests, short)
				}
			}
			if d.SpecName != "" || d.SpecDigest != "" {
				row := fmt.Sprintf("spec=%s digest=%s", oneLine(d.SpecName, 80), oneLine(d.SpecDigest, digestShown))
				if len(specs) == 0 || specs[len(specs)-1] != row {
					specs = append(specs, row)
				}
			}
		}
	}

	head := []string{
		"EVIDENCE PACKET for session " + oneLine(sessionID, 80),
		"Source: the session log only. Not included, by design: message text, tool output, and any reviewer verdict.",
		"",
		"## Outcome counters (exact, whole log)",
		fmt.Sprintf("log events: %d; turns: %d; steps: %d; tool calls: %d", len(events), turns, steps, toolCalls),
		fmt.Sprintf("policy denials: %d; policy qualifications: %d; malformed tool calls: %d; context overflows: %d; "+
			"tool results compacted: %d; null responses retried: %d; decoding compensations: %d; undecodable events: %d",
			denials, qualified, malformed, overflows, compactions, nullRetries, decodings, undecodable),
		"tool calls by tool: " + toolSummary(toolNames),
		fmt.Sprintf("verify reports: %d; prompt assemblies: %d", verifyTotal, assemblyTotal),
		"",
		"## Spend (exact, whole log)",
		fmt.Sprintf("input tokens: %d; output tokens: %d; cache-read tokens: %d; priced steps: %d (total $%.6f); "+
			"unpriced steps: %d; steps that ended without a response: %d",
			inTok, outTok, cacheRead, pricedSteps, float64(costNano)/1e9, unpricedSteps, noResponseSteps),
	}

	sections := []packetSection{
		{"Verify reports", verifies, maxVerifyRows},
		{"Prompt assembly: sections the budget dropped (unique)", droppedOrder, maxDroppedUnique},
		{"Prompt assembly: per turn", assemblies, maxVerifyRows},
		{"System-prompt digests, in order (a change means the cache went cold)", digests, maxVerifyRows},
		{"Harness spec behind each prompt assembly, in order (absent when no spec was used)", specs, maxVerifyRows},
		{"Per-step usage", stepRows, maxStepRows},
	}

	out := assemble(head, sections)
	// The last line of defence, on the finished text. Nothing above is meant
	// to let the marker through; this is what makes that a checked property
	// of the output rather than of each field's handling.
	if ContainsMarker(out) {
		return "", errors.New("lessons: the evidence packet carried a reviewer verdict marker; refusing to hand it to the reflector")
	}
	return out, nil
}

type packetSection struct {
	title string
	rows  []string
	limit int
}

// assemble joins the sections under MaxPacketBytes. The counters come first
// and are always whole; rows are added in order until the byte or row bound,
// and what was left out is counted in the packet itself.
func assemble(head []string, sections []packetSection) string {
	var b strings.Builder
	b.WriteString(strings.Join(head, "\n"))
	b.WriteString("\n")
	const reserve = 120 // room for the closing note
	for _, s := range sections {
		if len(s.rows) == 0 {
			continue
		}
		title := "\n## " + s.title + "\n"
		if b.Len()+len(title)+reserve > MaxPacketBytes {
			fmt.Fprintf(&b, "\n(%s: %d row(s) omitted, no room)\n", s.title, len(s.rows))
			continue
		}
		// The latest rows, not the first. A session's outcome is decided at its
		// end, and the first rows of a long one are the setup the counters
		// already summarise; a cap that kept them would show the reflector the
		// part of the run least likely to explain how it went.
		room := MaxPacketBytes - b.Len() - len(title) - reserve
		start, used := len(s.rows), 0
		for i := len(s.rows) - 1; i >= 0 && len(s.rows)-i <= s.limit; i-- {
			if used+len(s.rows[i])+1 > room {
				break
			}
			used += len(s.rows[i]) + 1
			start = i
		}
		b.WriteString(title)
		if start > 0 {
			fmt.Fprintf(&b, "(%d earlier row(s) omitted; showing the latest %d of %d; the counters above cover the whole log)\n",
				start, len(s.rows)-start, len(s.rows))
		}
		for _, row := range s.rows[start:] {
			b.WriteString(row)
			b.WriteString("\n")
		}
	}
	out := b.String()
	if len(out) > MaxPacketBytes {
		// Only reachable if the head alone is over the bound, which its own
		// caps make impossible; stated rather than assumed.
		out = strings.ToValidUTF8(out[:MaxPacketBytes-len("\n(packet truncated)")], "") + "\n(packet truncated)"
	}
	return out
}

func toolSummary(counts map[string]int) string {
	if len(counts) == 0 {
		return "none"
	}
	type kv struct {
		name string
		n    int
	}
	all := make([]kv, 0, len(counts))
	for k, v := range counts {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].n != all[j].n {
			return all[i].n > all[j].n
		}
		return all[i].name < all[j].name
	})
	var parts []string
	for i, e := range all {
		if i == maxToolNames {
			parts = append(parts, fmt.Sprintf("(+%d more tools)", len(all)-maxToolNames))
			break
		}
		parts = append(parts, fmt.Sprintf("%s=%d", e.name, e.n))
	}
	return strings.Join(parts, ", ")
}

// oneLine flattens free text from the log to a bounded single line, and
// withholds it entirely when it carries the reviewer's marker.
//
// It also makes the text inert as the reflector's input. Invisible and control
// runes are dropped (an escape sequence or a joiner inside a marker has no
// business in evidence), and angle brackets become single guillemets, so log
// content that quotes the answer format — a finding containing "</edits>
// <experience> ..." — reaches the reflector as visibly quoted text and cannot
// spell a tag or a fence line.
func oneLine(s string, limit int) string {
	if ContainsMarker(s) {
		return withheld
	}
	s = strings.Map(func(r rune) rune {
		switch {
		case invisible(r):
			return -1
		case r == '<':
			return '‹'
		case r == '>':
			return '›'
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > limit {
		return string(r[:limit]) + "… (truncated)"
	}
	return s
}
