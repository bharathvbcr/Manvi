// Package budget reserves a conservative charge before every HTTP attempt.
// Lost responses keep reservations; only authoritative usage or proven no-send
// can reduce them. A ledger governs its own campaign, not an account balance.
package budget

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/transport"
)

type Prices struct {
	InputNanoUSD    int64  `json:"input_nano_usd"`
	OutputNanoUSD   int64  `json:"output_nano_usd"`
	MaxInputTokens  int    `json:"max_input_tokens"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	Revision        string `json:"revision"`
}
type Attempt struct {
	ID              uint64     `json:"id"`
	State           string     `json:"state"`
	ReservedNanoUSD int64      `json:"reserved_nano_usd"`
	ChargedNanoUSD  int64      `json:"charged_nano_usd"`
	Usage           *llm.Usage `json:"usage,omitempty"`
}
type Snapshot struct {
	SchemaVersion int       `json:"schema_version"`
	LimitNanoUSD  int64     `json:"limit_nano_usd"`
	Prices        Prices    `json:"prices"`
	Attempts      []Attempt `json:"attempts"`
}
type Ledger struct {
	mu     sync.Mutex
	path   string
	lock   *os.File
	data   Snapshot
	closed bool
}

func Open(path string, limit int64, prices Prices) (*Ledger, error) {
	if path == "" || limit <= 0 || limit > 1_000_000_000_000 || prices.InputNanoUSD <= 0 || prices.InputNanoUSD > 1_000_000 || prices.OutputNanoUSD <= 0 || prices.OutputNanoUSD > 1_000_000 || prices.MaxInputTokens <= 0 || prices.MaxInputTokens > 10_000_000 || prices.MaxOutputTokens <= 0 || prices.MaxOutputTokens > 1_000_000 || prices.Revision == "" {
		return nil, errors.New("invalid campaign price bounds")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("campaign is locked; inspect its owner before recovering a stale lock: %w", err)
	}
	l := &Ledger{path: path, lock: lock, data: Snapshot{1, limit, prices, []Attempt{}}}
	b, err := readLedger(path)
	if err == nil {
		if len(b) > 4<<20 {
			l.Close()
			return nil, errors.New("campaign ledger exceeds4 MiB")
		}
		decoder := json.NewDecoder(bytes.NewReader(b))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&l.data); err != nil {
			l.Close()
			return nil, err
		}
		if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
			l.Close()
			return nil, errors.New("trailing campaign ledger data")
		}
		if l.data.SchemaVersion != 1 || l.data.LimitNanoUSD != limit || l.data.Prices != prices {
			l.Close()
			return nil, errors.New("campaign configuration differs from existing ledger")
		}
		if err := validateSnapshot(l.data); err != nil {
			l.Close()
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		l.Close()
		return nil, err
	}
	return l, nil
}
func (l *Ledger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	err := l.lock.Close()
	remove := os.Remove(l.path + ".lock")
	if err != nil {
		return err
	}
	return remove
}
func (l *Ledger) save(next Snapshot) error {
	if err := validateSnapshot(next); err != nil {
		return err
	}
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(l.path), ".campaign-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, l.path); err != nil {
		return err
	}
	l.data = next
	return nil
}
func (l *Ledger) Snapshot() Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return copySnapshot(l.data)
}
func (l *Ledger) reserve(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if l.closed {
		return 0, errors.New("campaign closed")
	}
	if len(l.data.Attempts) >= 4096 {
		return 0, errors.New("campaign attempt count exceeded")
	}
	p := l.data.Prices
	reserve := int64(p.MaxInputTokens)*p.InputNanoUSD + int64(p.MaxOutputTokens)*p.OutputNanoUSD
	used := int64(0)
	for _, a := range l.data.Attempts {
		used += a.ReservedNanoUSD + a.ChargedNanoUSD
	}
	if reserve > l.data.LimitNanoUSD-used {
		return 0, errors.New("campaign budget exhausted by charges and unresolved reservations")
	}
	id := uint64(len(l.data.Attempts) + 1)
	next := copySnapshot(l.data)
	next.Attempts = append(next.Attempts, Attempt{ID: id, State: "reserved", ReservedNanoUSD: reserve})
	if err := l.save(next); err != nil {
		return 0, err
	}
	return id, nil
}
func (l *Ledger) transportResult(id uint64, r transport.AttemptResult) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errors.New("campaign closed")
	}
	if id == 0 || id > uint64(len(l.data.Attempts)) {
		return errors.New("no provider attempt for transport result")
	}
	next := copySnapshot(l.data)
	a := &next.Attempts[id-1]
	if a.State != "reserved" {
		return errors.New("attempt already has a transport result")
	}
	switch r.State {
	case transport.AttemptNotSent, transport.AttemptAccepted, transport.AttemptRejected, transport.AttemptUnknown:
	default:
		return errors.New("invalid transport attempt state")
	}
	a.State = string(r.State)
	if r.State == transport.AttemptNotSent {
		a.ReservedNanoUSD = 0
	}
	return l.save(next)
}
func (l *Ledger) settle(id uint64, u llm.Usage) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errors.New("campaign closed")
	}
	if id == 0 || id > uint64(len(l.data.Attempts)) {
		return errors.New("no provider attempt to settle")
	}
	charge, err := usageCharge(l.data.Prices, u)
	if err != nil {
		return err
	}
	next := copySnapshot(l.data)
	a := &next.Attempts[id-1]
	if a.State != "accepted" {
		return errors.New("only an accepted attempt can settle")
	}
	a.Usage = &u
	a.ChargedNanoUSD = charge
	a.ReservedNanoUSD = 0
	a.State = "settled"
	return l.save(next)
}

type Provider struct {
	Inner  llm.Provider
	Ledger *Ledger
}

func (p *Provider) Name() string                                   { return p.Inner.Name() }
func (p *Provider) Capability(model string) (llm.Capability, bool) { return p.Inner.Capability(model) }
func (p *Provider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	if ctx == nil {
		return nil, errors.New("provider context required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.Inner == nil {
		return nil, errors.New("inner provider required")
	}
	gated, ok := p.Inner.(transport.AttemptGatedProvider)
	if !ok || !gated.AttemptGateSupported() {
		return nil, errors.New("provider does not support transport attempt admission")
	}

	if p.Ledger == nil {
		return nil, errors.New("campaign ledger required")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	var mu sync.Mutex
	var last uint64
	gate := func(ctx context.Context, _ transport.AttemptInfo) (func(transport.AttemptResult) error, error) {
		id, err := p.Ledger.reserve(ctx)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		last = id
		mu.Unlock()
		return func(r transport.AttemptResult) error { return p.Ledger.transportResult(id, r) }, nil
	}
	stream, err := p.Inner.Stream(transport.WithAttemptGate(ctx, gate), req)
	if err != nil {
		cancel()
		return nil, err
	}
	return &budgetStream{ctx: ctx, inner: stream, cancel: cancel, settle: func(u llm.Usage) error { mu.Lock(); id := last; mu.Unlock(); return p.Ledger.settle(id, u) }}, nil
}

type budgetStream struct {
	ctx    context.Context
	inner  llm.Stream
	cancel context.CancelFunc
	settle func(llm.Usage) error
	once   sync.Once
	err    error
}

func (s *budgetStream) Next() (llm.Chunk, error) {
	if err := s.ctx.Err(); err != nil {
		return llm.Chunk{}, err
	}
	chunk, err := s.inner.Next()
	if cancelled := s.ctx.Err(); cancelled != nil {
		return llm.Chunk{}, cancelled
	}
	return chunk, err
}
func (s *budgetStream) Response() (llm.Response, error) {
	if err := s.ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	r, err := s.inner.Response()
	if cancelled := s.ctx.Err(); cancelled != nil {
		return llm.Response{}, cancelled
	}
	if err != nil {
		return r, err
	}
	s.once.Do(func() { s.err = s.settle(r.Usage) })
	if s.err != nil {
		return r, s.err
	}
	return r, nil
}
func (s *budgetStream) Close() error { s.cancel(); return s.inner.Close() }
