package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A session that only exists in memory is a session that can be driven exactly
// once. That is fine for a face a human is sitting in front of, and wrong for
// every other caller: a CI job that retries, a script that iterates on one
// task, a benchmark that measures the second turn. Each of those re-pays the
// whole prompt's prefill — twenty to thirty-five seconds against a local
// server — to arrive back at a history the harness already had.
//
// So the log is durable, and it is durable as *the log*: the file holds
// events, and a resumed run rebuilds a Log from them and projects it through
// DeriveMessages like any other. There is deliberately no second path that
// turns a file into messages. The invariant at the top of this package is only
// worth what its narrowest exception allows, and "except when resuming" would
// be a wide one.
//
// The file format is one JSON document per generation:
//
//	{"manvi_session":1,"id":…,"created_at":…,"checksum":"sha256:…","events":[…]}
//
// written as <dir>/<id>.<generation>.json. Three properties follow from that
// layout, and each one exists to close a failure this package must not have.
//
// Atomicity: a generation is written to a temporary file, synced, and then
// linked into place. os.Link fails rather than overwrites, so two runs racing
// to save the same session cannot interleave and cannot silently clobber one
// another — the loser is told. A run that dies mid-write leaves a temporary
// file that nothing reads and the next save removes; the last complete
// generation is untouched, so a crash cannot poison the next resume.
//
// Integrity: the checksum covers the exact encoded event bytes. A file that
// has been truncated fails to parse; one that has been altered in place fails
// the checksum. Either way Load returns an error naming the file. It never
// returns an empty log — a session that could not be read must not be
// indistinguishable from a session with nothing in it, because resuming into
// an empty history looks exactly like a fresh start and produces a turn that
// silently forgot everything it was supposed to continue.
//
// Boundedness: a session's oldest whole turns are dropped once the file
// exceeds a size bound, and the sessions directory keeps only its most recent
// entries. Both are recorded in the header and reported by Load, because a
// history that quietly lost its beginning is a history that lies about what
// the model saw.

// FormatVersion is the on-disk layout this package writes and accepts.
const FormatVersion = 1

const (
	// DefaultMaxBytes bounds one session file. Beyond it the oldest whole
	// turns are dropped: whole, so that a surviving turn always begins where
	// a turn begins rather than in the middle of a tool exchange.
	DefaultMaxBytes = 8 << 20
	// DefaultMaxSessions bounds how many sessions the directory keeps.
	DefaultMaxSessions = 50
	// tempMaxAge is how long an abandoned temporary file is left alone before
	// a later save sweeps it. It is generous because a live writer's temp file
	// must never be removed underneath it, and the cost of waiting is a few
	// stale bytes in a directory that is already ignored by git.
	tempMaxAge = 6 * time.Hour
)

// ErrNoSessions reports that the directory holds nothing to continue.
var ErrNoSessions = errors.New("session: no saved sessions")

// NotFoundError reports a reference that matched no session.
type NotFoundError struct{ Ref string }

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("session: no session matches %q", e.Ref)
}

// AmbiguousError reports a prefix that matched more than one session. It
// carries the candidates rather than choosing one: picking arbitrarily would
// resume the wrong conversation and look like it worked.
type AmbiguousError struct {
	Ref        string
	Candidates []string
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("session: %q matches %d sessions: %s",
		e.Ref, len(e.Candidates), strings.Join(e.Candidates, ", "))
}

// CorruptError reports a session file that could not be trusted.
type CorruptError struct {
	Path   string
	Detail string
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("session file %s is unreadable: %s — it cannot be resumed; "+
		"start a fresh session, or resume a different one", e.Path, e.Detail)
}

// ConflictError reports that a session changed underneath this run.
type ConflictError struct {
	ID     string
	Detail string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("session %s was written by another run: %s — "+
		"refusing to overwrite it", e.ID, e.Detail)
}

// header is the on-disk document.
//
// Events is a raw message on both encode and decode so the checksum covers the
// exact bytes the file carries, rather than a re-encoding of them that could
// differ while the events did not.
type header struct {
	Version   int       `json:"manvi_session"`
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Turns     int       `json:"turns"`
	// TrimmedTurns is the cumulative count of whole turns dropped from the
	// front of this session to hold it within the size bound.
	TrimmedTurns int `json:"trimmed_turns,omitempty"`
	// Oversize records a session whose most recent turn alone exceeds the
	// bound. Nothing more can be dropped without discarding the turn being
	// resumed, so the file is written anyway and says so.
	Oversize bool            `json:"oversize,omitempty"`
	Checksum string          `json:"checksum"`
	Events   json.RawMessage `json:"events"`
}

// Summary describes a stored session without reading its events.
type Summary struct {
	ID         string
	Generation int
	Path       string
	UpdatedAt  time.Time
}

// LoadReport describes a session that was restored.
type LoadReport struct {
	ID           string
	Path         string
	Generation   int
	Events       int
	Turns        int
	TrimmedTurns int
	Oversize     bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// SaveReport describes a session that was written.
type SaveReport struct {
	ID         string
	Path       string
	Generation int
	Events     int
	// TrimmedTurns is how many whole turns this save dropped. Reported so the
	// caller can say it out loud: history that vanished quietly is the failure
	// the bound is allowed to cause and is not allowed to hide.
	TrimmedTurns int
	Oversize     bool
}

// Store persists session logs under one directory.
type Store struct {
	dir         string
	maxBytes    int
	maxSessions int

	mu sync.Mutex
	// seen records the whole-file digest this process last read or wrote for a
	// session. A save compares the file on disk against it, which is what
	// catches a second run that saved and pruned in between — the generation
	// number alone would not, because that run's prune makes its newer file
	// look like the one this run loaded.
	seen map[string]string
}

// NewStore returns a store over dir. The directory is created on first save.
func NewStore(dir string) *Store {
	return &Store{
		dir:         dir,
		maxBytes:    DefaultMaxBytes,
		maxSessions: DefaultMaxSessions,
		seen:        map[string]string{},
	}
}

// Dir is where sessions are kept.
func (s *Store) Dir() string { return s.dir }

// SetLimits overrides the retention bounds. A non-positive value leaves the
// corresponding default in place: a store with no bound at all is the thing
// this exists to prevent, so it is not expressible.
func (s *Store) SetLimits(maxBytes, maxSessions int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if maxBytes > 0 {
		s.maxBytes = maxBytes
	}
	if maxSessions > 0 {
		s.maxSessions = maxSessions
	}
}

// NewID returns an identifier for a fresh session.
//
// It is random rather than sequential or time-derived so that a short prefix
// is usually enough to name one, which is the whole point of accepting a
// prefix. A counter or a timestamp would make every id share a long head and
// every abbreviation ambiguous.
func NewID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("session: cannot generate an id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func validID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

// List returns the stored sessions, most recently written first.
func (s *Store) List() ([]Summary, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("session: reading %s: %w", s.dir, err)
	}

	// Only the newest generation of each id is a session; the older ones are
	// previous saves a prune has not reached yet.
	newest := map[string]Summary{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		id, generation, ok := parseName(entry.Name())
		if !ok {
			continue
		}
		current, exists := newest[id]
		if exists && current.Generation >= generation {
			continue
		}
		path := filepath.Join(s.dir, entry.Name())
		newest[id] = Summary{
			ID: id, Generation: generation,
			Path:      path,
			UpdatedAt: writtenAt(path, entry),
		}
	}

	out := make([]Summary, 0, len(newest))
	for _, summary := range newest {
		out = append(out, summary)
	}
	// The id breaks a modification-time tie so the order is total. Two files
	// written in the same clock tick would otherwise sort differently from run
	// to run, and "the most recent session" would not name one session.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Latest names the most recently written session.
func (s *Store) Latest() (string, error) {
	sessions, err := s.List()
	if err != nil {
		return "", err
	}
	if len(sessions) == 0 {
		return "", ErrNoSessions
	}
	return sessions[0].ID, nil
}

// Resolve turns a full id or a unique prefix into an id.
func (s *Store) Resolve(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", &NotFoundError{Ref: ref}
	}
	sessions, err := s.List()
	if err != nil {
		return "", err
	}

	var matches []string
	for _, summary := range sessions {
		if summary.ID == ref {
			return summary.ID, nil
		}
		if strings.HasPrefix(summary.ID, ref) {
			matches = append(matches, summary.ID)
		}
	}
	switch len(matches) {
	case 0:
		return "", &NotFoundError{Ref: ref}
	case 1:
		return matches[0], nil
	default:
		sort.Strings(matches)
		return "", &AmbiguousError{Ref: ref, Candidates: matches}
	}
}

// Load restores a session's log.
func (s *Store) Load(id string) (*Log, LoadReport, error) {
	if !validID(id) {
		return nil, LoadReport{}, &NotFoundError{Ref: id}
	}
	generation, path, err := s.current(id)
	if err != nil {
		return nil, LoadReport{}, err
	}
	if generation == 0 {
		return nil, LoadReport{}, &NotFoundError{Ref: id}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, LoadReport{}, fmt.Errorf("session: reading %s: %w", path, err)
	}
	if len(raw) == 0 {
		// An empty file is corruption, not an empty session. The distinction
		// is the whole reason this returns an error: a caller handed an empty
		// log would resume a conversation by forgetting it.
		return nil, LoadReport{}, &CorruptError{Path: path, Detail: "the file is empty"}
	}

	var doc header
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, LoadReport{}, &CorruptError{Path: path, Detail: err.Error()}
	}
	if doc.Version != FormatVersion {
		return nil, LoadReport{}, &CorruptError{Path: path,
			Detail: fmt.Sprintf("it is format version %d and this build writes version %d",
				doc.Version, FormatVersion)}
	}
	if doc.ID != id {
		return nil, LoadReport{}, &CorruptError{Path: path,
			Detail: fmt.Sprintf("it names session %q but is filed under %q", doc.ID, id)}
	}
	if len(doc.Events) == 0 {
		return nil, LoadReport{}, &CorruptError{Path: path, Detail: "it carries no events"}
	}
	if got := digest(doc.Events); got != doc.Checksum {
		return nil, LoadReport{}, &CorruptError{Path: path,
			Detail: fmt.Sprintf("its checksum is %s but its events hash to %s", doc.Checksum, got)}
	}

	var events []Event
	if err := json.Unmarshal(doc.Events, &events); err != nil {
		return nil, LoadReport{}, &CorruptError{Path: path, Detail: err.Error()}
	}
	log, err := RestoreLog(events)
	if err != nil {
		return nil, LoadReport{}, &CorruptError{Path: path, Detail: err.Error()}
	}

	s.mu.Lock()
	s.seen[id] = digest(raw)
	s.mu.Unlock()

	return log, LoadReport{
		ID: id, Path: path, Generation: generation,
		Events: len(events), Turns: doc.Turns,
		TrimmedTurns: doc.TrimmedTurns, Oversize: doc.Oversize,
		CreatedAt: doc.CreatedAt, UpdatedAt: doc.UpdatedAt,
	}, nil
}

// Save writes the log as this session's next generation.
func (s *Store) Save(id string, log *Log) (SaveReport, error) {
	if !validID(id) {
		return SaveReport{}, fmt.Errorf("session: %q is not a valid session id", id)
	}
	if log == nil {
		return SaveReport{}, errors.New("session: nothing to save")
	}
	events := log.Events()
	if len(events) == 0 {
		// Refused rather than written. An empty file is what Load treats as
		// corruption, and a session with no events is nothing to continue.
		return SaveReport{}, errors.New("session: refusing to save a log with no events")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return SaveReport{}, fmt.Errorf("session: creating %s: %w", s.dir, err)
	}

	generation, path, err := s.current(id)
	if err != nil {
		return SaveReport{}, err
	}
	createdAt := time.Now().UTC()
	trimmedBefore := 0
	if generation > 0 {
		existing, readErr := os.ReadFile(path)
		if readErr != nil {
			return SaveReport{}, fmt.Errorf("session: reading %s: %w", path, readErr)
		}
		if want, ok := s.seen[id]; !ok || want != digest(existing) {
			return SaveReport{}, &ConflictError{ID: id,
				Detail: fmt.Sprintf("%s no longer holds the contents this run loaded", path)}
		}
		var doc header
		if json.Unmarshal(existing, &doc) == nil && !doc.CreatedAt.IsZero() {
			createdAt = doc.CreatedAt
			trimmedBefore = doc.TrimmedTurns
		}
	}

	kept, droppedTurns, oversize, err := trimToBudget(events, s.maxBytes)
	if err != nil {
		return SaveReport{}, err
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return SaveReport{}, fmt.Errorf("session: encoding events: %w", err)
	}
	body, err := json.Marshal(header{
		Version:      FormatVersion,
		ID:           id,
		CreatedAt:    createdAt,
		UpdatedAt:    time.Now().UTC(),
		Turns:        countTurns(kept),
		TrimmedTurns: trimmedBefore + droppedTurns,
		Oversize:     oversize,
		Checksum:     digest(encoded),
		Events:       encoded,
	})
	if err != nil {
		return SaveReport{}, fmt.Errorf("session: encoding session %s: %w", id, err)
	}
	body = append(body, '\n')

	target := filepath.Join(s.dir, name(id, generation+1))
	if err := writeNew(s.dir, target, body); err != nil {
		return SaveReport{}, err
	}
	s.seen[id] = digest(body)

	// Pruning is best-effort on purpose: the session is already durable, and
	// failing the run because an old generation could not be unlinked would
	// turn a housekeeping problem into a lost turn. What it must not do is
	// skip silently forever — the next save tries again, over the same
	// directory, and the bound is on what accumulates rather than on any one
	// removal succeeding.
	s.prune(id, generation+1)

	return SaveReport{
		ID: id, Path: target, Generation: generation + 1,
		Events: len(kept), TrimmedTurns: droppedTurns, Oversize: oversize,
	}, nil
}

// writeNew materialises body at target, which must not already exist.
//
// The temporary file is synced before it is linked, so the bytes are on the
// disk before the name that will be read points at them. Link rather than
// rename because rename replaces: if another run already published this
// generation, this one must fail rather than quietly win.
func writeNew(dir, target string, body []byte) error {
	tmp, err := os.CreateTemp(dir, ".session-*.tmp")
	if err != nil {
		return fmt.Errorf("session: creating a temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("session: writing %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("session: syncing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("session: closing %s: %w", tmpName, err)
	}
	if err := os.Link(tmpName, target); err != nil {
		if errors.Is(err, os.ErrExist) {
			id, generation, _ := parseName(filepath.Base(target))
			return &ConflictError{ID: id,
				Detail: fmt.Sprintf("another run already published generation %d", generation)}
		}
		return fmt.Errorf("session: publishing %s: %w", target, err)
	}
	// Syncing the file put the bytes on the disk; syncing the directory is what
	// puts the *name* there. Without this the log survives a crash only as an
	// unreferenced inode: the generation that was published reads back as
	// missing, and a log whose last turns can vanish is not the record the
	// projection is allowed to assume it is.
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("session: syncing %s after publishing %s: %w", dir, target, err)
	}
	return nil
}

// syncDir flushes a directory's own entries.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

// current reports the newest generation stored for a session, or zero.
func (s *Store) current(id string) (int, string, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("session: reading %s: %w", s.dir, err)
	}
	best, path := 0, ""
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		gotID, generation, ok := parseName(entry.Name())
		if !ok || gotID != id || generation <= best {
			continue
		}
		best, path = generation, filepath.Join(s.dir, entry.Name())
	}
	return best, path, nil
}

// prune removes superseded generations, sessions past the retention bound, and
// temporary files a dead run left behind.
func (s *Store) prune(id string, keepGeneration int) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-tempMaxAge)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		nm := entry.Name()
		if strings.HasSuffix(nm, ".tmp") {
			if info, err := entry.Info(); err == nil && info.ModTime().Before(cutoff) {
				_ = os.Remove(filepath.Join(s.dir, nm))
			}
			continue
		}
		gotID, generation, ok := parseName(nm)
		if ok && gotID == id && generation < keepGeneration {
			_ = os.Remove(filepath.Join(s.dir, nm))
		}
	}

	sessions, err := s.List()
	if err != nil || len(sessions) <= s.maxSessions {
		return
	}
	for _, stale := range sessions[s.maxSessions:] {
		s.removeSession(stale.ID)
		delete(s.seen, stale.ID)
	}
}

func (s *Store) removeSession(id string) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		gotID, _, ok := parseName(entry.Name())
		if ok && gotID == id {
			_ = os.Remove(filepath.Join(s.dir, entry.Name()))
		}
	}
}

func name(id string, generation int) string {
	return fmt.Sprintf("%s.%d.json", id, generation)
}

func parseName(base string) (string, int, bool) {
	rest, ok := strings.CutSuffix(base, ".json")
	if !ok {
		return "", 0, false
	}
	id, gen, ok := strings.Cut(rest, ".")
	if !ok || !validID(id) {
		return "", 0, false
	}
	generation, err := strconv.Atoi(gen)
	if err != nil || generation <= 0 {
		return "", 0, false
	}
	return id, generation, true
}

// writtenAt reports when a session file was written.
//
// It reads the stamp the file records rather than trusting its modification
// time. Modification time is the obvious answer and the wrong one: several
// filesystems keep it to whole seconds, so two sessions written in the same
// second would tie and "the most recent session" would name whichever the
// directory happened to yield first. --continue resuming an arbitrary one of
// two conversations is precisely the silent wrong answer this package is
// built to avoid.
//
// Only the head of the file is read. The stamp is a header field and the
// events follow it, so the cost does not scale with the session — which is
// what makes it affordable to do this for every session on every listing.
func writtenAt(path string, entry os.DirEntry) time.Time {
	fallback := time.Time{}
	if info, err := entry.Info(); err == nil {
		fallback = info.ModTime()
	}
	file, err := os.Open(path)
	if err != nil {
		return fallback
	}
	defer func() { _ = file.Close() }()

	var head [1024]byte
	n, err := file.Read(head[:])
	if n == 0 || (err != nil && n == 0) {
		return fallback
	}
	const key = `"updated_at":"`
	start := strings.Index(string(head[:n]), key)
	if start < 0 {
		return fallback
	}
	rest := string(head[start+len(key) : n])
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return fallback
	}
	stamp, err := time.Parse(time.RFC3339Nano, rest[:end])
	if err != nil {
		return fallback
	}
	return stamp
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func countTurns(events []Event) int {
	turns := 0
	for _, event := range events {
		if event.Type == TurnStart {
			turns++
		}
	}
	return turns
}

// trimToBudget drops whole turns off the front until the encoded events fit.
//
// Whole turns, because a turn is the smallest unit whose removal leaves a
// coherent history: it opens with the user message that started it, so what
// remains still begins where a conversation begins rather than at an assistant
// message answering a prompt that is no longer there.
//
// The most recent turn is never dropped. A bound that could discard the turn
// being resumed would make "resume" mean "start over", which is the failure
// this whole file exists to rule out — so an outsized final turn is written
// and flagged instead.
func trimToBudget(events []Event, maxBytes int) ([]Event, int, bool, error) {
	sizes := make([]int, len(events))
	total := len("[]")
	for i, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			return nil, 0, false, fmt.Errorf("session: encoding event %d: %w", event.Seq, err)
		}
		sizes[i] = len(encoded) + 1 // the separating comma
		total += sizes[i]
	}
	if total <= maxBytes {
		return events, 0, false, nil
	}

	// Turn boundaries, as index ranges over the event slice. Events recorded
	// before the first turn/start form the leading group and are dropped
	// first.
	var starts []int
	for i, event := range events {
		if event.Type == TurnStart {
			starts = append(starts, i)
		}
	}
	if len(starts) == 0 || starts[0] > 0 {
		starts = append([]int{0}, starts...)
	}

	dropped, from := 0, 0
	for total > maxBytes && dropped < len(starts)-1 {
		for i := starts[dropped]; i < starts[dropped+1]; i++ {
			total -= sizes[i]
		}
		dropped++
		from = starts[dropped]
	}
	return events[from:], dropped, total > maxBytes, nil
}
