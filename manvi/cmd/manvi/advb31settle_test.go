package main

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/agent"
	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// A turn that dropped a block type must say so to the operator; Clean() is
// false for it, so the notice path is entered, and has to name the cause.
func TestOpenB31SettleOutcomeNoticeNamesUnknownKinds(t *testing.T) {
	t.Skip("known-open: needs agent/loop.go (held by w24-verdicts-b); see ft-efcfd01a")
	got := noticeText(outcomeNotices(agent.Outcome{
		Steps: 1, Decoding: llm.DecodingReport{UnknownKinds: []string{"mystery_block"}, UnknownKindsMore: 2},
	}, 500))
	if !strings.Contains(got, "mystery_block") {
		t.Fatalf("an unrecognised block type was dropped from the turn and no notice names it; notices:\n%q", got)
	}
}
