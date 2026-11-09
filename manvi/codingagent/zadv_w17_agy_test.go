package codingagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// Adversarial tests for lane w17-agy (ft-f37eab2c, ft-47905b10), by the
// independent tester. Tests named ...Gap... log a finding and pass; a test that
// fails marks a BREAK and says so in its message.

// ---- neutraliseText --------------------------------------------------------

// Edge encodings, pinned to what the function does today (all HOLD the
// criterion's list). Overlong and surrogate encodings are invalid UTF-8 and are
// shown byte by byte, never decoded to the control they imitate.
func TestAdvW17NeutraliseEdgeEncodings(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"overlong NUL", "\xc0\x80", `\xc0\x80`},
		{"overlong ESC (c0 9b)", "\xc0\x9b", `\xc0\x9b`},
		{"overlong 3-byte ESC", "\xe0\x80\x9b", `\xe0\x80\x9b`},
		{"overlong 4-byte LF", "\xf0\x80\x80\x8a", `\xf0\x80\x80\x8a`},
		{"surrogate", "\xed\xa0\x80", `\xed\xa0\x80`},
		{"high surrogate pair as UTF-8", "\xed\xa0\xbd\xed\xb8\x80", `\xed\xa0\xbd\xed\xb8\x80`},
		{"beyond U+10FFFF", "\xf4\x90\x80\x80", `\xf4\x90\x80\x80`},
		{"lone 0x9b byte (C1 CSI)", "\x9b31m", `\x9b31m`},
		{"U+009B encoded", "\xc2\x9b31m", `\x9b31m`},
		{"NEL", "\u0085", `\x85`},
		{"DEL", "\x7f", `\x7f`},
		{"CR", "a\rb", `a\x0db`},
		{"real U+FFFD kept", "\xef\xbf\xbd", "�"},
		{"U+FFFD then invalid byte", "\xef\xbf\xbd\xff", "�" + `\xff`},
		{"truncated 4-byte at end", "ab\xf0\x9f\x98", `ab\xf0\x9f\x98`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := neutraliseText(tc.in); got != tc.want {
				t.Fatalf("neutraliseText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Invisible and line-separator characters outside the criterion's list. They
// pass through unchanged today. GAP (out of the stated criterion): U+2028/2029,
// U+FEFF, U+00AD, the deprecated U+206A..206F formatters, U+2060..2064, the Unicode
// tag block (U+E0000..E007F, which can smuggle invisible ASCII) and U+FFF9..FFFB
// are not neutralised. The test pins that so a later change is deliberate.
func TestAdvW17NeutraliseGapInvisibleCharsPassThrough(t *testing.T) {
	for _, r := range []rune{0x2028, 0x2029, 0xFEFF, 0x00AD, 0x180E, 0x2060, 0x2062, 0x206A, 0x206F, 0xFFF9, 0xFFFB, 0xE0001, 0xE0041, 0xE007F} {
		in := "a" + string(r) + "b"
		if got := neutraliseText(in); got == in {
			t.Logf("GAP: %U passes through neutraliseText unchanged", r)
		} else {
			t.Errorf("%U is now neutralised (%q); update this pin", r, got)
		}
	}
}

// The output is ambiguous: text the agent wrote as a literal backslash escape is
// the same string as text neutralised from the real control. A reader of the
// record cannot tell `\x1b` (agent wrote a backslash) from ESC. GAP, not a
// criterion break: the criterion asks only that the control not reach the host.
func TestAdvW17NeutraliseGapLiteralEscapesSpoofNeutralisedOnes(t *testing.T) {
	pairs := [][2]string{
		{"\x1b[2J", `\x1b[2J`},
		{"\u202eevil", `<U+202E>evil`},
		{"\xff", `\xff`},
	}
	for _, p := range pairs {
		if neutraliseText(p[0]) == neutraliseText(p[1]) {
			t.Logf("GAP: %q and %q neutralise to the same record text %q", p[0], p[1], neutraliseText(p[0]))
		} else {
			t.Errorf("%q and %q are now distinguishable; update this pin", p[0], p[1])
		}
	}
}

// Property: for arbitrary bytes the output is valid UTF-8, holds no rune the
// function is meant to remove, is a fixed point (double neutralisation is a
// no-op), never grows past 4x, and keeps every newline and tab.
func TestAdvW17NeutraliseProperties(t *testing.T) {
	state := uint64(17)
	next := func(n int) int { // a fixed xorshift stream: reproducible, not a source of secrets
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		return int(state % uint64(n)) //nolint:gosec // n is a small positive constant and the result is below n
	}
	alphabet := []string{"a", " ", "\n", "\t", "\x00", "\x1b", "\x7f", "\x9b", "\xc2\x9b", "\xff", "\xc0\x80", "\xed\xa0\x80", "\u202e", "\u2066", "\u200f", "\u061c", "€", "😀", "\xf0\x9f", "\xef\xbf\xbd", "\\", "x1b", "<U+202E>"}
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		for n := next(24); n > 0; n-- {
			if next(5) == 0 {
				b.WriteByte(byte(next(256))) //nolint:gosec // value is below 256
			} else {
				b.WriteString(alphabet[next(len(alphabet))])
			}
		}
		in := b.String()
		out := neutraliseText(in)
		if !utf8.ValidString(out) {
			t.Fatalf("invalid UTF-8 out for %q: %q", in, out)
		}
		for j := 0; j < len(out); {
			r, n := utf8.DecodeRuneInString(out[j:])
			if mustNeutralise(r, n) && !crlfAt(out, j) { // a CR that starts a CRLF is kept on purpose (A-R1)
				t.Fatalf("output %q for %q still holds %U", out, in, r)
			}
			j += n
		}
		if neutraliseText(out) != out {
			t.Fatalf("not a fixed point: %q -> %q -> %q", in, out, neutraliseText(out))
		}
		if len(out) > 4*len(in) {
			t.Fatalf("grew %d -> %d for %q", len(in), len(out), in)
		}
		if strings.Count(out, "\n") != strings.Count(in, "\n") || strings.Count(out, "\t") != strings.Count(in, "\t") {
			t.Fatalf("newline/tab count changed for %q -> %q", in, out)
		}
		if out == in && strings.ContainsAny(in, "\x00\x1b\x7f") {
			t.Fatalf("control left in place for %q", in)
		}
	}
}

// Worst-case 1 MiB inputs stay linear in allocations: one builder grown a few
// times, never per rune.
func TestAdvW17NeutraliseWorstCaseAllocatesBoundedly(t *testing.T) {
	for name, unit := range map[string]string{"all ESC": "\x1b", "all invalid": "\xff", "all bidi": "\u202e", "late first hit": "clean text "} {
		s := strings.Repeat(unit, (1<<20)/len(unit))
		if name == "late first hit" {
			s += "\x1b"
		}
		var out string
		allocs := testing.AllocsPerRun(3, func() { out = neutraliseText(s) })
		if allocs > 8 {
			t.Errorf("%s: %v allocations for 1 MiB", name, allocs)
		}
		if len(out) > 4*len(s)+16 {
			t.Errorf("%s: output %d bytes for %d in", name, len(out), len(s))
		}
	}
}

// ---- unneutralised paths (reported, not criterion) --------------------------

// GAP: only Event.Text is neutralised. Tool.Name/Input/Output/ParentID/Status,
// CancelledID/ResolvedID (opaque ids) and Request.Payload (a JSON document shown
// to the approver) carry agent bytes unchanged. serve/managed.go records Tool
// fields into the run output (the "[tools]" snapshot) and Payload into the
// decision row. This test only logs which fields keep a raw control or bidi rune.
func TestAdvW17NeutraliseGapNonTextFieldsCarryRawAgentBytes(t *testing.T) {
	raw := func(s string) bool { return neutraliseText(s) != s }

	cl := &Claude{sessionID: "S", started: true}
	ev, ok, err := cl.assistant(claudeFrame{Message: json.RawMessage(`{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"Bash\u001b[2J","input":{"command":"\u001b]0;x\u0007"}}]}`)})
	if err != nil || !ok || len(ev.Tools) != 1 {
		t.Fatalf("assistant frame: ok=%v err=%v tools=%d", ok, err, len(ev.Tools))
	}
	if raw(ev.Tools[0].Name) {
		t.Log("GAP: Claude Tool.Name keeps a raw ESC")
	}
	if raw(ev.Tools[0].Input) {
		t.Log("GAP: Claude Tool.Input keeps raw control bytes (JSON-escaped input is decoded by claudeRaw only for string inputs)")
	}
	ev, ok, err = cl.userResults(claudeFrame{Message: json.RawMessage(`{"content":[{"type":"tool_result","tool_use_id":"t1","content":"\u001b[31mboom\u202e"}]}`)})
	if err != nil || !ok {
		t.Fatalf("user frame: ok=%v err=%v", ok, err)
	}
	if raw(ev.Tools[0].Output) {
		t.Log("GAP: Claude Tool.Output keeps raw ESC and bidi bytes")
	}

	// A permission request's payload is the provider's JSON, passed through.
	cl.turnID = "T"
	cl.pending, cl.seen = map[string]*claudePending{}, map[string]bool{}
	ev, ok, err = cl.capture(claudeFrame{RequestID: "r1", Request: json.RawMessage(`{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls \u202e gnp.exe"}}`)})
	if err != nil || !ok || ev.Request == nil {
		t.Fatalf("capture: ok=%v err=%v", ok, err)
	}
	if strings.ContainsRune(ev.Request.Payload, 0x202e) {
		t.Log("GAP: Request.Payload carries a raw U+202E to the approver")
	}
}

// ---- withdrawn ids ----------------------------------------------------------

func advWire(t *testing.T, buf int) *connection {
	t.Helper()
	ctx, fail := context.WithCancelCause(t.Context())
	return &connection{ctx: ctx, fail: fail, cancel: func() { fail(nil) }, frames: make(chan wireRead, buf), validate: func([]byte) error { return nil }}
}

func advAgyIdle(t *testing.T) *Agy {
	t.Helper()
	return &Agy{sessionID: "S", turnID: "T", started: true, promptID: json.RawMessage("7"),
		options: Options{Mode: "ask"}, pending: map[string]*agyPending{}, seen: map[string]bool{},
		wire: advWire(t, 512)}
}

func advAgyPerm(id string) packet {
	return packet{Method: "session/request_permission", ID: json.RawMessage(id),
		Params: json.RawMessage(`{"sessionId":"S","toolCall":{"toolCallId":"tc","name":"write_file","rawInput":{"p":1}},"options":[{"optionId":"a","name":"A","kind":"allow_once"},{"optionId":"d","name":"D","kind":"reject_once"}]}`)}
}

var advAgyEnd = packet{ID: json.RawMessage("7"), Result: json.RawMessage(`{"stopReason":"end_turn"}`)}

func advAgyOffer(t *testing.T, s *Agy, id string) Request {
	t.Helper()
	e, ok, err := s.event(advAgyPerm(id))
	if err != nil || !ok || e.Request == nil {
		t.Fatalf("offer %s: ok=%v err=%v", id, ok, err)
	}
	return *e.Request
}

// A fresh callback with a new id, offered after another was withdrawn, is
// answerable; the withdrawn one stays ErrCallbackWithdrawn; an id replayed by the
// provider is refused at capture as a duplicate (the cumulative `seen`), so the
// withdrawn set can never mask a legitimate fresh request.
func TestAdvW17AgyFreshCallbackAfterWithdrawalIsNotRefusedAsWithdrawn(t *testing.T) {
	s := advAgyIdle(t)
	r1 := advAgyOffer(t, s, `1`)
	if e, ok, err := s.event(advAgyEnd); err != nil || !ok || e.CancelledID != r1.ID {
		t.Fatalf("turn end: %+v ok=%v err=%v", e, ok, err)
	}
	if err := s.ValidateResponse(r1, Response{Decision: "allow_once"}); !errors.Is(err, ErrCallbackWithdrawn) {
		t.Fatalf("answer to the withdrawn callback = %v", err)
	}
	// The next turn: the session is live again (ContinueTurn's own state reset).
	s.finished, s.deferred, s.promptID = false, nil, json.RawMessage("8")
	r2 := advAgyOffer(t, s, `2`)
	if err := s.ValidateResponse(r2, Response{Decision: "allow_once"}); err != nil {
		t.Fatalf("a fresh callback after a withdrawal was refused: %v", err)
	}
	if _, _, err := s.event(advAgyPerm(`1`)); err == nil {
		t.Fatal("provider replaying a withdrawn id was captured again")
	}
	if err := s.ValidateResponse(r1, Response{Decision: "allow_once"}); !errors.Is(err, ErrCallbackWithdrawn) {
		t.Fatalf("withdrawn id answered differently after a replay attempt: %v", err)
	}
}

// Ids are withdrawn only from pending, which is a subset of the ids `seen`
// admitted, and `seen` is bounded by the run budget (2048). The withdrawn set's
// bound must therefore be at least that budget, or the eviction order would turn
// an old withdrawn answer into a plain refusal.
func TestAdvW17WithdrawnBoundCoversEveryAdapterBudget(t *testing.T) {
	for name, budget := range map[string]int{"claude": claudeMaxRequests, "agy": 2048, "codex": 2048} {
		if maxWithdrawnIDs < budget {
			t.Errorf("BREAK: maxWithdrawnIDs=%d < %s request budget %d", maxWithdrawnIDs, name, budget)
		}
	}
	// Oldest-first eviction at the bound, and re-adding an evicted id works.
	var w withdrawnIDs
	for i := 0; i < maxWithdrawnIDs; i++ {
		w.add(fmt.Sprint("id", i))
	}
	w.add("id0") // already present: must not evict anything
	if !w.has("id0") || len(w.order) != maxWithdrawnIDs {
		t.Fatal("re-adding a present id changed the set")
	}
	w.add("new")
	if w.has("id0") || !w.has("id1") || !w.has("new") {
		t.Fatal("eviction is not oldest-first")
	}
	w.add("id0")
	if !w.has("id0") || w.has("id1") {
		t.Fatal("an evicted id is not re-admitted, or the wrong one was evicted")
	}
}

// withdrawalEvents on a duplicate id list (a caller bug today, a cheap check
// tomorrow) must not double-record.
func TestAdvW17WithdrawalEventsDeduplicatesTheSet(t *testing.T) {
	var w withdrawnIDs
	ev := w.withdrawalEvents([]string{"b", "a", "b"})
	if len(w.order) != 2 {
		t.Fatalf("order holds %d entries for 2 distinct ids", len(w.order))
	}
	if got := len(ev); got != 3 {
		t.Logf("note: %d CancelledID events for 3 ids (one per input id)", got)
	}
}

// An answer that races the turn end, run against the real adapter state under
// -race: every outcome is nil (answered before the end was read) or
// ErrCallbackWithdrawn, never a plain stale refusal; after the race the host
// still receives the withdrawal then the status exactly once.
func TestAdvW17ClaudeAnswerRacingTurnEndIsNeverStale(t *testing.T) {
	for iter := 0; iter < 30; iter++ {
		s := &Claude{sessionID: "S", started: true, turnID: "T", pending: map[string]*claudePending{}, seen: map[string]bool{},
			wire: advWire(t, 8)}
		e, ok, err := s.event([]byte(`{"type":"control_request","request_id":"r1","session_id":"S","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`))
		if err != nil || !ok || e.Request == nil {
			t.Fatalf("capture: %v", err)
		}
		req := *e.Request
		start := make(chan struct{})
		var wg sync.WaitGroup
		results := make([]error, 4)
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for n := 0; n < 50; n++ {
					results[i] = s.ValidateResponse(req, Response{Decision: "allow_once"})
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s.wire.frames <- wireRead{raw: []byte(`{"type":"result","subtype":"success","session_id":"S","result":"done"}`)}
		}()
		close(start)
		wg.Wait()
		for _, err := range results {
			if err != nil && !errors.Is(err, ErrCallbackWithdrawn) {
				t.Fatalf("BREAK: answer racing the turn end = %v, want nil or ErrCallbackWithdrawn", err)
			}
		}
		// Settle: whichever goroutine drained, the events come out in order.
		if err := s.ValidateResponse(req, Response{Decision: "allow_once"}); !errors.Is(err, ErrCallbackWithdrawn) {
			t.Fatalf("settled answer = %v", err)
		}
		var got []string
		for len(got) < 2 {
			ev, err := s.Next(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case ev.CancelledID != "":
				got = append(got, "cancel")
			case ev.Status != "":
				got = append(got, "status")
			}
		}
		if !slices.Equal(got, []string{"cancel", "status"}) {
			t.Fatalf("events = %v", got)
		}
	}
}

// An answer after the connection ended returns an error promptly and never hangs
// or panics, for both adapters.
func TestAdvW17AnswerAfterConnectionClosedErrors(t *testing.T) {
	ctx, fail := context.WithCancelCause(t.Context())
	s := advAgyIdle(t)
	r := advAgyOffer(t, s, `1`)
	s.wire = &connection{ctx: ctx, fail: fail, frames: make(chan wireRead)}
	close(s.wire.frames)
	if err := s.ValidateResponse(r, Response{Decision: "allow_once"}); err == nil {
		t.Fatal("validate on a closed connection returned nil")
	}
	// The pending callback was never withdrawn by a status; Next reports it before the error.
	e, err := s.Next(t.Context())
	t.Logf("Next after close: %+v err=%v", e, err)
}

// ---- drainBacklog budget ----------------------------------------------------

// A peer that fills the preflight with events and then ends the turn with the
// pending cap's worth of callbacks open. The turn end queues its follow-ups
// inside event, before the budget check, so the backlog can pass the documented
// 128 by up to the pending cap (32) plus the returned event. It cannot grow past
// that: the turn end happens once, and a continuation needs an empty backlog.
// HOLDS as bounded; the excess over maxDeferredEvents is logged as a finding.
func TestAdvW17AgyDrainBacklogBudgetStaysBounded(t *testing.T) {
	hard := maxDeferredEvents + 32 + 1
	worst := 0
	for chunks := 0; chunks <= 200; chunks++ {
		s := advAgyIdle(t)
		for i := 0; i < 32; i++ {
			advAgyOffer(t, s, fmt.Sprint(100+i))
		}
		s.deferred = nil
		for i := 0; i < chunks; i++ {
			s.wire.frames <- wireRead{value: packet{Method: "session/update", Params: json.RawMessage(`{"sessionId":"S","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"}}}`)}}
		}
		s.wire.frames <- wireRead{value: advAgyEnd}
		_ = s.drain()
		worst = max(worst, len(s.deferred))
		if len(s.deferred) > hard {
			t.Fatalf("BREAK: chunks=%d deferred %d exceeds %d", chunks, len(s.deferred), hard)
		}
		// A second drain on the finished session adds nothing more from a replayed end.
		s.wire.frames <- wireRead{value: advAgyEnd}
		before := len(s.deferred)
		if err := s.drain(); err == nil && len(s.deferred) != before {
			t.Fatalf("BREAK: a replayed turn end grew the backlog %d -> %d", before, len(s.deferred))
		}
	}
	if worst > maxDeferredEvents {
		t.Logf("FINDING: backlog reached %d, past the documented bound %d (maxDeferredEvents comment says it bounds the backlog left deferred)", worst, maxDeferredEvents)
	}
}

// ---- cap vs cancel ----------------------------------------------------------

// The caller's own context cancelled while a cap error frame is queued: three
// select cases are ready. The result must not depend on which one select picks.
func TestAdvW17ReceiveFrameCapAndCallerCancelBothReadyIsDeterministic(t *testing.T) {
	cause := errors.New("managed provider output exceeded 32 MiB")
	seen := map[string]int{}
	for i := 0; i < 300; i++ {
		ctx, fail := context.WithCancelCause(t.Context())
		c := &connection{ctx: ctx, fail: fail, frames: make(chan wireRead, 2)}
		c.frames <- wireRead{err: cause}
		fail(cause)
		rctx, stop := context.WithCancel(t.Context())
		stop() // the caller gave up at the same moment
		_, _, err := c.receiveFrame(rctx)
		switch {
		case errors.Is(err, cause):
			seen["cap"]++
		case errors.Is(err, context.Canceled):
			seen["canceled"]++
		default:
			seen[fmt.Sprint(err)]++
		}
	}
	if len(seen) != 1 {
		t.Fatalf("BREAK (PARTIAL: nondeterministic when the caller's ctx is cancelled too): outcomes over 300 runs = %v", seen)
	}
}

// The stderr cap cancels the connection too. It is a cap breach as much as the
// stdout one, but it cancels without recording a cause, so every reader reports a
// bare context.Canceled, which serve classes as an interruption.
func TestAdvW17StderrCapRecordsItsCause(t *testing.T) {
	ctx, fail := context.WithCancelCause(t.Context())
	c := &connection{ctx: ctx, fail: fail, cancel: func() { fail(nil) }}
	w := &limitedDiscard{remaining: 4, cancel: c.cancel}
	if _, err := w.Write(make([]byte, 10)); err == nil {
		t.Fatal("over-cap stderr write succeeded")
	}
	<-c.ctx.Done()
	if err := c.err(); err == nil || !strings.Contains(err.Error(), "stderr") {
		t.Fatalf("BREAK (PARTIAL: stderr cap): connection ended with %v, want the stderr cap error", err)
	}
}

// Cancel first, cap second: the first cause wins and stays; a later frame error
// never rewrites the reason a Close recorded.
func TestAdvW17FirstCauseWins(t *testing.T) {
	ctx, fail := context.WithCancelCause(t.Context())
	c := &connection{ctx: ctx, fail: fail, cancel: func() { fail(nil) }}
	c.cancel()
	c.fail(errors.New("late cap"))
	if err := c.err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want Canceled", err)
	}
}
