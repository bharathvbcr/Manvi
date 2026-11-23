package agent

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// The batch: several independent lookups in one step.
//
// A model that reads five files one step at a time pays five round trips, and
// every one of them re-sends the whole conversation. A batch is one step: the
// loop admits each inner call through the same checks a top-level call passes,
// runs the admitted ones concurrently, and returns one result holding only the
// lines each call asked to keep. It is batching, not scripting — a call cannot
// see another's result. The tools it may carry are the registry's Batchable
// ones and nothing else.
//
// What the inner calls are not: tool_use blocks. They are never logged as
// session.ToolCall or session.ToolResult, because the session projection turns
// those into provider messages, and a result answering a call the provider
// never issued is an orphan it rejects. The batch call itself carries the one
// result; the gate's verdict on each inner call is still recorded, under a
// derived ID, by recordPolicy.

const (
	// batchConcurrency bounds how many inner calls run at once. Each can start
	// a subprocess (dcgrep, devmap, git), so this is a bound on processes too.
	batchConcurrency = 4
	// defaultBatchLines is how many lines of one call's answer are kept when
	// the call names no max_lines.
	defaultBatchLines = 200
	// maxBatchLines is the most max_lines may ask for.
	maxBatchLines = 2000
	// maxBatchPattern bounds a match expression.
	maxBatchPattern = 512
	// maxBatchCallBytes bounds what one call contributes. Line caps alone do
	// not bound a result: a minified file is one line.
	maxBatchCallBytes = 32 << 10
	// maxBatchBytes bounds what the calls' answers contribute to a batch
	// result together; each section's one-line header comes on top.
	maxBatchBytes = 128 << 10
)

// batchCallBudget is the byte budget the call whose turn it is gets in the
// fold: its own bound, or what the calls before it left of the batch, whichever
// is less. Sixteen small answers therefore do not shrink each other's room, and
// one large answer gets the full per-call bound when the rest are small.
//
// It is decided as the fold proceeds, in call order, so a handler running
// concurrently cannot know it; the MaxBytes in its View is the per-call bound,
// an upper limit. A handler whose side effect depends on its answer being shown
// whole defers it on Result.OnShown, and runBatch commits that only after
// batchKeep has said the answer was kept in full.
func batchCallBudget(remaining int) int {
	return max(0, min(maxBatchCallBytes, remaining))
}

type batchItem struct {
	Tool     string         `json:"tool"`
	Args     jsontext.Value `json:"args"`
	Match    string         `json:"match"`
	MaxLines int            `json:"max_lines"`
}

// batchSlot is one inner call: admitted and run, or refused with a reason.
type batchSlot struct {
	item    batchItem
	id      llm.CallID
	refusal string
	filter  *regexp.Regexp
	limit   int
	result  tools.Result
}

func (l *Loop) runBatch(ctx context.Context, out *Outcome, tally *stepTally, call llm.ToolCallBlock) tools.Result {
	var args struct {
		Calls []batchItem `json:"calls"`
	}
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return tools.Errorf("bad arguments to %s: %v", tools.BatchToolName, err)
	}
	if len(args.Calls) == 0 || len(args.Calls) > tools.MaxBatchCalls {
		return tools.Errorf("%s takes 1..%d calls, got %d", tools.BatchToolName, tools.MaxBatchCalls, len(args.Calls))
	}

	// Admission is serial and in order, because the repeat ledger and the
	// offered set are unsynchronised loop state and because a refusal has to
	// be the same whatever the scheduler does.
	slots := make([]*batchSlot, len(args.Calls))
	for i, item := range args.Calls {
		slot := &batchSlot{item: item, id: llm.CallID(fmt.Sprintf("%s#%d", call.ID, i+1))}
		slots[i] = slot
		slot.refusal = l.admitBatchItem(slot)
	}

	// Run what was admitted, bounded. Only the handlers run concurrently;
	// nothing here touches loop state.
	var wg sync.WaitGroup
	sem := make(chan struct{}, batchConcurrency)
	for _, slot := range slots {
		if slot.refusal != "" {
			continue
		}
		wg.Add(1)
		go func(slot *batchSlot) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				slot.result = tools.Errorf("not run: %v", ctx.Err())
				return
			}
			defer func() { <-sem }()
			view := tools.View{MaxLines: slot.limit, MaxBytes: maxBatchCallBytes, Filtered: slot.filter != nil}
			slot.result = l.tools.Run(tools.WithView(ctx, view),
				tools.Call{ID: slot.id, Name: slot.item.Tool, Arguments: []byte(slot.item.Args)})
		}(slot)
	}
	wg.Wait()

	// Fold serially, in call order, so accounting and the evidence trail read
	// exactly as if the calls had been made one after another.
	var text strings.Builder
	fmt.Fprintf(&text, "%d call(s):\n", len(slots))
	remaining := maxBatchBytes
	for i, slot := range slots {
		fmt.Fprintf(&text, "\n## [%d] %s", i+1, slot.item.Tool)
		if slot.refusal != "" {
			fmt.Fprintf(&text, " — refused, not run\n%s\n", slot.refusal)
			continue
		}
		out.ToolCalls++
		l.account(out, tally, slot.item.Tool, slot.result)
		if err := l.recordPolicy(out, slot.id, slot.item.Tool, slot.result); err != nil {
			return tools.Errorf("recording batch call %d: %v", i+1, err)
		}
		if slot.result.IsError {
			text.WriteString(" — error")
		}
		text.WriteString("\n")
		kept, whole := batchKeep(slot, batchCallBudget(remaining))
		text.WriteString(kept)
		remaining -= len(kept)
		// Committed only for an answer the model was shown in full: a file
		// the fold cut is not a file read, whatever the handler hoped.
		if whole && !slot.result.IsError && slot.result.OnShown != nil {
			slot.result.OnShown()
		}
	}
	return tools.Result{Text: text.String()}
}

// SerialLookups is what the loop does about consecutive steps that each made a
// single read-only lookup while devcouncil_batch was offered — the shape a
// batch exists to replace, since every one of those steps re-sends the whole
// conversation.
type SerialLookups string

const (
	// SerialLookupsOff says nothing and refuses nothing.
	SerialLookupsOff SerialLookups = "off"
	// SerialLookupsNote appends one line to the result every
	// serialLookupNote steps in the run.
	SerialLookupsNote SerialLookups = "note"
	// SerialLookupsRefuse notes at serialLookupNote and refuses the lookup
	// that would make serialLookupRefuse in a row. It is the default.
	SerialLookupsRefuse SerialLookups = "refuse"
)

// serialLookupModes is every accepted spelling. Empty is the default, refuse.
var serialLookupModes = map[SerialLookups]bool{
	"": true, SerialLookupsOff: true, SerialLookupsNote: true, SerialLookupsRefuse: true,
}

const (
	// serialLookupNote is how many consecutive one-lookup steps earn a note.
	serialLookupNote = 3
	// serialLookupRefuse is the run length at which, in refuse mode, the next
	// lone lookup is refused instead of run.
	serialLookupRefuse = 5
)

// serialLookupMode is the configured mode with the default applied.
func (l *Loop) serialLookupMode() SerialLookups {
	if l.cfg.SerialLookups == "" {
		return SerialLookupsRefuse
	}
	return l.cfg.SerialLookups
}

// singleLookup reports whether a step's calls are one lookup: one batchable
// call, or a batch holding exactly one call. The second counts because
// wrapping a lone lookup in a batch is otherwise the way round the rule.
func (l *Loop) singleLookup(calls []llm.ToolCallBlock) bool {
	if l.tools == nil || len(calls) != 1 {
		return false
	}
	call := calls[0]
	if call.Name != tools.BatchToolName {
		return l.tools.Batchable(call.Name)
	}
	var probe struct {
		Calls []jsontext.Value `json:"calls"`
	}
	if err := json.Unmarshal(call.Arguments, &probe); err != nil {
		return false
	}
	return len(probe.Calls) == 1
}

// refuseSerialLookup returns the refusal for a lone lookup that would extend
// the run to serialLookupRefuse, and resets the run, so the same call sent
// again runs: a chain of lookups that really do depend on each other costs one
// step per serialLookupRefuse and can never deadlock. ok is false when the call
// should run.
//
// The refusal happens before the call reaches runCall, so it never enters the
// repeat ledger and never reaches the progress tracker: it is the harness's
// decision, not the model circling, and chargeStep charges it as one step.
func (l *Loop) refuseSerialLookup(single bool) (tools.Result, bool) {
	if !single || l.serialLookupMode() != SerialLookupsRefuse || !l.allows(tools.BatchToolName) ||
		l.serialLookups+1 < serialLookupRefuse {
		return tools.Result{}, false
	}
	l.serialLookups = 0
	return tools.Result{IsError: true, Text: fmt.Sprintf("[harness: refused, not run: %d steps in a row each "+
		"made a single lookup. Send the lookups you still need together in one %s call. If this one depends "+
		"on the answer before it, send it again on its own and it will run.]", serialLookupRefuse-1,
		tools.BatchToolName)}, true
}

// noteSerialLookup counts a lone lookup that ran, and appends one line to its
// result when the run reaches serialLookupNote. Appended after the call was
// observed, so the stall tracker's digest of the result is unchanged, and only
// said when the batch tool is actually offered.
func (l *Loop) noteSerialLookup(result tools.Result) tools.Result {
	l.serialLookups++
	mode := l.serialLookupMode()
	if mode == SerialLookupsOff || l.serialLookups != serialLookupNote || len(result.Content) > 0 ||
		!l.allows(tools.BatchToolName) {
		return result
	}
	if mode == SerialLookupsNote {
		l.serialLookups = 0
	}
	result.Text += fmt.Sprintf("\n\n[harness: %d steps in a row each made a single lookup. Lookups that do not "+
		"depend on each other's answers belong in one %s call.]", serialLookupNote, tools.BatchToolName)
	if mode == SerialLookupsRefuse {
		result.Text = strings.TrimSuffix(result.Text, "]") + fmt.Sprintf(" A lone lookup that would make %d "+
			"in a row is refused once.]", serialLookupRefuse)
	}
	return result
}

// admitBatchItem returns why one inner call may not run, or "" when it may.
func (l *Loop) admitBatchItem(slot *batchSlot) string {
	args, refusal := l.admitInner("a batch", slot.item.Tool, slot.item.Args)
	if refusal != "" {
		return refusal
	}
	slot.item.Args = args
	if slot.item.MaxLines < 0 || slot.item.MaxLines > maxBatchLines {
		return fmt.Sprintf("max_lines must be in 1..%d", maxBatchLines)
	}
	slot.limit = slot.item.MaxLines
	if slot.limit == 0 {
		slot.limit = defaultBatchLines
	}
	if slot.item.Match != "" {
		if len(slot.item.Match) > maxBatchPattern {
			return fmt.Sprintf("match is longer than %d bytes", maxBatchPattern)
		}
		re, err := regexp.Compile(slot.item.Match)
		if err != nil {
			return fmt.Sprintf("match is not a valid regular expression: %v", err)
		}
		slot.filter = re
	}
	return ""
}

// innerKind names a container tool the way a refusal does.
func innerKind(name string) string {
	if name == tools.ScriptToolName {
		return "a script"
	}
	return "a batch"
}

// admitInner is the admission every call made on a container tool's behalf
// passes, a batch item or a script's call alike: a tool the registry has,
// offered this step, a read-only lookup, not a container itself, and within the
// repeat ledger a top-level call is counted in. One function, so the two
// containers cannot drift apart about what may run inside them. where names the
// container in the refusal ("a batch", "a script"). It returns the arguments to
// run with (an absent object becomes {}) and the refusal, "" when the call may
// run.
func (l *Loop) admitInner(where, name string, args jsontext.Value) (jsontext.Value, string) {
	switch {
	case name == "":
		return args, "a call needs a tool name"
	case name == tools.BatchToolName || name == tools.ScriptToolName:
		return args, fmt.Sprintf("%s cannot contain %s", where, innerKind(name))
	case !l.tools.Has(name):
		// A model that read a short name somewhere sends it here; saying "not
		// batchable" sent it off to make every lookup on its own.
		if full := "devcouncil_" + name; l.tools.Batchable(full) && l.allows(full) {
			return args, fmt.Sprintf("no tool is named %s; the lookup is %s", boundField(name), full)
		}
		return args, fmt.Sprintf("no tool is named %s", boundField(name))
	case !l.tools.Batchable(name):
		return args, fmt.Sprintf("%s cannot run inside %s: only read-only lookups can. Call it on its own.", boundField(name), where)
	case !l.allows(name):
		return args, l.notOfferedText(name)
	}
	if len(args) == 0 {
		args = jsontext.Value("{}")
	}
	// The same ledger a top-level call is counted in, so a container is not a
	// way round the repeat refusal.
	if repeat := l.repeats.seen(name, []byte(args)); repeat > RepeatLimit {
		return args, fmt.Sprintf("this exact call to %s was already made %d times in this turn with the same "+
			"arguments, so it was not run again", name, repeat-1)
	}
	return args, ""
}

// batchKeep is the part of one answer the call asked to keep, and a line that
// says what was left out. A filtered answer that did not say so would read as
// the whole answer. whole is true only when nothing at all was left out: no
// filter, no line cap reached, no byte bound reached.
func batchKeep(slot *batchSlot, budget int) (text string, whole bool) {
	if len(slot.result.Content) > 0 {
		return "[this answer is not text and was left out of the batch; call the tool on its own]\n", false
	}
	lines := strings.SplitAfter(slot.result.Text, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	total := len(lines)
	var kept strings.Builder
	matched, shown := 0, 0
	clipped := false
	for _, line := range lines {
		if slot.filter != nil && !slot.filter.MatchString(line) {
			continue
		}
		matched++
		// Once a line has not fit, nothing after it is shown: keeping a later,
		// shorter line would leave a gap the model reads as consecutive text.
		// Matches are still counted so the filter note stays accurate.
		if clipped || shown >= slot.limit {
			continue
		}
		if kept.Len()+len(line) > budget {
			clipped = true
			continue
		}
		shown++
		kept.WriteString(line)
		if !strings.HasSuffix(line, "\n") {
			kept.WriteString("\n")
		}
	}
	if clipped {
		fmt.Fprintf(&kept, "[the answer stops after %d line(s): the next one passed the batch's byte bound, and nothing after it is shown; call the tool on its own for the rest]\n", shown)
	}
	switch {
	case slot.filter != nil:
		fmt.Fprintf(&kept, "[%d of %d lines matched; %d shown]\n", matched, total, shown)
	case shown < total:
		fmt.Fprintf(&kept, "[%d of %d lines shown; raise max_lines or read the rest on its own]\n", shown, total)
	}
	return kept.String(), slot.filter == nil && !clipped && shown == total
}
