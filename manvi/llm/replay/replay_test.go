package replay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"manvi/llm"
)

// This package had no test file. Its playback half was exercised second-hand by
// eight other packages that build fixtures in Go and drive their loop tests
// through them; its recording half — Load, NewRecord, Save, and the recording
// stream — was executed by nothing at all, in a package whose doc comment
// offers exactly that as the way "a real session becomes an offline regression
// test". An offered facility nobody has run is a claim, not a feature.

// drain runs a stream to completion the way the turn driver does and returns
// the settled response.
func drain(t *testing.T, s llm.Stream) ([]llm.Chunk, llm.Response) {
	t.Helper()
	var chunks []llm.Chunk
	for {
		chunk, err := s.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		chunks = append(chunks, chunk)
	}
	resp, err := s.Response()
	if err != nil {
		t.Fatalf("Response after drain: %v", err)
	}
	return chunks, resp
}

// text pulls the visible text out of a settled message. It is a helper rather
// than an inline assertion because an unchecked one panics with "interface
// conversion" and nothing else, and the thing a reader needs to know is which
// block kind arrived instead.
func text(t *testing.T, m llm.Message) string {
	t.Helper()
	if len(m.Content) == 0 {
		t.Fatalf("message carries no content blocks: %+v", m)
	}
	block, ok := m.Content[0].(llm.TextBlock)
	if !ok {
		t.Fatalf("first content block is %T (kind %q), want a TextBlock", m.Content[0], m.Content[0].Kind())
	}
	return block.Text
}

func textTurn(body string, stop llm.StopReason) Turn {
	return Turn{
		Chunks:     []llm.Chunk{{Kind: llm.ChunkText, Text: body}},
		Message:    llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: body}}},
		StopReason: stop,
		Usage:      llm.Usage{InputTokens: 11, OutputTokens: 22},
	}
}

func ask(text string) llm.Request {
	return llm.Request{
		Model:    "mock-model",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: text}}}},
	}
}

func TestReplayPlaysRecordedTurnsInOrderAndRecordsWhatItWasAsked(t *testing.T) {
	p := New(Fixture{Turns: []Turn{
		textTurn("first", llm.StopToolUse),
		textTurn("second", llm.StopEndTurn),
	}})

	if got := p.Name(); got != "replay" {
		t.Errorf("Name() = %q, want the default %q", got, "replay")
	}
	if got := p.Remaining(); got != 2 {
		t.Errorf("Remaining() = %d before anything played, want 2", got)
	}

	for i, want := range []string{"first", "second"} {
		s, err := p.Stream(context.Background(), ask(want))
		if err != nil {
			t.Fatalf("turn %d: Stream: %v", i, err)
		}
		chunks, resp := drain(t, s)
		if len(chunks) != 1 || chunks[0].Text != want {
			t.Errorf("turn %d streamed %v, want one chunk of %q", i, chunks, want)
		}
		if got := text(t, resp.Message); got != want {
			t.Errorf("turn %d settled on %q, want %q", i, got, want)
		}
		if resp.Usage.OutputTokens != 22 {
			t.Errorf("turn %d lost its usage: %+v", i, resp.Usage)
		}
		// Provenance is filled in from the request rather than left nil, so a
		// replayed message is attributable the way a real one is.
		if resp.Message.Provenance == nil || resp.Message.Provenance.Model != "mock-model" {
			t.Errorf("turn %d has provenance %+v, want the request's model", i, resp.Message.Provenance)
		}
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}

	if got := p.Remaining(); got != 0 {
		t.Errorf("Remaining() = %d after both turns played, want 0", got)
	}
	reqs := p.Requests()
	if len(reqs) != 2 || text(t, reqs[0].Messages[0]) != "first" {
		t.Errorf("Requests() did not record what the loop asked for: %+v", reqs)
	}
}

// A fixture that runs out must say so. An empty response here would look like a
// model that chose to stop, and the test above it would pass for the wrong
// reason — which is the whole failure mode this package exists to avoid.
func TestReplayRefusesToInventATurnPastTheFixture(t *testing.T) {
	p := New(Fixture{Turns: []Turn{textTurn("only", llm.StopEndTurn)}})
	if _, err := p.Stream(context.Background(), ask("one")); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	_, err := p.Stream(context.Background(), ask("two"))
	if err == nil {
		t.Fatal("a second turn was served from a one-turn fixture")
	}
	if !strings.Contains(err.Error(), "exhausted") {
		t.Errorf("error %q does not say the fixture ran out", err)
	}
}

func TestARecordedErrorReplaysAsAnError(t *testing.T) {
	p := New(Fixture{Turns: []Turn{{Err: "the server hung up"}}})
	_, err := p.Stream(context.Background(), ask("hello"))
	if err == nil || !strings.Contains(err.Error(), "the server hung up") {
		t.Fatalf("Stream error = %v, want the recorded failure", err)
	}
}

func TestAStreamRefusesToSettleBeforeItIsDrained(t *testing.T) {
	p := New(Fixture{Turns: []Turn{textTurn("text", llm.StopEndTurn)}})
	s, err := p.Stream(context.Background(), ask("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Response(); err == nil {
		t.Error("Response() answered before the stream was drained; a partial turn would read as a settled one")
	}
}

func TestAClosedStreamRefusesToKeepReading(t *testing.T) {
	p := New(Fixture{Turns: []Turn{textTurn("text", llm.StopEndTurn)}})
	s, err := p.Stream(context.Background(), ask("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next(); err == nil || errors.Is(err, io.EOF) {
		t.Errorf("Next() after Close returned %v, want a refusal rather than a clean end", err)
	}
}

// An unknown model is a false return, not a permissive default — the same rule
// the real adapters follow, so a fixture cannot make a request serviceable that
// a live provider would reject at assembly.
func TestCapabilityIsUnknownRatherThanPermissive(t *testing.T) {
	p := New(Fixture{Capabilities: []llm.Capability{{Model: "known", ContextWindow: 4096}}})
	got, ok := p.Capability("known")
	if !ok {
		t.Fatal("a model the fixture declares is not served")
	}
	if got.Provider != "replay" {
		t.Errorf("Provider = %q, want it filled in from the adapter name", got.Provider)
	}
	if _, ok := p.Capability("never-declared"); ok {
		t.Error("a model the fixture does not describe was served anyway")
	}
}

func TestLoadReadsAFixtureAndRefusesOneItCannotRead(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "good.json")
	encoded, err := json.Marshal(Fixture{Provider: "recorded", Turns: []Turn{textTurn("from disk", llm.StopEndTurn)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(good, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Load(good)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Name() != "recorded" {
		t.Errorf("Name() = %q, want the fixture's own provider", p.Name())
	}
	if p.Remaining() != 1 {
		t.Errorf("Remaining() = %d, want the one recorded turn", p.Remaining())
	}

	if _, err := Load(filepath.Join(dir, "absent.json")); err == nil {
		t.Error("Load of a missing fixture succeeded")
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil {
		t.Error("Load of an unparseable fixture succeeded, so a corrupt fixture would replay as an empty session")
	}
}

// scripted is a provider standing in for a live one, so Record has something
// real to wrap.
type scripted struct {
	turns []Turn
	at    int
	err   error
}

func (s *scripted) Name() string { return "scripted" }

func (s *scripted) Capability(model string) (llm.Capability, bool) {
	if model != "mock-model" {
		return llm.Capability{}, false
	}
	return llm.Capability{Provider: "scripted", Model: model, ContextWindow: 8192}, true
}

func (s *scripted) Stream(context.Context, llm.Request) (llm.Stream, error) {
	if s.err != nil {
		return nil, s.err
	}
	turn := s.turns[s.at]
	s.at++
	return &scriptedStream{turn: turn}, nil
}

type scriptedStream struct {
	turn Turn
	at   int
	done bool
}

func (s *scriptedStream) Next() (llm.Chunk, error) {
	if s.at >= len(s.turn.Chunks) {
		s.done = true
		return llm.Chunk{}, io.EOF
	}
	c := s.turn.Chunks[s.at]
	s.at++
	return c, nil
}

func (s *scriptedStream) Response() (llm.Response, error) {
	if !s.done {
		return llm.Response{}, errors.New("scripted: not drained")
	}
	return llm.Response{
		Message:    s.turn.Message,
		StopReason: s.turn.StopReason,
		Usage:      s.turn.Usage,
	}, nil
}

func (s *scriptedStream) Close() error { return nil }

// The round trip this package promises: drive a live provider through Record,
// write the fixture to disk, load it back, and replay it. If the recorded
// session does not reproduce, the facility does not work — and until this test
// existed nothing had ever run it.
func TestRecordRoundTripsALiveSessionThroughDisk(t *testing.T) {
	live := &scripted{turns: []Turn{
		textTurn("recorded one", llm.StopToolUse),
		textTurn("recorded two", llm.StopEndTurn),
	}}
	rec := NewRecord(live)

	if rec.Name() != "scripted" {
		t.Errorf("Name() = %q, want the wrapped provider's", rec.Name())
	}
	// Asked twice; the fixture must carry it once.
	if _, ok := rec.Capability("mock-model"); !ok {
		t.Fatal("the wrapped provider's capability did not come through")
	}
	if _, ok := rec.Capability("mock-model"); !ok {
		t.Fatal("second Capability call disagreed with the first")
	}
	if _, ok := rec.Capability("not-served"); ok {
		t.Error("a model the wrapped provider does not serve was reported as served")
	}

	for i := range 2 {
		s, err := rec.Stream(context.Background(), ask("go"))
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		drain(t, s)
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}

	fixture := rec.Fixture()
	if len(fixture.Turns) != 2 {
		t.Fatalf("recorded %d turn(s), want 2", len(fixture.Turns))
	}
	if len(fixture.Capabilities) != 1 {
		t.Errorf("recorded %d capabilities, want the same one recorded once: %+v",
			len(fixture.Capabilities), fixture.Capabilities)
	}

	path := filepath.Join(t.TempDir(), "session.json")
	if err := rec.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	back, err := Load(path)
	if err != nil {
		t.Fatalf("Load of the file just saved: %v", err)
	}
	if back.Remaining() != 2 {
		t.Fatalf("the reloaded fixture holds %d turn(s), want 2", back.Remaining())
	}
	if _, ok := back.Capability("mock-model"); !ok {
		t.Error("the reloaded fixture does not serve the model the session ran on")
	}

	for i, want := range []string{"recorded one", "recorded two"} {
		s, err := back.Stream(context.Background(), ask("go"))
		if err != nil {
			t.Fatalf("replay turn %d: %v", i, err)
		}
		chunks, resp := drain(t, s)
		if len(chunks) != 1 || chunks[0].Text != want {
			t.Errorf("replay turn %d streamed %v, want one chunk of %q", i, chunks, want)
		}
		if got := text(t, resp.Message); got != want {
			t.Errorf("replay turn %d settled on %q, want %q", i, got, want)
		}
		if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 22 {
			t.Errorf("replay turn %d lost the recorded usage: %+v", i, resp.Usage)
		}
	}

	stops := []llm.StopReason{llm.StopToolUse, llm.StopEndTurn}
	for i, turn := range fixture.Turns {
		if turn.StopReason != stops[i] {
			t.Errorf("recorded turn %d stopped as %q, want %q", i, turn.StopReason, stops[i])
		}
	}
}

// A turn the provider failed is recorded as a failure. Dropping it would leave
// a fixture that replays a session shorter than the one that happened.
func TestRecordCapturesAFailedTurnAsAFailedTurn(t *testing.T) {
	rec := NewRecord(&scripted{err: errors.New("upstream refused")})
	if _, err := rec.Stream(context.Background(), ask("go")); err == nil {
		t.Fatal("the wrapped provider's failure did not surface")
	}
	fixture := rec.Fixture()
	if len(fixture.Turns) != 1 || fixture.Turns[0].Err == "" {
		t.Fatalf("the failure was not recorded as a turn: %+v", fixture.Turns)
	}
	if !strings.Contains(fixture.Turns[0].Err, "upstream refused") {
		t.Errorf("recorded error %q does not name the cause", fixture.Turns[0].Err)
	}

	// And it replays as one.
	replayed := New(fixture)
	if _, err := replayed.Stream(context.Background(), ask("go")); err == nil {
		t.Error("a recorded failure replayed as a success")
	}
}

func TestSaveReportsAPathItCannotWrite(t *testing.T) {
	rec := NewRecord(&scripted{})
	if err := rec.Save(filepath.Join(t.TempDir(), "no-such-dir", "f.json")); err == nil {
		t.Error("Save into a directory that does not exist reported success")
	}
}
