package devcouncil

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// debateTool is the structured-disagreement primitive: two to four
// participants answer one question independently, read each other's answers
// for a bounded number of rounds, and a judge who sees the answers but not the
// models that wrote them decides.
//
// It exists because a single model reviewing its own plan agrees with itself,
// and a parent asking two children the same question and reading both answers
// has done the comparison in its own head, with its own bias, and kept no
// record of it. Here the disagreement is the artifact: every round's positions
// come back, the judge's dissent line names the strongest objection left
// standing, and a judge that reached no decision says so rather than reading
// as agreement.
//
// It runs its own rounds rather than going through runFanout. runFanout is one
// pool run over children whose requests all exist before any starts; a debate
// is up to four sequential pools, each built from the answers of the one
// before, and its outcome is a transcript and a verdict, not a per-child
// outcome list. What it shares with runFanout it takes from the same owners:
// delegationBounds for the depth rule, FanoutFor for the width, the runner's
// admitter for the cap. Every participant and the judge are read-only children,
// so a debate changes no file; the tool is classed with the other dispatching
// tools rather than as a read-only tool, because what it returns is the
// children's answers, which carry repository content each child read through
// its own read gate, and a read-only tool here is one that answers from the
// repository directly and must classify itself for the read gate.
//
// It is a dispatching tool, in the sub-agent group, so the structural depth
// rule removes it from every child's registry with the others: a participant
// cannot start a debate of its own. Its name is written only at its schema,
// the one definition site, so no message can route a model to it in a profile
// that does not offer it.

const (
	minDebaters       = 2
	maxDebaters       = 4
	maxDebateRounds   = 3
	maxDebateQuestion = 16 << 10
	maxDebateContext  = 64 << 10
	// maxCarriedAnswer bounds what of one participant's answer the next round
	// and the judge are shown. A participant that wrote a book does not get to
	// crowd the others out of the judge's context.
	maxCarriedAnswer = 8000
)

// debateLabels name participants to each other and to the judge. Letters,
// not models: a judge told which answer came from the dearer model weighs the
// price tag, and a participant told it is arguing with a frontier model
// concedes to the name.
var debateLabels = []string{"A", "B", "C", "D"}

func (r *Registry) debateTools() []tools.Tool {
	return []tools.Tool{{
		Schema: schema("devcouncil_debate",
			"Run a structured debate: 2-4 read-only participants (tiers or team member names, at least two on different models) "+
				"answer a question independently, then rebut each other for up to 3 rounds; a judge who sees the answers but not "+
				"the models decides, and names the strongest objection left standing. Use it for a decision worth a second model's "+
				"disagreement — a design choice, a diagnosis, a risky change — not for lookups.",
			`{"type":"object","properties":{"question":{"type":"string","description":"the question to decide, self-contained"},"participants":{"type":"array","items":{"type":"string"},"minItems":2,"maxItems":4,"description":"2-4 tiers (basic, lite, heavy, advisor) or team member names; at least two must run different models"},"rounds":{"type":"integer","minimum":1,"maximum":3,"description":"rounds including the opening one; default 2"},"judge":{"type":"string","description":"tier or team member name that decides; default advisor"},"context":{"type":"string","description":"optional material every participant and the judge receive"}},"required":["question","participants"]}`),
		Group:    tools.GroupSubagent,
		Handler:  r.debate,
		Extended: true,
	}}
}

// debater is one participant as the debate runs it.
type debater struct {
	Label     string `json:"label"`
	Asked     string `json:"asked"`
	Tier      string `json:"tier"`
	Member    string `json:"member,omitempty"`
	Placement string `json:"placement"`
	tier      agents.Tier
	// provider and model are what the placement resolves to, compared to
	// refuse a debate between copies of one model however each was named.
	provider, model string
}

// sameness is what two participants must differ in: the provider and model
// they run on, compared case-insensitively. A runner that cannot say falls
// back to the placement as written.
func (d debater) sameness() string {
	if strings.TrimSpace(d.model) == "" {
		return strings.ToLower(d.Placement)
	}
	return strings.ToLower(strings.TrimSpace(d.provider)) + "/" + strings.ToLower(strings.TrimSpace(d.model))
}

// debateTurn is one participant's answer in one round.
type debateTurn struct {
	Label    string `json:"label"`
	Position string `json:"position,omitempty"`
	Answer   string `json:"answer,omitempty"`
	Error    string `json:"error,omitempty"`
	Steps    int    `json:"steps,omitempty"`
	// Notes are the harness's notes on how the participant ran (a failover,
	// a step ceiling). They are reported to the caller and never shown to the
	// other participants or the judge: they name tiers, members and providers.
	Notes []string `json:"notes,omitempty"`
}

func (r *Registry) debate(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Question     string   `json:"question"`
		Participants []string `json:"participants"`
		Rounds       int      `json:"rounds"`
		Judge        string   `json:"judge"`
		Context      string   `json:"context"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	args.Question = strings.TrimSpace(args.Question)
	switch {
	case args.Question == "":
		return tools.Errorf("question is required")
	case len(args.Question) > maxDebateQuestion:
		return tools.Errorf("question is %d bytes, above the bound of %d; put the material in context", len(args.Question), maxDebateQuestion)
	case len(args.Context) > maxDebateContext:
		return tools.Errorf("context is %d bytes, above the bound of %d", len(args.Context), maxDebateContext)
	case len(args.Participants) < minDebaters || len(args.Participants) > maxDebaters:
		return tools.Errorf("participants must name %d to %d tiers or team members, got %d", minDebaters, maxDebaters, len(args.Participants))
	}
	rounds := args.Rounds
	if rounds == 0 {
		rounds = 2
	}
	if rounds < 1 || rounds > maxDebateRounds {
		return tools.Errorf("rounds must be 1 to %d, got %d", maxDebateRounds, args.Rounds)
	}

	bounds, refusal := r.delegationBounds()
	if refusal != nil {
		return *refusal
	}
	runner := r.deps.SubAgent
	if runner == nil {
		return tools.Errorf("a debate is not available: this harness has no sub-agent runner attached, so no " +
			"participant could be run — nothing was decided; reason it through in this turn instead")
	}

	// Every participant and the judge are placed the way a dispatching model
	// places any child: by tier or team member, never by id, and no higher
	// than the advisor. Checked for all of them before any runs.
	roster := teamRoster(runner)
	place := func(what, spec string) (debater, error) {
		tier, member, err := roster.MemberPlacement(spec, agents.TierAdvisor)
		if err != nil {
			return debater{}, fmt.Errorf("%s %q: %w", what, spec, err)
		}
		if tier == "" {
			return debater{}, fmt.Errorf("%s %q: name a tier or a team member", what, spec)
		}
		d := debater{Asked: strings.TrimSpace(spec), Tier: tier.String(), tier: tier, Placement: "inherit"}
		if member != nil {
			d.Member, d.Placement = member.Name, member.Placement()
			d.provider, d.model = member.Spec.Provider, member.Spec.Model
		} else if v, ok := runner.(TeamView); ok {
			if p := v.TierPlacement(tier); p != "" {
				d.Placement = p
			}
			d.provider, d.model = v.TierModel(tier)
		}
		return d, nil
	}
	debaters := make([]debater, 0, len(args.Participants))
	distinct := map[string]bool{}
	for i, spec := range args.Participants {
		d, err := place("participant", spec)
		if err != nil {
			return tools.Errorf("%v. Nothing was run", err)
		}
		d.Label = debateLabels[i]
		distinct[d.sameness()] = true
		debaters = append(debaters, d)
	}
	if len(distinct) < minDebaters {
		return tools.Errorf("every participant runs %q: a debate between copies of one model is one model agreeing "+
			"with itself. Name tiers or team members on at least two different models. Nothing was run",
			debaters[0].Placement)
	}
	judgeSpec := strings.TrimSpace(args.Judge)
	if judgeSpec == "" {
		judgeSpec = agents.TierAdvisor.String()
	}
	judge, err := place("judge", judgeSpec)
	if err != nil {
		return tools.Errorf("%v. Nothing was run", err)
	}
	judge.Label = "judge"

	placements := make([]string, 0, len(debaters)+1)
	for _, d := range append(append([]debater(nil), debaters...), judge) {
		placements = append(placements, d.Placement)
	}
	width, narrowed := agents.FanoutFor(bounds, placements)

	transcript := make([][]debateTurn, 0, rounds)
	latest := make([]debateTurn, len(debaters))
	// dropped marks a participant that failed a round. It sits out the rest,
	// and its last good answer, if it had one, is still the one the judge weighs.
	dropped := make([]bool, len(debaters))
	for round := 1; round <= rounds; round++ {
		if err := ctx.Err(); err != nil {
			return tools.Errorf("the debate was cancelled in round %d: %v", round, err)
		}
		reqs := make([]SubAgentRequest, len(debaters))
		live := make([]bool, len(debaters))
		for i, d := range debaters {
			if dropped[i] {
				continue
			}
			live[i] = true
			reqs[i] = debateRequest(d, debatePrompt(args.Question, args.Context, d.Label, round, rounds, latest, i))
		}
		if res := admitDebate(ctx, runner, reqs, live); res != nil {
			return *res
		}
		turns := runDebateRound(ctx, runner, debaters, reqs, live, width)
		for i := range turns {
			if !live[i] {
				continue
			}
			if turns[i].Error != "" {
				dropped[i] = true
				if latest[i].Answer != "" {
					continue
				}
			}
			latest[i] = turns[i]
		}
		transcript = append(transcript, turns)
		if answered := countAnswered(latest); answered < minDebaters {
			return tools.Errorf("the debate stopped in round %d: only %d participant(s) still had an answer, so there "+
				"was nothing left to weigh. Nothing was decided. Round results: %s", round, answered, renderTurns(turns))
		}
	}

	judgeReq := debateRequest(judge, judgePrompt(args.Question, args.Context, latest))
	if res := admitDebate(ctx, runner, []SubAgentRequest{judgeReq}, []bool{true}); res != nil {
		return *res
	}
	verdict, jerr := runner.RunSubAgent(ctx, judgeReq)
	decision := parseDebateVerdict(verdict.Summary)
	payload := map[string]any{
		"question":     args.Question,
		"participants": debaters,
		"rounds":       transcript,
		"judge":        judge,
		"decided":      decision.Decided,
		"decision":     decision.Decision,
		"confidence":   decision.Confidence,
		"dissent":      decision.Dissent,
	}
	if narrowed != "" {
		payload["fanout_narrowed"] = narrowed
	}
	switch {
	case jerr != nil:
		payload["decided"] = false
		payload["judge_error"] = jerr.Error()
	case !decision.Decided:
		payload["judge_note"] = "the judge did not end with a DECISION: line, so no decision was reached — " +
			"this is not agreement; read the rounds"
		payload["judge_answer"] = clip(verdict.Summary, maxCarriedAnswer)
	}
	return ok(payload)
}

// debateRequest is one read-only child of the debate. It carries no task, no
// lease, no escalation and no verdict marker: a participant argues, it does
// not work, and the judge's answer is parsed here against the contract its
// prompt states.
func debateRequest(d debater, prompt string) SubAgentRequest {
	return SubAgentRequest{
		Label:    "debate " + d.Label,
		Prompt:   prompt,
		ReadOnly: true,
		Tier:     d.tier,
		Member:   d.Member,
	}
}

// admitDebate asks the runner's spend cap whether a round can start at all.
// A round the cap cannot cover is refused before any participant runs, as a
// fan-out is.
func admitDebate(ctx context.Context, runner SubAgentRunner, reqs []SubAgentRequest, live []bool) *tools.Result {
	admitter, ok := runner.(SubAgentAdmitter)
	if !ok {
		return nil
	}
	var asked []SubAgentRequest
	for i, req := range reqs {
		if live[i] {
			asked = append(asked, req)
		}
	}
	budget, reserves, err := admitter.AdmitSubAgents(asked)
	if err != nil {
		res := tools.Errorf("the debate could not be admitted against the spend cap, so no participant was started: %v", err)
		return &res
	}
	if budget == nil {
		return nil
	}
	var need int64
	for _, n := range reserves {
		need += n
	}
	room, err := budget.Headroom(ctx)
	if err != nil {
		res := tools.Errorf("the spend cap could not be read, so no participant was started: %v", err)
		return &res
	}
	if need > room {
		res := tools.Errorf("the next debate round needs %s and the spend cap has %s left, so no participant was started",
			budget.Render(need), budget.Render(room))
		return &res
	}
	return nil
}

// runDebateRound runs the live participants at most width at a time and
// returns one turn per participant, in order.
func runDebateRound(ctx context.Context, runner SubAgentRunner, debaters []debater, reqs []SubAgentRequest,
	live []bool, width int) []debateTurn {
	turns := make([]debateTurn, len(debaters))
	if width < 1 {
		width = 1
	}
	sem := make(chan struct{}, width)
	var wg sync.WaitGroup
	for i := range debaters {
		turns[i].Label = debaters[i].Label
		if !live[i] {
			turns[i].Error = "dropped out in an earlier round"
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// A panic in one participant fails that participant, as a
			// fan-out's pool does, rather than the process.
			defer func() {
				if p := recover(); p != nil {
					turns[i].Error = fmt.Sprintf("the participant's run panicked: %v", p)
				}
			}()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				turns[i].Error = ctx.Err().Error()
				return
			}
			defer func() { <-sem }()
			res, err := runner.RunSubAgent(ctx, reqs[i])
			turns[i].Steps = res.Steps
			if err != nil {
				turns[i].Error = err.Error()
				return
			}
			answer, notes := SplitSubAgentNotes(res.Summary)
			turns[i].Notes = notes
			if strings.TrimSpace(answer) == "" {
				turns[i].Error = "finished with no answer"
				return
			}
			turns[i].Answer = clip(answer, maxCarriedAnswer)
			turns[i].Position = lastTagged(answer, "POSITION:")
		}(i)
	}
	wg.Wait()
	return turns
}

func countAnswered(turns []debateTurn) int {
	n := 0
	for _, t := range turns {
		if t.Error == "" && t.Answer != "" {
			n++
		}
	}
	return n
}

func renderTurns(turns []debateTurn) string {
	parts := make([]string, 0, len(turns))
	for _, t := range turns {
		state := "answered"
		if t.Error != "" {
			state = t.Error
		}
		parts = append(parts, t.Label+": "+state)
	}
	return strings.Join(parts, "; ")
}

// debatePrompt is participant self's instruction for one round. The opening
// round is blind; every later round shows the other participants' latest
// answers, labelled by letter only.
func debatePrompt(question, extra, self string, round, rounds int, latest []debateTurn, me int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are participant %s in a structured debate between independent models (round %d of %d). ", self, round, rounds)
	b.WriteString("Decide on the evidence: read the code or run read-only checks with your tools where it matters, and say what you checked. ")
	b.WriteString("Do not defer to the other participants because of who they might be; defer only to evidence.\n\nQuestion:\n")
	b.WriteString(question)
	if strings.TrimSpace(extra) != "" {
		b.WriteString("\n\nContext:\n")
		b.WriteString(extra)
	}
	if round > 1 {
		b.WriteString("\n\nThe other participants' latest answers follow. Any of them may be wrong, and so may yours. " +
			"Rebut what you disagree with and why, concede what you got wrong, and give your current answer.\n")
		for i, t := range latest {
			if i == me || t.Error != "" || t.Answer == "" {
				continue
			}
			fmt.Fprintf(&b, "\n--- participant %s ---\n%s\n", t.Label, t.Answer)
		}
		if mine := latest[me]; mine.Answer != "" {
			fmt.Fprintf(&b, "\n--- your previous answer ---\n%s\n", mine.Answer)
		}
	}
	b.WriteString("\n\nEnd your reply with exactly one line of the form\nPOSITION: <your answer in one or two sentences>")
	return b.String()
}

// judgePrompt shows the judge every surviving final answer by letter.
func judgePrompt(question, extra string, latest []debateTurn) string {
	var b strings.Builder
	b.WriteString("You are the judge of a structured debate between independent models. You are not told which model wrote which answer. ")
	b.WriteString("Weigh the evidence each answer cites — check it with your read-only tools where it is checkable — not its confidence, ")
	b.WriteString("its length or how many answers agree.\n\nQuestion:\n")
	b.WriteString(question)
	if strings.TrimSpace(extra) != "" {
		b.WriteString("\n\nContext:\n")
		b.WriteString(extra)
	}
	b.WriteString("\n\nFinal answers:\n")
	for _, t := range latest {
		if t.Error != "" || t.Answer == "" {
			continue
		}
		fmt.Fprintf(&b, "\n--- participant %s ---\n%s\n", t.Label, t.Answer)
	}
	b.WriteString("\n\nGive your reasoning, then end with exactly these three lines:\n" +
		"DECISION: <the answer you decide on, in one or two sentences>\n" +
		"CONFIDENCE: <low, medium or high>\n" +
		"DISSENT: <the strongest objection still standing, or none>")
	return b.String()
}

// debateVerdict is what the judge's closing lines said.
type debateVerdict struct {
	Decided    bool
	Decision   string
	Confidence string
	Dissent    string
}

// parseDebateVerdict reads the judge's closing lines. Without a DECISION line
// nothing was decided, whatever the prose sounds like; a confidence that is
// not one of the three words is dropped rather than guessed.
func parseDebateVerdict(answer string) debateVerdict {
	v := debateVerdict{
		Decision: lastTagged(answer, "DECISION:"),
		Dissent:  lastTagged(answer, "DISSENT:"),
	}
	v.Decided = v.Decision != ""
	switch c := strings.ToLower(lastTagged(answer, "CONFIDENCE:")); c {
	case "low", "medium", "high":
		v.Confidence = c
	}
	return v
}

// lastTagged is the text after the last line that starts with tag, trimmed,
// or "" when no line does.
func lastTagged(text, tag string) string {
	found := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "*_#> "))
		if rest, ok := strings.CutPrefix(line, tag); ok {
			found = strings.TrimSpace(strings.Trim(strings.TrimSpace(rest), "*_"))
		}
	}
	return found
}

// clip bounds s to at most n runes, saying so when it cut.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n]) + "\n[… cut at " + fmt.Sprint(n) + " characters]"
}
