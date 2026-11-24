package devcouncil

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/agents"
)

// debateTool is the tool under test, named here rather than in the package so
// the only production string naming it is its schema.
const debateTool = "devcouncil_debate"

// debateRunner answers each child from answer, records every request, and
// knows the operator's team.
type debateRunner struct {
	mu     sync.Mutex
	seen   []SubAgentRequest
	roster agents.Roster
	tiers  map[agents.Tier]string
	answer func(req SubAgentRequest) (SubAgentResult, error)
}

func (r *debateRunner) RunSubAgent(_ context.Context, req SubAgentRequest) (SubAgentResult, error) {
	r.mu.Lock()
	r.seen = append(r.seen, req)
	r.mu.Unlock()
	return r.answer(req)
}

func (r *debateRunner) TeamRoster() agents.Roster          { return r.roster }
func (r *debateRunner) TierPlacement(t agents.Tier) string { return r.tiers[t] }
func (r *debateRunner) TierModel(t agents.Tier) (string, string) {
	return placementModel(r.tiers[t], "anthropic/parent-model")
}

// placementModel resolves a placement as written in a test double's tier map
// to provider and model, the way the harness runner's TierModel does: "" is the
// parent's, a bare id runs on the parent's provider.
func placementModel(placement, parent string) (string, string) {
	pp, pm, _ := strings.Cut(parent, "/")
	switch p, m, ok := strings.Cut(placement, "/"); {
	case placement == "":
		return pp, pm
	case ok:
		return p, m
	default:
		return pp, placement
	}
}

func (r *debateRunner) requests() []SubAgentRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]SubAgentRequest(nil), r.seen...)
}

func newDebateRunner(t *testing.T, answer func(SubAgentRequest) (SubAgentResult, error)) *debateRunner {
	t.Helper()
	team, err := agents.ParseRoster("fable=advisor:anthropic/claude-fable-5-1,carbon=advisor:gemini/gemini-4-hydrogen,scout=basic:gemini/gemini-4-hydrogen")
	if err != nil {
		t.Fatal(err)
	}
	return &debateRunner{roster: team, answer: answer}
}

func debateAnswers(req SubAgentRequest) (SubAgentResult, error) {
	if req.Label == "debate judge" {
		return SubAgentResult{Summary: "B checked the lock.\nDECISION: release the lock in a defer\nCONFIDENCE: High\nDISSENT: none", Steps: 2}, nil
	}
	return SubAgentResult{Summary: "I read lock.go.\nPOSITION: " + req.Member + " says defer the unlock", Steps: 1}, nil
}

func decodeDebate(t *testing.T, text string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("debate result is not JSON: %v\n%s", err, text)
	}
	return out
}

func TestADebateRunsRoundsAndAJudgeOnTheNamedMembers(t *testing.T) {
	runner := newDebateRunner(t, debateAnswers)
	f, _ := fixtureWithRoles(t, runner)

	res := f.call(debateTool, map[string]any{
		"question":     "Where should the lock be released?",
		"participants": []string{"fable", "carbon"},
		"rounds":       2,
		"judge":        "fable",
	})
	if res.IsError {
		t.Fatalf("debate failed: %s", res.Text)
	}
	out := decodeDebate(t, res.Text)
	if out["decided"] != true || out["decision"] != "release the lock in a defer" || out["confidence"] != "high" || out["dissent"] != "none" {
		t.Fatalf("verdict = %v / %v / %v / %v", out["decided"], out["decision"], out["confidence"], out["dissent"])
	}
	if rounds, _ := out["rounds"].([]any); len(rounds) != 2 {
		t.Fatalf("rounds = %v, want 2", out["rounds"])
	}

	seen := runner.requests()
	if len(seen) != 5 {
		t.Fatalf("%d children ran, want 2 participants x 2 rounds + 1 judge", len(seen))
	}
	for _, req := range seen {
		if !req.ReadOnly || req.TaskID != "" || req.Escalation != nil {
			t.Errorf("%s was not a plain read-only child: %+v", req.Label, req)
		}
		if req.Tier != agents.TierAdvisor || (req.Member != "fable" && req.Member != "carbon") {
			t.Errorf("%s placed on %q/%q", req.Label, req.Tier, req.Member)
		}
	}
	// The second round shows each participant the other's answer, by letter.
	second := seen[2:4]
	for _, req := range second {
		if !strings.Contains(req.Prompt, "round 2 of 2") || !strings.Contains(req.Prompt, "--- participant") {
			t.Errorf("round two prompt for %s does not carry the other answer:\n%s", req.Label, req.Prompt)
		}
	}
	// Nobody is told which model wrote what.
	judge := seen[4]
	for _, leak := range []string{"anthropic", "gemini", "claude-fable", "hydrogen"} {
		if strings.Contains(judge.Prompt, leak) {
			t.Errorf("the judge was told %q", leak)
		}
		for _, req := range second {
			if strings.Contains(req.Prompt, leak) {
				t.Errorf("participant %s was told %q", req.Label, leak)
			}
		}
	}
}

func TestADebateBetweenCopiesOfOneModelIsRefused(t *testing.T) {
	runner := newDebateRunner(t, debateAnswers)
	f, _ := fixtureWithRoles(t, runner)

	res := f.call(debateTool, map[string]any{
		"question":     "q",
		"participants": []string{"carbon", "scout"},
	})
	if !res.IsError || !strings.Contains(res.Text, "one model") {
		t.Fatalf("two members on one model = %s; want a refusal", res.Text)
	}
	if n := len(runner.requests()); n != 0 {
		t.Fatalf("%d children ran after a refusal", n)
	}
}

func TestADebateRefusesARawModelID(t *testing.T) {
	runner := newDebateRunner(t, debateAnswers)
	f, _ := fixtureWithRoles(t, runner)

	res := f.call(debateTool, map[string]any{
		"question":     "q",
		"participants": []string{"fable", "anthropic/claude-opus-5-5"},
	})
	if !res.IsError || !strings.Contains(res.Text, "not a tier") {
		t.Fatalf("a raw id = %s; want a refusal", res.Text)
	}
	if n := len(runner.requests()); n != 0 {
		t.Fatalf("%d children ran after a refusal", n)
	}
}

func TestAJudgeWithoutADecisionLineDecidesNothing(t *testing.T) {
	runner := newDebateRunner(t, func(req SubAgentRequest) (SubAgentResult, error) {
		if req.Label == "debate judge" {
			return SubAgentResult{Summary: "Both make good points and I broadly agree with A."}, nil
		}
		return debateAnswers(req)
	})
	f, _ := fixtureWithRoles(t, runner)

	res := f.call(debateTool, map[string]any{
		"question": "q", "participants": []string{"fable", "carbon"}, "rounds": 1,
	})
	if res.IsError {
		t.Fatal(res.Text)
	}
	out := decodeDebate(t, res.Text)
	if out["decided"] != false || out["judge_note"] == nil {
		t.Fatalf("prose with no DECISION line was read as a decision: %v", out)
	}
}

func TestADebateWithTooFewAnswersStops(t *testing.T) {
	runner := newDebateRunner(t, func(req SubAgentRequest) (SubAgentResult, error) {
		if req.Member == "carbon" {
			return SubAgentResult{}, errors.New("provider overloaded")
		}
		return debateAnswers(req)
	})
	f, _ := fixtureWithRoles(t, runner)

	res := f.call(debateTool, map[string]any{
		"question": "q", "participants": []string{"fable", "carbon"},
	})
	if !res.IsError || !strings.Contains(res.Text, "Nothing was decided") {
		t.Fatalf("a debate with one answer = %s; want it stopped", res.Text)
	}
	for _, req := range runner.requests() {
		if req.Label == "debate judge" {
			t.Fatal("the judge ran on a one-sided debate")
		}
	}
}

func TestDebateBoundsItsArguments(t *testing.T) {
	runner := newDebateRunner(t, debateAnswers)
	f, _ := fixtureWithRoles(t, runner)
	for name, args := range map[string]map[string]any{
		"one participant":   {"question": "q", "participants": []string{"fable"}},
		"five participants": {"question": "q", "participants": []string{"fable", "carbon", "lite", "basic", "heavy"}},
		"four rounds":       {"question": "q", "participants": []string{"fable", "carbon"}, "rounds": 4},
		"no question":       {"participants": []string{"fable", "carbon"}},
		"huge question":     {"question": strings.Repeat("x", maxDebateQuestion+1), "participants": []string{"fable", "carbon"}},
	} {
		if res := f.call(debateTool, args); !res.IsError {
			t.Errorf("%s was accepted: %s", name, res.Text)
		}
	}
	if n := len(runner.requests()); n != 0 {
		t.Fatalf("%d children ran from refused debates", n)
	}
}

func TestTheVerdictParserReadsOnlyItsContract(t *testing.T) {
	v := parseDebateVerdict("**DECISION:** keep it\nCONFIDENCE: certain\n")
	if !v.Decided || v.Decision != "keep it" || v.Confidence != "" {
		t.Fatalf("= %+v; an unknown confidence must be dropped, not guessed", v)
	}
	if v := parseDebateVerdict("I decided: keep it"); v.Decided {
		t.Fatalf("prose was read as a decision: %+v", v)
	}
}

// The judge and the other participants never see the harness's notes on how a
// participant ran: a failover note names the providers on both sides of it.
func TestHarnessNotesNeverReachTheJudgeOrTheOtherParticipants(t *testing.T) {
	runner := newDebateRunner(t, func(req SubAgentRequest) (SubAgentResult, error) {
		res, err := debateAnswers(req)
		res.Summary = AppendSubAgentNote(res.Summary,
			"gemini was unavailable (http 503 after 4 attempt(s)); re-ran once on the advisor tier's fallback anthropic/claude-opus-5-5")
		res.Summary = AppendSubAgentNote(res.Summary, "it runs on team member "+req.Member+", which the operator placed")
		return res, err
	})
	f, _ := fixtureWithRoles(t, runner)
	res := f.call(debateTool, map[string]any{
		"question": "Where should the lock be released?", "participants": []string{"fable", "carbon"}, "rounds": 2,
	})
	if res.IsError {
		t.Fatalf("debate failed: %s", res.Text)
	}
	seen := runner.requests()
	if len(seen) != 5 {
		t.Fatalf("ran %d children, want 5", len(seen))
	}
	for _, req := range seen[2:] {
		for _, leak := range []string{"this sub-agent", "gemini", "claude-opus", "team member"} {
			if strings.Contains(req.Prompt, leak) {
				t.Errorf("%s was told %q:\n%s", req.Label, leak, req.Prompt)
			}
		}
	}
	if !strings.Contains(res.Text, "re-ran once") {
		t.Fatalf("the notes were withheld from the caller too: %s", res.Text)
	}
}

// Copies of one model are refused however each was named: a tier on the
// session's provider is written as a bare id, a member as provider/model.
func TestADebateBetweenOneModelNamedTwoWaysIsRefused(t *testing.T) {
	runner := newDebateRunner(t, debateAnswers)
	runner.tiers = map[agents.Tier]string{agents.TierAdvisor: "claude-fable-5-1"}
	f, _ := fixtureWithRoles(t, runner)
	res := f.call(debateTool, map[string]any{
		"question": "q", "participants": []string{"advisor", "fable"},
	})
	if !res.IsError || !strings.Contains(res.Text, "copies of one model") {
		t.Fatalf("one model under two names was allowed to debate itself: %s", res.Text)
	}
	if n := len(runner.requests()); n != 0 {
		t.Fatalf("%d children ran before the refusal", n)
	}
}

// A participant that fails a later round keeps its last good answer for the
// judge, and sits out the rounds after.
func TestAParticipantThatFailsALaterRoundKeepsItsEarlierAnswer(t *testing.T) {
	runner := newDebateRunner(t, func(req SubAgentRequest) (SubAgentResult, error) {
		if req.Member == "carbon" && strings.Contains(req.Prompt, "round 2 of 2") {
			return SubAgentResult{}, errors.New("provider down")
		}
		return debateAnswers(req)
	})
	f, _ := fixtureWithRoles(t, runner)
	res := f.call(debateTool, map[string]any{
		"question": "q", "participants": []string{"fable", "carbon"}, "rounds": 2,
	})
	if res.IsError {
		t.Fatalf("debate failed: %s", res.Text)
	}
	seen := runner.requests()
	judge := seen[len(seen)-1]
	if !strings.Contains(judge.Prompt, "carbon says defer the unlock") {
		t.Fatalf("the judge lost carbon's round-one answer:\n%s", judge.Prompt)
	}
}

// A panicking participant fails that participant, not the process.
func TestAPanickingParticipantFailsOnlyItself(t *testing.T) {
	runner := newDebateRunner(t, func(req SubAgentRequest) (SubAgentResult, error) {
		if req.Member == "carbon" {
			panic("boom")
		}
		return debateAnswers(req)
	})
	f, _ := fixtureWithRoles(t, runner)
	res := f.call(debateTool, map[string]any{
		"question": "q", "participants": []string{"fable", "carbon"}, "rounds": 1,
	})
	if !res.IsError || !strings.Contains(res.Text, "panicked") {
		t.Fatalf("a panic was not reported as that participant's failure: %s", res.Text)
	}
}
