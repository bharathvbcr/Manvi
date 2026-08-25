package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"manvi/llm"
)

// driver keeps ONE server alive across a sequence of calls.
//
// roundTrip builds a fresh server per invocation, which is right for the
// stateless ops and silently wrong for these: the compaction ledger and the
// calibrator live on the server, so a per-call server means every test of
// "what does the second step remember" actually measures a first step twice.
type driver struct {
	t   *testing.T
	srv *Server
	out *strings.Builder
	n   int
}

func newDriver(t *testing.T) *driver {
	t.Helper()
	out := &strings.Builder{}
	return &driver{t: t, srv: New(out, hostOpts()), out: out}
}

// call dispatches one request against the shared server and returns its
// response, so state accumulated by earlier calls is still there.
func (d *driver) call(req Request) Response {
	d.t.Helper()
	before := d.out.Len()
	d.srv.dispatch(context.Background(), req)
	if err := d.srv.flush(); err != nil {
		d.t.Fatalf("flush: %v", err)
	}
	line := strings.TrimSpace(d.out.String()[before:])
	var resp Response
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		d.t.Fatalf("undecodable response %q: %v", line, err)
	}
	d.n++
	return resp
}

func (d *driver) callPrepare(p PrepareParams) PrepareResult {
	d.t.Helper()
	return decodePrepare(d.t, d.call(prepareRequest(d.t, fmt.Sprint(d.n), p)))
}

func prepareRequest(t *testing.T, id string, p PrepareParams) Request {
	t.Helper()
	params, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return Request{ID: id, Op: OpChatPrepare, Params: params}
}

func settleRequest(t *testing.T, id string, p SettleParams) Request {
	t.Helper()
	params, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return Request{ID: id, Op: OpChatSettle, Params: params}
}

func decodePrepare(t *testing.T, resp Response) PrepareResult {
	t.Helper()
	if !resp.OK {
		t.Fatalf("chat.prepare failed: %+v", resp.Error)
	}
	var r PrepareResult
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func decodeSettle(t *testing.T, resp Response) SettleResult {
	t.Helper()
	if !resp.OK {
		t.Fatalf("chat.settle failed: %+v", resp.Error)
	}
	var r SettleResult
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// bulkyHistory builds a turn with `n` large tool results, the shape a real
// agent loop produces: grep and read output that dwarfs the conversation.
func bulkyHistory(n, linesEach int) []WireMessage {
	msgs := []WireMessage{{Role: "user", Text: "fix the build"}}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("call_%d", i)
		msgs = append(msgs, WireMessage{
			Role:      "assistant",
			ToolCalls: []WireToolCall{{ID: id, Name: "Grep", Arguments: `{"pattern":"x"}`}},
		})
		var b strings.Builder
		for ln := 0; ln < linesEach; ln++ {
			fmt.Fprintf(&b, "src/pkg/file%d.go:%d: some matching line of source code here\n", i, ln)
		}
		msgs = append(msgs, WireMessage{Role: "tool", ToolCallID: id, Text: b.String()})
	}
	return msgs
}

func TestTheSessionTableStaysBoundedUnderAMintingHost(t *testing.T) {
	sessions := newSessionTableForTest()
	for i := 0; i < maxChatSessions*3; i++ {
		id := fmt.Sprintf("session-%d", i)
		s := sessions.get(id)
		s.compacted[llm.CallID(id)] = "shortened"
		if got := len(sessions.byID); got > maxChatSessions {
			t.Fatalf("table grew to %d at insertion %d; cap is %d", got, i, maxChatSessions)
		}
	}
}

func TestEvictionKeepsAContinuouslyUsedSession(t *testing.T) {
	sessions := newSessionTableForTest()
	sessions.get("active")

	// Fill the table, then keep minting new sessions around one long-lived
	// conversation that keeps getting used — the shape of a desktop app with
	// one tab open for hours while others open and close around it.
	for i := 0; i < maxChatSessions; i++ {
		sessions.get(fmt.Sprintf("filler-%d", i))
	}
	for i := 0; i < 20; i++ {
		sessions.get("active") // the host's ongoing turn
		sessions.get(fmt.Sprintf("churn-%d", i))
		if got := len(sessions.byID); got > maxChatSessions {
			t.Fatalf("table grew to %d; cap is %d", got, maxChatSessions)
		}
		if _, ok := sessions.byID["active"]; !ok {
			t.Fatal("the continuously used session was evicted")
		}
	}
}

func TestIdleSessionsAreDroppedOnTheNextAccess(t *testing.T) {
	sessions := newSessionTableForTest()
	stale := sessions.get("stale")
	sessions.get("fresh")

	// Only the stale one goes back in time.
	stale.lastUsed = time.Now().Add(-2 * chatSessionIdleTTL)

	_ = sessions.get("anything-else")
	sessions.evictLocked("anything-else")

	if _, ok := sessions.byID["stale"]; ok {
		t.Fatal("an idle-past-TTL session survived a sweep")
	}
	if _, ok := sessions.byID["fresh"]; !ok {
		t.Fatal("a recently used session was swept for idleness")
	}
}

func newSessionTableForTest() *sessionTable {
	return &sessionTable{byID: map[string]*ChatSession{}}
}

// The property the whole plane exists for. A local server's KV cache is keyed
// on an unchanged token prefix, so a result shortened to a *different* string
// on a later step invalidates everything after it and costs a full re-prefill.
// Compaction must therefore be one-way: shortened once, that exact text
// forever.
func TestCompactionIsOneWay_SoThePromptPrefixOnlyEverGrows(t *testing.T) {
	history := bulkyHistory(10, 60)
	params := PrepareParams{
		SessionID:     "tab-1",
		System:        "You are a LaTeX assistant.",
		Messages:      history,
		ContextWindow: 8192,
	}

	d := newDriver(t)
	first := d.callPrepare(params)
	if len(first.Steps) == 0 {
		t.Fatal("nothing was compacted; the fixture is not over budget")
	}

	// Record what each result became, then replay the same history with the
	// shortened text substituted, exactly as a host would.
	shortened := map[string]string{}
	for _, s := range first.Steps {
		shortened[s.ToolCallID] = s.Text
	}
	for i := range history {
		if replacement, ok := shortened[history[i].ToolCallID]; ok && history[i].Role == "tool" {
			history[i].Text = replacement
		}
	}

	// A second step: one more exchange appended, the rest unchanged.
	history = append(history,
		WireMessage{Role: "assistant", Text: "Looking at the matches now."},
		WireMessage{Role: "user", Text: "and the second error?"})
	params.Messages = history

	second := d.callPrepare(params)

	for _, s := range second.Steps {
		if previous, seen := shortened[s.ToolCallID]; seen {
			t.Errorf("%s was compacted twice; the prefix moved.\n  was: %q\n  now: %q",
				s.ToolCallID, truncate(previous), truncate(s.Text))
		}
	}
}

// A different conversation must not inherit another's ledger, or a result the
// second one never saw is reported as already shortened and never gets
// shortened at all.
func TestSessionsDoNotShareACompactionLedger(t *testing.T) {
	params := PrepareParams{
		SessionID:     "tab-A",
		Messages:      bulkyHistory(10, 60),
		ContextWindow: 8192,
	}
	d := newDriver(t)
	a := d.callPrepare(params)
	if len(a.Steps) == 0 {
		t.Fatal("the fixture did not trigger compaction")
	}

	// Same server, different conversation.
	params.SessionID = "tab-B"
	b := d.callPrepare(params)
	if len(b.Steps) != len(a.Steps) {
		t.Errorf("a fresh session planned %d steps, the first planned %d — the ledger leaked",
			len(b.Steps), len(a.Steps))
	}
}

// Compaction must aim *past* the threshold. Landing exactly on it means the
// next tool result crosses it again and the prefix moves again, which is the
// cost this is all meant to avoid.
func TestCompactionAimsPastTheThresholdNotAtIt(t *testing.T) {
	r := newDriver(t).callPrepare(PrepareParams{
		SessionID:     "tab-1",
		Messages:      bulkyHistory(10, 60),
		ContextWindow: 8192,
	})

	if r.Target >= r.Threshold {
		t.Fatalf("Target %d is not below Threshold %d", r.Target, r.Threshold)
	}
	if len(r.Steps) > 0 && r.After > r.Target {
		// Not a hard failure — the floors bound how much can be reclaimed —
		// but landing above target with headroom left is the defect.
		t.Logf("after=%d target=%d threshold=%d (insufficient=%v)",
			r.After, r.Target, r.Threshold, r.Insufficient)
	}
	if r.After > r.Before {
		t.Errorf("compaction made the prompt bigger: %d → %d", r.Before, r.After)
	}
}

// The estimator runs high — around 25% against a real Qwen tokenizer, more on
// the JSON that tool results are made of. The server returns an exact count on
// every response, so the harness must converge on it rather than compact a
// model down to a fraction of the context it really had.
func TestTheBudgetCalibratesTowardsWhatTheServerCounts(t *testing.T) {
	base := PrepareParams{
		SessionID:     "tab-cal",
		Messages:      bulkyHistory(3, 20),
		ContextWindow: 32768,
	}

	d := newDriver(t)
	first := d.callPrepare(base)
	if first.CalibrationSamples != 0 {
		t.Fatalf("samples = %d before any observation", first.CalibrationSamples)
	}
	if first.CalibrationRatio != 1 {
		t.Fatalf("ratio = %v before any observation, want 1", first.CalibrationRatio)
	}

	// The server counted 40% fewer tokens than we estimated — the direction
	// the estimator actually errs in.
	observed := int(float64(first.After) * 0.6)
	base.ObservedPromptTokens = observed
	second := d.callPrepare(base)

	if second.CalibrationSamples != 1 {
		t.Fatalf("samples = %d after one observation", second.CalibrationSamples)
	}
	if second.CalibrationRatio >= 1 {
		t.Errorf("ratio = %v; an over-estimating estimator should be scaled down",
			second.CalibrationRatio)
	}
	if second.Before >= first.Before {
		t.Errorf("the calibrated estimate (%d) did not fall below the raw one (%d), "+
			"so more history was not made available", second.Before, first.Before)
	}
}

// A nonsense reading — a rejected request, a cumulative total, a proxy
// rewriting the prompt — must not move the budget.
func TestAWildObservationIsRejectedRatherThanBelieved(t *testing.T) {
	base := PrepareParams{
		SessionID:     "tab-wild",
		Messages:      bulkyHistory(3, 20),
		ContextWindow: 32768,
	}
	d := newDriver(t)
	first := d.callPrepare(base)

	base.ObservedPromptTokens = first.After * 50 // absurd
	second := d.callPrepare(base)
	if second.CalibrationSamples != 0 {
		t.Errorf("an out-of-bounds ratio was accepted (samples=%d, ratio=%v)",
			second.CalibrationSamples, second.CalibrationRatio)
	}
}

func TestPrepareRefusesAMissingSessionID(t *testing.T) {
	resp := roundTrip(t, hostOpts(), prepareRequest(t, "1", PrepareParams{
		Messages:      bulkyHistory(2, 5),
		ContextWindow: 8192,
	}))[0]
	if resp.OK {
		t.Fatal("chat.prepare accepted a request with no session_id")
	}
	if !strings.Contains(resp.Error.Message, "session_id") {
		t.Errorf("the refusal does not name the missing field: %s", resp.Error.Message)
	}
}

func TestForgetDropsTheLedger(t *testing.T) {
	params := PrepareParams{
		SessionID:     "tab-drop",
		Messages:      bulkyHistory(10, 60),
		ContextWindow: 8192,
	}
	d := newDriver(t)
	first := d.callPrepare(params)

	forget, err := json.Marshal(ForgetParams{SessionID: "tab-drop"})
	if err != nil {
		t.Fatal(err)
	}
	if resp := d.call(Request{ID: "forget", Op: OpChatForget, Params: forget}); !resp.OK {
		t.Fatalf("chat.forget failed: %+v", resp.Error)
	}
	after := d.callPrepare(params)
	if len(after.Steps) != len(first.Steps) {
		t.Errorf("after forget the plan had %d steps, want the original %d",
			len(after.Steps), len(first.Steps))
	}
}

// --- chat.settle ---

// The failure this prevents: a server without a tool parser for its model
// returns the call as prose, the host renders it as an answer, and a turn that
// did nothing reports success.
func TestSettleRecoversToolCallsTheServerDidNotParse(t *testing.T) {
	tools := []WireTool{{
		Name:        "Read",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"}}}`),
	}}

	cases := []struct {
		name, content, wantFormat string
	}{
		{
			name:       "hermes json",
			content:    `Let me look. <tool_call>{"name":"Read","arguments":{"file_path":"main.tex"}}</tool_call>`,
			wantFormat: "hermes-json",
		},
		{
			name: "qwen nested xml",
			content: `<tool_call><function=Read><parameter=file_path>main.tex</parameter>` +
				`</function></tool_call>`,
			wantFormat: "qwen-xml",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeSettle(t, roundTrip(t, hostOpts(), settleRequest(t, "1", SettleParams{
				Content: tc.content,
				Tools:   tools,
			}))[0])

			if len(got.Calls) != 1 {
				t.Fatalf("recovered %d calls, want 1 (format=%q text=%q)",
					len(got.Calls), got.Format, got.Text)
			}
			if got.Calls[0].Name != "Read" {
				t.Errorf("call name = %q, want Read", got.Calls[0].Name)
			}
			if !strings.Contains(got.Calls[0].Arguments, "main.tex") {
				t.Errorf("arguments lost the path: %s", got.Calls[0].Arguments)
			}
			if got.Format != tc.wantFormat {
				t.Errorf("Format = %q, want %q — a silent compensation is one nobody fixes",
					got.Format, tc.wantFormat)
			}
			if strings.Contains(got.Text, "<tool_call>") {
				t.Errorf("the call markup survived into the visible answer: %q", got.Text)
			}
		})
	}
}

// When the server did parse the calls, text that merely looks like one is
// prose. Recovering it would dispatch a call the model never made.
func TestSettleDoesNotRecoverWhenTheServerAlreadyParsedTheCalls(t *testing.T) {
	got := decodeSettle(t, roundTrip(t, hostOpts(), settleRequest(t, "1", SettleParams{
		Content:           `Here is how you would write it: <tool_call>{"name":"Read","arguments":{}}</tool_call>`,
		Tools:             []WireTool{{Name: "Read"}},
		ServerParsedCalls: true,
	}))[0])
	if len(got.Calls) != 0 {
		t.Errorf("recovered %d calls from a reply whose calls the server parsed", len(got.Calls))
	}
}

// An unmatched closing tag is reported, not acted on.
//
// Qwen3's template can end the prompt with a bare <think>, so generation starts
// inside the block and only the closing tag is emitted — but a model writing
// *about* think tags produces the identical byte stream, and in this harness
// that is the ordinary case rather than the exotic one. Settling used to guess
// prefill and move everything before the tag into reasoning, which deleted the
// answer whenever the guess was wrong. The suspicion is now surfaced through
// Reclassified so a host can declare the prefill, and the text is left alone.
func TestSettleReportsAPossiblePrefillWithoutEatingTheAnswer(t *testing.T) {
	got := decodeSettle(t, roundTrip(t, hostOpts(), settleRequest(t, "1", SettleParams{
		Content: "The user wants the preamble fixed. I should check main.tex first." +
			"</think>I'll start by reading main.tex.",
		Tools: []WireTool{{Name: "Read"}},
	}))[0])

	if !got.Reclassified {
		t.Error("an unmatched closing tag was not treated as a prefilled block")
	}
	if strings.Contains(got.Text, "</think>") {
		t.Errorf("a literal closing tag survived into the answer: %q", got.Text)
	}
	if !strings.Contains(got.Text, "check main.tex first") {
		t.Errorf("text before the unmatched tag was deleted from the answer: %q", got.Text)
	}
	if !strings.Contains(got.Text, "I'll start by reading") {
		t.Errorf("the real answer was lost: %q", got.Text)
	}
}

// A model that thinks about calling a tool writes something that looks like a
// call inside its think block. Extracting it would dispatch a call the model
// was only considering — so reasoning must be separated first.
func TestACallInsideAThinkBlockIsNotDispatched(t *testing.T) {
	got := decodeSettle(t, roundTrip(t, hostOpts(), settleRequest(t, "1", SettleParams{
		Content: `<think>I could call <tool_call>{"name":"Read","arguments":{"file_path":"x"}}` +
			`</tool_call> but the file is already open.</think>The file is already open.`,
		Tools: []WireTool{{Name: "Read"}},
	}))[0])
	if len(got.Calls) != 0 {
		t.Errorf("dispatched %d call(s) the model only contemplated", len(got.Calls))
	}
	if !strings.Contains(got.Text, "already open") {
		t.Errorf("the answer was lost: %q", got.Text)
	}
}

// Local servers truncate readily (mlx-vlm defaults to 2048). A call cut off
// mid-arguments must never run — half an argument object is a different
// request, not a smaller one — but the turn must not fail either, or every
// completed step is discarded over it.
func TestATruncatedToolCallIsRetryableRatherThanFatal(t *testing.T) {
	got := decodeSettle(t, roundTrip(t, hostOpts(), settleRequest(t, "1", SettleParams{
		Content:      `Reading it now. <tool_call>{"name":"Write","arguments":{"file_path":"a.tex","content":"\\doc`,
		Tools:        []WireTool{{Name: "Write"}},
		FinishReason: "length",
	}))[0])

	if len(got.Calls) != 0 {
		t.Fatalf("a call cut off mid-arguments was recovered and would be dispatched: %+v", got.Calls)
	}
	if !got.Truncated {
		t.Error("Truncated was not set for finish_reason=length")
	}
	if !got.TruncatedMidCall {
		t.Fatal("the truncation was not recognised as landing inside a call")
	}
	if got.RetryMessage == "" {
		t.Error("no retry message, so the host can only fail the turn")
	}
	if !strings.Contains(got.RetryMessage, "not run") {
		t.Errorf("the retry message does not say the call did not execute: %q", got.RetryMessage)
	}
}

// A clean reply must not be labelled truncated, or every turn ends with a
// spurious retry.
func TestAnOrdinaryReplyIsNotReportedAsTruncated(t *testing.T) {
	got := decodeSettle(t, roundTrip(t, hostOpts(), settleRequest(t, "1", SettleParams{
		Content:      "Done — the preamble now loads amsmath.",
		FinishReason: "stop",
	}))[0])
	if got.Truncated || got.TruncatedMidCall {
		t.Errorf("a clean reply was reported truncated: %+v", got)
	}
	if got.Text != "Done — the preamble now loads amsmath." {
		t.Errorf("the text was altered: %q", got.Text)
	}
	if got.Format != "" {
		t.Errorf("Format = %q for a reply that needed no recovery", got.Format)
	}
}

func truncate(s string) string {
	const limit = 60
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

// TestSettleDoesNotEatAnAnswerWhenTheServerSeparatedReasoning.
//
// The streaming path cancels a declared prefill the moment a reasoning delta
// arrives on its own channel. This op could not: it receives Content alone, so
// the contradiction is invisible here and the host is the only one who can see
// it. Without a way to say so, a host driving its own HTTP client against a
// server that separates reasoning had every answer filed as reasoning and got
// an empty Text back, with nothing saying why.
func TestSettleDoesNotEatAnAnswerWhenTheServerSeparatedReasoning(t *testing.T) {
	s := &Server{}
	params, err := json.Marshal(SettleParams{
		Content:            "The bug is on line 42; I fixed it.",
		AssumePrefill:      true, // the operator's declaration, wrong for this server
		ReasoningOutOfBand: true, // and the server just contradicted it
		ServerParsedCalls:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, serr := s.settle(params)
	if serr != nil {
		t.Fatalf("settle: %v", serr)
	}
	res, ok := got.(SettleResult)
	if !ok {
		t.Fatalf("settle returned %T", got)
	}
	if res.Text != "The bug is on line 42; I fixed it." {
		t.Errorf("Text = %q; the answer was filed as reasoning and the host gets nothing", res.Text)
	}
	if res.Reasoning != "" {
		t.Errorf("Reasoning = %q, want empty; content beside out-of-band reasoning is the answer", res.Reasoning)
	}
	if !res.PrefillDisproved {
		t.Error("PrefillDisproved = false; a host that is not told keeps the wrong setting forever")
	}
}

// With no contradiction reported, the declaration still stands — this op must
// not quietly stop honouring a setting that is correct for the server in use.
func TestSettleStillHonoursAnUncontradictedPrefill(t *testing.T) {
	s := &Server{}
	params, _ := json.Marshal(SettleParams{
		Content:           "thinking out loud</think>the answer",
		AssumePrefill:     true,
		ServerParsedCalls: true,
	})
	got, serr := s.settle(params)
	if serr != nil {
		t.Fatalf("settle: %v", serr)
	}
	res := got.(SettleResult)
	if res.Text != "the answer" {
		t.Errorf("Text = %q, want %q", res.Text, "the answer")
	}
	if res.PrefillDisproved {
		t.Error("PrefillDisproved = true with nothing contradicting the declaration")
	}
}

// A server that mislabels a capped generation as a normal stop is caught by its
// own numbers, the same way the streaming path catches it.
func TestSettleCatchesAMislabelledOutputCap(t *testing.T) {
	s := &Server{}
	params, _ := json.Marshal(SettleParams{
		Content:           "cut off mid-",
		FinishReason:      "stop", // the server's claim, and it is wrong
		OutputTokens:      16384,
		MaxTokensApplied:  16384,
		ServerParsedCalls: true,
	})
	got, serr := s.settle(params)
	if serr != nil {
		t.Fatalf("settle: %v", serr)
	}
	if !got.(SettleResult).Truncated {
		t.Error("Truncated = false for a reply that used the entire bound the host sent; " +
			"finish_reason is a claim and this one is wrong")
	}
}

// freshHistory is bulkyHistory with tool_call_ids nobody has used before,
// which is what a long conversation actually looks like: every step's results
// are new ones, and the step before it has scrolled out of the window the host
// still sends.
func freshHistory(round, n, linesEach int) []WireMessage {
	msgs := bulkyHistory(n, linesEach)
	for i := range msgs {
		if msgs[i].ToolCallID != "" {
			msgs[i].ToolCallID = fmt.Sprintf("r%d_%s", round, msgs[i].ToolCallID)
		}
		for j := range msgs[i].ToolCalls {
			msgs[i].ToolCalls[j].ID = fmt.Sprintf("r%d_%s", round, msgs[i].ToolCalls[j].ID)
		}
	}
	return msgs
}

// maxChatSessions caps how many ledgers exist and never capped how large one
// grows. The ledger is append-only — a plan adds an entry and only chat.forget
// ever removed one — so a single long-lived session grew linearly with
// requests: 2,000 entries and 621 KB after 40 of them, with no knee to level
// off at. A ledger entry only earns its place while the host is still sending
// the result it describes; once that message is gone the entry substitutes
// into nothing and suppresses nothing, and keeping it is pure residue.
func TestOneSessionsLedgerDoesNotGrowWithEveryRequest(t *testing.T) {
	const rounds = 40
	d := newDriver(t)
	var last []WireMessage
	for round := 0; round < rounds; round++ {
		last = freshHistory(round, 20, 60)
		if r := d.callPrepare(PrepareParams{
			SessionID:     "one-tab",
			Messages:      last,
			ContextWindow: 4096,
		}); len(r.Steps) == 0 {
			t.Fatalf("round %d compacted nothing; the fixture is not over budget", round)
		}
	}

	session := d.srv.chat.get("one-tab")
	live := map[llm.CallID]bool{}
	for _, m := range last {
		if m.Role == "tool" {
			live[llm.CallID(m.ToolCallID)] = true
		}
	}
	residue := 0
	for id := range session.compacted {
		if !live[id] {
			residue++
		}
	}
	if residue > 0 {
		t.Fatalf("the ledger holds %d entries for tool results the host no longer sends "+
			"(%d entries total after %d requests); it grows with requests rather than "+
			"with the conversation", residue, len(session.compacted), rounds)
	}
	if len(session.compacted) > len(live) {
		t.Fatalf("ledger holds %d entries for %d live tool results", len(session.compacted), len(live))
	}
}

// Pruning to what the host still sends is bounded by what the host sends, and
// one request is bounded by maxLineBytes. The cap is the second bound, for the
// history that itself keeps growing. It is not free — a dropped entry gets
// shortened again, possibly to different text — so it takes the front of the
// history, furthest from the protected tail the model is reasoning over.
func TestTheLedgerCapHoldsWhenTheHistoryItselfKeepsGrowing(t *testing.T) {
	session := newChatSession()
	var history []WireMessage
	const overflow = 500
	for i := 0; i < maxCompactedEntries+overflow; i++ {
		id := fmt.Sprintf("call_%d", i)
		session.compacted[llm.CallID(id)] = "shortened"
		history = append(history, WireMessage{Role: "tool", ToolCallID: id, Text: "x"})
	}

	session.pruneLedger(history)

	if got := len(session.compacted); got > maxCompactedEntries {
		t.Fatalf("ledger holds %d entries, cap is %d", got, maxCompactedEntries)
	}
	if _, ok := session.compacted["call_0"]; ok {
		t.Error("the oldest entry survived the cap; eviction must start at the front")
	}
	newest := llm.CallID(fmt.Sprintf("call_%d", maxCompactedEntries+overflow-1))
	if _, ok := session.compacted[newest]; !ok {
		t.Errorf("%s was evicted; the newest results are the ones the model is reasoning over", newest)
	}
}

// A ledger entry for a result still in the history must survive pruning, or
// the one-way property dies with it and the prompt prefix moves every step.
func TestPruningKeepsWhatTheHistoryStillReferences(t *testing.T) {
	session := newChatSession()
	session.compacted["kept"] = "short"
	session.compacted["gone"] = "short"

	session.pruneLedger([]WireMessage{
		{Role: "user", Text: "hello"},
		{Role: "assistant", ToolCalls: []WireToolCall{{ID: "kept", Name: "Grep"}}},
		{Role: "tool", ToolCallID: "kept", Text: "short"},
	})

	if _, ok := session.compacted["kept"]; !ok {
		t.Error("an entry for a result still in the history was pruned; it would be re-shortened")
	}
	if _, ok := session.compacted["gone"]; ok {
		t.Error("an entry for a result the host no longer sends was kept")
	}
}

// The budget numbers are arithmetic inputs, not opaque values. A window near
// math.MaxInt64 overflows agent.Budget.Target() — the wire reported
// threshold_tokens 9223372036854771711 with target_tokens 922337203685474712,
// which is not 70% of anything — and an overflowed budget does not look wrong,
// it looks like a plausible number. Refused rather than clamped: a plan for a
// window the model does not have is indistinguishable from one that fits.
func TestPrepareRefusesBudgetNumbersTheArithmeticCannotHonour(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    PrepareParams
	}{
		{"window at MaxInt64", PrepareParams{ContextWindow: math.MaxInt64}},
		{"window past the ceiling", PrepareParams{ContextWindow: maxTokenCount + 1}},
		{"reserved output at MaxInt64", PrepareParams{ContextWindow: 8192, ReservedOutput: math.MaxInt64}},
		{"negative reserved output", PrepareParams{ContextWindow: 8192, ReservedOutput: -1}},
		{"overhead at MaxInt64", PrepareParams{ContextWindow: 8192, Overhead: math.MaxInt64}},
		{"negative overhead", PrepareParams{ContextWindow: 8192, Overhead: -1}},
		{"observed prompt tokens at MaxInt64",
			PrepareParams{ContextWindow: 8192, ObservedPromptTokens: math.MaxInt64}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.p.SessionID = "tab-1"
			tc.p.Messages = []WireMessage{{Role: "user", Text: "hi"}}
			resp := newDriver(t).call(prepareRequest(t, "1", tc.p))
			if resp.OK {
				var got PrepareResult
				if err := json.Unmarshal(resp.Result, &got); err != nil {
					t.Fatal(err)
				}
				t.Fatalf("accepted: threshold_tokens=%d target_tokens=%d — target is %.1f%% of "+
					"threshold, and the contract says 70%%",
					got.Threshold, got.Target, 100*float64(got.Target)/float64(got.Threshold))
			}
			if resp.Error.Code != ErrBadRequest {
				t.Fatalf("code = %q, want %q", resp.Error.Code, ErrBadRequest)
			}
		})
	}
}

// The bound must not refuse the windows real models actually have.
func TestPrepareAcceptsTheLargestRealWindows(t *testing.T) {
	for _, window := range []int{4096, 32768, 262144, 1 << 20, maxTokenCount} {
		r := newDriver(t).callPrepare(PrepareParams{
			SessionID:     "tab-1",
			Messages:      []WireMessage{{Role: "user", Text: "hi"}},
			ContextWindow: window,
		})
		if r.Threshold <= 0 || r.Target <= 0 || r.Target > r.Threshold {
			t.Errorf("window %d: threshold=%d target=%d", window, r.Threshold, r.Target)
		}
		if ratio := float64(r.Target) / float64(r.Threshold); ratio < 0.69 || ratio > 0.71 {
			t.Errorf("window %d: target is %.3f of threshold, want ~0.70", window, ratio)
		}
	}
}
