package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"
)

const maxWorkbenchInput = 256 << 10
const maxWorkbenchReply = 2 << 20

// WorkbenchMethods is the profile API this client understands. It excludes
// repository-local execution leases, permissions and provider operations.
func WorkbenchMethods() []string {
	return []string{
		"workspaces.list", "workspaces.get", "workspaces.put", "workspaces.delete",
		"repositories.list", "repositories.get", "repositories.put",
		"items.list", "items.get", "items.brief.get", "items.put", "items.delete", "items.history",
		"events.list",
		"attention.list", "attention.get", "attention.update",
		"notifications.settings.get", "notifications.settings.put", "notifications.pending.list",
		"notifications.delivery.get", "notifications.claim", "notifications.finish",
		"notifications.activate", "notifications.activations.list", "notifications.ack",
		"decisions.create", "decisions.decide", "decisions.claim", "decisions.resolve", "decisions.get", "decisions.list",
		"enhancements.create", "enhancements.complete", "enhancements.get", "enhancements.list",
		"enhancements.accept", "enhancements.dismiss", "enhancements.undo",
		"enhancements.claim", "enhancements.recover", "enhancements.revise",
		"automation.get", "automation.put", "automation.list", "automation.prepare",
		"runs.prepare", "runs.claim", "runs.started", "runs.protocol", "runs.finish", "runs.cancel", "runs.get", "runs.list",
	}
}

// WorkbenchError is a refusal from the canonical transactional store. Conflicts
// remain distinguishable from unavailable storage and uncertain transport loss.
type WorkbenchError struct {
	Code    string
	Message string
}

func (e *WorkbenchError) Error() string { return "workbench: " + e.Code + ": " + e.Message }

// Workbench uses this client's existing lazy, bounded process pool. Hosts create
// a separate Client for their profile database, never reuse the lease client.
// Mutations carry request_id and expected_revision; transport failures are not
// retried here because the caller must retain the same idempotency identity.
func (c *Client) Workbench(ctx context.Context, method string, input json.RawMessage) (json.RawMessage, error) {
	if !slices.Contains(WorkbenchMethods(), method) {
		return nil, &WorkbenchError{Code: "unknown_method", Message: "unsupported workbench method"}
	}
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	if len(input) > maxWorkbenchInput || !utf8.Valid(input) || !json.Valid(input) || bytes.TrimSpace(input)[0] != '{' {
		return nil, &WorkbenchError{Code: "invalid_input", Message: "request must be a JSON object of at most 256 KiB"}
	}
	out, err := c.run(ctx, "work", "--method", method, "--input", string(input))
	if err != nil {
		return nil, err
	}
	if len(out.Workbench) == 0 {
		return nil, errors.New("store: work returned no validated result")
	}
	var wire workbenchReply
	if err := json.Unmarshal(out.Workbench, &wire); err != nil {
		return nil, fmt.Errorf("store: work result: %w", err)
	}
	listing := strings.HasSuffix(method, ".list") || method == "items.history"
	if listing != (wire.Items != nil) {
		return nil, errors.New("store: work returned a different result shape than requested")
	}
	if !listing && !strings.HasSuffix(method, ".get") {
		if wire.Sequence == nil || *wire.Sequence <= 0 {
			return nil, errors.New("store: work mutation has no committed event sequence")
		}
	}
	return out.Workbench, nil
}

type workbenchReply struct {
	OK         *bool             `json:"ok"`
	Code       string            `json:"code"`
	Error      string            `json:"error"`
	Item       json.RawMessage   `json:"item"`
	Items      []json.RawMessage `json:"items"`
	Total      *int64            `json:"total"`
	Shown      *int64            `json:"shown"`
	HasMore    *bool             `json:"has_more"`
	NextCursor json.RawMessage   `json:"next_cursor"`
	Sequence   *int64            `json:"sequence"`
}

// decodeWorkbenchReply runs only after the shared decoder has checked process
// exit and truncation. Positive identification prevents {} or a lease response
// from turning into an empty-but-healthy board through zero-value decoding.
func decodeWorkbenchReply(raw []byte) (json.RawMessage, error) {
	if len(raw) > maxWorkbenchReply {
		return nil, fmt.Errorf("store: work %w", errOversize)
	}
	if !utf8.Valid(raw) {
		return nil, errors.New("store: work result contains invalid UTF-8")
	}
	if err := uniqueWorkbenchFields(raw); err != nil {
		return nil, err
	}
	var wire workbenchReply
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("store: invalid work result: %w", err)
	}
	if wire.OK == nil {
		return nil, errors.New("store: work result has no explicit outcome")
	}
	if !*wire.OK {
		if wire.Code == "" || strings.TrimSpace(wire.Error) == "" {
			return nil, errors.New("store: work refusal has no code or reason")
		}
		return nil, &WorkbenchError{Code: wire.Code, Message: wire.Error}
	}
	if wire.Code != "" || wire.Error != "" {
		return nil, errors.New("store: contradictory work result")
	}
	if wire.Items != nil {
		if len(wire.Item) != 0 || wire.Total == nil || wire.Shown == nil || wire.HasMore == nil ||
			*wire.Shown != int64(len(wire.Items)) || *wire.Total < *wire.Shown || len(wire.Items) > 200 || len(wire.NextCursor) == 0 {
			return nil, errors.New("store: work page has incomplete or inconsistent counts")
		}
		if *wire.HasMore && (len(wire.Items) == 0 || bytes.Equal(wire.NextCursor, []byte("null"))) {
			return nil, errors.New("store: work page cannot advance its cursor")
		}
		for _, item := range wire.Items {
			if len(item) == 0 || bytes.TrimSpace(item)[0] != '{' {
				return nil, errors.New("store: work page contains a non-object")
			}
		}
	} else {
		var identity struct {
			ID       string `json:"id"`
			Revision int64  `json:"revision"`
		}
		if len(wire.Item) == 0 || json.Unmarshal(wire.Item, &identity) != nil || identity.ID == "" || identity.Revision <= 0 {
			return nil, errors.New("store: work result has no versioned record")
		}
	}
	return bytes.Clone(raw), nil
}

// Use encoding/json for syntax and escaping, but refuse duplicate envelope
// fields instead of letting different consumers disagree on first/last wins.
func uniqueWorkbenchFields(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("store: work result is not an object")
	}
	seen := make(map[string]bool)
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return fmt.Errorf("store: work field: %w", err)
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return errors.New("store: work result has duplicate or invalid fields")
		}
		seen[key] = true
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return fmt.Errorf("store: work field value: %w", err)
		}
	}
	if _, err := d.Token(); err != nil {
		return fmt.Errorf("store: work object: %w", err)
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return errors.New("store: work result has trailing data")
	}
	return nil
}
