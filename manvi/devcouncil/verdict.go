package devcouncil

import (
	"fmt"
	"strconv"
	"strings"
)

// A judging child answers in prose and in one machine-readable line. This file
// is the reader for that line.
//
// It exists because "completed" does not mean "passed". A child's status is set
// from whether its summary was non-empty, so a critic that ran, found three
// defects and described them at length is reported exactly like one that found
// none: both completed, both have a summary, and an advance rule reading that
// field advances on either. Scraping the prose for a word like PASSED is not a
// fix, it is a hope — "PASSED" appears in "this has not PASSED", in a quoted
// test name, and in a sentence explaining what passing would require.
//
// So the contract is narrow and stated to the child: one line, at the start of
// a line, the marker followed by PASS or FAIL. Everything about the reader
// fails closed. No line is no judgement. An unreadable line is no judgement.
// Two lines that disagree is no judgement — a child that said both is a child
// whose answer cannot be acted on, and picking one of them would be inventing
// the tie-break. And a pass that still lists findings is downgraded by
// SubAgentVerdict.Reconcile, because those two cannot both be true.
//
// No judgement is never a pass. That asymmetry is the whole design: the cost of
// refusing to advance a turn that was fine is one more look by a person, and
// the cost of advancing one that was not is a defect certified by a machine
// that did not read its own verdict.

// VerdictMarker is the line prefix a judging child is asked to emit. It is
// deliberately not a word that occurs in ordinary prose about reviewing code.
const VerdictMarker = "MANVI-VERDICT:"

// MaxVerdictFindings bounds how many objections travel with a verdict.
//
// The findings reach a session event and, on a failure, the next turn's model
// context. A judge that lists ninety is not more informative than one that
// lists the first twelve, and an unbounded list is a child deciding how much of
// the parent's context window to spend.
const MaxVerdictFindings = 12

// MaxVerdictFindingRunes bounds one objection's length, for the same reason.
const MaxVerdictFindingRunes = 400

// VerdictInstruction is what a judging child is told, appended to its role
// prompt. Stated as a requirement with its failure mode named, because a child
// that omits the line is not refused — it is recorded as having reached no
// judgement, and it should know that is what silence buys.
func VerdictInstruction() string {
	return "End your answer with a single line in exactly this form:\n" +
		"  " + VerdictMarker + " PASS\n" +
		"or\n" +
		"  " + VerdictMarker + " FAIL\n" +
		"followed, when you fail it, by one line per objection beginning \"- \".\n" +
		"A missing, unreadable or self-contradicting line is recorded as no judgement, " +
		"which is treated as a failure to certify — not as approval."
}

// ParseVerdict reads a child's answer for the verdict line.
//
// marker empty means the caller asked for no judgement, and none is produced:
// an ordinary builder's summary must never be mined for a word that happens to
// look like a verdict.
func ParseVerdict(marker, summary string) SubAgentVerdict {
	marker = strings.TrimSpace(marker)
	if marker == "" {
		return SubAgentVerdict{}
	}

	var (
		decided  bool
		passed   bool
		conflict bool
		findings []string
	)
	for line := range strings.Lines(summary) {
		trimmed := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(trimmed, marker); ok {
			word := strings.ToUpper(strings.TrimSpace(rest))
			// Compared whole rather than by prefix. "PASSES ONLY IF" starts
			// with PASS, and a reader that matched on the prefix would certify
			// on a sentence saying the opposite.
			var claim bool
			switch word {
			case "PASS":
				claim = true
			case "FAIL":
				claim = false
			default:
				// A marker line nobody can read is not a judgement, and it is
				// not a silent one either: it means the child tried to answer
				// and the answer did not arrive.
				conflict = true
				continue
			}
			if decided && passed != claim {
				// Two markers, disagreeing. There is no honest tie-break, so
				// the whole verdict is void.
				conflict = true
			}
			decided, passed = true, claim
			continue
		}
		// Objections are read only after the marker, because that is where the
		// contract puts them: "followed, when you fail it, by one line per
		// objection". Anything bulleted before it is the judge's own working —
		// "- checked the boundary cases", "- read the call sites" — and
		// counting that as an objection would turn every thorough review into a
		// contradiction Reconcile then downgrades, which is a gate that can
		// never pass anything.
		//
		// The consequence at the other end is deliberate: a bullet after a PASS
		// line *is* a contradiction, because the contract says those lines only
		// follow a failure. Reconcile refuses it, and that is the direction to
		// be wrong in.
		if !decided {
			continue
		}
		if objection, ok := strings.CutPrefix(trimmed, "- "); ok {
			objection = strings.TrimSpace(objection)
			if objection == "" || len(findings) >= MaxVerdictFindings {
				continue
			}
			findings = append(findings, TruncateRunes(objection, MaxVerdictFindingRunes))
		}
	}

	if conflict || !decided {
		return SubAgentVerdict{Judged: false}
	}
	// Handed to Reconcile whole rather than pre-cleaned. An earlier draft
	// dropped the findings when the claim was PASS, which quietly disarmed the
	// one check that exists for a judge contradicting itself: the pass went out
	// certified because the evidence against it had been deleted on the way.
	return SubAgentVerdict{Judged: true, Passed: passed, Findings: findings}.Reconcile()
}

// TruncateRunes cuts on rune boundaries and says that it cut. A silently
// shortened finding reads as a complete one.
func TruncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "… (truncated)"
}

// The per-criterion form of the same contract.
//
// A task's completion is not one question but one per acceptance criterion, so
// the judge is asked for one marker line per criterion, "MANVI-VERDICT[n]:
// PASS|FAIL", n counting from 1. Each criterion's lines go through ParseVerdict
// under that criterion's own marker, so every rule above — whole-word compare,
// disagreeing lines void the verdict, a pass listing objections is downgraded —
// applies to each criterion unchanged rather than being restated for a second
// reader that could drift from the first.
//
// A criterion with no line is "unaddressed" and is unmet: silence about a
// criterion is not evidence that it holds.

// Criterion statuses. Everything except CriterionMet is not a pass.
const (
	CriterionMet         = "met"
	CriterionUnmet       = "unmet"
	CriterionUnaddressed = "unaddressed"
	CriterionConflict    = "conflict"
)

// CriterionMarker is the marker line prefix for acceptance criterion n, counted
// from 1.
func CriterionMarker(n int) string {
	return "MANVI-VERDICT[" + strconv.Itoa(n) + "]:"
}

// CriterionVerdictInstruction tells a judge the shape its answer must take for
// count criteria, with the failure mode of silence named.
func CriterionVerdictInstruction(count int) string {
	return fmt.Sprintf("Judge each of the %d numbered acceptance criteria separately. For every criterion n "+
		"(1 to %d) end your answer with one line in exactly this form:\n"+
		"  MANVI-VERDICT[n]: PASS\nor\n  MANVI-VERDICT[n]: FAIL\n"+
		"followed, when you fail it, by one line per objection beginning \"- \". "+
		"A criterion with no line, an unreadable line or two lines that disagree is recorded as not met.",
		count, count)
}

// CriterionJudgement is the reading of one criterion's marker lines.
type CriterionJudgement struct {
	// Index is the criterion's number, from 1.
	Index int `json:"index"`
	// Status is one of the Criterion* constants.
	Status string `json:"status"`
	// Findings are the judge's objections, bounded as ParseVerdict bounds them.
	Findings []string `json:"findings,omitempty"`
}

// CriterionVerdicts is a judge's answer read against count criteria.
type CriterionVerdicts struct {
	// Emitted is whether the answer held any marker line at all, readable or
	// not. False means the judge reached no judgement of any kind.
	Emitted bool `json:"emitted"`
	// Items has exactly one entry per criterion, in order.
	Items []CriterionJudgement `json:"items"`
}

// ParseCriterionVerdicts reads a judge's answer for count criteria.
func ParseCriterionVerdicts(summary string, count int) CriterionVerdicts {
	out := CriterionVerdicts{Items: make([]CriterionJudgement, count)}
	buckets := make([]strings.Builder, count+1)
	seen := make([]bool, count+1)
	current := 0
	for line := range strings.Lines(summary) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "MANVI-VERDICT") {
			out.Emitted = true
			current = 0
			if rest, ok := strings.CutPrefix(trimmed, "MANVI-VERDICT["); ok {
				if digits, _, found := strings.Cut(rest, "]:"); found {
					if n, err := strconv.Atoi(digits); err == nil && n >= 1 && n <= count {
						current = n
						seen[n] = true
					}
				}
			}
		}
		if current > 0 {
			// #nosec G104 -- strings.Builder writes cannot fail.
			buckets[current].WriteString(line)
			if !strings.HasSuffix(line, "\n") {
				// #nosec G104 -- strings.Builder writes cannot fail.
				buckets[current].WriteString("\n")
			}
		}
	}
	for n := 1; n <= count; n++ {
		item := CriterionJudgement{Index: n, Status: CriterionUnaddressed}
		if seen[n] {
			verdict := ParseVerdict(CriterionMarker(n), buckets[n].String())
			switch {
			case !verdict.Judged:
				item.Status = CriterionConflict
			case verdict.Passed:
				item.Status = CriterionMet
			default:
				item.Status = CriterionUnmet
				item.Findings = verdict.Findings
			}
		}
		out.Items[n-1] = item
	}
	return out
}
