package ui

import (
	"context"
	"errors"
	"strings"
)

// Request is a blocked operation put to whoever can clear it.
type Request struct {
	ID       string
	Rule     string
	Severity string
	// Path is the subject of the decision. It is a file path when Subject is
	// empty or "path", and a shell command line when Subject is "command".
	Path string
	// Subject says which of those Path is, so the question put to a human says
	// what they are actually approving.
	//
	// Without it the card labelled every subject "path", and a blocked shell
	// command was therefore shown to an operator as a path — the same command
	// line, under a word that means the opposite thing. Approving a write to a
	// file and approving the execution of a command are not the same decision,
	// and the prompt must not be able to confuse them.
	Subject string
	// Reason is the justification or question context.
	Reason string
	// Diff, when non-empty, carries the unified diff of a file write/edit for preview.
	Diff   string
	TaskID string
	// Grantable is false for a rule no authority can clear. The prompt still
	// shows it, because a human being told what happened is useful even when
	// there is nothing to decide — but it is shown as a refusal, not a question.
	Grantable bool

	// Choices, when non-empty, makes this a question rather than an allow/deny:
	// the operator picks among these options instead of clearing a block.
	//
	// It lives on this type rather than on a second prompting seam beside it
	// because the harness already has exactly one way to reach whoever is
	// sitting at the keyboard — the terminal implements it, a headless run
	// refuses through it, and every card, keybinding and transcript entry is
	// built around it. A parallel asker would be a second thing to keep in step
	// with all of that, and the first divergence between them would be a
	// question put to nobody while the approval beside it was put to a human.
	Choices []string
	// MultiSelect allows more than one of Choices to be picked. A seam that
	// cannot express it must refuse rather than answer with one, because an
	// answer of one where several were wanted is indistinguishable, in the
	// result, from a human who deliberately picked one.
	MultiSelect bool
}

// IsQuestion reports whether this request asks the operator to choose rather
// than to permit.
func (r Request) IsQuestion() bool { return len(r.Choices) > 0 }

// SubjectLabel is the noun the prompt uses for Path. It defaults to "path",
// which is what every producer meant before the field existed.
func (r Request) SubjectLabel() string {
	switch r.Subject {
	case "command":
		return "command"
	case "question":
		return "question"
	}
	return "path"
}

// Decision is the answer.
type Decision struct {
	Allow bool
	// Reason is recorded on the grant. It is required for an allow: a grant
	// with no recorded reason cannot be reviewed later, which is the whole
	// point of recording it.
	Reason string
	// By names the authority, for the ledger.
	By string

	// Chosen are the options picked from Request.Choices. Empty on every
	// allow/deny decision, and empty on a question nobody answered.
	Chosen []string
	// WriteIn is a free-text answer given in place of one of the Choices.
	WriteIn string
}

// Answered reports whether a question actually came back with an answer.
//
// Callers ask this rather than reading Allow, because Allow alone is true on a
// decision carrying no choice at all — which is what a seam that could not put
// the question to anybody returns. Treating that as an answer is how a default
// nobody picked ends up recorded as a human's decision.
func (d Decision) Answered() bool {
	return d.Allow && (len(d.Chosen) > 0 || strings.TrimSpace(d.WriteIn) != "")
}

// Approver answers approval requests. The terminal implements it by asking; a
// headless run implements it by refusing, or by consulting a policy.
type Approver interface {
	Approve(ctx context.Context, req Request) (Decision, error)
}

// ErrNoApprover reports that nothing could answer.
var ErrNoApprover = errors.New("ui: no approver is attached to answer this request")
