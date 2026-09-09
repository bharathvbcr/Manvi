package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// DecisionRequest is captured by the owning provider adapter while its callback
// is live. Payload is the full reviewable request, including any file changes;
// the adapter must not substitute a clipped terminal preview.
type DecisionRequest struct {
	ID                string `json:"id"`
	RequestID         string `json:"request_id"`
	RunID             string `json:"run_id"`
	OwnerID           string `json:"owner_id"`
	SessionID         string `json:"session_id"`
	ProviderThreadID  string `json:"provider_thread_id"`
	ProviderTurnID    string `json:"provider_turn_id"`
	ProtocolRequestID string `json:"protocol_request_id"`
	Kind              string `json:"kind"`
	Payload           string `json:"payload"`
	Deadline          int64  `json:"deadline"`
}

// CreateDecision hashes the exact payload before its immutable capture. The
// canonical Rust store validates its JSON, identities, live run and deadline.
func (c *Client) CreateDecision(ctx context.Context, request DecisionRequest) (json.RawMessage, error) {
	if len(request.Payload) == 0 || len(request.Payload) > 65536 || !json.Valid([]byte(request.Payload)) {
		return nil, errors.New("decision payload must be complete JSON within 64 KiB")
	}
	digest := sha256.Sum256([]byte(request.Payload))
	wire := struct {
		DecisionRequest
		ExpectedRevision int    `json:"expected_revision"`
		PayloadDigest    string `json:"payload_digest"`
	}{DecisionRequest: request, PayloadDigest: hex.EncodeToString(digest[:])}
	raw, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	return c.Workbench(ctx, "decisions.create", raw)
}

// ClaimDecision consumes permission to answer the exact retained callback once.
// A transport error is uncertain, not a reason to write a second provider reply.
// The adapter must still confirm the same callback is open before calling this.
func (c *Client) ClaimDecision(ctx context.Context, request DecisionRequest, requestID string, revision int64) (json.RawMessage, error) {
	digest := sha256.Sum256([]byte(request.Payload))
	raw, err := json.Marshal(struct {
		ID                string `json:"id"`
		RequestID         string `json:"request_id"`
		ExpectedRevision  int64  `json:"expected_revision"`
		OwnerID           string `json:"owner_id"`
		SessionID         string `json:"session_id"`
		ProviderThreadID  string `json:"provider_thread_id"`
		ProviderTurnID    string `json:"provider_turn_id"`
		ProtocolRequestID string `json:"protocol_request_id"`
		PayloadDigest     string `json:"payload_digest"`
	}{request.ID, requestID, revision, request.OwnerID, request.SessionID, request.ProviderThreadID, request.ProviderTurnID, request.ProtocolRequestID, hex.EncodeToString(digest[:])})
	if err != nil {
		return nil, err
	}
	return c.Workbench(ctx, "decisions.claim", raw)
}
