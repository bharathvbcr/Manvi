package budget

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/transport"
)

// PromptOverheadTokens is added to a request's byte length to bound its prompt
// in tokens. A token covers at least one byte of the JSON the request is sent
// as, so the byte length bounds what the caller wrote; the overhead covers what
// the provider adds on its side, such as the instructions it wraps tool
// definitions in.
const PromptOverheadTokens = 4096

// MaxTokensReporter is implemented by a provider that fills in an output bound
// a request left unset. Without it the model's full output ceiling is reserved.
type MaxTokensReporter interface {
	AppliedMaxTokens(req llm.Request) int
}

// Provider meters an inner provider against a Ledger.
type Provider struct {
	Inner  llm.Provider
	Ledger *Ledger
}

func (p *Provider) Name() string                                   { return p.Inner.Name() }
func (p *Provider) Capability(model string) (llm.Capability, bool) { return p.Inner.Capability(model) }

// ReplayableOn forwards the inner provider's rule, so metering a provider never
// changes which reasoning it is handed back.
func (p *Provider) ReplayableOn(fromModel, toModel string) bool {
	if r, ok := p.Inner.(llm.ReasoningReplayer); ok {
		return r.ReplayableOn(fromModel, toModel)
	}
	return fromModel == toModel
}

// Stream reserves before every HTTP attempt the inner provider makes, retries
// included, and settles the attempt that answered from its reported usage.
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
	if p.Ledger == nil {
		return nil, errors.New("budget ledger required")
	}
	gated, ok := p.Inner.(transport.AttemptGatedProvider)
	if !ok || !gated.AttemptGateSupported() {
		return nil, fmt.Errorf("budget: provider %s does not route every attempt through the admission gate", p.Inner.Name())
	}
	pricer, ok := p.Inner.(Pricer)
	if !ok {
		return nil, fmt.Errorf("budget: provider %s publishes no prices", p.Inner.Name())
	}
	prices, ok := pricer.PriceOf(req.Model)
	if !ok {
		return nil, fmt.Errorf("budget: no price for %s model %q; it cannot be metered", p.Inner.Name(), req.Model)
	}
	capability, ok := p.Inner.Capability(req.Model)
	if !ok || capability.ContextWindow <= 0 || capability.MaxOutputTokens <= 0 {
		return nil, fmt.Errorf("budget: %s model %q has no known limits to bound a reservation", p.Inner.Name(), req.Model)
	}
	outputBound := capability.MaxOutputTokens
	if reporter, ok := p.Inner.(MaxTokensReporter); ok {
		outputBound = reporter.AppliedMaxTokens(req)
	} else if req.MaxTokens > 0 {
		outputBound = req.MaxTokens
	}

	rejector, _ := p.Inner.(UnbilledRejector)
	var mu sync.Mutex
	var last uint64
	gate := func(ctx context.Context, info transport.AttemptInfo) (func(transport.AttemptResult) error, error) {
		promptBound := min(info.RequestBytes+PromptOverheadTokens, capability.ContextWindow)
		amount, err := prices.Reserve(promptBound, outputBound)
		if err != nil {
			return nil, err
		}
		id, err := p.Ledger.reserve(ctx, req.Model, amount)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		last = id
		mu.Unlock()
		return func(r transport.AttemptResult) error {
			return p.Ledger.transportResult(id, r, r.State == transport.AttemptRejected && rejector != nil && rejector.RejectionUnbilled(r.Status))
		}, nil
	}
	lastID := func() uint64 { mu.Lock(); defer mu.Unlock(); return last }

	stream, err := p.Inner.Stream(transport.WithAttemptGate(ctx, gate), req)
	if err != nil {
		// An attempt the server accepted before the adapter gave up was still
		// sent; it keeps its reservation.
		if id := lastID(); id != 0 {
			err = errors.Join(err, p.Ledger.retain(id))
		}
		return nil, err
	}
	return &budgetStream{ctx: ctx, inner: stream, prices: prices, outputBound: outputBound, ledger: p.Ledger, id: lastID()}, nil
}

type budgetStream struct {
	ctx         context.Context
	inner       llm.Stream
	prices      Prices
	outputBound int
	ledger      *Ledger
	id          uint64

	once sync.Once
	err  error
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

// resolve settles the attempt from usage, or retains its reservation when no
// trustworthy usage exists. It runs once, whichever of Response and Close
// reaches it first.
func (s *budgetStream) resolve(usage *llm.Usage) error {
	s.once.Do(func() {
		if usage == nil {
			s.err = s.ledger.retainAt(s.id, s.partialBound())
			return
		}
		charge, err := s.prices.Charge(*usage)
		if err != nil {
			s.err = errors.Join(fmt.Errorf("%w; its reservation is kept", err), s.ledger.retain(s.id))
			return
		}
		s.err = s.ledger.settle(s.id, charge)
	})
	return s.err
}

// partialBound bounds an unsettled attempt by the prompt usage its stream
// reported before it ended, or returns -1 to keep the full reservation when
// the stream reported none.
func (s *budgetStream) partialBound() int64 {
	reporter, ok := s.inner.(UsageReporter)
	if !ok {
		return -1
	}
	u, ok := reporter.UsageSoFar()
	if !ok {
		return -1
	}
	bound, err := s.prices.Bound(u, s.outputBound)
	if err != nil {
		return -1
	}
	return bound
}

func (s *budgetStream) Response() (llm.Response, error) {
	if err := s.ctx.Err(); err != nil {
		return llm.Response{}, errors.Join(err, s.resolve(nil))
	}
	r, err := s.inner.Response()
	if cancelled := s.ctx.Err(); cancelled != nil {
		return llm.Response{}, errors.Join(cancelled, s.resolve(nil))
	}
	if err != nil {
		return r, errors.Join(err, s.resolve(nil))
	}
	if err := s.resolve(&r.Usage); err != nil {
		return r, err
	}
	return r, nil
}

// Close releases the stream. One closed before its response settled keeps its
// reservation: the server may have generated, and billed, all of it.
func (s *budgetStream) Close() error {
	return errors.Join(s.inner.Close(), s.resolve(nil))
}
