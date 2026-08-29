package ui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
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

// DenyAll refuses everything, which is the correct default for an unattended
// run: an approval nobody is present to give must not be granted by absence.
type DenyAll struct{}

// Approve refuses.
func (DenyAll) Approve(ctx context.Context, req Request) (Decision, error) {
	return Decision{Allow: false, Reason: "no operator attached; unattended runs do not self-approve"}, nil
}

// Prompter asks a human on a terminal.
type Prompter struct {
	In  io.Reader
	Out io.Writer
	// Sink receives the request and the decision, so the transcript records the
	// question as well as the answer.
	Sink Sink
	// Timeout bounds how long a prompt waits. Zero means wait indefinitely,
	// which is right for an attended session and wrong for anything else.
	Timeout time.Duration

	reader  *bufio.Reader
	counter atomic.Uint64
}

// NewPrompter builds an interactive approver.
func NewPrompter(in io.Reader, out io.Writer, sink Sink) *Prompter {
	if sink == nil {
		sink = Discard
	}
	return &Prompter{In: in, Out: out, Sink: sink, reader: bufio.NewReader(in)}
}

// Approve asks, and records both the question and the answer.
func (p *Prompter) Approve(ctx context.Context, req Request) (Decision, error) {
	if req.ID == "" {
		req.ID = fmt.Sprintf("APPROVAL-%04d", p.counter.Add(1))
	}
	p.Sink.Emit(Event{
		Kind: KindApproval, At: time.Now().UTC(), ApprovalID: req.ID,
		Text: req.Reason, Rule: req.Rule, Severity: req.Severity,
		Path: req.Path, TaskID: req.TaskID, Grantable: req.Grantable,
	})

	if !req.Grantable {
		decision := Decision{Allow: false, Reason: "this rule is never grantable by any authority"}
		p.record(req, decision)
		return decision, nil
	}

	if req.IsQuestion() {
		return p.ask(ctx, req)
	}

	// The prompt itself is written with Sanitize applied to every field that
	// came from outside, because the question a human answers must not be
	// paintable by the content it is about.
	fmt.Fprintf(p.Out, "\nAllow %s on %s %s? [y/N] ",
		Sanitize(req.Rule), req.SubjectLabel(), Sanitize(req.Path))

	answer, err := p.read(ctx)
	if err != nil {
		// A prompt that could not be answered is a refusal, never an allow.
		// Treating an unanswerable question as consent is the one failure mode
		// an approval seam must not have.
		decision := Decision{Allow: false, Reason: fmt.Sprintf("prompt could not be answered: %v", err)}
		p.record(req, decision)
		return decision, nil
	}

	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer != "y" && answer != "yes" {
		decision := Decision{Allow: false, Reason: "declined by the operator"}
		p.record(req, decision)
		return decision, nil
	}

	fmt.Fprint(p.Out, "Reason (required, recorded on the grant): ")
	reason, err := p.read(ctx)
	if err != nil {
		decision := Decision{Allow: false, Reason: fmt.Sprintf("reason could not be read: %v", err)}
		p.record(req, decision)
		return decision, nil
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		// Not defaulted to something like "approved by operator". A grant with
		// a manufactured reason reads in a later review exactly like one with a
		// real reason, and there is no way to tell them apart afterwards.
		decision := Decision{Allow: false, Reason: "no reason given; a grant that cannot be reviewed later is not issued"}
		p.record(req, decision)
		return decision, nil
	}

	decision := Decision{Allow: true, Reason: reason, By: "human"}
	p.record(req, decision)
	return decision, nil
}

// ask puts a multiple-choice question to the operator.
//
// It is a distinct path from Approve's y/N because the two decisions are not
// the same shape: "allow this write" has two answers and needs a recorded
// reason, "which of these four" has four and needs none. Bending one into the
// other produced a prompt that asked whether to allow a question.
func (p *Prompter) ask(ctx context.Context, req Request) (Decision, error) {
	fmt.Fprintf(p.Out, "\n%s\n", Sanitize(req.Reason))
	for i, opt := range req.Choices {
		fmt.Fprintf(p.Out, "  %d) %s\n", i+1, Sanitize(opt))
	}
	if req.MultiSelect {
		fmt.Fprint(p.Out, "Choose (numbers, comma-separated) or type an answer: ")
	} else {
		fmt.Fprint(p.Out, "Choose a number, or type an answer: ")
	}

	line, err := p.read(ctx)
	if err != nil {
		// Unreadable is unanswered. It is never an answer, for the same reason
		// an unanswerable approval is never an allow.
		decision := Decision{Allow: false, Reason: fmt.Sprintf("question could not be answered: %v", err)}
		p.record(req, decision)
		return decision, nil
	}
	line = strings.TrimSpace(line)
	if line == "" {
		decision := Decision{Allow: false, Reason: "no answer given"}
		p.record(req, decision)
		return decision, nil
	}

	chosen, numeric, err := parseChoice(line, req)
	switch {
	case err != nil:
		decision := Decision{Allow: false, Reason: err.Error()}
		p.record(req, decision)
		return decision, nil
	case !numeric:
		// Anything that is not a list of option numbers is a write-in. The tool
		// this serves advertises free text, so discarding it would drop an
		// answer the operator actually gave.
		decision := Decision{Allow: true, Reason: "written in by the operator", By: "human", WriteIn: line}
		p.record(req, decision)
		return decision, nil
	}
	decision := Decision{Allow: true, Reason: "chosen by the operator", By: "human", Chosen: chosen}
	p.record(req, decision)
	return decision, nil
}

// parseChoice reads a line as option numbers. It reports numeric=false when the
// line is not a number list at all, which is a write-in rather than a mistake,
// and an error only when it is a number list the question cannot accept.
func parseChoice(line string, req Request) (chosen []string, numeric bool, err error) {
	fields := strings.Split(line, ",")
	for _, f := range fields {
		n, convErr := strconv.Atoi(strings.TrimSpace(f))
		if convErr != nil {
			return nil, false, nil
		}
		if n < 1 || n > len(req.Choices) {
			return nil, true, fmt.Errorf("option %d is not one of the %d offered", n, len(req.Choices))
		}
		chosen = append(chosen, req.Choices[n-1])
	}
	if !req.MultiSelect && len(chosen) > 1 {
		return nil, true, errors.New("this question takes one answer; several were given")
	}
	return chosen, true, nil
}

func (p *Prompter) record(req Request, d Decision) {
	text := "denied: " + d.Reason
	if d.Allow {
		text = "allowed: " + d.Reason
	}
	p.Sink.Emit(Event{
		Kind: KindApprovalDone, At: time.Now().UTC(),
		ApprovalID: req.ID, Text: text, Rule: req.Rule, Path: req.Path,
	})
}

// read reads one line, honouring the context and the timeout.
//
// The read runs on its own goroutine because a blocking read on a terminal
// cannot be interrupted. That goroutine may outlive this call — it is parked
// on stdin — which is why the channel is buffered: an abandoned read must not
// leak a goroutine blocked on a send nobody will receive.
func (p *Prompter) read(ctx context.Context) (string, error) {
	if p.reader == nil {
		p.reader = bufio.NewReader(p.In)
	}
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := p.reader.ReadString('\n')
		ch <- result{line, err}
	}()

	var timeout <-chan time.Time
	if p.Timeout > 0 {
		timer := time.NewTimer(p.Timeout)
		defer timer.Stop()
		timeout = timer.C
	}

	select {
	case r := <-ch:
		if r.err != nil && r.line == "" {
			return "", r.err
		}
		return r.line, nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-timeout:
		return "", fmt.Errorf("no answer within %s", p.Timeout)
	}
}

// AutoApprover answers from a fixed rule set, for scripted and headless runs.
//
// It exists so an unattended run can be given a narrow, explicit standing
// permission — "unplanned scope in this task, for this hour" — instead of the
// two bad alternatives: a human who is not there, or approving everything.
type AutoApprover struct {
	// Rules are the rule IDs this approver will clear. Anything else is denied.
	Rules map[string]bool
	// Reason is recorded on every grant it issues. Required.
	Reason string
	// Sink records what it decided.
	Sink Sink
}

// Approve clears a listed rule and refuses everything else.
func (a AutoApprover) Approve(ctx context.Context, req Request) (Decision, error) {
	if a.Reason == "" {
		return Decision{}, errors.New("ui: an auto-approver must carry a reason to record on its grants")
	}
	// A rule list says which blocks may be cleared. It cannot say which of a
	// question's options a human would have picked, so it does not answer one:
	// a guess written into Chosen reads, downstream, exactly like a human's
	// answer, and nothing afterwards can tell them apart.
	if req.IsQuestion() {
		decision := Decision{Allow: false, Reason: "a standing approval cannot answer a question; no operator is attached"}
		a.emit(req, decision)
		return decision, nil
	}
	// Grantability is checked before the rule list, so a misconfigured list
	// naming a hard rule cannot clear one.
	if !req.Grantable || !a.Rules[req.Rule] {
		decision := Decision{Allow: false, Reason: "not in this run's standing approvals"}
		a.emit(req, decision)
		return decision, nil
	}
	decision := Decision{Allow: true, Reason: a.Reason, By: "human"}
	a.emit(req, decision)
	return decision, nil
}

func (a AutoApprover) emit(req Request, d Decision) {
	if a.Sink == nil {
		return
	}
	text := "denied: " + d.Reason
	if d.Allow {
		text = "allowed by standing approval: " + d.Reason
	}
	a.Sink.Emit(Event{
		Kind: KindApprovalDone, At: time.Now().UTC(),
		ApprovalID: req.ID, Text: text, Rule: req.Rule, Path: req.Path,
	})
}
