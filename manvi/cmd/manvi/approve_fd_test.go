package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/ui"
)

// approvalEvents is a sink that keeps what it was given, so a test can wait for the
// request a host would read before it answers.
type approvalEvents struct {
	mu     sync.Mutex
	events []ui.Event
}

func (l *approvalEvents) Emit(e ui.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *approvalEvents) kinds(kind ui.Kind) []ui.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []ui.Event
	for _, e := range l.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// waitFor polls until the condition holds or two seconds pass.
func waitUntilTrue(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// pendingApproval asks one approval on a goroutine and returns once its
// approval.request is on the stream, which is the moment a host may answer.
func pendingApproval(t *testing.T, ap *fdApprover, log *approvalEvents, ctx context.Context, req ui.Request) <-chan ui.Decision {
	t.Helper()
	before := len(log.kinds(ui.KindApproval))
	done := make(chan ui.Decision, 1)
	go func() {
		d, err := ap.Approve(ctx, req)
		if err != nil {
			t.Errorf("approve: %v", err)
		}
		done <- d
	}()
	waitUntilTrue(t, "approval.request", func() bool { return len(log.kinds(ui.KindApproval)) > before })
	return done
}

func receiveDecision(t *testing.T, done <-chan ui.Decision, within time.Duration) ui.Decision {
	t.Helper()
	select {
	case d := <-done:
		return d
	case <-time.After(within):
		t.Fatalf("Approve did not return within %s", within)
		return ui.Decision{}
	}
}

// A line written before any request is pending is not an answer to the next
// one. Audit B: `{"allow":true}` sitting in the pipe approved a request the
// host had never been shown.
func TestApproveFDLineBeforeTheRequestIsDiscardedAndReported(t *testing.T) {
	read, write := approvalPipe(t)
	if _, err := write.WriteString(`{"allow":true,"reason":"answer to an earlier request"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	log := &approvalEvents{}
	ap := newFDApprover(read, log)
	// The reader owns the stream from construction; give it the stale line.
	// Where the implementation reports the discard, wait for that report.
	deadline := time.Now().Add(time.Second)
	for len(log.kinds(ui.KindNotice)) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	d, err := ap.Approve(ctx, ui.Request{Rule: "protected.path", Path: "other-file", Reason: "different request"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Allow {
		t.Fatalf("a line that predates the request approved it: %+v", d)
	}
	notices := log.kinds(ui.KindNotice)
	if len(notices) == 0 || !strings.Contains(notices[0].Text, "no approval was pending") {
		t.Fatalf("the discarded line left no event on the stream: %+v", notices)
	}
}

// A request cancelled while its host answer is still on the way must not hand
// that answer to the next request: the late line has nothing pending to land
// on, and the next request waits for its own.
func TestApproveFDAnswerToACancelledRequestDoesNotApproveTheNext(t *testing.T) {
	read, write := approvalPipe(t)
	log := &approvalEvents{}
	ap := newFDApprover(read, log)

	ctx1, cancel1 := context.WithCancel(context.Background())
	first := pendingApproval(t, ap, log, ctx1, ui.Request{ID: "first", Reason: "one"})
	cancel1()
	if d := receiveDecision(t, first, 2*time.Second); d.Allow {
		t.Fatalf("cancelled request allowed: %+v", d)
	}

	// The host answers the request it was shown, after the turn gave up on it.
	if _, err := write.WriteString(`{"approval_id":"first","allow":true,"reason":"late"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	waitUntilTrue(t, "the late line to be discarded", func() bool { return len(log.kinds(ui.KindNotice)) > 0 })

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	second := pendingApproval(t, ap, log, ctx2, ui.Request{ID: "second", Reason: "two"})
	select {
	case d := <-second:
		t.Fatalf("the late answer to the first request decided the second: %+v", d)
	case <-time.After(150 * time.Millisecond):
	}
	if _, err := write.WriteString(`{"approval_id":"second","allow":true,"reason":"yes"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if d := receiveDecision(t, second, 2*time.Second); !d.Allow || d.Reason != "yes" {
		t.Fatalf("the second request did not get its own answer: %+v", d)
	}
}

func TestApproveFDChosenIsCheckedAgainstTheOffer(t *testing.T) {
	cases := []struct {
		name   string
		req    ui.Request
		line   string
		answer bool
	}{
		{"unoffered value, single select", ui.Request{Choices: []string{"a", "b"}},
			`{"allow":true,"chosen":["not-an-offered-option"]}`, false},
		{"two values, single select", ui.Request{Choices: []string{"a", "b"}},
			`{"allow":true,"chosen":["a","b"]}`, false},
		{"offered plus unoffered, multi select", ui.Request{Choices: []string{"a", "b"}, MultiSelect: true},
			`{"allow":true,"chosen":["a","zzz"]}`, false},
		{"duplicate value, multi select", ui.Request{Choices: []string{"a", "b"}, MultiSelect: true},
			`{"allow":true,"chosen":["a","a"]}`, false},
		{"one offered value, single select", ui.Request{Choices: []string{"a", "b"}},
			`{"allow":true,"chosen":["b"]}`, true},
		{"two offered values, multi select", ui.Request{Choices: []string{"a", "b"}, MultiSelect: true},
			`{"allow":true,"chosen":["a","b"]}`, true},
		{"write-in beside no choice", ui.Request{Choices: []string{"a", "b"}},
			`{"allow":true,"write_in":"neither"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			read, write := approvalPipe(t)
			log := &approvalEvents{}
			ap := newFDApprover(read, log)
			tc.req.ID = "q"
			done := pendingApproval(t, ap, log, context.Background(), tc.req)
			if _, err := write.WriteString(tc.line + "\n"); err != nil {
				t.Fatal(err)
			}
			d := receiveDecision(t, done, 2*time.Second)
			if d.Answered() != tc.answer {
				t.Fatalf("answered = %v, want %v: %+v", d.Answered(), tc.answer, d)
			}
			if !tc.answer && (d.Allow || len(d.Chosen) != 0 || strings.TrimSpace(d.Reason) == "") {
				t.Fatalf("refusal = %+v, want a reason and no fabricated choice", d)
			}
		})
	}
}

func TestApproveFDIDIsCheckedWhenEchoedAndWarnedWhenOmitted(t *testing.T) {
	t.Run("matching id is accepted without a warning", func(t *testing.T) {
		read, write := approvalPipe(t)
		log := &approvalEvents{}
		ap := newFDApprover(read, log)
		done := pendingApproval(t, ap, log, context.Background(), ui.Request{ID: "id-1", Reason: "r"})
		_, _ = write.WriteString(`{"approval_id":"id-1","allow":true,"reason":"ok"}` + "\n")
		if d := receiveDecision(t, done, 2*time.Second); !d.Allow {
			t.Fatalf("matching id refused: %+v", d)
		}
		if n := len(log.kinds(ui.KindNotice)); n != 0 {
			t.Fatalf("a bound decision produced %d notices", n)
		}
	})
	t.Run("another request's id is refused", func(t *testing.T) {
		read, write := approvalPipe(t)
		log := &approvalEvents{}
		ap := newFDApprover(read, log)
		done := pendingApproval(t, ap, log, context.Background(), ui.Request{ID: "id-2", Reason: "r"})
		_, _ = write.WriteString(`{"approval_id":"id-1","allow":true,"reason":"ok"}` + "\n")
		if d := receiveDecision(t, done, 2*time.Second); d.Allow {
			t.Fatalf("a line naming another request's id approved this one: %+v", d)
		}
	})
	t.Run("an omitted id is accepted with a warning event", func(t *testing.T) {
		read, write := approvalPipe(t)
		log := &approvalEvents{}
		ap := newFDApprover(read, log)
		done := pendingApproval(t, ap, log, context.Background(), ui.Request{ID: "id-3", Reason: "r"})
		_, _ = write.WriteString(`{"allow":true,"reason":"ok"}` + "\n")
		if d := receiveDecision(t, done, 2*time.Second); !d.Allow {
			t.Fatalf("an unbound decision was refused; the contract keeps approval_id optional: %+v", d)
		}
		notices := log.kinds(ui.KindNotice)
		if len(notices) != 1 || !strings.Contains(notices[0].Text, "approval_id") || notices[0].ApprovalID != "id-3" {
			t.Fatalf("no warning for the unbound decision: %+v", notices)
		}
	})
	t.Run("a request with no id is given one before the host sees it", func(t *testing.T) {
		read, write := approvalPipe(t)
		log := &approvalEvents{}
		ap := newFDApprover(read, log)
		done := pendingApproval(t, ap, log, context.Background(), ui.Request{Reason: "an escalation"})
		id := log.kinds(ui.KindApproval)[0].ApprovalID
		if id == "" {
			t.Fatal("approval.request carried no approval_id")
		}
		_, _ = write.WriteString(`{"approval_id":"` + id + `","allow":true,"reason":"ok"}` + "\n")
		if d := receiveDecision(t, done, 2*time.Second); !d.Allow {
			t.Fatalf("the id the host was shown was refused: %+v", d)
		}
	})
}

// The decision line is bounded and an over-long one is a refusal, not a
// buffer. TotalAlloc is the process-wide running total, so the stream is
// large enough that buffering it would dwarf anything else the test allocates.
func TestApproveFDOverLongLineIsRefusedWithoutBeingBuffered(t *testing.T) {
	read, write := approvalPipe(t)
	log := &approvalEvents{}
	ap := newFDApprover(read, log)
	done := pendingApproval(t, ap, log, context.Background(), ui.Request{ID: "big", Reason: "r"})

	const total = 64 << 20
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	go func() {
		chunk := []byte(strings.Repeat("A", 1<<20))
		for sent := 0; sent < total; sent += len(chunk) {
			if _, err := write.Write(chunk); err != nil {
				return
			}
		}
		_, _ = write.WriteString("\n")
	}()
	d := receiveDecision(t, done, 20*time.Second)
	runtime.ReadMemStats(&after)
	if d.Allow || strings.TrimSpace(d.Reason) == "" {
		t.Fatalf("an over-long line was not refused with a reason: %+v", d)
	}
	if grown := after.TotalAlloc - before.TotalAlloc; grown > total/4 {
		t.Fatalf("a %d MiB line cost %d MiB of allocation", total>>20, grown>>20)
	}

	// The reader resynchronised at the newline: the next request is answered
	// from the next line, not from the tail of the one that was refused.
	second := pendingApproval(t, ap, log, context.Background(), ui.Request{ID: "next", Reason: "r"})
	_, _ = write.WriteString(`{"approval_id":"next","allow":true,"reason":"ok"}` + "\n")
	if d := receiveDecision(t, second, 2*time.Second); !d.Allow {
		t.Fatalf("the stream did not resynchronise after the refused line: %+v", d)
	}
}

func TestApproveFDLineAtTheCapIsAccepted(t *testing.T) {
	read, write := approvalPipe(t)
	log := &approvalEvents{}
	ap := newFDApprover(read, log)
	done := pendingApproval(t, ap, log, context.Background(), ui.Request{ID: "cap", Reason: "r"})
	head := `{"approval_id":"cap","allow":true,"reason":"`
	tail := `"}`
	reason := strings.Repeat("r", maxDecisionLine-len(head)-len(tail))
	go func() { _, _ = write.WriteString(head + reason + tail + "\n") }()
	if d := receiveDecision(t, done, 5*time.Second); !d.Allow {
		t.Fatalf("a decision line of exactly %d bytes was refused: %+v", maxDecisionLine, d)
	}
}

// A writer that floods the descriptor with lines nobody asked for cannot flood
// the transcript: the first few are reported, then the report says it stops.
func TestApproveFDDiscardReportsAreBounded(t *testing.T) {
	read, write := approvalPipe(t)
	log := &approvalEvents{}
	_ = newFDApprover(read, log)
	for i := 0; i < 3*maxDiscardNotices; i++ {
		if _, err := write.WriteString("{\"allow\":true}\n"); err != nil {
			t.Fatal(err)
		}
	}
	waitUntilTrue(t, "the reader to reach the notice bound", func() bool {
		return len(log.kinds(ui.KindNotice)) >= maxDiscardNotices
	})
	time.Sleep(100 * time.Millisecond) // anything beyond the bound would land by now
	notices := log.kinds(ui.KindNotice)
	if len(notices) != maxDiscardNotices {
		t.Fatalf("%d discard notices for %d lines, want exactly %d", len(notices), 3*maxDiscardNotices, maxDiscardNotices)
	}
	if last := notices[len(notices)-1].Text; !strings.Contains(last, "not reported") {
		t.Fatalf("the last notice does not say reporting stops: %q", last)
	}
}

func TestReadDecisionLineBoundsWhatItKeeps(t *testing.T) {
	long := strings.Repeat("x", 3*maxDecisionLine)
	r := bufio.NewReader(strings.NewReader(long + "\nsecond\n"))
	line, tooLong, err := readDecisionLine(r)
	if err != nil || !tooLong {
		t.Fatalf("over-cap line: tooLong=%v err=%v", tooLong, err)
	}
	if len(line) > maxDecisionLine {
		t.Fatalf("kept %d bytes of an over-cap line, cap is %d", len(line), maxDecisionLine)
	}
	line, tooLong, err = readDecisionLine(r)
	if err != nil || tooLong || string(line) != "second\n" {
		t.Fatalf("next line = %q tooLong=%v err=%v", line, tooLong, err)
	}
	if _, _, err = readDecisionLine(r); !errors.Is(err, io.EOF) {
		t.Fatalf("end of stream = %v, want io.EOF", err)
	}
}

func TestApproveFDClosedDescriptorRefusesEveryRequest(t *testing.T) {
	read, write := approvalPipe(t)
	log := &approvalEvents{}
	ap := newFDApprover(read, log)
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		d, err := ap.Approve(ctx, ui.Request{ID: "x", Reason: "r"})
		cancel()
		if err != nil || d.Allow || !strings.Contains(d.Reason, "closed") {
			t.Fatalf("request %d on a closed descriptor = %+v, %v", i, d, err)
		}
	}
}

// Requests are answered one at a time: a second caller waits its turn, and a
// waiting caller whose context ends returns without having been asked.
func TestApproveFDQueuedRequestHonoursItsContext(t *testing.T) {
	read, _ := approvalPipe(t)
	log := &approvalEvents{}
	ap := newFDApprover(read, log)
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	_ = pendingApproval(t, ap, log, ctx1, ui.Request{ID: "holder", Reason: "r"})

	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	d, err := ap.Approve(ctx2, ui.Request{ID: "queued", Reason: "r"})
	if err != nil || d.Allow {
		t.Fatalf("queued request = %+v, %v", d, err)
	}
	for _, e := range log.kinds(ui.KindApproval) {
		if e.ApprovalID == "queued" {
			t.Fatal("a request that never got its turn was put to the host")
		}
	}
}
