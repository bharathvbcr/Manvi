package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	mapclient "github.com/bharathvbcr/Manvi/manvi/dc/devmap"
)

// DevmapClient is the narrow code-intelligence service a host module needs.
// A host can supply the stock subprocess client or its own in-process adapter.
type DevmapClient interface {
	Status(context.Context) (*mapclient.Status, error)
	Advanced(context.Context, mapclient.AdvancedQuery) (mapclient.AdvancedResult, error)
}

// DevmapModule exposes deep code-intelligence queries on the host plane.
type DevmapModule struct {
	Client DevmapClient
}

func (m DevmapModule) Configure(r *Router) error {
	if nilInterface(m.Client) {
		return errors.New("devmap module has no client")
	}
	if err := r.Register(OpDevmapStatus, m.status); err != nil {
		return err
	}
	return r.Register(OpDevmapQuery, m.query)
}

func (m DevmapModule) status(ctx context.Context, raw json.RawMessage) (any, *Error) {
	if len(raw) != 0 {
		var params map[string]json.RawMessage
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, badRequest("devmap.status params: %v", err)
		}
		if len(params) != 0 {
			return nil, badRequest("devmap.status takes no params")
		}
	}
	status, err := m.Client.Status(ctx)
	if err != nil {
		return nil, dependencyError("devmap status", err)
	}
	if status == nil {
		return nil, dependencyError("devmap status", errors.New("adapter returned no status"))
	}
	return status, nil
}

type devmapQueryParams struct {
	Kind          mapclient.QueryKind `json:"kind"`
	Query         string              `json:"query,omitempty"`
	To            string              `json:"to,omitempty"`
	Targets       []string            `json:"targets,omitempty"`
	Depth         int                 `json:"depth"`
	Budget        int                 `json:"budget,omitempty"`
	MinConfidence float64             `json:"min_confidence,omitempty"`
	MinRung       string              `json:"min_rung,omitempty"`
}

func (m DevmapModule) query(ctx context.Context, raw json.RawMessage) (any, *Error) {
	var p devmapQueryParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, badRequest("devmap.query params: %v", err)
	}
	query := mapclient.AdvancedQuery{
		Kind: p.Kind, Query: p.Query, To: p.To, Targets: p.Targets,
		Depth: p.Depth, Budget: p.Budget, MinConfidence: p.MinConfidence, MinRung: p.MinRung,
	}
	if err := mapclient.ValidateAdvancedQuery(query); err != nil {
		return nil, badRequest("devmap.query params: %v", err)
	}
	result, err := m.Client.Advanced(ctx, query)
	if err != nil {
		return nil, dependencyError("devmap query", err)
	}
	if err := mapclient.ValidateAdvancedResult(query.Kind, result); err != nil {
		return nil, dependencyError("devmap query", err)
	}
	return result, nil
}

func dependencyError(operation string, err error) *Error {
	retryable := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
	return &Error{Code: ErrDependency, Message: fmt.Sprintf("%s unavailable: %v", operation, err), Retryable: retryable}
}
