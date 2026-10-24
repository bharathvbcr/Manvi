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
// JSON decision line. The sink's lock is not held across that read: Emit
// locks only for the write, and this returns from Emit before reading.
type fdApprover struct {
	file   *os.File
	reader *bufio.Reader
	sink   ui.Sink
	mu     sync.Mutex
}

func newFDApprover(file *os.File, sink ui.Sink) *fdApprover {
	return &fdApprover{file: file, reader: bufio.NewReader(file), sink: sink}
}

type approvalDecisionLine struct {
	ApprovalID *string  `json:"approval_id"`
	Allow      bool     `json:"allow"`
	Chosen     []string `json:"chosen"`
	WriteIn    string   `json:"write_in"`
	Reason     string   `json:"reason"`
}

func (a *fdApprover) Approve(ctx context.Context, req ui.Request) (ui.Decision, error) {
	if err := ctx.Err(); err != nil {
		return declineDecision("cancelled"), nil //nolint:nilerr // a cancelled turn is a refusal, never consent and never an approver error
	}
	a.sink.Emit(ui.Event{
		Kind: ui.KindApproval, At: time.Now().UTC(),
		Text: req.Reason, Rule: req.Rule, Severity: req.Severity,
		Path: req.Path, TaskID: req.TaskID,
		Grantable: req.Grantable, ApprovalID: req.ID,
		Choices: req.Choices, MultiSelect: req.MultiSelect,
	})
	if err := ctx.Err(); err != nil {
		return declineDecision("cancelled"), nil //nolint:nilerr // a cancelled turn is a refusal, never consent and never an approver error
	}
	line, err := a.readLine(ctx)
	if err != nil {
		reason := "the approval fd closed before a decision"
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			reason = "cancelled"
		}
		return declineDecision(reason), nil
	}
	parsed, err := parseDecisionLine(line)
	if err != nil {
		return declineDecision("the approval decision was not one JSON object"), nil //nolint:nilerr // a malformed decision line is a refusal, never consent
	}
	if parsed.ApprovalID != nil && *parsed.ApprovalID != req.ID {
		return declineDecision("approval_id does not match the request"), nil
	}
	if req.IsQuestion() {
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
			}, nil
		}
		return declineDecision(parsed.Reason), nil
	}
	if parsed.Allow {
		return ui.Decision{Allow: true, Reason: parsed.Reason, By: "human"}, nil
	}
	return declineDecision(parsed.Reason), nil
}

func declineDecision(reason string) ui.Decision {
	if strings.TrimSpace(reason) == "" {
		reason = "declined"
	}
	return ui.Decision{Allow: false, Reason: reason}
}

// readLine reads one line on a goroutine so ctx.Done can return a refusal
// without waiting out the descriptor. The read is unblocked by expiring the
// file's deadline; the call then waits for that read to finish so a later
// Approve cannot observe a consumer still sitting on the buffer. The
// approver's own mutex is held here, never the sink's.
func (a *fdApprover) readLine(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_ = a.file.SetReadDeadline(time.Time{})

	type readResult struct {
		line []byte
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		line, err := a.reader.ReadBytes('\n')
		ch <- readResult{line, err}
	}()
	select {
	case res := <-ch:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if res.err != nil && len(bytes.TrimSpace(res.line)) == 0 {
			return nil, res.err
		}
		return res.line, nil
	case <-ctx.Done():
		_ = a.file.SetReadDeadline(time.Now().Add(-time.Second))
		<-ch
		return nil, ctx.Err()
	}
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
