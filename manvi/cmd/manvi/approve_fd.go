package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/ui"
)

// maxDecisionLine bounds one decision line, newline excluded. A real decision
// is a few hundred bytes; the cap is generous enough for a long reason or
// write-in and small enough that a host, or anything holding the descriptor,
// cannot make the process buffer an unbounded line.
const maxDecisionLine = 64 << 10

// maxDiscardNotices bounds how many unsolicited lines are reported on the
// stream, so a writer that floods the descriptor cannot flood the transcript.
const maxDiscardNotices = 16

// claimApproveFD is run once, straight after the flags are parsed and before
// this process starts any child: it marks the descriptor close-on-exec, so no
// tool, MCP server or git the harness spawns inherits it.
//
// Marking it later is too late. Every spawn site starts its child with
// whatever descriptors are inheritable at that moment, and the model chooses
// what its shell tool runs. A child holding a writable end can answer its own
// escalation, and one holding the read end can take the host's decisions.
//
// A descriptor that is not open is refused here, with the flag named, rather
// than surfacing later as an approval that "closed before a decision".
func claimApproveFD(opts runOptions) error {
	if !opts.approveFDSet {
		return nil
	}
	if err := setCloseOnExec(opts.approveFD); err != nil {
		return fmt.Errorf("--approve-fd %d: %w", opts.approveFD, err)
	}
	return nil
}

// approverForRun is nil when --approve-fd was not set, which is what
// nativeToolsWith is given for every run that did not ask. The descriptor is
// wrapped, not duped, and the *os.File is retained so its finalizer does not
// close the caller's fd out from under a later read.
func approverForRun(opts runOptions, sink ui.Sink) (ui.Approver, error) {
	if !opts.approveFDSet {
		return nil, nil
	}
	f := os.NewFile(uintptr(opts.approveFD), "approve-fd")
	if f == nil {
		return nil, fmt.Errorf("--approve-fd %d: not a usable file descriptor", opts.approveFD)
	}
	return newFDApprover(f, sink), nil
}

// fdApprover answers one approval by writing approval.request and reading one
// JSON decision line.
//
// One goroutine owns the descriptor for the life of the process and turns it
// into lines. A request is answered only by a line that arrives while it is
// pending; a line that arrives with nothing pending is discarded and reported,
// so an answer written early, or late for a request the turn gave up on, can
// never decide a request it was not written for. The descriptor is read with
// plain blocking reads and never switched to non-blocking: O_NONBLOCK belongs
// to the open file description the host shares, and changing it would change
// the host's own I/O on the same descriptor.
//
// A request waits on that goroutine and on its context, so a cancelled turn
// returns at once whatever the host does. Neither the approver's mutex nor the
// sink's is held while waiting.
type fdApprover struct {
	file *os.File
	sink ui.Sink
	// slot admits one request at a time: a decision line carries no request
	// of its own unless the host echoes the id, so two pending requests could
	// not be told apart.
	slot chan struct{}

	mu        sync.Mutex
	pending   *pendingDecision
	closedErr error
	discarded int
}

// pendingDecision is the one request currently waiting for a line.
type pendingDecision struct{ lines chan decisionLine }

// decisionLine is what the reader hands to the pending request: one line, or
// the error that ended the stream.
type decisionLine struct {
	line    []byte
	tooLong bool
	err     error
}

func newFDApprover(file *os.File, sink ui.Sink) *fdApprover {
	a := &fdApprover{file: file, sink: sink, slot: make(chan struct{}, 1)}
	go a.readLoop(bufio.NewReader(file))
	return a
}

type approvalDecisionLine struct {
	ApprovalID *string  `json:"approval_id"`
	Allow      bool     `json:"allow"`
	Chosen     []string `json:"chosen"`
	WriteIn    string   `json:"write_in"`
	Reason     string   `json:"reason"`
}

// readLoop is the descriptor's only reader. It ends when the descriptor does
// (end of file, or closed), after telling any pending request why.
func (a *fdApprover) readLoop(r *bufio.Reader) {
	for {
		line, tooLong, err := readDecisionLine(r)
		if len(line) > 0 || tooLong {
			a.deliver(decisionLine{line: line, tooLong: tooLong})
		}
		if err != nil {
			a.finish(err)
			return
		}
	}
}

// deliver hands a line to the pending request, or discards it.
func (a *fdApprover) deliver(l decisionLine) {
	a.mu.Lock()
	p := a.pending
	a.pending = nil
	var note string
	if p == nil {
		a.discarded++
		switch {
		case a.discarded < maxDiscardNotices:
			note = "a line arrived on the approval descriptor while no approval was pending and was discarded"
		case a.discarded == maxDiscardNotices:
			note = "a line arrived on the approval descriptor while no approval was pending and was discarded; further discarded lines are not reported"
		}
	}
	a.mu.Unlock()
	if p != nil {
		p.lines <- l // capacity 1, and the slot admits one request: never blocks
		return
	}
	if note != "" {
		a.sink.Emit(ui.Event{Kind: ui.KindNotice, At: time.Now().UTC(), Text: note})
	}
}

// finish records the end of the stream and releases a pending request.
func (a *fdApprover) finish(err error) {
	a.mu.Lock()
	a.closedErr = err
	p := a.pending
	a.pending = nil
	a.mu.Unlock()
	if p != nil {
		p.lines <- decisionLine{err: err}
	}
}

// await registers a pending request, or reports that the stream has ended.
func (a *fdApprover) await() (*pendingDecision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closedErr != nil {
		return nil, a.closedErr
	}
	a.pending = &pendingDecision{lines: make(chan decisionLine, 1)}
	return a.pending, nil
}

// withdraw un-registers p if no line has claimed it.
func (a *fdApprover) withdraw(p *pendingDecision) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pending == p {
		a.pending = nil
	}
}

func (a *fdApprover) Approve(ctx context.Context, req ui.Request) (ui.Decision, error) {
	cancelled := declineDecision("cancelled")
	if ctx.Err() != nil {
		return cancelled, nil
	}
	select {
	case a.slot <- struct{}{}:
		defer func() { <-a.slot }()
	case <-ctx.Done():
		return cancelled, nil
	}
	if req.ID == "" {
		// Every request the host is shown has an id it can echo.
		req.ID = ui.NewRequestID("approval")
	}

	p, err := a.await()
	if err != nil {
		return declineDecision("the approval fd closed before a decision"), nil //nolint:nilerr // a closed descriptor is a refusal, never consent
	}
	defer a.withdraw(p)

	a.sink.Emit(ui.Event{
		Kind: ui.KindApproval, At: time.Now().UTC(),
		Text: req.Reason, Rule: req.Rule, Severity: req.Severity,
		Path: req.Path, TaskID: req.TaskID,
		Grantable: req.Grantable, ApprovalID: req.ID,
		Choices: req.Choices, MultiSelect: req.MultiSelect,
	})

	var got decisionLine
	select {
	case got = <-p.lines:
	case <-ctx.Done():
		return cancelled, nil
	}
	if ctx.Err() != nil {
		return cancelled, nil
	}
	if got.err != nil {
		return declineDecision("the approval fd closed before a decision"), nil
	}
	if got.tooLong {
		return declineDecision(fmt.Sprintf("the approval decision line is over %d bytes", maxDecisionLine)), nil
	}
	return a.decide(req, got.line), nil
}

// decide turns one line into the decision for req.
func (a *fdApprover) decide(req ui.Request, line []byte) ui.Decision {
	parsed, err := parseDecisionLine(line)
	if err != nil {
		return declineDecision("the approval decision was not one JSON object")
	}
	if parsed.ApprovalID != nil {
		if *parsed.ApprovalID != req.ID {
			return declineDecision("approval_id does not match the request")
		}
	} else {
		// The id is optional in the contract, so an unbound line is accepted.
		// It is bound to nothing but timing, and the stream says so.
		a.sink.Emit(ui.Event{
			Kind: ui.KindNotice, At: time.Now().UTC(), ApprovalID: req.ID,
			Text: "the approval decision carried no approval_id, so it is bound to this request only by arriving while it was pending",
		})
	}
	if req.IsQuestion() {
		if err := checkChosen(req, parsed.Chosen); err != nil {
			return declineDecision(err.Error())
		}
		decision := ui.Decision{
			Allow:   parsed.Allow,
			Chosen:  parsed.Chosen,
			WriteIn: parsed.WriteIn,
			Reason:  parsed.Reason,
			By:      "human",
		}
		// Answered, not Allow. A line that says allow with nothing chosen is
		// not an answer, and this must not invent one.
		if decision.Answered() {
			return ui.Decision{
				Allow: true, Chosen: parsed.Chosen, WriteIn: parsed.WriteIn,
				Reason: parsed.Reason, By: "human",
			}
		}
		return declineDecision(parsed.Reason)
	}
	if parsed.Allow {
		return ui.Decision{Allow: true, Reason: parsed.Reason, By: "human"}
	}
	return declineDecision(parsed.Reason)
}

// checkChosen refuses a selection the question did not offer: a value that is
// not among its choices, the same one twice, or more than one where the
// question is not multi-select.
func checkChosen(req ui.Request, chosen []string) error {
	if len(chosen) > 1 && !req.MultiSelect {
		return errors.New("more than one option was chosen for a single-select question")
	}
	offered := make(map[string]bool, len(req.Choices))
	for _, c := range req.Choices {
		offered[c] = true
	}
	seen := make(map[string]bool, len(chosen))
	for _, c := range chosen {
		if !offered[c] {
			return errors.New("a chosen value is not among the offered choices")
		}
		if seen[c] {
			return errors.New("a choice was selected more than once")
		}
		seen[c] = true
	}
	return nil
}

func declineDecision(reason string) ui.Decision {
	if strings.TrimSpace(reason) == "" {
		reason = "declined"
	}
	return ui.Decision{Allow: false, Reason: reason}
}

// readDecisionLine reads one line without ever holding more than
// maxDecisionLine bytes of it. A longer line is reported with tooLong and no
// content, and the rest of it is read and dropped up to its newline, so the
// next call starts on the next line. The line is returned with its newline;
// at the end of the stream a final unterminated line comes back with io.EOF.
func readDecisionLine(r *bufio.Reader) (line []byte, tooLong bool, err error) {
	for {
		chunk, readErr := r.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > maxDecisionLine+1 { // the +1 is the newline
				tooLong, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(readErr, bufio.ErrBufferFull) {
			continue
		}
		err = readErr
		break
	}
	if !tooLong && len(bytes.TrimSuffix(line, []byte("\n"))) > maxDecisionLine {
		tooLong, line = true, nil
	}
	return line, tooLong, err
}

// parseDecisionLine accepts one JSON object and nothing after it. null, an
// array, two objects, or truncated JSON are refusals for the caller.
func parseDecisionLine(line []byte) (approvalDecisionLine, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return approvalDecisionLine{}, errors.New("not one JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	var parsed approvalDecisionLine
	if err := dec.Decode(&parsed); err != nil {
		return approvalDecisionLine{}, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return approvalDecisionLine{}, errors.New("not one JSON object")
	}
	return parsed, nil
}
