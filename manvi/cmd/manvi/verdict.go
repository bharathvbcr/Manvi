package main

import (
	"strings"

	"manvi/devcouncil"
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

// verdictMarker is the line prefix a judging child is asked to emit. It is
// deliberately not a word that occurs in ordinary prose about reviewing code.
const verdictMarker = "MANVI-VERDICT:"

// maxVerdictFindings bounds how many objections travel with a verdict.
//
// The findings reach a session event and, on a failure, the next turn's model
// context. A judge that lists ninety is not more informative than one that
// lists the first twelve, and an unbounded list is a child deciding how much of
// the parent's context window to spend.
const maxVerdictFindings = 12

// maxVerdictFindingRunes bounds one objection's length, for the same reason.
const maxVerdictFindingRunes = 400

// verdictInstruction is what a judging child is told, appended to its role
// prompt. Stated as a requirement with its failure mode named, because a child
// that omits the line is not refused — it is recorded as having reached no
// judgement, and it should know that is what silence buys.
func verdictInstruction() string {
	return "End your answer with a single line in exactly this form:\n" +
		"  " + verdictMarker + " PASS\n" +
		"or\n" +
		"  " + verdictMarker + " FAIL\n" +
		"followed, when you fail it, by one line per objection beginning \"- \".\n" +
		"A missing, unreadable or self-contradicting line is recorded as no judgement, " +
		"which is treated as a failure to certify — not as approval."
}

// parseVerdict reads a child's answer for the verdict line.
//
// marker empty means the caller asked for no judgement, and none is produced:
// an ordinary builder's summary must never be mined for a word that happens to
// look like a verdict.
func parseVerdict(marker, summary string) devcouncil.SubAgentVerdict {
	marker = strings.TrimSpace(marker)
	if marker == "" {
		return devcouncil.SubAgentVerdict{}
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
			if objection == "" || len(findings) >= maxVerdictFindings {
				continue
			}
			findings = append(findings, truncateRunes(objection, maxVerdictFindingRunes))
		}
	}

	if conflict || !decided {
		return devcouncil.SubAgentVerdict{Judged: false}
	}
	// Handed to Reconcile whole rather than pre-cleaned. An earlier draft
	// dropped the findings when the claim was PASS, which quietly disarmed the
	// one check that exists for a judge contradicting itself: the pass went out
	// certified because the evidence against it had been deleted on the way.
	return devcouncil.SubAgentVerdict{Judged: true, Passed: passed, Findings: findings}.Reconcile()
}

// truncateRunes cuts on rune boundaries and says that it cut. A silently
// shortened finding reads as a complete one.
func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "… (truncated)"
}
