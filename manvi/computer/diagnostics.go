package computer

import (
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
