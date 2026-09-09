package replay

import (
	"context"
	"errors"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

func TestReplayOwnsFixtureAndRequestSnapshots(t *testing.T) {
	f := Fixture{Turns: []Turn{textTurn("original", llm.StopEndTurn)}}
	p := New(f)
	f.Turns[0].Message.Content[0] = llm.TextBlock{Text: "modified"}
	req := ask("original request")
	s, err := p.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	req.Messages[0].Content[0] = llm.TextBlock{Text: "modified request"}
	_, resp := drain(t, s)
	if resp.Message.Text() != "original" {
		t.Fatal("fixture shares caller-owned blocks")
	}
	got := p.Requests()
	got[0].Messages[0].Content[0] = llm.TextBlock{Text: "modified snapshot"}
	if p.Requests()[0].Messages[0].Text() != "original request" {
		t.Fatal("request records share mutable blocks")
	}
}

func TestReplayCancellationDoesNotSpendFixture(t *testing.T) {
	p := New(Fixture{Turns: []Turn{textTurn("original", llm.StopEndTurn)}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Stream(ctx, ask("go")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Stream error=%v", err)
	}
	if p.Remaining() != 1 {
		t.Fatal("cancellation consumed fixture")
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	s, err := p.Stream(ctx, ask("go"))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := s.Next(); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Next error=%v", err)
	}
}

func TestStrictReplayRejectsDriftWithoutConsumingTurn(t *testing.T) {
	want := ask("expected")
	turn := textTurn("answer", llm.StopEndTurn)
	turn.Request = &want
	p := New(Fixture{StrictRequests: true, Turns: []Turn{turn}})
	if _, err := p.Stream(context.Background(), ask("different")); err == nil {
		t.Fatal("strict request drift accepted")
	}
	if p.Remaining() != 1 || len(p.Requests()) != 0 {
		t.Fatal("mismatch consumed a fixture turn")
	}
	s, err := p.Stream(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	_, resp := drain(t, s)
	if resp.Message.Text() != "answer" {
		t.Fatal("matching request failed")
	}
	missing := New(Fixture{StrictRequests: true, Turns: []Turn{textTurn("unexamined", llm.StopEndTurn)}})
	if _, err := missing.Stream(context.Background(), want); err == nil {
		t.Fatal("strict fixture without expected request accepted")
	}
}

func TestRecordingOwnsResponsesAndRecordsOnce(t *testing.T) {
	turn := textTurn("recorded", llm.StopEndTurn)
	turn.Malformed = []llm.MalformedCall{{Name: "bad", Reason: "unusable"}}
	turn.Decoding = llm.DecodingReport{ReasoningReclassified: true}
	r := NewRecord(New(Fixture{Turns: []Turn{turn}}))
	s, err := r.Stream(context.Background(), ask("question"))
	if err != nil {
		t.Fatal(err)
	}
	_, resp := drain(t, s)
	resp.Message.Content[0] = llm.TextBlock{Text: "mutated"}
	if _, err := s.Response(); err != nil {
		t.Fatal(err)
	}
	f := r.Fixture()
	if len(f.Turns) != 1 || f.Turns[0].Request == nil || len(f.Turns[0].Malformed) != 1 || !f.Turns[0].Decoding.ReasoningReclassified || f.Turns[0].Message.Text() != "recorded" {
		t.Fatalf("record=%+v", f)
	}
	f.Turns[0].Message.Content[0] = llm.TextBlock{Text: "fixture mutation"}
	if r.Fixture().Turns[0].Message.Text() != "recorded" {
		t.Fatal("recorded fixture is borrowed")
	}
}
