package devmap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

type QueryKind string

const (
	QueryExplore  QueryKind = "explore"
	QueryImpact   QueryKind = "impact"
	QueryTrace    QueryKind = "trace"
	QueryAffected QueryKind = "affected"
)

// AdvancedQuery is the bounded, typed subset of devmap's graph API exposed to
// embedding hosts. It deliberately cannot name an arbitrary subprocess command.
type AdvancedQuery struct {
	Kind          QueryKind
	Query         string
	To            string
	Targets       []string
	Depth         int
	Budget        int
	MinConfidence float64
	MinRung       string
}

// AdvancedResult preserves the producer's complete JSON envelope and pairs it
// with the observed post-query index/schema report. Advanced verifies matching
// store identity and generation before and after the query; it does not claim a
// database transaction spans the subprocess calls.
type AdvancedResult struct {
	Data  json.RawMessage `json:"data"`
	Index *Status         `json:"index"`
}

// ValidateAdvancedQuery validates host-controlled query fields without
// invoking a dependency. A zero budget is valid and means the client's
// configured default; all other bounds are complete here so every adapter can
// classify caller errors the same way.
func ValidateAdvancedQuery(q AdvancedQuery) error {
	if q.Depth < 1 || q.Depth > 64 {
		return fmt.Errorf("depth must be in 1..64, got %d", q.Depth)
	}
	if q.Budget < 0 || q.Budget > 1_000_000 {
		return fmt.Errorf("budget must be zero (configured default) or in 1..1000000, got %d", q.Budget)
	}
	if math.IsNaN(q.MinConfidence) || math.IsInf(q.MinConfidence, 0) || q.MinConfidence < 0 || q.MinConfidence > 1 {
		return fmt.Errorf("min_confidence must be in 0..1, got %g", q.MinConfidence)
	}
	if q.MinRung != "" && q.MinRung != "deterministic" && q.MinRung != "high" && q.MinRung != "speculative" {
		return fmt.Errorf("unknown min_rung %q", q.MinRung)
	}
	if len(q.Query) > 4096 || len(q.To) > 4096 {
		return fmt.Errorf("query endpoints must be at most 4096 bytes")
	}
	switch q.Kind {
	case QueryExplore:
		if q.Query == "" {
			return fmt.Errorf("explore query is empty")
		}
	case QueryImpact:
		if q.Query == "" {
			return fmt.Errorf("impact target is empty")
		}
	case QueryTrace:
		if q.Query == "" || q.To == "" {
			return fmt.Errorf("trace requires from and to")
		}
	case QueryAffected:
		if len(q.Targets) == 0 || len(q.Targets) > 128 {
			return fmt.Errorf("affected requires 1..128 targets, got %d", len(q.Targets))
		}
		for _, target := range q.Targets {
			if target == "" || len(target) > 4096 {
				return fmt.Errorf("affected targets must be 1..4096 bytes each")
			}
		}
	default:
		return fmt.Errorf("unsupported advanced query %q", q.Kind)
	}
	return nil
}

func (c *Client) Advanced(ctx context.Context, q AdvancedQuery) (AdvancedResult, error) {
	if err := ValidateAdvancedQuery(q); err != nil {
		return AdvancedResult{}, err
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	budget := q.Budget
	if budget == 0 {
		budget = c.Budget
	}
	if budget < 1 || budget > 1_000_000 {
		return AdvancedResult{}, fmt.Errorf("budget must be in 1..1000000, got %d", budget)
	}

	args := []string{string(q.Kind), "--budget", strconv.Itoa(budget), "--depth", strconv.Itoa(q.Depth)}
	switch q.Kind {
	case QueryExplore:
		args = append(args, "--min-confidence", strconv.FormatFloat(q.MinConfidence, 'g', -1, 64), "--", q.Query)
	case QueryImpact:
		if q.MinRung != "" {
			args = append(args, "--min-rung", q.MinRung)
		}
		args = append(args, "--", q.Query)
	case QueryTrace:
		if q.MinRung != "" {
			args = append(args, "--min-rung", q.MinRung)
		}
		args = append(args, "--", q.Query, q.To)
	case QueryAffected:
		args = append(args, "--min-confidence", strconv.FormatFloat(q.MinConfidence, 'g', -1, 64), "--")
		args = append(args, q.Targets...)
	}

	before, err := c.Status(ctx)
	if err != nil {
		return AdvancedResult{}, fmt.Errorf("check %s readiness: %w", q.Kind, err)
	}
	if err := validateAdvancedStatus(before, q.Kind); err != nil {
		return AdvancedResult{}, err
	}

	var data json.RawMessage
	if _, err := c.decode(ctx, &data, c.Timeout, args...); err != nil {
		return AdvancedResult{}, err
	}
	if err := validateAdvancedEnvelope(q.Kind, data); err != nil {
		return AdvancedResult{}, err
	}
	status, err := c.Status(ctx)
	if err != nil {
		return AdvancedResult{}, fmt.Errorf("qualify %s result with index status: %w", q.Kind, err)
	}
	if err := validateAdvancedStatus(status, q.Kind); err != nil {
		return AdvancedResult{}, err
	}
	if status.DBPath != before.DBPath || status.GenerationID != before.GenerationID {
		return AdvancedResult{}, fmt.Errorf("devmap index changed during %s (before db=%q generation=%d; after db=%q generation=%d); the answer cannot be qualified atomically",
			q.Kind, before.DBPath, before.GenerationID, status.DBPath, status.GenerationID)
	}
	return AdvancedResult{Data: data, Index: status}, nil
}

// ValidateAdvancedResult checks an interchangeable adapter's success value at
// the module boundary. A nil status or a payload lacking the producer's
// completeness envelope is a dependency contract failure, never ok:true.
func ValidateAdvancedResult(kind QueryKind, result AdvancedResult) error {
	if err := validateAdvancedStatus(result.Index, kind); err != nil {
		return fmt.Errorf("devmap %s adapter returned incompatible index status: %w", kind, err)
	}
	return validateAdvancedEnvelope(kind, result.Data)
}

func validateAdvancedEnvelope(kind QueryKind, data json.RawMessage) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("devmap %s did not return a JSON object; completeness fields cannot be qualified", kind)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &top); err != nil || top == nil {
		return fmt.Errorf("devmap %s did not return a JSON object", kind)
	}
	var envelope json.RawMessage
	switch kind {
	case QueryExplore:
		envelope = top["definitions"]
		if !rawJSONObject(top["blast_radius"]) {
			return fmt.Errorf("devmap explore envelope lacks object blast_radius completeness data")
		}
	case QueryImpact, QueryTrace:
		envelope = trimmed
	case QueryAffected:
		envelope = top["tests"]
		if !rawJSONArray(top["targets"]) || !rawJSONObject(top["blast_radius"]) {
			return fmt.Errorf("devmap affected envelope lacks targets or blast_radius completeness data")
		}
	default:
		return fmt.Errorf("unsupported advanced query %q", kind)
	}
	var counts struct {
		Items      json.RawMessage `json:"items"`
		Resolution json.RawMessage `json:"resolution"`
		Shown      *int            `json:"shown"`
		Total      *int            `json:"total"`
		Hidden     *int            `json:"hidden"`
		Truncated  *bool           `json:"truncated"`
		TokensUsed *int            `json:"tokens_used"`
	}
	if err := json.Unmarshal(envelope, &counts); err != nil || !rawJSONArray(counts.Items) || !rawResolution(counts.Resolution) ||
		counts.Shown == nil || counts.Total == nil || counts.Hidden == nil || counts.Truncated == nil || counts.TokensUsed == nil {
		return fmt.Errorf("devmap %s envelope lacks required completeness fields items, resolution, shown, total, hidden, truncated, or tokens_used", kind)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(counts.Items, &items); err != nil {
		return fmt.Errorf("devmap %s envelope items are not an array", kind)
	}
	if *counts.Shown < 0 || *counts.Total < 0 || *counts.Hidden < 0 || *counts.Shown > *counts.Total ||
		*counts.TokensUsed < 0 || *counts.Shown != len(items) ||
		*counts.Hidden != *counts.Total-*counts.Shown || *counts.Truncated != (*counts.Hidden > 0) {
		return fmt.Errorf("devmap %s envelope carries impossible completeness counts", kind)
	}
	return nil
}

func rawJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

func rawJSONArray(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '['
}

func rawResolution(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	switch trimmed[0] {
	case '"':
		var value string
		return json.Unmarshal(trimmed, &value) == nil && strings.TrimSpace(value) != ""
	case '{':
		var value map[string]json.RawMessage
		return json.Unmarshal(trimmed, &value) == nil && len(value) > 0
	default:
		return false
	}
}

func validateAdvancedStatus(status *Status, kind QueryKind) error {
	if status == nil {
		return fmt.Errorf("devmap status is missing")
	}
	if status.HostContractVersion != 1 {
		return fmt.Errorf("devmap host contract %d is unsupported; this build requires 1", status.HostContractVersion)
	}
	// The versioned host contract owns compatibility. Schema numbers and the
	// relation string are diagnostics; treating exact equality as the contract
	// would reject a future v1 producer that explicitly advertises a compatible
	// additive reader.
	if !status.ReaderReady || !status.QueryReady {
		return fmt.Errorf("devmap query is not ready (schema_relation=%q reader_ready=%t query_ready=%t)",
			status.SchemaRelation, status.ReaderReady, status.QueryReady)
	}
	if strings.TrimSpace(status.DBPath) == "" || status.GenerationID <= 0 {
		return fmt.Errorf("devmap status lacks a concrete snapshot identity (db_path=%q generation_id=%d)",
			status.DBPath, status.GenerationID)
	}
	available, ok := status.Capabilities[string(kind)].(bool)
	if !ok || !available {
		return fmt.Errorf("devmap did not advertise capability %q; this operation is unavailable", kind)
	}
	return nil
}
