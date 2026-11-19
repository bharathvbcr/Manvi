// Package catalog stores immutable compiled capabilities outside the pure reducer.
package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

type Store struct{ Root string }

// Status is the approval gate for a catalog revision.
type Status string

const (
	StatusDraft    Status = "draft"
	StatusApproved Status = "approved"
	StatusRevoked  Status = "revoked"
)

type Entry struct {
	ID          string     `json:"id"`
	Revision    string     `json:"revision"`
	SHA256      string     `json:"sha256"`
	Description string     `json:"description"`
	Path        string     `json:"path"`
	Status      Status     `json:"status"`
	Promoted    bool       `json:"promoted"` // true iff Status == approved (compat)
	Stability   *Stability `json:"stability,omitempty"`
}

var segment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,95}$`)

// AllowsUnattended reports whether replay --unattended may use this entry.
func (e Entry) AllowsUnattended() bool { return e.Status == StatusApproved }

// CheckUnattended refuses draft and revoked entries for unattended replay.
func CheckUnattended(e Entry) error {
	if e.Status == StatusApproved {
		return nil
	}
	if e.Status == "" {
		return errors.New("unattended replay refuses catalog entry without status (requires approved)")
	}
	return fmt.Errorf("unattended replay refuses catalog status %q (requires approved)", e.Status)
}

func normalizeStatus(e *Entry) {
	switch e.Status {
	case StatusDraft, StatusApproved, StatusRevoked:
		e.Promoted = e.Status == StatusApproved
	case "":
		if e.Promoted {
			e.Status = StatusApproved
		} else {
			e.Status = StatusDraft
		}
	}
}

func draftEntry(id, revision, sha, description, path string) Entry {
	return Entry{ID: id, Revision: revision, SHA256: sha, Description: description, Path: path, Status: StatusDraft, Promoted: false}
}

func (s Store) Put(p *workflow.Program) (Entry, error) {
	if p == nil {
		return Entry{}, errors.New("compiled capability required")
	}
	c := p.Capability()
	if !segment.MatchString(c.ID) || !segment.MatchString(c.Revision) || c.Revision == "current" {
		return Entry{}, errors.New("invalid or reserved catalog identity")
	}
	root, err := s.open(true)
	if err != nil {
		return Entry{}, err
	}
	defer root.Close()
	dir, err := openChild(root, c.ID, true)
	if err != nil {
		return Entry{}, err
	}
	defer dir.Close()
	name := c.Revision + ".json"
	entry := draftEntry(c.ID, c.Revision, p.Digest(), c.Description, filepath.Join(s.Root, c.ID, name))
	if err := publish(dir, name, p.Bytes(), false); err != nil {
		if errors.Is(err, ErrPublishedUnsynced) {
			// Published: the entry is real and comes back beside the error.
			return entry, err
		}
		if !errors.Is(err, os.ErrExist) {
			return Entry{}, err
		}
		raw, err := readRegular(dir, name)
		if err != nil {
			return Entry{}, err
		}
		if !bytes.Equal(raw, p.Bytes()) {
			return Entry{}, errors.New("revision is immutable; increment revision before changing it")
		}
	}
	return entry, nil
}

// EntryError names one catalog entry List could not return and why. Name is the
// file inside the identity directory, empty when the whole identity is skipped.
type EntryError struct {
	ID   string
	Name string
	Err  error
}

func (e EntryError) String() string {
	if e.Name == "" {
		return e.ID + ": " + e.Err.Error()
	}
	return e.ID + "/" + e.Name + ": " + e.Err.Error()
}

// PartialListError is what List returns beside the rows it could read when it
// had to leave entries out: an identity that is a link, a pointer that cannot be
// trusted, a revision that does not compile. One bad entry must not hide every
// good one, and must not be hidden either, so each skipped entry is carried here
// with its reason. A failure on the catalog root itself returns no rows and a
// plain error. An identity whose pointer cannot be trusted is skipped whole: its
// revisions' status is not known, and listing them as drafts would be a guess.
type PartialListError struct{ Skipped []EntryError }

func (e *PartialListError) Error() string {
	parts := make([]string, len(e.Skipped))
	for i, s := range e.Skipped {
		parts[i] = s.String()
	}
	return fmt.Sprintf("catalog: %d entries could not be listed: %s", len(e.Skipped), strings.Join(parts, "; "))
}

// Unwrap exposes each skipped entry's cause to errors.Is and errors.As.
func (e *PartialListError) Unwrap() []error {
	errs := make([]error, len(e.Skipped))
	for i, s := range e.Skipped {
		errs[i] = s.Err
	}
	return errs
}

// List returns every readable catalog revision, sorted by identity and
// revision. Entries that cannot be returned do not hide the rest: the rows come
// back with a *PartialListError naming each one (see there).
func (s Store) List() ([]Entry, error) {
	out := []Entry{}
	root, err := s.open(false)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	dirs, err := directoryEntries(root)
	if err != nil {
		return nil, err
	}
	var skipped []EntryError
	for _, entry := range dirs {
		if !segment.MatchString(entry.Name()) {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			skipped = append(skipped, EntryError{ID: entry.Name(), Err: errors.New("catalog identity is a symlink")})
			continue
		}
		if !entry.IsDir() {
			continue
		}
		rows, bad, err := s.listIdentity(root, entry.Name())
		skipped = append(skipped, bad...)
		if err != nil {
			skipped = append(skipped, EntryError{ID: entry.Name(), Err: err})
			continue
		}
		out = append(out, rows...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID == out[j].ID {
			return out[i].Revision < out[j].Revision
		}
		return out[i].ID < out[j].ID
	})
	if len(skipped) > 0 {
		sort.Slice(skipped, func(i, j int) bool { return skipped[i].String() < skipped[j].String() })
		return out, &PartialListError{Skipped: skipped}
	}
	return out, nil
}

// readPointer reads and validates an identity's current.json. has is false when
// there is none. A pointer that is present but cannot be trusted is an error:
// it decides what every revision's status is.
func readPointer(dir *os.Root, id string) (current Entry, has bool, err error) {
	raw, err := readRegular(dir, "current.json")
	if errors.Is(err, os.ErrNotExist) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, err
	}
	if err := json.Unmarshal(raw, &current); err != nil {
		return Entry{}, false, err
	}
	if current.ID != id || !segment.MatchString(current.Revision) || current.Revision == "current" {
		return Entry{}, false, errors.New("invalid promotion identity")
	}
	normalizeStatus(&current)
	switch current.Status {
	case StatusDraft, StatusApproved, StatusRevoked:
	default:
		return Entry{}, false, errors.New("invalid catalog status")
	}
	return current, true, nil
}

// revisionEntry compiles one revision file of identity id and joins it with the
// pointer: the entry carries the pointer's status only when id, revision and
// digest all match it, and is a draft otherwise. matched reports that join.
func (s Store) revisionEntry(dir *os.Root, id, name string, current Entry, hasCurrent bool) (entry Entry, matched bool, err error) {
	revision := strings.TrimSuffix(name, ".json")
	if !segment.MatchString(revision) {
		return Entry{}, false, errors.New("invalid catalog revision filename")
	}
	raw, err := readRegular(dir, name)
	if err != nil {
		return Entry{}, false, err
	}
	p, err := workflow.Compile(raw)
	if err != nil {
		return Entry{}, false, fmt.Errorf("catalog artifact %s: %w", name, err)
	}
	c := p.Capability()
	if c.ID != id || c.Revision != revision {
		return Entry{}, false, errors.New("catalog identity does not match artifact path")
	}
	entry = draftEntry(id, revision, p.Digest(), c.Description, filepath.Join(s.Root, id, name))
	if hasCurrent && current.ID == id && current.Revision == revision && current.SHA256 == p.Digest() {
		entry.Status = current.Status
		entry.Promoted = current.Status == StatusApproved
		entry.Stability = current.Stability
		matched = true
	}
	return entry, matched, nil
}

// listIdentity returns the rows of one identity. bad names the revision files
// that were left out; err is set when the identity as a whole cannot be
// trusted (its pointer is invalid, or names no revision that is readable).
func (s Store) listIdentity(root *os.Root, id string) (rows []Entry, bad []EntryError, err error) {
	dir, err := openChild(root, id, false)
	if err != nil {
		return nil, nil, err
	}
	defer dir.Close()
	files, err := directoryEntries(dir)
	if err != nil {
		return nil, nil, err
	}
	current, hasCurrent, err := readPointer(dir, id)
	if err != nil {
		return nil, nil, err
	}
	pointerFound := !hasCurrent
	for _, file := range files {
		if file.Name() == "current.json" || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		entry, matched, err := s.revisionEntry(dir, id, file.Name(), current, hasCurrent)
		if err != nil {
			bad = append(bad, EntryError{ID: id, Name: file.Name(), Err: err})
			continue
		}
		pointerFound = pointerFound || matched
		rows = append(rows, entry)
	}
	if !pointerFound {
		return nil, bad, errors.New("promotion does not match an immutable artifact")
	}
	return rows, bad, nil
}

// Get returns one catalog entry by identity and revision. It opens that
// identity and compiles that revision only, so another identity's junk, or
// another revision's, cannot make it fail. It is as strict about its own entry
// as List is: a pointer that is invalid, or that names this revision under a
// different digest, is an error and never a draft.
func (s Store) Get(id, revision string) (Entry, error) {
	if !segment.MatchString(id) || !segment.MatchString(revision) || revision == "current" {
		return Entry{}, errors.New("invalid catalog identity")
	}
	unknown := errors.New("unknown catalog revision")
	root, err := s.open(false)
	if errors.Is(err, os.ErrNotExist) {
		return Entry{}, unknown
	}
	if err != nil {
		return Entry{}, err
	}
	defer root.Close()
	dir, err := openChild(root, id, false)
	if errors.Is(err, os.ErrNotExist) {
		return Entry{}, unknown
	}
	if err != nil {
		return Entry{}, err
	}
	defer dir.Close()
	current, hasCurrent, err := readPointer(dir, id)
	if err != nil {
		return Entry{}, err
	}
	entry, matched, err := s.revisionEntry(dir, id, revision+".json", current, hasCurrent)
	if errors.Is(err, os.ErrNotExist) {
		return Entry{}, unknown
	}
	if err != nil {
		return Entry{}, err
	}
	if hasCurrent && !matched {
		// List skips an identity whose pointer names no readable revision with its
		// digest, so Get must not serve a row List omits: the pointer's own
		// revision is checked too. That is the one other revision it compiles.
		_, pointed, perr := s.revisionEntry(dir, id, current.Revision+".json", current, hasCurrent)
		if perr != nil || !pointed {
			return Entry{}, errors.New("promotion does not match an immutable artifact")
		}
	}
	return entry, nil
}

// Promote marks a revision approved (current pointer). Existing stability is
// preserved when re-approving the same digest.
func (s Store) Promote(id, revision, sha string) error {
	_, err := s.setStatus(id, revision, sha, StatusApproved)
	return err
}

// Approve is the draft→approved gate; identical to Promote.
func (s Store) Approve(id, revision, sha string) error {
	_, err := s.setStatus(id, revision, sha, StatusApproved)
	return err
}

// Revoke marks the current approved (or already revoked) pointer as revoked.
func (s Store) Revoke(id, revision, sha string) error {
	_, err := s.RevokeReport(id, revision, sha)
	return err
}

// RevokeReport is Revoke that also names the foreign current.json keys it had to
// drop. A revocation must never fail for want of room: when keeping every key
// another writer stored would push the pointer past the size every reader
// accepts, the revoked pointer is written with only the keys this build owns and
// the dropped names are returned. Approve, Promote and SetStability refuse
// instead, because a refused approval is safe and a dropped restriction is not.
func (s Store) RevokeReport(id, revision, sha string) (dropped []string, err error) {
	return s.setStatus(id, revision, sha, StatusRevoked)
}

func (s Store) setStatus(id, revision, sha string, status Status) ([]string, error) {
	if !segment.MatchString(id) || !segment.MatchString(revision) || revision == "current" {
		return nil, errors.New("invalid promotion identity")
	}
	if status != StatusApproved && status != StatusRevoked {
		return nil, errors.New("catalog status must be approved or revoked")
	}
	root, err := s.open(false)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	dir, err := openChild(root, id, false)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	raw, err := readRegular(dir, revision+".json")
	if err != nil {
		return nil, err
	}
	p, err := workflow.Compile(raw)
	if err != nil {
		return nil, err
	}
	c := p.Capability()
	if c.ID != id || c.Revision != revision || p.Digest() != sha {
		return nil, errors.New("promotion does not match a compiled revision")
	}
	// The pointer is read, changed and replaced whole, so no other writer may
	// run between the read and the rename: a revocation that landed in that gap
	// would be overwritten with a stale "approved". updateCurrent holds the lock.
	return s.updateCurrent(dir, id, true, status == StatusRevoked, func(prior Entry, exists bool, fields map[string]json.RawMessage) error {
		if !exists && status == StatusRevoked {
			return errors.New("cannot revoke without a catalog pointer")
		}
		e := Entry{
			ID:          id,
			Revision:    revision,
			SHA256:      sha,
			Description: c.Description,
			Path:        filepath.Join(s.Root, id, revision+".json"),
			Status:      status,
			Promoted:    status == StatusApproved,
		}
		// Stability belongs to one digest: re-approving it keeps the stored object
		// as written, a different revision starts without one.
		kept, keep := fields["stability"]
		keep = keep && prior.ID == id && prior.Revision == revision && prior.SHA256 == sha
		if err := overlayOwned(fields, e); err != nil {
			return err
		}
		if keep {
			if kept, err = withoutNearDuplicates(kept, jsonKeys(reflect.TypeOf(Stability{}))); err != nil {
				return err
			}
			fields["stability"] = kept
		}
		return nil
	})
}

// withoutNearDuplicates drops from a stored JSON object the keys that decode into
// one of owned without being it, keeping the owned keys as written, so a kept
// object cannot hide an override of its own fields (see matchesOwned).
func withoutNearDuplicates(raw json.RawMessage, owned []string) (json.RawMessage, error) {
	var inner map[string]json.RawMessage
	if err := json.Unmarshal(raw, &inner); err != nil || inner == nil {
		return raw, err
	}
	dropNearDuplicates(inner, owned, true)
	return encodeFields(inner)
}
