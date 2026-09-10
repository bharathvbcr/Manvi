// Package catalog stores immutable compiled capabilities outside the pure reducer.
package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	for _, entry := range dirs {
		if !segment.MatchString(entry.Name()) {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil, errors.New("catalog identity is a symlink")
		}
		if !entry.IsDir() {
			continue
		}
		rows, err := s.listIdentity(root, entry.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID == out[j].ID {
			return out[i].Revision < out[j].Revision
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}
func (s Store) listIdentity(root *os.Root, id string) ([]Entry, error) {
	dir, err := openChild(root, id, false)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	files, err := directoryEntries(dir)
	if err != nil {
		return nil, err
	}
	var current Entry
	hasCurrent := false
	if raw, err := readRegular(dir, "current.json"); err == nil {
		if err := json.Unmarshal(raw, &current); err != nil {
			return nil, err
		}
		if current.ID != id || !segment.MatchString(current.Revision) || current.Revision == "current" {
			return nil, errors.New("invalid promotion identity")
		}
		normalizeStatus(&current)
		switch current.Status {
		case StatusDraft, StatusApproved, StatusRevoked:
		default:
			return nil, errors.New("invalid catalog status")
		}
		hasCurrent = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	out := []Entry{}
	pointerFound := !hasCurrent
	for _, file := range files {
		if file.Name() == "current.json" || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		name := file.Name()
		revision := strings.TrimSuffix(name, ".json")
		if !segment.MatchString(revision) {
			return nil, errors.New("invalid catalog revision filename")
		}
		raw, err := readRegular(dir, name)
		if err != nil {
			return nil, err
		}
		p, err := workflow.Compile(raw)
		if err != nil {
			return nil, fmt.Errorf("catalog artifact %s: %w", name, err)
		}
		c := p.Capability()
		if c.ID != id || c.Revision != revision {
			return nil, errors.New("catalog identity does not match artifact path")
		}
		entry := draftEntry(id, revision, p.Digest(), c.Description, filepath.Join(s.Root, id, name))
		if hasCurrent && current.ID == id && current.Revision == revision && current.SHA256 == p.Digest() {
			entry.Status = current.Status
			entry.Promoted = current.Status == StatusApproved
			entry.Stability = current.Stability
			pointerFound = true
		}
		out = append(out, entry)
	}
	if !pointerFound {
		return nil, errors.New("promotion does not match an immutable artifact")
	}
	return out, nil
}

// Get returns one catalog entry by identity and revision.
func (s Store) Get(id, revision string) (Entry, error) {
	if !segment.MatchString(id) || !segment.MatchString(revision) || revision == "current" {
		return Entry{}, errors.New("invalid catalog identity")
	}
	rows, err := s.List()
	if err != nil {
		return Entry{}, err
	}
	for _, e := range rows {
		if e.ID == id && e.Revision == revision {
			return e, nil
		}
	}
	return Entry{}, errors.New("unknown catalog revision")
}

// FindByDigest returns the catalog entry whose immutable digest matches.
func (s Store) FindByDigest(sha string) (Entry, error) {
	if sha == "" {
		return Entry{}, errors.New("digest required")
	}
	rows, err := s.List()
	if err != nil {
		return Entry{}, err
	}
	for _, e := range rows {
		if e.SHA256 == sha {
			return e, nil
		}
	}
	return Entry{}, errors.New("unknown catalog digest")
}

// Promote marks a revision approved (current pointer). Existing stability is
// preserved when re-approving the same digest.
func (s Store) Promote(id, revision, sha string) error {
	return s.setStatus(id, revision, sha, StatusApproved)
}

// Approve is the draft→approved gate; identical to Promote.
func (s Store) Approve(id, revision, sha string) error {
	return s.setStatus(id, revision, sha, StatusApproved)
}

// Revoke marks the current approved (or already revoked) pointer as revoked.
func (s Store) Revoke(id, revision, sha string) error {
	return s.setStatus(id, revision, sha, StatusRevoked)
}

func (s Store) setStatus(id, revision, sha string, status Status) error {
	if !segment.MatchString(id) || !segment.MatchString(revision) || revision == "current" {
		return errors.New("invalid promotion identity")
	}
	if status != StatusApproved && status != StatusRevoked {
		return errors.New("catalog status must be approved or revoked")
	}
	root, err := s.open(false)
	if err != nil {
		return err
	}
	defer root.Close()
	dir, err := openChild(root, id, false)
	if err != nil {
		return err
	}
	defer dir.Close()
	raw, err := readRegular(dir, revision+".json")
	if err != nil {
		return err
	}
	p, err := workflow.Compile(raw)
	if err != nil {
		return err
	}
	c := p.Capability()
	if c.ID != id || c.Revision != revision || p.Digest() != sha {
		return errors.New("promotion does not match a compiled revision")
	}
	var prior Entry
	if meta, err := readRegular(dir, "current.json"); err == nil {
		if err := json.Unmarshal(meta, &prior); err != nil {
			return err
		}
		normalizeStatus(&prior)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else if status == StatusRevoked {
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
	if prior.ID == id && prior.Revision == revision && prior.SHA256 == sha {
		e.Stability = prior.Stability
	}
	metadata, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return publish(dir, "current.json", metadata, true)
}

// GenerateGo embeds the exact immutable artifact and delegates execution to
// Manvi. It does not emit an independent series of native input instructions.
func GenerateGo(p *workflow.Program) ([]byte, error) {
	if p == nil {
		return nil, errors.New("compiled capability required")
	}
	c := p.Capability()
	var b strings.Builder
	b.WriteString("// Code generated by Manvi capability codegen; DO NOT EDIT.\npackage capability\n\nimport (\"context\"; \"github.com/bharathvbcr/Manvi/manvi/computer\"; \"github.com/bharathvbcr/Manvi/manvi/workflow\")\n\ntype Parameters struct {\n")
	fields := map[string]bool{}
	for _, p := range c.Parameters {
		field := goName(p.Name)
		if fields[field] {
			return nil, errors.New("parameter names collide as Go identifiers")
		}
		fields[field] = true
		typ := goType(p.Type)
		fmt.Fprintf(&b, "%s %s\n", field, typ)
	}
	b.WriteString("}\n\nfunc Invoke(ctx context.Context, desktop computer.Desktop, session computer.Session, inputs Parameters, privacy computer.PrivacyPolicy, record func(computer.Record) error) (*computer.Run,error) {\n")
	fmt.Fprintf(&b, "program, err := workflow.Compile([]byte(%s))\nif err != nil {return nil,err}\nparameters := map[string]workflow.Value{\n", strconv.Quote(string(p.Bytes())))
	for _, p := range c.Parameters {
		field := goName(p.Name)
		switch p.Type {
		case "string":
			fmt.Fprintf(&b, "%q: {Type: \"string\",Text:inputs.%s},\n", p.Name, field)
		case "integer":
			fmt.Fprintf(&b, "%q: {Type: \"integer\",Integer:inputs.%s},\n", p.Name, field)
		case "boolean":
			fmt.Fprintf(&b, "%q: {Type: \"boolean\",Boolean:inputs.%s},\n", p.Name, field)
		case "money":
			fmt.Fprintf(&b, "%q: inputs.%s,\n", p.Name, field)
		}
	}
	b.WriteString("}\nreturn computer.StartRun(ctx,computer.RunOptions{Program:program,Inputs:parameters,Desktop:desktop,Session:session,Privacy:privacy,OnRecord:record})\n}\n")
	return format.Source([]byte(b.String()))
}
func goName(s string) string {
	var out strings.Builder
	upper := true
	for _, r := range s {
		if r == '.' || r == '-' || r == '_' {
			upper = true
			continue
		}
		if upper && r >= 'a' && r <= 'z' {
			r -= 32
		}
		out.WriteRune(r)
		upper = false
	}
	return out.String()
}
func goType(t string) string {
	switch t {
	case "string":
		return "string"
	case "integer":
		return "int64"
	case "boolean":
		return "bool"
	default:
		return "workflow.Value"
	}
}
