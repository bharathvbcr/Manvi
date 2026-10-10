// Package budget caps what a provider account can spend, across every model and
// every concurrent session that shares one ledger file.
//
// Every HTTP attempt reserves a conservative bound before it is sent. Only two
// things reduce a reservation: proof that nothing was sent, and authoritative
// usage from the provider. Everything else — a lost stream, a rejected or
// unclassifiable attempt, a crash — keeps it, because the provider may have
// billed it. The ledger therefore over-reports spend rather than under-reports
// it, and it governs its own tally, not the account balance the provider holds:
// spend made outside this ledger is invisible to it.
package budget

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/llm/transport"
)

const (
	schemaVersion = 2
	// maxInFlight bounds concurrent unresolved attempts, which is also the
	// bound on what a crash can leave behind in the file.
	maxInFlight = 256
	// maxLedgerBytes rejects a file no valid ledger could grow to: settled
	// attempts are folded into totals, so size tracks only what is in flight.
	maxLedgerBytes = 1 << 20
	// resolveTimeout bounds the ledger write that resolves an attempt. Those
	// writes run after the request's own context may be gone, and a reservation
	// must never be refunded merely because a deadline elapsed — a write that
	// times out leaves the reservation in place.
	resolveTimeout = 30 * time.Second
)

// lockWait bounds how long one ledger operation waits for another to release
// the file. A variable so a test can shorten it.
var lockWait = resolveTimeout

const (
	stateReserved = "reserved" // admitted, HTTP outcome not yet classified
	stateAccepted = "accepted" // the server answered; awaiting usage
)

// ErrExhausted reports that an attempt's reservation would exceed the limit.
var ErrExhausted = errors.New("budget exhausted")

// Attempt is one in-flight HTTP attempt holding a reservation.
type Attempt struct {
	ID              uint64 `json:"id"`
	Model           string `json:"model"`
	State           string `json:"state"`
	ReservedNanoUSD int64  `json:"reserved_nano_usd"`
	PID             int    `json:"pid"`
	StartedUnix     int64  `json:"started_unix"`
}

// Snapshot is the persisted ledger.
type Snapshot struct {
	SchemaVersion int `json:"schema_version"`
	// SpentNanoUSD is priced from authoritative provider usage.
	SpentNanoUSD int64 `json:"spent_nano_usd"`
	// RetainedNanoUSD is reservations kept because the charge could not be
	// proven either way. It is an upper bound on real spend, not a measurement.
	RetainedNanoUSD  int64 `json:"retained_nano_usd"`
	Settled          int64 `json:"settled"`
	RetainedAttempts int64 `json:"retained_attempts"`
	// RefusedAttempts counts attempts the provider rejected with a status it
	// declares unbilled; their reservations were released.
	RefusedAttempts int64 `json:"refused_attempts,omitempty"`
	// Overruns counts settled attempts whose real charge exceeded their
	// reservation. The charge is recorded in full either way; a non-zero count
	// means the reservation bound was too tight and the limit could have been
	// passed by the difference.
	Overruns int64     `json:"overruns"`
	NextID   uint64    `json:"next_id"`
	InFlight []Attempt `json:"in_flight"`
}

// InFlightNanoUSD totals the reservations still awaiting resolution.
func (s Snapshot) InFlightNanoUSD() int64 {
	var total int64
	for _, a := range s.InFlight {
		total += a.ReservedNanoUSD
	}
	return total
}

// CommittedNanoUSD is everything counted against the limit.
func (s Snapshot) CommittedNanoUSD() int64 {
	return s.SpentNanoUSD + s.RetainedNanoUSD + s.InFlightNanoUSD()
}

func (s Snapshot) clone() Snapshot {
	s.InFlight = append([]Attempt(nil), s.InFlight...)
	return s
}

func (s Snapshot) find(id uint64) int {
	for i, a := range s.InFlight {
		if a.ID == id {
			return i
		}
	}
	return -1
}

func (s *Snapshot) remove(i int) Attempt {
	a := s.InFlight[i]
	s.InFlight = append(s.InFlight[:i], s.InFlight[i+1:]...)
	return a
}

func validate(s Snapshot) error {
	if s.SchemaVersion != schemaVersion {
		return fmt.Errorf("budget: ledger schema %d, want %d", s.SchemaVersion, schemaVersion)
	}
	if s.SpentNanoUSD < 0 || s.RetainedNanoUSD < 0 || s.Settled < 0 || s.RetainedAttempts < 0 || s.RefusedAttempts < 0 || s.Overruns < 0 {
		return errors.New("budget: negative ledger total")
	}
	if len(s.InFlight) > maxInFlight {
		return fmt.Errorf("budget: %d in-flight attempts exceeds %d", len(s.InFlight), maxInFlight)
	}
	total := s.SpentNanoUSD
	if total > math.MaxInt64-s.RetainedNanoUSD {
		return errors.New("budget: ledger total overflows")
	}
	total += s.RetainedNanoUSD
	seen := map[uint64]bool{}
	for _, a := range s.InFlight {
		if a.ID == 0 || a.ID >= s.NextID || seen[a.ID] {
			return fmt.Errorf("budget: invalid or duplicate attempt id %d", a.ID)
		}
		seen[a.ID] = true
		if a.State != stateReserved && a.State != stateAccepted {
			return fmt.Errorf("budget: attempt %d has invalid state %q", a.ID, a.State)
		}
		if a.ReservedNanoUSD <= 0 || a.Model == "" {
			return fmt.Errorf("budget: attempt %d must name its model and hold a reservation", a.ID)
		}
		if total > math.MaxInt64-a.ReservedNanoUSD {
			return errors.New("budget: ledger total overflows")
		}
		total += a.ReservedNanoUSD
	}
	return nil
}

// Ledger is a handle on one ledger file. It holds no lock between operations:
// each one locks the file, re-reads it, applies one change and writes it back,
// so any number of processes can share a limit without overspending it.
type Ledger struct {
	mu    sync.Mutex
	path  string
	limit int64
}

// Open validates the ledger at path, creating its directory if needed. A
// missing file is an empty ledger; a corrupt one is refused rather than reset.
func Open(path string, limitNanoUSD int64) (*Ledger, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("budget: ledger path %q must be absolute", path)
	}
	if limitNanoUSD <= 0 || limitNanoUSD > 1_000_000_000_000_000 {
		return nil, fmt.Errorf("budget: limit %d nano-USD outside (0, $1M]", limitNanoUSD)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := &Ledger{path: path, limit: limitNanoUSD}
	if _, err := l.Snapshot(); err != nil {
		return nil, err
	}
	return l, nil
}

// Limit is the cap this handle enforces.
func (l *Ledger) Limit() int64 { return l.limit }

// Path is the ledger file.
func (l *Ledger) Path() string { return l.path }

// Snapshot reads the current ledger.
func (l *Ledger) Snapshot() (Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()
	var out Snapshot
	err := l.update(ctx, func(s *Snapshot) error { out = s.clone(); return errReadOnly })
	if errors.Is(err, errReadOnly) {
		err = nil
	}
	return out, err
}

// Inspect reads a ledger without enforcing a limit, for reporting.
func Inspect(path string) (Snapshot, error) {
	l := &Ledger{path: path, limit: 1}
	return l.Snapshot()
}

// ReconcileAt reclaims the dead attempts in the ledger at path without
// enforcing a limit, for an operator command that has none to hand.
func ReconcileAt(path string) ([]Attempt, error) {
	l := &Ledger{path: path, limit: 1}
	return l.Reconcile()
}

var errReadOnly = errors.New("read only")

func (l *Ledger) update(ctx context.Context, mutate func(*Snapshot) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// Every operation holds the lock for one read and one write, so a wait
	// longer than this means a holder is wedged; give up rather than hang a
	// caller whose own context has no deadline.
	lockCtx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	unlock, err := lockFile(lockCtx, l.path+".lock")
	if err != nil {
		return err
	}
	defer unlock()
	// The wait for the lock is where a cancelled caller would otherwise slip
	// through and reserve after it had already given up.
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := load(l.path)
	if err != nil {
		return err
	}
	next := current.clone()
	if err := mutate(&next); err != nil {
		return err
	}
	if err := validate(next); err != nil {
		return err
	}
	return save(l.path, next)
}

func load(path string) (Snapshot, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return Snapshot{SchemaVersion: schemaVersion, NextID: 1, InFlight: []Attempt{}}, nil
	}
	if err != nil {
		return Snapshot{}, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxLedgerBytes+1))
	if err != nil {
		return Snapshot{}, err
	}
	if len(raw) > maxLedgerBytes {
		return Snapshot{}, fmt.Errorf("budget: ledger %s exceeds %d bytes", path, maxLedgerBytes)
	}
	var s Snapshot
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&s); err != nil {
		return Snapshot{}, fmt.Errorf("budget: ledger %s: %w", path, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return Snapshot{}, fmt.Errorf("budget: ledger %s has trailing data", path)
	}
	if s.InFlight == nil {
		s.InFlight = []Attempt{}
	}
	if err := validate(s); err != nil {
		return Snapshot{}, fmt.Errorf("budget: ledger %s: %w", path, err)
	}
	return s, nil
}

func save(path string, s Snapshot) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".budget-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	// The rename is durable only once the directory entry is.
	return syncDir(filepath.Dir(path))
}

// reclaimDead retains every in-flight attempt whose owning process has exited.
//
// Such an attempt can never be resolved: the only code that would settle or
// release it died with its process. It may have been billed, so it is kept as
// retained spend, never refunded. That leaves the committed total unchanged; what
// it frees is the in-flight slot, which a crashed fan-out would otherwise hold
// until every later reservation failed at maxInFlight.
//
// The current process's own attempts are never touched, and an owner whose
// liveness cannot be established is treated as alive. Misjudging a live owner
// as dead costs that owner its settlement (the request errors); it never lets
// spend escape the ledger.
func reclaimDead(s *Snapshot, gone func(int) bool) []Attempt {
	self := os.Getpid()
	var reclaimed []Attempt
	kept := s.InFlight[:0]
	for _, a := range s.InFlight {
		if a.PID != self && gone(a.PID) {
			reclaimed = append(reclaimed, a)
			s.RetainedNanoUSD += a.ReservedNanoUSD
			s.RetainedAttempts++
			continue
		}
		kept = append(kept, a)
	}
	s.InFlight = kept
	return reclaimed
}

// Reconcile retains the reservations of attempts whose process has exited and
// returns them. Reserve does the same before every admission; this is for an
// operator who wants the ledger's in-flight list to be truthful now.
func (l *Ledger) Reconcile() ([]Attempt, error) {
	ctx, cancel := resolveContext()
	defer cancel()
	var reclaimed []Attempt
	err := l.update(ctx, func(s *Snapshot) error {
		reclaimed = reclaimDead(s, processGone)
		return nil
	})
	return reclaimed, err
}

func (l *Ledger) reserve(ctx context.Context, model string, amount int64) (uint64, error) {
	if model == "" || amount <= 0 {
		return 0, errors.New("budget: a reservation needs a model and a positive amount")
	}
	var id uint64
	err := l.update(ctx, func(s *Snapshot) error {
		reclaimDead(s, processGone)
		if len(s.InFlight) >= maxInFlight {
			return fmt.Errorf("budget: %d attempts already in flight", maxInFlight)
		}
		committed := s.CommittedNanoUSD()
		if amount > l.limit-committed {
			return fmt.Errorf("%w: this request may cost up to %s, and %s of the %s limit is already spent or reserved",
				ErrExhausted, USD(amount), USD(committed), USD(l.limit))
		}
		id = s.NextID
		s.NextID++
		s.InFlight = append(s.InFlight, Attempt{
			ID: id, Model: model, State: stateReserved, ReservedNanoUSD: amount,
			PID: os.Getpid(), StartedUnix: time.Now().Unix(),
		})
		return nil
	})
	return id, err
}

func resolveContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), resolveTimeout)
}

// transportResult records how an attempt's HTTP exchange ended. refund is the
// provider's own statement that a rejected attempt's status proves it was
// refused before any billable work; without it a rejection keeps its
// reservation.
func (l *Ledger) transportResult(id uint64, r transport.AttemptResult, refund bool) error {
	ctx, cancel := resolveContext()
	defer cancel()
	return l.update(ctx, func(s *Snapshot) error {
		i := s.find(id)
		if i < 0 {
			return fmt.Errorf("budget: no in-flight attempt %d", id)
		}
		if s.InFlight[i].State != stateReserved {
			return fmt.Errorf("budget: attempt %d already has a transport result", id)
		}
		switch r.State {
		case transport.AttemptNotSent:
			s.remove(i)
		case transport.AttemptRejected:
			if refund {
				s.remove(i)
				s.RefusedAttempts++
				return nil
			}
			a := s.remove(i)
			s.RetainedNanoUSD += a.ReservedNanoUSD
			s.RetainedAttempts++
		case transport.AttemptAccepted:
			s.InFlight[i].State = stateAccepted
		case transport.AttemptUnknown:
			a := s.remove(i)
			s.RetainedNanoUSD += a.ReservedNanoUSD
			s.RetainedAttempts++
		default:
			return fmt.Errorf("budget: invalid transport state %q", r.State)
		}
		return nil
	})
}

func (l *Ledger) settle(id uint64, charge int64) error {
	if charge < 0 {
		return errors.New("budget: negative charge")
	}
	ctx, cancel := resolveContext()
	defer cancel()
	return l.update(ctx, func(s *Snapshot) error {
		i := s.find(id)
		if i < 0 || s.InFlight[i].State != stateAccepted {
			return fmt.Errorf("budget: attempt %d is not an accepted in-flight attempt", id)
		}
		if charge > math.MaxInt64-s.CommittedNanoUSD() {
			return errors.New("budget: charge overflows ledger total")
		}
		a := s.remove(i)
		s.SpentNanoUSD += charge
		s.Settled++
		if charge > a.ReservedNanoUSD {
			s.Overruns++
		}
		return nil
	})
}

// retain moves an attempt's reservation to the retained total. It is
// idempotent: an attempt already resolved is left alone.
func (l *Ledger) retain(id uint64) error { return l.retainAt(id, -1) }

// retainAt retains an attempt at bound instead of its reservation, when a
// tighter upper bound on its charge is known; a negative bound means the
// reservation. The bound is recorded even when it exceeds the reservation,
// because it is the better-founded of the two.
func (l *Ledger) retainAt(id uint64, bound int64) error {
	ctx, cancel := resolveContext()
	defer cancel()
	return l.update(ctx, func(s *Snapshot) error {
		i := s.find(id)
		if i < 0 {
			return nil
		}
		a := s.remove(i)
		amount := a.ReservedNanoUSD
		if bound >= 0 {
			amount = bound
		}
		if amount > math.MaxInt64-s.CommittedNanoUSD() {
			return errors.New("budget: retained amount overflows ledger total")
		}
		s.RetainedNanoUSD += amount
		s.RetainedAttempts++
		return nil
	})
}

// USD renders nano-USD as dollars.
func USD(nano int64) string {
	sign := ""
	if nano < 0 {
		sign, nano = "-", -nano
	}
	cents := (nano + 5_000_000) / 10_000_000
	return fmt.Sprintf("%s$%d.%02d", sign, cents/100, cents%100)
}
