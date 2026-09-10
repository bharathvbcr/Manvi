package computer

import (
	"errors"
	"sort"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

// MatchDiagnostics explains selector evidence. Score is the fraction of
// requested semantic fields matched, not a probability or authorization.
// Native Resolve remains the only authority for actionable target selection.
type MatchDiagnostics struct {
	Selector        workflow.Selector `json:"selector"`
	ExactMatches    int               `json:"exact_matches"`
	TotalCandidates int               `json:"total_candidates"`
	Truncated       bool              `json:"truncated"`
	Confidence      string            `json:"confidence"`
	Candidates      []MatchCandidate  `json:"candidates"`
}
type MatchCandidate struct {
	NodeID         string   `json:"node_id"`
	Role           string   `json:"role"`
	Name           string   `json:"name"`
	MatchedFields  int      `json:"matched_fields"`
	RequiredFields int      `json:"required_fields"`
	Score          float64  `json:"score"`
	Missing        []string `json:"missing"`
	Enabled        bool     `json:"enabled"`
	Editable       bool     `json:"editable"`
}

func Diagnose(selector workflow.Selector, o Observation) MatchDiagnostics {
	d := MatchDiagnostics{Selector: selector.Clone(), Confidence: "missing", Candidates: []MatchCandidate{}}
	if selector.Visual != nil {
		d.Confidence = "visual_requires_native_resolution"
		return d
	}
	byID := map[string]Node{}
	for _, n := range o.Nodes {
		byID[n.ID] = n
	}
	for _, n := range o.Nodes {
		c := MatchCandidate{NodeID: n.ID, Role: n.Role, Name: n.Name, Enabled: n.Enabled, Editable: n.Editable, Missing: []string{}}
		check := func(field string, requested, matched bool) {
			if !requested {
				return
			}
			c.RequiredFields++
			if matched {
				c.MatchedFields++
			} else {
				c.Missing = append(c.Missing, field)
			}
		}
		check("role", selector.Role != "", n.Role == selector.Role)
		check("name", selector.Name != "", n.Name == selector.Name)
		check("identifier", selector.Identifier != "", n.Identifier != nil && *n.Identifier == selector.Identifier)
		ancestorMatched := false
		parent := n.Parent
		visited := map[string]bool{}
		for depth := 0; parent != nil && depth < 40; depth++ {
			if visited[*parent] {
				break
			}
			visited[*parent] = true
			p, ok := byID[*parent]
			if !ok {
				break
			}
			if p.Name == selector.Ancestor {
				ancestorMatched = true
				break
			}
			parent = p.Parent
		}
		check("ancestor", selector.Ancestor != "", ancestorMatched)
		if c.RequiredFields == 0 || c.MatchedFields == 0 {
			continue
		}
		c.Score = float64(c.MatchedFields) / float64(c.RequiredFields)
		if c.MatchedFields == c.RequiredFields {
			d.ExactMatches++
		}
		d.Candidates = append(d.Candidates, c)
	}
	sort.SliceStable(d.Candidates, func(i, j int) bool {
		if d.Candidates[i].Score == d.Candidates[j].Score {
			return d.Candidates[i].NodeID < d.Candidates[j].NodeID
		}
		return d.Candidates[i].Score > d.Candidates[j].Score
	})
	d.TotalCandidates = len(d.Candidates)
	if len(d.Candidates) > 200 {
		d.Candidates = d.Candidates[:200]
		d.Truncated = true
	}
	switch {
	case !o.Complete:
		d.Confidence = "incomplete_observation"
	case d.ExactMatches > 1:
		d.Confidence = "ambiguous"
	case d.ExactMatches == 1:
		d.Confidence = "unique_semantic_match"
	}
	return d
}

// TargetDrift is a read-only ladder walk result for one capability target.
type TargetDrift struct {
	Target        string `json:"target"`
	StrategyIndex int    `json:"strategy_index"`
	Matches       int    `json:"matches"`
	Ambiguous     bool   `json:"ambiguous"`
	Found         bool   `json:"found"`
	Reason        string `json:"reason,omitempty"`
}

// DryResolveLadder walks target.Ladder() against a single observation using
// Diagnose (no Act). The first rung with a unique exact match wins; ambiguity
// on a rung stops the walk and is reported without falling through.
func DryResolveLadder(targetName string, target workflow.Selector, o Observation) TargetDrift {
	out := TargetDrift{Target: targetName, StrategyIndex: -1, Reason: "missing"}
	ladder := target.Ladder()
	for i, rung := range ladder {
		d := Diagnose(rung, o)
		switch {
		case d.ExactMatches == 1:
			return TargetDrift{Target: targetName, StrategyIndex: i, Matches: 1, Found: true}
		case d.ExactMatches > 1:
			return TargetDrift{Target: targetName, StrategyIndex: i, Matches: d.ExactMatches, Ambiguous: true, Reason: "ambiguous"}
		}
	}
	if len(ladder) == 0 {
		out.Reason = "empty_ladder"
	}
	return out
}

// DryResolveTargets reports rung index/ambiguity for every capability target.
func DryResolveTargets(targets map[string]workflow.Selector, o Observation) []TargetDrift {
	names := make([]string, 0, len(targets))
	for name := range targets {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]TargetDrift, 0, len(names))
	for _, name := range names {
		out = append(out, DryResolveLadder(name, targets[name], o))
	}
	return out
}

// ExplainResolve turns a native matcher failure into an observation the
// engine can wait on, instead of a hard error. Diagnose is the evidence;
// Resolve remains the authority.
func ExplainResolve(err error, selector workflow.Selector, o Observation) (reason string, matches int, handled bool) {
	var brokerErr *BrokerError
	if !errors.As(err, &brokerErr) || (brokerErr.Code != "target_missing" && brokerErr.Code != "target_ambiguous") {
		return "", 0, false
	}
	if brokerErr.Code == "target_ambiguous" {
		matches = 2
	}
	d := Diagnose(selector, o)
	return brokerErr.Error() + "; match=" + d.Confidence, matches, true
}
