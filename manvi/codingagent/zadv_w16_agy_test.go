package codingagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Adversarial regression tests for lane w16-agy, criteria C and D (the agy ACP
// adapter and the cap changes). The stand-in child below is scriptable per
// scenario; it speaks just enough of ACP v1 to get through the handshake and
// then misbehaves in the prompt phase.

const zadvSession = "zadv-sess"

func TestZadvW16AgyChild(t *testing.T) {
	scenario := ""
	for _, a := range os.Args {
		if s, ok := strings.CutPrefix(a, "zadv-agy:"); ok {
			scenario = s
		}
	}
	if scenario == "" {
		return
	}
	out := bufio.NewWriterSize(os.Stdout, 1<<20)
	emit := func(format string, a ...any) {
		fmt.Fprintf(out, format+"\n", a...) //nolint:errcheck // a fixture child writing to its own stdout
		_ = out.Flush()
	}
	update := func(body string) {
		emit(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":%q,"update":%s}}`, zadvSession, body)
	}
	chunk := func(text string) {
		b, _ := json.Marshal(text)
		update(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":` + string(b) + `}}`)
	}
	record := func(line []byte) {
		if path := os.Getenv("ZADV_FRAMES"); path != "" {
			if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil { //nolint:gosec // a fixture child appending to a test-supplied path
				_, _ = f.Write(append(line, '\n'))
				_ = f.Close()
			}
		}
	}
	lines := make(chan []byte, 256)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 4096), MaxFrameBytes+1)
		for sc.Scan() {
			lines <- append([]byte(nil), sc.Bytes()...)
		}
		close(lines)
	}()
	waitFrame := func(d time.Duration) []byte {
		select {
		case l, ok := <-lines:
			if !ok {
				return nil
			}
			record(l)
			return l
		case <-time.After(d):
			return nil
		}
	}
	perm := func(id, sess, toolJSON string) {
		emit(`{"jsonrpc":"2.0","id":%s,"method":"session/request_permission","params":{"sessionId":%q,"toolCall":%s,"options":[{"optionId":"allow","name":"Allow","kind":"allow_once"},{"optionId":"deny","name":"Deny","kind":"reject_once"}]}}`, id, sess, toolJSON)
	}
	writeTool := `{"toolCallId":"tc-w","name":"write_file","rawInput":{"path":"x"}}`

	for line := range lines {
		var in struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(line, &in)
		switch in.Method {
		case "initialize":
			switch scenario {
			case "silent-init":
				continue
			case "init-wrong-id":
				emit(`{"jsonrpc":"2.0","id":9999,"result":{"protocolVersion":1,"agentInfo":{"name":"Antigravity ACP Server","version":"1.5.3"}}}`)
				continue
			case "init-notif-flood":
				for i := 0; i < 200; i++ {
					update(`{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"c"}]}`)
				}
			case "init-foreign-notif":
				emit(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"someone-else","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"EARLY"}}}}`)
			}
			reply := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentInfo":{"name":"Antigravity ACP Server","version":"1.5.3"}}}`, in.ID)
			emit("%s", reply)
			if scenario == "init-dup-response" {
				emit("%s", reply)
			}
		case "session/new":
			if scenario == "early-chunk" {
				chunk("EARLY-TEXT")
			}
			emit(`{"jsonrpc":"2.0","id":%s,"result":{"sessionId":%q,"models":{"currentModelId":"m"},"modes":{"currentModeId":"default","availableModes":[{"id":"default"},{"id":"accept-edits"},{"id":"plan"}]}}}`, in.ID, zadvSession)
		case "session/set_mode":
			emit(`{"jsonrpc":"2.0","id":%s,"result":{}}`, in.ID)
		case "session/prompt":
			record([]byte("PROMPT"))
			switch scenario {
			case "exit-mid-turn":
				os.Exit(7)
			case "exit-mid-perm":
				perm(`"p1"`, zadvSession, writeTool)
				time.Sleep(200 * time.Millisecond)
				os.Exit(7)
			case "perm-autoreply-probe":
				perm(`"p1"`, zadvSession, writeTool)
				got := waitFrame(600 * time.Millisecond)
				if got == nil {
					chunk("autoreply:none")
				} else {
					var r struct {
						Result struct {
							Outcome struct {
								Outcome  string `json:"outcome"`
								OptionID string `json:"optionId"`
							} `json:"outcome"`
						} `json:"result"`
					}
					_ = json.Unmarshal(got, &r)
					chunk("autoreply:" + r.Result.Outcome.Outcome + "/" + r.Result.Outcome.OptionID)
				}
				emit(`{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}`, in.ID)
			case "perm-other-session":
				perm(`"p1"`, "not-our-session", writeTool)
				waitFrame(500 * time.Millisecond)
			case "perm-nameless":
				perm(`"p1"`, zadvSession, `{"toolCallId":"tc-w"}`)
				waitFrame(500 * time.Millisecond)
			case "perm-no-allow-option":
				emit(`{"jsonrpc":"2.0","id":"p1","method":"session/request_permission","params":{"sessionId":%q,"toolCall":%s,"options":[{"optionId":"always","kind":"allow_always"}]}}`, zadvSession, writeTool)
				waitFrame(500 * time.Millisecond)
			case "callback-fs-write":
				emit(`{"jsonrpc":"2.0","id":"f1","method":"fs/write_text_file","params":{"sessionId":%q,"path":"/etc/passwd","content":"x"}}`, zadvSession)
				waitFrame(500 * time.Millisecond)
			case "perm-flood":
				for i := 0; i < 40; i++ {
					perm(fmt.Sprintf(`"p%d"`, i), zadvSession, fmt.Sprintf(`{"toolCallId":"tc-%d","name":"write_file"}`, i))
				}
				waitFrame(2 * time.Second)
			case "perm-then-cancelled":
				perm(`"p1"`, zadvSession, writeTool)
				time.Sleep(100 * time.Millisecond)
				emit(`{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"cancelled"}}`, in.ID)
				waitFrame(500 * time.Millisecond)
			case "perm-then-end":
				perm(`"p1"`, zadvSession, writeTool)
				time.Sleep(100 * time.Millisecond)
				emit(`{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}`, in.ID)
				waitFrame(500 * time.Millisecond)
			case "perm-end-buffered":
				perm(`"p1"`, zadvSession, writeTool)
				perm(`"p2"`, zadvSession, writeTool)
				emit(`{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}`, in.ID)
				waitFrame(500 * time.Millisecond)
			case "dup-end":
				chunk("hello")
				emit(`{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}`, in.ID)
				emit(`{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}`, in.ID)
				waitFrame(500 * time.Millisecond)
			case "unknown-resp-id":
				emit(`{"jsonrpc":"2.0","id":"ghost","result":{"stopReason":"end_turn"}}`)
				waitFrame(500 * time.Millisecond)
			case "late-update":
				emit(`{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}`, in.ID)
				chunk("LATE-TEXT")
				waitFrame(500 * time.Millisecond)
			case "flood-updates":
				body := `{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"c","description":"` + strings.Repeat("d", 4000) + `"}]}`
				for i := 0; i < 10000; i++ {
					update(body)
				}
				chunk("end")
				emit(`{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}`, in.ID)
				waitFrame(500 * time.Millisecond)
			case "exact-max-frame":
				prefix := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"` + zadvSession + `","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"`
				suffix := `"}}}}`
				pad := MaxFrameBytes - len(prefix) - len(suffix)
				fmt.Fprintf(out, "%s%s%s\n", prefix, strings.Repeat("a", pad), suffix) //nolint:errcheck // a fixture child writing to its own stdout
				_ = out.Flush()
				emit(`{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}`, in.ID)
				waitFrame(500 * time.Millisecond)
			case "big-frames":
				prefix := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"` + zadvSession + `","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"`
				suffix := `"}}}}`
				pad := MaxFrameBytes - len(prefix) - len(suffix)
				for i := 0; i < 45; i++ {
					fmt.Fprintf(out, "%s%s%s\n", prefix, strings.Repeat("b", pad), suffix) //nolint:errcheck // a fixture child writing to its own stdout
					_ = out.Flush()
				}
				waitFrame(2 * time.Second)
			case "ansi-text":
				chunk("ok \x1b]0;pwned\x07\x1b[2J\u202eevil\u2066")
				emit(`{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}`, in.ID)
				waitFrame(500 * time.Millisecond)
			case "silent":
				waitFrame(30 * time.Second)
			case "multiturn":
				chunk("turn-text")
				emit(`{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}`, in.ID)
			case "failed-turn":
				emit(`{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"refusal"}}`, in.ID)
				waitFrame(500 * time.Millisecond)
			default:
				if raw, ok := strings.CutPrefix(scenario, "raw:"); ok {
					fmt.Fprintln(out, strings.ReplaceAll(raw, "SESS", zadvSession)) //nolint:errcheck // a fixture child writing to its own stdout
					_ = out.Flush()
					waitFrame(500 * time.Millisecond)
				}
			}
		default:
			// A client response arriving outside waitFrame.
			if in.Method == "" {
				record(line)
			}
		}
	}
	os.Exit(0)
}

func zadvArgs(scenario string) func(mode string) []string {
	return func(mode string) []string {
		return append([]string{"-test.run=^TestZadvW16AgyChild$", "--", "zadv-agy:" + scenario}, agyArgs(mode)...)
	}
}

func zadvOpen(t *testing.T, scenario, mode string) (*Agy, error) {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s, err := openAgy(t.Context(), Options{Program: bin, Cwd: t.TempDir(), Mode: mode, AcknowledgeBypass: mode == "bypass"}, zadvArgs(scenario))
	if err == nil {
		t.Cleanup(func() { _, _ = s.Close() })
	}
	return s, err
}

func zadvMust(t *testing.T, scenario, mode string) *Agy {
	t.Helper()
	s, err := zadvOpen(t, scenario, mode)
	if err != nil {
		t.Fatalf("open %s/%s: %v", scenario, mode, err)
	}
	return s
}

// zadvDrain reads events until a status, an error, or the deadline.
func zadvDrain(t *testing.T, s *Agy, d time.Duration) (events []Event, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	defer cancel()
	for {
		e, err := s.Next(ctx)
		if err != nil {
			return events, err
		}
		events = append(events, e)
		if e.Status != "" {
			return events, nil
		}
	}
}

func zadvFrames(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/frames"
	t.Setenv("ZADV_FRAMES", path)
	return path
}

func zadvRead(path string) string {
	b, _ := os.ReadFile(path) //nolint:gosec // test temp file
	return string(b)
}

func zadvStart(t *testing.T, s *Agy) {
	t.Helper()
	if err := s.StartTurn(t.Context(), "do the thing"); err != nil {
		t.Fatal(err)
	}
}

// --- handshake -------------------------------------------------------------

func TestZadvW16HandshakeFaultsFailClosedAndFast(t *testing.T) {
	prev := agyHandshake
	agyHandshake = 700 * time.Millisecond
	t.Cleanup(func() { agyHandshake = prev })
	for _, scn := range []string{"silent-init", "init-wrong-id", "init-dup-response", "init-foreign-notif", "init-notif-flood"} {
		t.Run(scn, func(t *testing.T) {
			done := make(chan error, 1)
			go func() { _, err := zadvOpen(t, scn, "ask"); done <- err }()
			select {
			case err := <-done:
				if err == nil && scn != "init-notif-flood" {
					t.Fatalf("%s: handshake succeeded", scn)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("%s: handshake hung past its deadline", scn)
			}
		})
	}
}

// A notification that arrives before the session exists, and is valid for the
// session once it does, is not a turn event: it never becomes agent text.
func TestZadvW16EarlyChunkBeforeTurnIsNotDelivered(t *testing.T) {
	s := zadvMust(t, "early-chunk", "ask")
	if err := s.StartTurn(t.Context(), "x"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 600*time.Millisecond)
	defer cancel()
	for {
		e, err := s.Next(ctx)
		if err != nil {
			return
		}
		if strings.Contains(e.Text, "EARLY") {
			t.Fatalf("a pre-turn chunk was delivered as turn text: %q", e.Text)
		}
	}
}

// --- permission modes ------------------------------------------------------

// In every mode except preapproved a permission request must reach the host and
// nothing may be written back to the child until the host answers. Inspect and
// bypass in particular are never auto-allowed.
func TestZadvW16PermissionIsNeverAutoAnsweredOutsidePreapproved(t *testing.T) {
	for _, mode := range []string{"inspect", "ask", "edit", "auto_review", "bypass"} {
		t.Run(mode, func(t *testing.T) {
			s := zadvMust(t, "perm-autoreply-probe", mode)
			zadvStart(t, s)
			events, err := zadvDrain(t, s, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			var gotRequest bool
			var text string
			for _, e := range events {
				gotRequest = gotRequest || e.Request != nil
				text += e.Text
			}
			if !gotRequest {
				t.Fatalf("mode %s: the permission request never reached the host (events %+v)", mode, events)
			}
			if !strings.Contains(text, "autoreply:none") {
				t.Fatalf("mode %s: adapter answered a permission without the host: %q", mode, text)
			}
		})
	}
}

// preapproved never allows: the only reply is a reject or a cancel.
func TestZadvW16PreapprovedNeverAllowsAWrite(t *testing.T) {
	s := zadvMust(t, "perm-autoreply-probe", "preapproved")
	zadvStart(t, s)
	events, err := zadvDrain(t, s, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for _, e := range events {
		if e.Request != nil {
			t.Fatalf("preapproved surfaced a request to the host: %+v", e.Request)
		}
		text += e.Text
	}
	if strings.Contains(text, "allow") || !strings.Contains(text, "autoreply:selected/deny") {
		t.Fatalf("preapproved reply = %q, want a selected reject", text)
	}
}

// Respond with allow_once in inspect mode is the host's decision, but a deny
// must produce the reject option and an allow the allow option, once.
func TestZadvW16InspectDecisionIsSingleUse(t *testing.T) {
	s := zadvMust(t, "perm-autoreply-probe", "inspect")
	zadvStart(t, s)
	var req *Request
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for req == nil {
		e, err := s.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		req = e.Request
	}
	if err := s.Respond(ctx, *req, Response{Decision: "deny"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Respond(ctx, *req, Response{Decision: "allow_once"}); err == nil {
		t.Fatal("a second answer to a consumed callback was accepted")
	}
}

// A callback the adapter cannot construct a safe answer for is refused and
// nothing is written to the child.
func TestZadvW16UnsafeCallbacksAreRefusedWithoutAnyReply(t *testing.T) {
	for _, scn := range []string{"perm-other-session", "perm-nameless", "perm-no-allow-option", "callback-fs-write"} {
		t.Run(scn, func(t *testing.T) {
			frames := zadvFrames(t)
			s := zadvMust(t, scn, "bypass")
			zadvStart(t, s)
			events, err := zadvDrain(t, s, 5*time.Second)
			for _, e := range events {
				if e.Request != nil {
					t.Fatalf("an unsafe callback reached the host as an approvable request: %+v", e.Request)
				}
			}
			if err == nil {
				t.Fatal("expected the unsafe callback to fail the turn")
			}
			time.Sleep(300 * time.Millisecond)
			if got := strings.TrimSpace(strings.ReplaceAll(zadvRead(frames), "PROMPT", "")); got != "" {
				t.Fatalf("adapter wrote %q to the child after an unsafe callback", got)
			}
		})
	}
}

// Pending callbacks are bounded (32); a flood fails the run instead of growing.
func TestZadvW16PermissionFloodIsBounded(t *testing.T) {
	s := zadvMust(t, "perm-flood", "ask")
	zadvStart(t, s)
	events, err := zadvDrain(t, s, 10*time.Second)
	requests := 0
	for _, e := range events {
		if e.Request != nil {
			requests++
		}
	}
	if err == nil || requests > 32 {
		t.Fatalf("%d requests surfaced, err=%v; want at most 32 and a failure", requests, err)
	}
}

// --- turn lifecycle and ordering ------------------------------------------

func TestZadvW16ChildExitMidTurnEndsNextAndDone(t *testing.T) {
	s := zadvMust(t, "exit-mid-turn", "ask")
	zadvStart(t, s)
	_, err := zadvDrain(t, s, 5*time.Second)
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Next after the child died = %v, want a prompt error (not a hang)", err)
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done never closed after the child exited")
	}
}

func TestZadvW16RespondAfterChildDiedDoesNotHang(t *testing.T) {
	s := zadvMust(t, "exit-mid-perm", "ask")
	zadvStart(t, s)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var req *Request
	for req == nil {
		e, err := s.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		req = e.Request
	}
	time.Sleep(500 * time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- s.Respond(ctx, *req, Response{Decision: "allow_once"}) }()
	select {
	case <-done: //nolint:errcheck // only completion is observed; the Respond error is not what the test asserts
	case <-time.After(8 * time.Second):
		t.Fatal("Respond hung after the child exited")
	}
}

// Exactly one end of turn, even if the child sends two; the duplicate is a
// failure, never a second terminal event.
func TestZadvW16DuplicateTurnEndYieldsExactlyOneStatus(t *testing.T) {
	s := zadvMust(t, "dup-end", "ask")
	zadvStart(t, s)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	statuses, starts := 0, 0
	for {
		e, err := s.Next(ctx)
		if err != nil {
			break
		}
		if e.Status != "" {
			statuses++
		}
		if e.TurnID != "" {
			starts++
		}
	}
	if statuses != 1 || starts != 1 {
		t.Fatalf("statuses=%d turn starts=%d, want exactly 1 each", statuses, starts)
	}
}

func TestZadvW16ResponseToUnknownIDMidTurnIsRefused(t *testing.T) {
	s := zadvMust(t, "unknown-resp-id", "ask")
	zadvStart(t, s)
	events, err := zadvDrain(t, s, 5*time.Second)
	for _, e := range events {
		if e.Status != "" {
			t.Fatalf("a reply to an id the host never sent ended the turn with %q", e.Status)
		}
	}
	if err == nil {
		t.Fatal("expected an error for a reply to an unknown id")
	}
}

// A chunk after the turn's end is a late tail: it is not delivered, and does
// not become the next turn's text.
func TestZadvW16LateUpdateAfterEndIsDropped(t *testing.T) {
	s := zadvMust(t, "late-update", "ask")
	zadvStart(t, s)
	if _, err := zadvDrain(t, s, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
	defer cancel()
	for {
		e, err := s.Next(ctx)
		if err != nil {
			return
		}
		if strings.Contains(e.Text, "LATE") {
			t.Fatalf("late text delivered after the turn ended: %q", e.Text)
		}
	}
}

// When the provider ends a turn while a permission is still unanswered, the host
// must be told the callback is gone (Claude does this with CancelledID).
func TestZadvW16CancelledTurnWithPendingPermissionTellsTheHost(t *testing.T) {
	s := zadvMust(t, "perm-then-cancelled", "ask")
	zadvStart(t, s)
	events, err := zadvDrain(t, s, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var reqID string
	var told bool
	for _, e := range events {
		if e.Request != nil {
			reqID = e.Request.ID
		}
		if reqID != "" && (e.CancelledID == reqID || e.ResolvedID == reqID) {
			told = true
		}
	}
	if reqID == "" || !told {
		t.Fatalf("turn ended with a pending permission %q and the host was never told it was withdrawn: %+v", reqID, events)
	}
}

// A permission left unanswered when the turn completes must not wedge the
// session's next turn behind a callback that can no longer be answered.
func TestZadvW16UnansweredPermissionDoesNotBlockContinuationAfterCompletion(t *testing.T) {
	s := zadvMust(t, "perm-then-end", "ask")
	zadvStart(t, s)
	events, err := zadvDrain(t, s, 5*time.Second)
	if err != nil || events[len(events)-1].Status != "completed" {
		t.Fatalf("setup: %v %+v", err, events)
	}
	err = s.ContinueTurn(t.Context(), "next")
	if errors.Is(err, ErrTurnInFlight) {
		t.Fatalf("ContinueTurn after a completed turn is refused as in-flight forever: a stale unanswered callback is retained (%v)", err)
	}
}

func TestZadvW16ContinuationGuards(t *testing.T) {
	t.Run("after a failed turn", func(t *testing.T) {
		s := zadvMust(t, "failed-turn", "ask")
		zadvStart(t, s)
		if _, err := zadvDrain(t, s, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		if err := s.ContinueTurn(t.Context(), "again"); !errors.Is(err, ErrNoCompletedTurn) || !ContinueRefused(err) {
			t.Fatalf("ContinueTurn after a failed turn = %v", err)
		}
	})
	t.Run("while in flight", func(t *testing.T) {
		s := zadvMust(t, "silent", "ask")
		zadvStart(t, s)
		if err := s.ContinueTurn(t.Context(), "again"); !ContinueRefused(err) {
			t.Fatalf("ContinueTurn while a turn runs = %v", err)
		}
	})
	t.Run("after close", func(t *testing.T) {
		s := zadvMust(t, "multiturn", "ask")
		zadvStart(t, s)
		if _, err := zadvDrain(t, s, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Close(); err != nil {
			t.Logf("close: %v", err)
		}
		if err := s.ContinueTurn(t.Context(), "again"); !errors.Is(err, ErrSessionClosed) {
			t.Fatalf("ContinueTurn after Close = %v, want ErrSessionClosed", err)
		}
	})
	t.Run("start twice", func(t *testing.T) {
		s := zadvMust(t, "silent", "ask")
		zadvStart(t, s)
		if err := s.StartTurn(t.Context(), "second"); err == nil {
			t.Fatal("a second StartTurn was accepted")
		}
	})
	t.Run("bounds", func(t *testing.T) {
		s := zadvMust(t, "multiturn", "ask")
		zadvStart(t, s)
		if _, err := zadvDrain(t, s, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"", "  ", strings.Repeat("x", MaxContinueMessageBytes+1), "a\x00b", "\xff\xfe"} {
			if err := s.ContinueTurn(t.Context(), bad); !errors.Is(err, ErrContinueMessage) {
				t.Fatalf("ContinueTurn(%q...) = %v, want ErrContinueMessage", bad[:min(len(bad), 8)], err)
			}
		}
	})
}

// Turn start and end each exactly once per turn across a continuation.
func TestZadvW16TwoTurnsEmitStartAndEndExactlyOncePerTurn(t *testing.T) {
	s := zadvMust(t, "multiturn", "ask")
	zadvStart(t, s)
	count := func(events []Event) (starts, ends int) {
		for _, e := range events {
			if e.TurnID != "" {
				starts++
			}
			if e.Status != "" {
				ends++
			}
		}
		return
	}
	ev1, err := zadvDrain(t, s, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if st, en := count(ev1); st != 1 || en != 1 {
		t.Fatalf("turn 1: starts=%d ends=%d", st, en)
	}
	if err := s.ContinueTurn(t.Context(), "second"); err != nil {
		t.Fatal(err)
	}
	ev2, err := zadvDrain(t, s, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if st, en := count(ev2); st != 1 || en != 1 {
		t.Fatalf("turn 2: starts=%d ends=%d", st, en)
	}
	if ev1[0].TurnID == ev2[0].TurnID {
		t.Fatalf("both turns carry id %q", ev1[0].TurnID)
	}
}

// A cancelled Next context releases the session: the same session can be read
// again, and Close unblocks a reader that is waiting.
func TestZadvW16CancelledNextDoesNotWedgeAndCloseUnblocksAReader(t *testing.T) {
	s := zadvMust(t, "silent", "ask")
	zadvStart(t, s)
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
		_, err := s.Next(ctx)
		cancel()
		if i == 0 && err != nil {
			t.Fatalf("first Next (turn start) = %v", err)
		}
		if i > 0 && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Next #%d = %v, want a deadline", i, err)
		}
	}
	res := make(chan error, 1)
	go func() { _, err := s.Next(context.Background()); res <- err }()
	time.Sleep(150 * time.Millisecond)
	closed := make(chan struct{})
	go func() { _, _ = s.Close(); close(closed) }()
	select {
	case err := <-res:
		if err == nil {
			t.Fatal("Next returned an event after Close")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not unblock a waiting Next")
	}
	<-closed
}

// --- malformed frames ------------------------------------------------------

func TestZadvW16MalformedMidTurnFramesFailTheTurnNeverHangOrSucceed(t *testing.T) {
	frames := map[string]string{
		"not json":              `not json at all`,
		"array":                 `[1,2,3]`,
		"trailing garbage":      `{"jsonrpc":"2.0","method":"session/update","params":{}} {}`,
		"jsonrpc 1.0":           `{"jsonrpc":"1.0","method":"session/update","params":{}}`,
		"no jsonrpc":            `{"method":"session/update","params":{}}`,
		"method not string":     `{"jsonrpc":"2.0","method":5}`,
		"null id response":      `{"jsonrpc":"2.0","id":null,"result":{}}`,
		"object id response":    `{"jsonrpc":"2.0","id":{},"result":{}}`,
		"result and error":      `{"jsonrpc":"2.0","id":1,"result":{},"error":{}}`,
		"neither":               `{"jsonrpc":"2.0","id":1}`,
		"update params array":   `{"jsonrpc":"2.0","method":"session/update","params":[]}`,
		"update no params":      `{"jsonrpc":"2.0","method":"session/update"}`,
		"update no session":     `{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk"}}}`,
		"update with id":        `{"jsonrpc":"2.0","id":5,"method":"session/update","params":{"sessionId":"SESS","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"}}}}`,
		"chunk content string":  `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"SESS","update":{"sessionUpdate":"agent_message_chunk","content":"x"}}}`,
		"duplicate key":         `{"jsonrpc":"2.0","method":"session/update","method":"session/update","params":{}}`,
		"bad stop reason":       `{"jsonrpc":"2.0","id":2,"result":{"stopReason":"banana"}}`,
		"invalid utf8":          "{\"jsonrpc\":\"2.0\",\"method\":\"x\",\"params\":{\"a\":\"\xff\"}}",
		"nested depth bomb":     `{"jsonrpc":"2.0","method":"x","params":` + strings.Repeat("[", 50000) + strings.Repeat("]", 50000) + `}`,
		"permission float id":   `{"jsonrpc":"2.0","id":1.5,"method":"session/request_permission","params":{"sessionId":"SESS","toolCall":{"toolCallId":"a","name":"w"},"options":[{"optionId":"o","kind":"allow_once"}]}}`,
		"permission huge id":    `{"jsonrpc":"2.0","id":123456789012345678901234567890,"method":"session/request_permission","params":{"sessionId":"SESS","toolCall":{"toolCallId":"a","name":"w"},"options":[{"optionId":"o","kind":"allow_once"}]}}`,
		"permission object id":  `{"jsonrpc":"2.0","id":{"a":1},"method":"session/request_permission","params":{"sessionId":"SESS","toolCall":{"toolCallId":"a","name":"w"},"options":[{"optionId":"o","kind":"allow_once"}]}}`,
		"permission empty opts": `{"jsonrpc":"2.0","id":7,"method":"session/request_permission","params":{"sessionId":"SESS","toolCall":{"toolCallId":"a","name":"w"},"options":[]}}`,
	}
	for name, frame := range frames {
		t.Run(name, func(t *testing.T) {
			s := zadvMust(t, "raw:"+frame, "ask")
			zadvStart(t, s)
			events, err := zadvDrain(t, s, 5*time.Second)
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("hung on a malformed frame")
			}
			for _, e := range events {
				if e.Status == "completed" || e.Request != nil || e.Text != "" {
					t.Fatalf("a malformed frame produced a meaningful event: %+v", e)
				}
			}
			if err == nil && name != "chunk content string" {
				t.Fatalf("a malformed frame was accepted without error: events %+v", events)
			}
		})
	}
}

// --- caps (criterion D) ----------------------------------------------------

// A frame of exactly MaxFrameBytes is accepted end to end; one byte more is not.
func TestZadvW16FrameBoundaryIsExactlyMaxFrameBytes(t *testing.T) {
	s := zadvMust(t, "exact-max-frame", "ask")
	zadvStart(t, s)
	events, err := zadvDrain(t, s, 10*time.Second)
	if err != nil {
		t.Fatalf("a frame of exactly MaxFrameBytes was refused: %v", err)
	}
	var total int
	for _, e := range events {
		total += len(e.Text)
	}
	if total < MaxFrameBytes-1024 {
		t.Fatalf("delivered %d bytes of text from a %d-byte frame", total, MaxFrameBytes)
	}
}

// A flood of small notifications is bounded by the 32 MiB per-session cap, and
// the adapter's own buffers do not grow with it.
func TestZadvW16NotificationFloodHitsTheSessionCapWithBoundedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("streams ~40 MiB")
	}
	s := zadvMust(t, "flood-updates", "ask")
	zadvStart(t, s)
	var peak uint64
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > peak {
					peak = m.HeapAlloc
				}
			}
		}
	}()
	_, err := zadvDrain(t, s, 60*time.Second)
	close(stop)
	<-sampled
	// The memory bound is the deterministic assertion and is checked first.
	if peak > 256<<20 {
		t.Fatalf("heap peaked at %d MiB during a notification flood", peak>>20)
	}
	if err == nil {
		t.Fatal("a notification flood past the session cap ended without an error")
	}
	// The cap error is recorded before the connection is cancelled and readers
	// prefer it, so it is what every reader sees, never context.Canceled.
	if !strings.Contains(err.Error(), "32 MiB") || errors.Is(err, context.Canceled) {
		t.Fatalf("flood ended with %v instead of the 32 MiB cap", err)
	}
}

// A consumer that stops reading must not let a provider pin unbounded memory:
// frames queue behind a 64-slot channel and every frame counts against the 32 MiB
// session cap before it is queued, so the heap stays near the cap, not at
// (channel + claudeMaxQueue) x MaxFrameBytes = 576 MiB.
func TestZadvW16StalledReaderWithMaxSizeFramesKeepsMemoryBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("streams ~40 MiB")
	}
	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	s := zadvMust(t, "big-frames", "ask")
	zadvStart(t, s)
	time.Sleep(3 * time.Second) // the host never calls Next
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if grew := int64(after.HeapAlloc) - int64(before.HeapAlloc); grew > 160<<20 { //nolint:gosec // heap sizes far below int64 range
		t.Fatalf("heap grew by %d MiB with a stalled reader and 1 MiB frames; the per-session bound is not holding", grew>>20)
	}
	_, err := zadvDrain(t, s, 30*time.Second)
	if err == nil {
		t.Fatal("draining a session past its 32 MiB cap ended without an error")
	}
}

// --- shared-gap observation ------------------------------------------------

// Agent text reaches the host verbatim. This is the same for Claude and Codex (no
// adapter sanitises Event.Text), so it is a shared gap, not an agy divergence.
func TestZadvW16AgentTextCarriesNoTerminalEscapesOrBidiControls(t *testing.T) {
	t.Run("agy", func(t *testing.T) {
		s := zadvMust(t, "ansi-text", "ask")
		zadvStart(t, s)
		events, err := zadvDrain(t, s, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		requireNeutralised(t, events)
	})
	t.Run("claude", func(t *testing.T) {
		s := openFixture(t, "ansi-text", "ask")
		if err := s.StartTurn(t.Context(), "go"); err != nil {
			t.Fatal(err)
		}
		requireNeutralised(t, drainUntilStatus(t, s.Next))
	})
	t.Run("codex", func(t *testing.T) {
		s := fixture(t, "ansi-text")
		if err := s.StartTurn(t.Context(), "go"); err != nil {
			t.Fatal(err)
		}
		requireNeutralised(t, drainUntilStatus(t, s.Next))
	})
}

// drainUntilStatus reads events from next until a status, failing on an error.
func drainUntilStatus(t *testing.T, next func(context.Context) (Event, error)) []Event {
	t.Helper()
	var events []Event
	for {
		ctx, stop := context.WithTimeout(t.Context(), 10*time.Second)
		e, err := next(ctx)
		stop()
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
		if e.Status != "" {
			return events
		}
	}
}

// requireNeutralised checks that no raw control byte or bidi rune is left in any
// event's text, and that the controls are shown, not dropped.
func requireNeutralised(t *testing.T, events []Event) {
	t.Helper()
	var all strings.Builder
	for _, e := range events {
		for _, r := range e.Text {
			if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f || (r >= 0x80 && r <= 0x9f) ||
				r == 0x061c || r == 0x200e || r == 0x200f ||
				(r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
				t.Fatalf("agent text reaches the host with control rune %U: %q", r, e.Text)
			}
		}
		all.WriteString(e.Text)
	}
	for _, want := range []string{`\x1b]0;pwned\x07\x1b[2J`, "<U+202E>evil<U+2066>"} {
		if !strings.Contains(all.String(), want) {
			t.Fatalf("the record does not show %q: %q", want, all.String())
		}
	}
}

// --- turn-end withdrawal (ft-f37eab2c) -------------------------------------

// agyBufferedEnd opens the perm-end-buffered scenario, reads the first
// permission, and waits until the prompt response is sitting unread in the
// wire's queue, so the next drain is the one that reads it.
func agyBufferedEnd(t *testing.T) (*Agy, Request) {
	t.Helper()
	s := zadvMust(t, "perm-end-buffered", "ask")
	zadvStart(t, s)
	var req Request
	for req.ID == "" {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		e, err := s.Next(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if e.Request != nil {
			req = *e.Request
		}
	}
	// The second permission and the prompt response are still on the wire.
	deadline := time.Now().Add(10 * time.Second)
	for len(s.wire.frames) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the buffered prompt response never arrived")
		}
		runtime.Gosched()
	}
	return s, req
}

// An answer that races the turn end is a callback gone, not a stale or tampered
// one: ErrCallbackWithdrawn is the only error the host treats as benign.
func TestZadvW16AnswerRacingTurnEndIsWithdrawnNotStale(t *testing.T) {
	s, req := agyBufferedEnd(t)
	err := s.ValidateResponse(req, Response{Decision: "allow_once"})
	if !errors.Is(err, ErrCallbackWithdrawn) {
		t.Fatalf("answer after the buffered turn end = %v, want ErrCallbackWithdrawn", err)
	}
	// A second answer for the same id, and one through Respond, say the same.
	if err := s.Respond(t.Context(), req, Response{Decision: "deny"}); !errors.Is(err, ErrCallbackWithdrawn) {
		t.Fatalf("Respond for a withdrawn id = %v, want ErrCallbackWithdrawn", err)
	}
	if err := s.ValidateResponse(Request{ID: "never-seen"}, Response{Decision: "allow_once"}); err == nil || errors.Is(err, ErrCallbackWithdrawn) {
		t.Fatalf("an id that was never offered must stay a plain refusal, got %v", err)
	}
}

// drain() reads the turn end ahead of a Next call. The host must still see each
// withdrawal before the status, and nothing may be left behind a completed turn.
func TestZadvW16DrainDeliversWithdrawalsBeforeTheStatusAndLeavesNothingBehind(t *testing.T) {
	s, req := agyBufferedEnd(t)
	_ = s.ValidateResponse(req, Response{Decision: "allow_once"}) // the drain is what is under test; the verdict is checked above
	var got []string
	for {
		e, err := s.Next(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if e.CancelledID != "" {
			got = append(got, "cancel:"+e.CancelledID)
		}
		if e.Status != "" {
			got = append(got, "status:"+e.Status)
			break
		}
	}
	if want := "cancel:s:p1,cancel:s:p2,status:completed"; strings.Join(got, ",") != want {
		t.Fatalf("events = %v, want withdrawals then the status (%s)", got, want)
	}
	if err := s.ContinueTurn(t.Context(), "next"); err != nil {
		t.Fatalf("a follow-up after the completed status was refused: %v", err)
	}
}

// A terminal session still refuses a follow-up.
func TestZadvW16ContinueTurnAfterTerminalStatusIsStillRefused(t *testing.T) {
	s := zadvMust(t, "failed-turn", "ask")
	zadvStart(t, s)
	if _, err := zadvDrain(t, s, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := s.ContinueTurn(t.Context(), "again"); !ContinueRefused(err) {
		t.Fatalf("ContinueTurn on a failed session = %v, want a refusal", err)
	}
}

func TestWithdrawnIDsIsIdempotentAndBounded(t *testing.T) {
	var w withdrawnIDs
	if w.has("a") {
		t.Fatal("zero value reports an id")
	}
	w.add("a")
	w.add("a")
	if !w.has("a") || len(w.order) != 1 {
		t.Fatalf("double withdrawal recorded %d entries", len(w.order))
	}
	for i := 0; i < maxWithdrawnIDs+10; i++ {
		w.add(fmt.Sprintf("id-%d", i))
	}
	if len(w.set) != maxWithdrawnIDs || len(w.order) != maxWithdrawnIDs {
		t.Fatalf("set holds %d/%d, want %d", len(w.set), len(w.order), maxWithdrawnIDs)
	}
	if w.has("a") || !w.has(fmt.Sprintf("id-%d", maxWithdrawnIDs+9)) {
		t.Fatal("eviction dropped the newest or kept the oldest")
	}
}
