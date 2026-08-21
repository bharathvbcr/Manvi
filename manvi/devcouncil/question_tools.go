package devcouncil

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"manvi/flags"
	"manvi/tools"
	"manvi/ui"
)

// Question represents a structured multiple-choice or write-in prompt.
type Question struct {
	Question      string   `json:"question"`
	Options       []string `json:"options"`
	IsMultiSelect bool     `json:"is_multi_select,omitempty"`
}

// QuestionAnswer is the user's response to one question.
type QuestionAnswer struct {
	Question string   `json:"question"`
	Selected []string `json:"selected"`
	WriteIn  string   `json:"write_in,omitempty"`
}

// QuestionAsker is the interactive UI / pairing interface for soliciting user feedback.
type QuestionAsker interface {
	AskQuestions(ctx context.Context, questions []Question) ([]QuestionAnswer, error)
}

// ErrQuestionDeclined reports that the question reached a human who chose not
// to answer it.
//
// It is distinct from every other error an asker can return because the two
// mean opposite things about the seam: a declined question proves a human was
// there and said nothing, a failed one proves the seam is broken. Collapsing
// them would make an operator pressing Escape indistinguishable from a UI that
// never rendered the card.
var ErrQuestionDeclined = errors.New("devcouncil: the question was dismissed without an answer")

// ApproverAsker puts questions to whoever answers this run's approvals.
//
// It exists so pairing does not stand up a second way to reach the operator.
// The harness already has one: ui.Approver, which the write gate escalates a
// soft block through, which the TUI implements by raising a modal card on the
// asking session, and which a headless run implements by refusing. Questions go
// the same way, so a run that can interrupt a human for an approval can
// interrupt them for a question, and a run that cannot does neither.
type ApproverAsker struct {
	Approver ui.Approver
}

// AskQuestions puts each question to the approver in turn.
//
// One request per question rather than one for the batch, because the seam
// answers one decision at a time and the card is modal: batching would need a
// second card type that renders several questions at once, which is the
// parallel mechanism this type exists to avoid.
func (a ApproverAsker) AskQuestions(ctx context.Context, questions []Question) ([]QuestionAnswer, error) {
	if a.Approver == nil {
		// Not a silent fall-through to defaults. A nil approver here is a
		// wiring mistake, and answering around it would hide the mistake behind
		// answers nobody gave.
		return nil, ui.ErrNoApprover
	}
	answers := make([]QuestionAnswer, 0, len(questions))
	for i, q := range questions {
		req := ui.Request{
			ID:       fmt.Sprintf("QUESTION-%04d", i+1),
			Rule:     flags.PairQuestionsEnabled,
			Severity: "ask",
			Subject:  "question",
			// Reason is the card's body, which is where a question belongs: the
			// subject row is one truncated line and a real question is not one.
			Reason:      q.Question,
			Grantable:   true,
			Choices:     q.Options,
			MultiSelect: q.IsMultiSelect,
		}
		decision, err := a.Approver.Approve(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("putting question %d to the operator: %w", i+1, err)
		}
		// Answered, not Allow. A seam that could not reach anybody returns a
		// decision with nothing chosen, and reading Allow alone would turn that
		// into an answer with an empty selection.
		if !decision.Answered() {
			return nil, fmt.Errorf("%w: %s", ErrQuestionDeclined, decision.Reason)
		}
		answers = append(answers, QuestionAnswer{
			Question: q.Question,
			Selected: decision.Chosen,
			WriteIn:  decision.WriteIn,
		})
	}
	return answers, nil
}

func (r *Registry) askQuestionTools() []tools.Tool {
	return []tools.Tool{
		{
			Schema: schema("devcouncil_ask_question",
				"Ask the user one or more interactive questions to clarify underspecified requirements, "+
					"solicit design feedback, or pick a solution from options. Prioritize inquiry and clarify the "+
					"impact of decisions before undertaking complex tasks or designing the UI for collaborative workflows. "+
					"The result says who answered: `answered:true` with `answered_by:\"human\"` means a person chose, "+
					"and their choices are under `answers`. `answered:false` with `answered_by:\"none\"` means nobody was "+
					"available — no question was put to anyone, and the defaults the run proceeded on are under "+
					"`assumed_defaults`, which are assumptions and not answers. Never describe an assumed default as a "+
					"decision the user made; say what you assumed.",
				`{"type":"object","properties":{"questions":{"type":"array","items":{"type":"object","properties":{"question":{"type":"string"},"options":{"type":"array","items":{"type":"string"}},"is_multi_select":{"type":"boolean"}},"required":["question","options"]}}},"required":["questions"]}`),
			Group:    tools.GroupQuestion,
			Handler:  r.askQuestion,
			Extended: true,
		},
	}
}

func (r *Registry) askQuestion(ctx context.Context, call tools.Call) tools.Result {
	var args struct {
		Questions []Question `json:"questions"`
	}
	if err := decode(call, &args); err != nil {
		return tools.Errorf("bad arguments: %v", err)
	}
	if len(args.Questions) == 0 {
		return tools.Errorf("questions array is required and must not be empty")
	}

	for i, q := range args.Questions {
		if strings.TrimSpace(q.Question) == "" {
			return tools.Errorf("questions[%d] has an empty question text", i)
		}
		if len(q.Options) < 2 {
			return tools.Errorf("questions[%d] must offer at least 2 options", i)
		}
	}

	// 1. Put the questions to a human, when this run has one and pairing is on.
	//
	// Every path out of this branch either carries a human's answers or falls
	// to the unattended report below saying no human answered. What it must
	// never do is what it used to do for every call ever made — there was no
	// asker attached anywhere in the harness, so an attended operator sat at a
	// terminal while the tool picked option 0 on their behalf and reported it
	// as answered. A question answered by the thing that asked it is not a
	// question, and reporting it as one is the same defect the sub-agent
	// dispatch was fixed for: work reported as done that never happened.
	paired, pairedWhy := r.pairingEnabled()
	switch {
	case !paired:
		// pairedWhy explains it below; no human is consulted.
	case r.deps.QuestionAsker == nil:
		pairedWhy = "no interactive session is attached to this run, so there was nobody to ask"
	default:
		answers, err := r.deps.QuestionAsker.AskQuestions(ctx, args.Questions)
		switch {
		case errors.Is(err, ErrQuestionDeclined):
			pairedWhy = "the questions reached an operator who dismissed them without answering: " + err.Error()
		case err != nil:
			// A broken seam is an error, never a quiet fall-back to defaults.
			// Defaults chosen because the UI failed would be reported in the
			// same shape as defaults chosen because nobody was there, and the
			// failure would never surface.
			return tools.Errorf("asking questions: %v", err)
		default:
			if problem := validateAnswers(args.Questions, answers); problem != "" {
				return tools.Errorf("%s", problem)
			}
			return ok(map[string]any{
				"answered":    true,
				"answered_by": "human",
				"answers":     answers,
			})
		}
	}

	// 2. Nobody answered: YOLO, headless, or pairing switched off.
	//
	// The auto-resolution itself is wanted — an unattended run must not stall
	// on a question — but the result says plainly that these are assumptions,
	// and it does not put them under "answers". The distinction the calling
	// model needs is "a human chose this" against "no human was available and a
	// default was assumed", and it has to be readable from the result alone,
	// because that result is the only thing the model ever sees.
	defaults := make([]QuestionAnswer, 0, len(args.Questions))
	for _, q := range args.Questions {
		chosen := q.Options[0]
		for _, opt := range q.Options {
			if strings.HasPrefix(strings.ToLower(opt), "(recommended)") {
				chosen = opt
				break
			}
		}
		defaults = append(defaults, QuestionAnswer{
			Question: q.Question,
			Selected: []string{chosen},
		})
	}

	note := pairedWhy
	if r.postureIsYolo() {
		note += "; posture is " + flags.PostureYolo + ", which runs unattended by design"
	}

	return ok(map[string]any{
		"answered":         false,
		"answered_by":      "none",
		"note":             note,
		"assumed_defaults": defaults,
		"instruction": "Nobody was asked and nobody chose these. They are the defaults this run " +
			"proceeded on. State the assumption in your report, and do not describe any of it as " +
			"confirmed, agreed, or chosen by the user.",
	})
}

// pairingEnabled reports whether this run puts questions to a human, and why
// not when it does not.
//
// Absence of the setting means enabled: the flag exists to let an operator turn
// pairing off, and a registry that cannot answer is not an operator saying no.
// The asker being nil is the other gate, checked separately, so a run with the
// flag on and no UI still does not pretend to have asked.
func (r *Registry) pairingEnabled() (bool, string) {
	if r.settingOn(flags.PairQuestionsEnabled, true) {
		return true, ""
	}
	return false, flags.PairQuestionsEnabled + " is off, so this run does not put questions to a human"
}

func (r *Registry) postureIsYolo() bool {
	if r.deps.Gate == nil || r.deps.Gate.Flags == nil {
		return false
	}
	p, _, err := r.deps.Gate.Flags.String(flags.HarnessPosture)
	return err == nil && p == flags.PostureYolo
}

// validateAnswers checks that what came back is an answer to what was asked,
// and returns the complaint when it is not.
//
// An asker that returns a short slice, or an entry with nothing selected and
// nothing written, has not answered — and the caller must not be handed that
// under a key that says it did. Failing here is loud and recoverable; passing
// it through is the quiet version of the defect this file exists to prevent.
func validateAnswers(questions []Question, answers []QuestionAnswer) string {
	if len(answers) != len(questions) {
		return fmt.Sprintf(
			"the question seam returned %d answers for %d questions; a partial answer is not an answer",
			len(answers), len(questions))
	}
	for i, a := range answers {
		if len(a.Selected) == 0 && strings.TrimSpace(a.WriteIn) == "" {
			return fmt.Sprintf(
				"questions[%d] (%q) came back with nothing selected and nothing written in",
				i, questions[i].Question)
		}
	}
	return ""
}
