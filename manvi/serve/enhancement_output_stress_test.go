package serve

import (
	"strings"
	"testing"
)

func TestEnhancementUnwrapStressCorpus(t *testing.T) {
	record := enhancementSource(t, "fix E42", "Open src/main.go for #21.\nMust preserve 日本語.", "title", "description")
	valid := `{"title":"Resolve E42","description":"Investigate src/main.go for #21.\nMust preserve 日本語.","rationale":"Clarified."}`
	think := "<think>\n" + strings.Repeat("plan the rewrite. ", 400) + "\n</think>\n"
	wrappers := []string{
		valid,
		"```json\n" + valid + "\n```",
		"```JSON\n" + valid + "\n```",
		"```\n" + valid + "\n```",
		"<think>x</think>\n" + valid,
		"<THINK>x</THINK>\n" + valid,
		"<thinking>x</thinking>\n```json\n" + valid + "\n```",
		"</think>\n" + valid,
		think + valid,
		think + "```json\n" + valid + "\n```",
		"Sure, I can help.\nHere is the JSON:\n" + valid,
		"Okay.\n\n```json\n" + valid + "\n```",
		"JSON: " + valid,
		"\ufeff" + valid,
		"  \n" + valid + "\n  ",
		strings.ReplaceAll("```json\n"+valid+"\n```", "\n", "\r\n"),
		"<think>x</think>\nSure.\nHere is the JSON:\n```json\n" + valid + "\n```",
		strings.Repeat("<think>x</think>\n", 8) + valid,
	}
	for i, raw := range wrappers {
		proposal, err := decodeEnhancement([]byte(raw), record)
		if err != nil || proposal.Title == nil || *proposal.Title != "Resolve E42" {
			t.Fatalf("wrapper %d rejected: %v", i, err)
		}
	}
	refusals := []string{
		"<think>plan forever\n" + valid,
		"<think>plan</think>\nnot json",
		"```python\n" + valid + "\n```",
		"```json\n" + valid + "\n```\nThanks!",
		valid + "\n" + `{"title":"other"}`,
		`{"title":"Resolve E42","description":"Investigate src/main.go for #21.\nMust preserve 日本語.","worker_id":"attacker"}`,
		`{"title":"Resolve the failure","description":"Investigate src/main.go for #21.\nMust preserve 日本語."}`,
		strings.Repeat("<think>x</think>\n", 9) + valid,
		`{"title":null,"description":"Investigate src/main.go for #21.\nMust preserve 日本語."}`,
	}
	for i, raw := range refusals {
		if _, err := decodeEnhancement([]byte(raw), record); err == nil {
			t.Fatalf("refusal %d was accepted", i)
		}
	}
}
