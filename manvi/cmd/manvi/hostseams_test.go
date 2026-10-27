package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/ui"
)

func TestSessionStartEventCarriesTheOpenSessionID(t *testing.T) {
	var buf bytes.Buffer
	sink := ui.NewJSONSink(&buf, nil)
	const sessionID = "01HZXSESSION"
	emitSessionStart(sink, time.Unix(1_700_000_000, 0).UTC(), "dev", "anthropic/claude", sessionID)

	line := strings.TrimSpace(buf.String())
	if strings.Contains(line, "\n") {
		t.Fatalf("session start wrote more than one event:\n%s", buf.String())
	}
	var ev ui.Event
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, line)
	}
	if ev.Kind != ui.KindSessionStart {
		t.Fatalf("kind = %q, want session.start", ev.Kind)
	}
	if ev.SessionID != sessionID {
		t.Fatalf("session_id = %q, want %q", ev.SessionID, sessionID)
	}
}

func TestParseRunArgsChatOnlyDefaultsOff(t *testing.T) {
	off, err := parseRunArgs(newTestRegistry(t), []string{"-p", "hello"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if off.chatOnly {
		t.Fatal("absent --chat-only set chatOnly")
	}
	on, err := parseRunArgs(newTestRegistry(t), []string{"-p", "hello", "--chat-only"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !on.chatOnly {
		t.Fatal("--chat-only left chatOnly false")
	}
}

func TestParseRunArgsLeavesApproveFDUnset(t *testing.T) {
	opts, err := parseRunArgs(newTestRegistry(t), []string{"-p", "hello", "--json"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.approveFDSet || opts.approveFD != 0 {
		t.Fatalf("approve fd set without the flag: set=%v fd=%d", opts.approveFDSet, opts.approveFD)
	}
	approver, err := approverForRun(opts, ui.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if approver != nil {
		t.Fatal("absent --approve-fd installed an approver; nativeToolsWith must keep nil")
	}
}

func TestParseRunArgsApproveFDRequiresJSONAndANonStdioFD(t *testing.T) {
	if _, err := parseRunArgs(newTestRegistry(t), []string{"-p", "hello", "--approve-fd", "3"}, nil); err == nil {
		t.Fatal("--approve-fd without --json was accepted")
	}
	for _, fd := range []string{"0", "1", "2", "-1"} {
		_, err := parseRunArgs(newTestRegistry(t), []string{"-p", "hello", "--json", "--approve-fd", fd}, nil)
		if err == nil {
			t.Fatalf("--approve-fd %s was accepted", fd)
		}
	}
	if _, err := parseRunArgs(newTestRegistry(t), []string{"-p", "hello", "--json", "--approve-fd", "nope"}, nil); err == nil {
		t.Fatal("non-integer --approve-fd was accepted")
	}
	opts, err := parseRunArgs(newTestRegistry(t), []string{"-p", "hello", "--json", "--approve-fd", "3"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.approveFDSet || opts.approveFD != 3 {
		t.Fatalf("approve fd = set:%v %d, want set 3", opts.approveFDSet, opts.approveFD)
	}
}

func TestFDApproverBlocksUntilAnAnsweredQuestion(t *testing.T) {
	read, write := approvalPipe(t)

	var buf lockedBuffer
	ap := newFDApprover(read, ui.NewJSONSink(&buf, nil))
	req := ui.Request{
		ID: "QUESTION-0001", Reason: "which database?", Rule: "pair.questions.enabled",
		Severity: "ask", Path: "which database?", TaskID: "TASK-1", Grantable: true,
		Choices: []string{"sqlite", "postgres"}, MultiSelect: true,
	}

	type answer struct {
		d   ui.Decision
		err error
	}
	done := make(chan answer, 1)
	go func() {
		d, err := ap.Approve(context.Background(), req)
		done <- answer{d, err}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for buf.Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("approval.request was not emitted before the decision")
		}
		time.Sleep(5 * time.Millisecond)
	}
	var ev ui.Event
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &ev); err != nil {
		t.Fatalf("request event: %v\n%s", err, buf.String())
	}
	if ev.Kind != ui.KindApproval || ev.Text != req.Reason || ev.ApprovalID != req.ID ||
		ev.Rule != req.Rule || ev.Severity != req.Severity || ev.Path != req.Path ||
		ev.TaskID != req.TaskID || !ev.Grantable || !ev.MultiSelect ||
		len(ev.Choices) != 2 || ev.Choices[0] != "sqlite" {
		t.Fatalf("approval.request = %+v", ev)
	}

	select {
	case got := <-done:
		t.Fatalf("Approve returned before the decision line: %+v", got.d)
	default:
	}

	if _, err := write.WriteString("{\"approval_id\":\"QUESTION-0001\",\"allow\":true,\"chosen\":[\"sqlite\",\"postgres\"],\"reason\":\"both\"}\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if !got.d.Answered() || !got.d.Allow || got.d.By != "human" || got.d.Reason != "both" {
			t.Fatalf("decision = %+v", got.d)
		}
		if len(got.d.Chosen) != 2 || got.d.Chosen[0] != "sqlite" || got.d.Chosen[1] != "postgres" {
			t.Fatalf("chosen = %#v", got.d.Chosen)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Approve did not return after the decision line")
	}
}

func TestFDApproverDeclinesAnythingThatIsNotAnAnswer(t *testing.T) {
	cases := []struct {
		name string
		line string
		req  ui.Request
	}{
		{
			name: "allow false",
			line: "{\"approval_id\":\"QUESTION-0001\",\"allow\":false,\"chosen\":[\"sqlite\"],\"reason\":\"no\"}\n",
			req:  ui.Request{ID: "QUESTION-0001", Choices: []string{"sqlite", "postgres"}},
		},
		{
			name: "allow without a choice",
			line: "{\"approval_id\":\"QUESTION-0001\",\"allow\":true,\"reason\":\"unsure\"}\n",
			req:  ui.Request{ID: "QUESTION-0001", Choices: []string{"sqlite", "postgres"}},
		},
		{
			name: "id mismatch",
			line: "{\"approval_id\":\"other\",\"allow\":true,\"chosen\":[\"sqlite\"]}\n",
			req:  ui.Request{ID: "QUESTION-0001", Choices: []string{"sqlite", "postgres"}},
		},
		{
			name: "malformed",
			line: "not-json\n",
			req:  ui.Request{ID: "QUESTION-0001", Choices: []string{"sqlite", "postgres"}},
		},
		{
			name: "two objects",
			line: "{\"allow\":true,\"chosen\":[\"sqlite\"]}{\"allow\":true}\n",
			req:  ui.Request{ID: "QUESTION-0001", Choices: []string{"sqlite", "postgres"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			read, write := approvalPipe(t)
			log := &approvalEvents{}
			done := pendingApproval(t, newFDApprover(read, log), log, context.Background(), tc.req)
			if _, err := write.WriteString(tc.line); err != nil {
				t.Fatal(err)
			}
			d := receiveDecision(t, done, 2*time.Second)
			if d.Allow || d.Answered() || strings.TrimSpace(d.Reason) == "" {
				t.Fatalf("decision = %+v, want a refusal with a reason", d)
			}
			if len(d.Chosen) != 0 || d.WriteIn != "" {
				t.Fatalf("refusal fabricated an answer: %+v", d)
			}
		})
	}
}

func TestFDApproverWriteInAnswers(t *testing.T) {
	read, write := approvalPipe(t)
	log := &approvalEvents{}
	done := pendingApproval(t, newFDApprover(read, log), log, context.Background(), ui.Request{
		ID: "QUESTION-0001", Choices: []string{"sqlite", "postgres"},
	})
	if _, err := write.WriteString("{\"allow\":true,\"write_in\":\"neither\"}\n"); err != nil {
		t.Fatal(err)
	}
	d := receiveDecision(t, done, 2*time.Second)
	if !d.Answered() || d.WriteIn != "neither" || d.By != "human" {
		t.Fatalf("decision = %+v", d)
	}
}

func TestFDApproverCancelledContextRefuses(t *testing.T) {
	read, _ := approvalPipe(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d, err := newFDApprover(read, ui.Discard).Approve(ctx, ui.Request{ID: "1", Reason: "blocked"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Allow || d.Answered() || d.Reason == "" {
		t.Fatalf("cancelled decision = %+v", d)
	}

	read2, _ := approvalPipe(t)
	var buf lockedBuffer
	ap := newFDApprover(read2, ui.NewJSONSink(&buf, nil))
	ctx2, cancel2 := context.WithCancel(context.Background())
	done := make(chan ui.Decision, 1)
	go func() {
		decision, approveErr := ap.Approve(ctx2, ui.Request{ID: "2", Reason: "wait", Choices: []string{"a", "b"}})
		if approveErr != nil {
			t.Errorf("approve: %v", approveErr)
		}
		done <- decision
	}()
	deadline := time.Now().Add(2 * time.Second)
	for buf.Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("in-flight approve did not emit")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel2()
	select {
	case d := <-done:
		if d.Allow || d.Answered() {
			t.Fatalf("in-flight cancel allowed: %+v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled context did not unblock the read")
	}
}

func TestFDApproverClosedFDRefuses(t *testing.T) {
	read, write := approvalPipe(t)
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	d, err := newFDApprover(read, ui.Discard).Approve(context.Background(), ui.Request{ID: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Allow || d.Reason == "" {
		t.Fatalf("closed fd decision = %+v", d)
	}
}

func TestFDApproverReadsOneLineAtATime(t *testing.T) {
	read, write := approvalPipe(t)
	log := &approvalEvents{}
	ap := newFDApprover(read, log)
	first := pendingApproval(t, ap, log, context.Background(), ui.Request{ID: "Q1", Choices: []string{"a", "b"}})
	if _, err := write.WriteString("{\"approval_id\":\"Q1\",\"allow\":true,\"chosen\":[\"a\"]}\n"); err != nil {
		t.Fatal(err)
	}
	firstAnswer := receiveDecision(t, first, 2*time.Second)
	second := pendingApproval(t, ap, log, context.Background(), ui.Request{ID: "Q2", Choices: []string{"a", "b"}})
	if _, err := write.WriteString("{\"approval_id\":\"Q2\",\"allow\":true,\"write_in\":\"other\"}\n"); err != nil {
		t.Fatal(err)
	}
	secondAnswer := receiveDecision(t, second, 2*time.Second)
	if !firstAnswer.Answered() || len(firstAnswer.Chosen) != 1 || firstAnswer.Chosen[0] != "a" {
		t.Fatalf("first = %+v", firstAnswer)
	}
	if !secondAnswer.Answered() || secondAnswer.WriteIn != "other" {
		t.Fatalf("second = %+v", secondAnswer)
	}
}

func TestUserContentKeepsTextThenImages(t *testing.T) {
	blocks := userContent("look", []llm.ImageBlock{
		{MediaType: "image/png", Data: []byte{1}},
		{MediaType: "image/jpeg", Data: []byte{2}},
	})
	if len(blocks) != 3 {
		t.Fatalf("len = %d, want 3", len(blocks))
	}
	text, ok := blocks[0].(llm.TextBlock)
	if !ok || text.Text != "look" {
		t.Fatalf("text block = %#v", blocks[0])
	}
	png, ok := blocks[1].(llm.ImageBlock)
	if !ok || png.MediaType != "image/png" || !bytes.Equal(png.Data, []byte{1}) {
		t.Fatalf("first image = %#v", blocks[1])
	}
	jpeg, ok := blocks[2].(llm.ImageBlock)
	if !ok || jpeg.MediaType != "image/jpeg" || !bytes.Equal(jpeg.Data, []byte{2}) {
		t.Fatalf("second image = %#v", blocks[2])
	}
}

func TestParseRunArgsLoadsTwoImages(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "a.png")
	jpeg := filepath.Join(dir, "b.jpg")
	if err := os.WriteFile(png, []byte("\x89PNG\x0D\x0A\x1A\x0A"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jpeg, []byte("\xFF\xD8\xFF"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts, err := parseRunArgs(newTestRegistry(t), []string{
		"-p", "see", "--image", png, "--image", jpeg,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	blocks := userContent(opts.prompt, opts.images)
	if len(blocks) != 3 {
		t.Fatalf("len = %d, want text plus two images", len(blocks))
	}
	if _, ok := blocks[0].(llm.TextBlock); !ok {
		t.Fatalf("first block = %T", blocks[0])
	}
	if img, ok := blocks[1].(llm.ImageBlock); !ok || img.MediaType != "image/png" {
		t.Fatalf("first block = %#v, want image/png", blocks[1])
	}
	if img, ok := blocks[2].(llm.ImageBlock); !ok || img.MediaType != "image/jpeg" {
		t.Fatalf("second block = %#v, want image/jpeg", blocks[2])
	}
}

func TestLoadRunImagesRejectsBeforeAProviderWouldBeContacted(t *testing.T) {
	if _, err := loadRunImages([]string{"a", "b", "c", "d", "e"}); err == nil {
		t.Fatal("a fifth image was accepted")
	}

	dir := t.TempDir()
	if _, err := loadRunImages([]string{dir}); err == nil {
		t.Fatal("a directory was accepted")
	}
	missing := filepath.Join(dir, "missing.png")
	if _, err := loadRunImages([]string{missing}); err == nil {
		t.Fatal("a missing file was accepted")
	}

	over := filepath.Join(dir, "big.png")
	f, err := os.Create(over) // #nosec G304 -- a path under this test's own t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxImageBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRunImages([]string{over}); err == nil {
		t.Fatal("an image over 8 MiB was accepted")
	}

	text := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(text, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRunImages([]string{text}); err == nil {
		t.Fatal("a non-image was accepted")
	}
	bmp := filepath.Join(dir, "x.bmp")
	if err := os.WriteFile(bmp, []byte("BM..."), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRunImages([]string{bmp}); err == nil {
		t.Fatal("image/bmp was accepted")
	}

	gif := filepath.Join(dir, "g.gif")
	webp := filepath.Join(dir, "w.webp")
	if err := os.WriteFile(gif, []byte("GIF89a..."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(webp, []byte("RIFF\x00\x00\x00\x00WEBPVP"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadRunImages([]string{gif, webp})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].MediaType != "image/gif" || got[1].MediaType != "image/webp" {
		t.Fatalf("images = %+v", got)
	}

	exact := filepath.Join(dir, "exact.png")
	ef, err := os.Create(exact) // #nosec G304 -- a path under this test's own t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ef.WriteString("\x89PNG\x0D\x0A\x1A\x0A"); err != nil {
		t.Fatal(err)
	}
	if err := ef.Truncate(maxImageBytes); err != nil {
		t.Fatal(err)
	}
	if err := ef.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRunImages([]string{exact}); err != nil {
		t.Fatalf("exactly 8 MiB was refused: %v", err)
	}
}

func TestParseRunArgsFifthImageErrors(t *testing.T) {
	_, err := parseRunArgs(newTestRegistry(t), []string{
		"-p", "hi",
		"--image", "a", "--image", "b", "--image", "c", "--image", "d", "--image", "e",
	}, nil)
	if err == nil {
		t.Fatal("parse accepted a fifth image")
	}
}

// lockedBuffer is the JSON sink's output in a test that polls it while
// Approve, on another goroutine, is still writing.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// approvalPipe is the descriptor --approve-fd reads. Both ends close at
// cleanup; one a test already closed is not an error.
func approvalPipe(t *testing.T) (read, write *os.File) {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []*os.File{read, write} {
		t.Cleanup(func() {
			if err := f.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				t.Errorf("close %s: %v", f.Name(), err)
			}
		})
	}
	return read, write
}
