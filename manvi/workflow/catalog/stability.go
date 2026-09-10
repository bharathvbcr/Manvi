package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Stability summarizes N-run qualification confidence for a catalog entry.
// Rates are in [0,1]. MeanRungIndex and RecoveriesPerRun default to 0 when a
// report does not yet carry locator/recovery counters (thin qualify wiring).
type Stability struct {
	SuccessRate      float64 `json:"success_rate"`
	FirstAttemptRate float64 `json:"first_attempt_rate"`
	MeanRungIndex    float64 `json:"mean_rung_index"`
	RecoveriesPerRun float64 `json:"recoveries_per_run"`
	Runs             int     `json:"runs,omitempty"`
	Source           string  `json:"source,omitempty"`
}

// QualifyCampaign is the subset of Jarvis cmd/qualify report JSON used to
// compute Stability. Unknown fields are ignored so catalog stays decoupled.
type QualifyCampaign struct {
	Passed                int            `json:"passed"`
	Failed                int            `json:"failed"`
	Blocked               int            `json:"blocked"`
	FirstAttemptSuccesses int            `json:"first_attempt_successes"`
	Results               []QualifyTrial `json:"results"`
}

// QualifyTrial carries per-run counters. RungIndexes and Recoveries are optional
// until qualify records locator hits and recovery applications.
type QualifyTrial struct {
	Outcome            string `json:"outcome"`
	ObservationRetries int    `json:"observation_retries"`
	InputRetries       int    `json:"input_retries"`
	Recoveries         int    `json:"recoveries,omitempty"`
	RungIndexes        []int  `json:"rung_indexes,omitempty"`
}

// StabilityFromQualify derives Stability from an N-run qualify campaign report.
func StabilityFromQualify(r QualifyCampaign) (Stability, error) {
	runs := len(r.Results)
	if runs == 0 {
		runs = r.Passed + r.Failed + r.Blocked
	}
	if runs <= 0 {
		return Stability{}, errors.New("qualify report has no runs")
	}
	if r.Passed < 0 || r.Failed < 0 || r.Blocked < 0 || r.FirstAttemptSuccesses < 0 {
		return Stability{}, errors.New("qualify report counts must be non-negative")
	}
	if r.FirstAttemptSuccesses > r.Passed {
		return Stability{}, errors.New("first-attempt successes exceed passed count")
	}
	total := r.Passed + r.Failed + r.Blocked
	if total == 0 {
		total = runs
	}
	st := Stability{
		SuccessRate:      float64(r.Passed) / float64(total),
		FirstAttemptRate: float64(r.FirstAttemptSuccesses) / float64(total),
		Runs:             runs,
		Source:           "qualify",
	}
	var rungSum float64
	var rungN int
	var recoverySum float64
	for _, trial := range r.Results {
		recoverySum += float64(trial.Recoveries)
		for _, idx := range trial.RungIndexes {
			if idx < 0 {
				return Stability{}, fmt.Errorf("negative rung index %d", idx)
			}
			rungSum += float64(idx)
			rungN++
		}
	}
	if len(r.Results) > 0 {
		st.RecoveriesPerRun = recoverySum / float64(len(r.Results))
	}
	if rungN > 0 {
		st.MeanRungIndex = rungSum / float64(rungN)
	}
	return st, nil
}

// ParseQualifyReport decodes qualify campaign JSON into Stability.
func ParseQualifyReport(raw []byte) (Stability, error) {
	var campaign QualifyCampaign
	if err := json.Unmarshal(raw, &campaign); err != nil {
		return Stability{}, err
	}
	return StabilityFromQualify(campaign)
}

// SetStability attaches a stability summary to the current promotion pointer
// for the matching immutable revision. The revision must already be recorded
// in current.json (approved or revoked).
func (s Store) SetStability(id, revision, sha string, st Stability) error {
	if !segment.MatchString(id) || !segment.MatchString(revision) || revision == "current" {
		return errors.New("invalid catalog identity")
	}
	if sha == "" {
		return errors.New("stability requires revision digest")
	}
	if err := validateStability(st); err != nil {
		return err
	}
	root, err := s.open(false)
	if err != nil {
		return err
	}
	defer root.Close()
	dir, err := openChild(root, id, false)
	if err != nil {
		return err
	}
	defer dir.Close()
	raw, err := readRegular(dir, "current.json")
	if err != nil {
		return err
	}
	var current Entry
	if err := json.Unmarshal(raw, &current); err != nil {
		return err
	}
	normalizeStatus(&current)
	if current.ID != id || current.Revision != revision || current.SHA256 != sha {
		return errors.New("stability does not match current catalog pointer")
	}
	copy := st
	current.Stability = &copy
	metadata, err := json.Marshal(current)
	if err != nil {
		return err
	}
	return publish(dir, "current.json", metadata, true)
}

// ApplyQualifyReport parses a qualify N-run report and stores its Stability on
// the matching current catalog pointer.
func (s Store) ApplyQualifyReport(id, revision, sha string, report []byte) (Stability, error) {
	st, err := ParseQualifyReport(report)
	if err != nil {
		return Stability{}, err
	}
	if err := s.SetStability(id, revision, sha, st); err != nil {
		return Stability{}, err
	}
	return st, nil
}

func validateStability(st Stability) error {
	for _, v := range []float64{st.SuccessRate, st.FirstAttemptRate} {
		if v < 0 || v > 1 {
			return errors.New("stability rates must be in [0,1]")
		}
	}
	if st.MeanRungIndex < 0 || st.RecoveriesPerRun < 0 {
		return errors.New("stability rung and recovery averages must be non-negative")
	}
	if st.Runs < 0 {
		return errors.New("stability runs must be non-negative")
	}
	return nil
}
