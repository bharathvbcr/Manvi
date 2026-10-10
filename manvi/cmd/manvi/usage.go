package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/llm/budget"
	"github.com/bharathvbcr/Manvi/manvi/ui"
)

// usageEvent is the one usage line both faces emit for a finished turn: what
// the turn's own agent spent plus what every sub-agent it dispatched spent,
// totalled, priced, and split by model.
//
// The split is the point. Once a fan-out's children run on models other than
// the parent's, a single token total cannot be turned into money afterwards —
// the same thousand tokens cost several times more on one model than on
// another — so each model's share travels with its own cost, priced step by
// step from that model's own table as the responses arrived.
func usageEvent(own devcouncil.SubAgentUsage, delegated []devcouncil.SubAgentUsage) ui.Event {
	byModel := map[string]devcouncil.SubAgentUsage{}
	var total devcouncil.SubAgentUsage
	for _, u := range append([]devcouncil.SubAgentUsage{own}, delegated...) {
		if !u.Any() {
			continue
		}
		row := byModel[u.Model]
		row.Model = u.Model
		row.Add(u)
		byModel[u.Model] = row
		total.Add(u)
	}
	rows := make([]devcouncil.SubAgentUsage, 0, len(byModel))
	for _, row := range byModel {
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Model < rows[j].Model })

	e := ui.Event{
		Kind: ui.KindUsage, At: time.Now().UTC(),
		InputTokens: total.InputTokens, OutputTokens: total.OutputTokens,
		CostNanoUSD: total.CostNanoUSD, UnpricedSteps: total.UnpricedSteps,
		Text: usageText(total, rows),
	}
	for _, row := range rows {
		e.ByModel = append(e.ByModel, ui.ModelUsage{
			Model:       row.Model,
			InputTokens: row.InputTokens, OutputTokens: row.OutputTokens,
			ReasoningTokens: row.ReasoningTokens,
			CacheReadTokens: row.CacheReadTokens, CacheWriteTokens: row.CacheWriteTokens,
			CostNanoUSD: row.CostNanoUSD, UnpricedSteps: row.UnpricedSteps,
		})
	}
	return e
}

// usageText is the human half of the usage line: the cost, and where it was
// spent. One model gets one line; several get a line each, so the split is
// readable without the event's JSON.
func usageText(total devcouncil.SubAgentUsage, rows []devcouncil.SubAgentUsage) string {
	switch len(rows) {
	case 0:
		return ""
	case 1:
		return "cost " + costText(total) + " · " + rowText(rows[0], false)
	}
	lines := []string{fmt.Sprintf("cost %s across %d models:", costText(total), len(rows))}
	for _, row := range rows {
		lines = append(lines, rowText(row, true))
	}
	return strings.Join(lines, "\n")
}

func rowText(row devcouncil.SubAgentUsage, withCost bool) string {
	parts := []string{row.Model}
	if withCost {
		parts = append(parts, fmt.Sprintf("%d in / %d out", row.InputTokens, row.OutputTokens))
	}
	if row.CacheReadTokens > 0 || row.CacheWriteTokens > 0 {
		parts = append(parts, fmt.Sprintf("cache %d read / %d written", row.CacheReadTokens, row.CacheWriteTokens))
	}
	if withCost {
		parts = append(parts, costText(row))
	}
	return strings.Join(parts, " · ")
}

// costText renders a cost so that an incomplete one can never be read as the
// bill: steps no price table covered turn it into a lower bound, and a share
// with no priced step at all says so instead of printing $0.00.
func costText(u devcouncil.SubAgentUsage) string {
	switch {
	case u.UnpricedSteps == 0:
		return budget.USD(u.CostNanoUSD)
	case u.CostNanoUSD > 0:
		return fmt.Sprintf("at least %s (%d step(s) unpriced)", budget.USD(u.CostNanoUSD), u.UnpricedSteps)
	default:
		return fmt.Sprintf("unpriced (%d step(s), no price table)", u.UnpricedSteps)
	}
}
