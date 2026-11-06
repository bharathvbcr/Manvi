package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	mapclient "github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/devmap"
)

const maxWarmDevmapPoolEntries = 16

// WarmDevmapPool maintains a bounded per-root pool of warm `devmap mcp` stdio
// clients (`P0.3`). Up to maxWarmDevmapPoolEntries canonical repository roots
// keep persistent warm sessions; any overflow root receives an unpooled CLI
// client rather than evicting a session that may have an RPC in flight.
type WarmDevmapPool struct {
	binary  string
	mu      sync.Mutex
	closed  bool
	clients map[string]*mapclient.Client
}

// NewWarmDevmapPool builds a bounded warm `devmap mcp` client pool for binary.
func NewWarmDevmapPool(binary string) *WarmDevmapPool {
	return &WarmDevmapPool{
		binary:  binary,
		clients: make(map[string]*mapclient.Client),
	}
}

// ForRoot returns a warm `devmap` client scoped to root (canonicalized when
// resolvable).
func (p *WarmDevmapPool) ForRoot(root string) *mapclient.Client {
	canon := root
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != "" {
		canon = resolved
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return mapclient.New(p.binary, canon)
	}
	if existing, ok := p.clients[canon]; ok {
		return existing
	}
	c := mapclient.New(p.binary, canon)
	if len(p.clients) < maxWarmDevmapPoolEntries {
		c.EnableWarmMCP()
		p.clients[canon] = c
	}
	return c
}

// Close terminates every pooled warm `devmap mcp` subprocess.
func (p *WarmDevmapPool) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	toClose := make([]*mapclient.Client, 0, len(p.clients))
	for _, c := range p.clients {
		toClose = append(toClose, c)
	}
	p.clients = nil
	p.mu.Unlock()

	var joined error
	for _, c := range toClose {
		joined = errors.Join(joined, c.Close())
	}
	return joined
}

// DevmapClient is the narrow code-intelligence service a host module needs.
// A host can supply the stock subprocess client or its own in-process adapter.
type DevmapClient interface {
	Status(context.Context) (*mapclient.Status, error)
	Advanced(context.Context, mapclient.AdvancedQuery) (mapclient.AdvancedResult, error)
}

// DevmapModule exposes deep code-intelligence queries on the host plane. Every
// request names the repository it is about; ForRoot builds the client for that
// validated root. There is deliberately no client bound at startup: a sidecar's
// working directory is wherever its host launched it, and an answer about that
// tree would be indistinguishable from an answer about the right one.
type DevmapModule struct {
	ForRoot func(root string) DevmapClient
}

func (m DevmapModule) Configure(r *Router) error {
	if m.ForRoot == nil {
		return errors.New("devmap module has no client source")
	}
	if err := r.Register(OpDevmapStatus, m.status); err != nil {
		return err
	}
	return r.Register(OpDevmapQuery, m.query)
}

// client validates the request's root and returns the client bound to it.
func (m DevmapModule) client(ctx context.Context, op, raw string) (DevmapClient, string, *Error) {
	root, e := canonicalRepoRoot(ctx, "root", raw)
	if e != nil {
		return nil, "", e
	}
	client := m.ForRoot(root)
	if nilInterface(client) {
		return nil, "", dependencyError(op, errors.New("no client for this root"))
	}
	return client, root, nil
}

type devmapStatusParams struct {
	Root string `json:"root"`
}

func (m DevmapModule) status(ctx context.Context, raw json.RawMessage) (any, *Error) {
	var p devmapStatusParams
	if e := scanStrict(raw, &p); e != nil {
		return nil, badRequest("devmap.status params: %v", e.Message)
	}
	client, _, e := m.client(ctx, "devmap status", p.Root)
	if e != nil {
		return nil, e
	}
	status, err := client.Status(ctx)
	if err != nil {
		return nil, dependencyError("devmap status", err)
	}
	if status == nil {
		return nil, dependencyError("devmap status", errors.New("adapter returned no status"))
	}
	return status, nil
}

type devmapQueryParams struct {
	Root          string              `json:"root"`
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
	client, root, e := m.client(ctx, "devmap query", p.Root)
	if e != nil {
		return nil, e
	}
	query := mapclient.AdvancedQuery{
		Kind: p.Kind, Query: p.Query, To: p.To, Targets: p.Targets,
		Depth: p.Depth, Budget: p.Budget, MinConfidence: p.MinConfidence, MinRung: p.MinRung,
	}
	if err := mapclient.ValidateAdvancedQuery(query); err != nil {
		return nil, badRequest("devmap.query params: %v", err)
	}
	result, err := client.Advanced(ctx, query)
	if err != nil {
		return nil, dependencyError("devmap query", err)
	}
	if err := mapclient.ValidateAdvancedResult(query.Kind, result); err != nil {
		return nil, dependencyError("devmap query", err)
	}
	if err := envelopeRootMatches(result.Data, root); err != nil {
		return nil, dependencyError("devmap query", err)
	}
	return result, nil
}

// envelopeRootMatches refuses an answer whose envelope names a different
// repository than the one asked about. An envelope that names none is not
// refused: not every query kind carries repository.root. One whose repository
// field is present but unreadable is refused, because "could not read the
// root" must not pass as "no root to check".
func envelopeRootMatches(data json.RawMessage, root string) error {
	var env struct {
		Repository *struct {
			Root string `json:"root"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("the answer's repository field cannot be read: %w", err)
	}
	if env.Repository == nil || env.Repository.Root == "" {
		return nil
	}
	got := env.Repository.Root
	if resolved, err := filepath.EvalSymlinks(got); err == nil {
		got = resolved
	}
	if got != root {
		return fmt.Errorf("the answer is about %s, not the requested root %s", env.Repository.Root, root)
	}
	return nil
}

func dependencyError(operation string, err error) *Error {
	retryable := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
	return &Error{Code: ErrDependency, Message: fmt.Sprintf("%s unavailable: %v", operation, err), Retryable: retryable}
}
