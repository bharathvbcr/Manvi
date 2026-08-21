// Package artifacts manages persistent, structured project and session artifacts
// (implementation plans, walkthroughs, research notes, architecture designs, UI mockups)
// stored under .devcouncil/artifacts/ or a custom directory.
package artifacts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Metadata defines structured artifact properties.
type Metadata struct {
	Summary         string `json:"summary"`
	UserFacing      bool   `json:"user_facing"`
	RequestFeedback bool   `json:"request_feedback"`
}

// Artifact represents a persistent document with revision tracking.
type Artifact struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	Content   string    `json:"content"`
	Metadata  Metadata  `json:"metadata"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Revision  int       `json:"revision"`
}

// Store provides atomic artifact persistence and retrieval.
type Store struct {
	mu  sync.RWMutex
	dir string
}

// NewStore creates a new artifact store.
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		dir = ".devcouncil/artifacts"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("artifacts: creating store dir %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// sanitizeName ensures an artifact name contains no traversal elements.
func (s *Store) sanitizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("artifacts: name cannot be empty")
	}
	clean := filepath.Clean(name)
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return "", fmt.Errorf("artifacts: invalid artifact path %q", name)
	}
	return clean, nil
}

func (s *Store) metaPath(cleanName string) string {
	return filepath.Join(s.dir, cleanName+".meta.json")
}

func (s *Store) contentPath(cleanName string) string {
	return filepath.Join(s.dir, cleanName)
}

// Create creates a new artifact, returning an error if it already exists.
func (s *Store) Create(name, content string, meta Metadata) (*Artifact, error) {
	clean, err := s.sanitizeName(name)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cPath := s.contentPath(clean)
	if _, err := os.Stat(cPath); err == nil {
		return nil, fmt.Errorf("artifacts: artifact %q already exists; use Update to modify", clean)
	}

	if err := os.MkdirAll(filepath.Dir(cPath), 0o755); err != nil {
		return nil, err
	}

	now := time.Now()
	art := &Artifact{
		Name:      clean,
		Path:      cPath,
		Content:   content,
		Metadata:  meta,
		CreatedAt: now,
		UpdatedAt: now,
		Revision:  1,
	}

	if err := os.WriteFile(cPath, []byte(content), 0o644); err != nil {
		return nil, fmt.Errorf("artifacts: writing content for %s: %w", clean, err)
	}

	metaBytes, _ := json.MarshalIndent(art, "", "  ")
	if err := os.WriteFile(s.metaPath(clean), metaBytes, 0o644); err != nil {
		_ = os.Remove(cPath)
		return nil, fmt.Errorf("artifacts: writing metadata for %s: %w", clean, err)
	}

	return art, nil
}

// Update updates an existing artifact's content and metadata, incrementing its revision.
func (s *Store) Update(name, content string, meta *Metadata) (*Artifact, error) {
	clean, err := s.sanitizeName(name)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cPath := s.contentPath(clean)
	mPath := s.metaPath(clean)

	var existing Artifact
	metaData, err := os.ReadFile(mPath)
	if err == nil {
		_ = json.Unmarshal(metaData, &existing)
	}

	if existing.Name == "" {
		existing.Name = clean
		existing.Path = cPath
		existing.CreatedAt = time.Now()
		existing.Revision = 0
	}

	existing.Content = content
	existing.UpdatedAt = time.Now()
	existing.Revision++
	if meta != nil {
		existing.Metadata = *meta
	}

	if err := os.MkdirAll(filepath.Dir(cPath), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(cPath, []byte(content), 0o644); err != nil {
		return nil, fmt.Errorf("artifacts: writing %s: %w", clean, err)
	}

	metaBytes, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(mPath, metaBytes, 0o644); err != nil {
		return nil, fmt.Errorf("artifacts: writing meta for %s: %w", clean, err)
	}

	return &existing, nil
}

// Get retrieves an artifact by name.
func (s *Store) Get(name string) (*Artifact, error) {
	clean, err := s.sanitizeName(name)
	if err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	cPath := s.contentPath(clean)
	contentBytes, err := os.ReadFile(cPath)
	if err != nil {
		return nil, fmt.Errorf("artifacts: artifact %q not found", clean)
	}

	var art Artifact
	mPath := s.metaPath(clean)
	if metaBytes, err := os.ReadFile(mPath); err == nil {
		_ = json.Unmarshal(metaBytes, &art)
	}

	art.Name = clean
	art.Path = cPath
	art.Content = string(contentBytes)
	return &art, nil
}

// List returns all artifacts in the store.
func (s *Store) List() ([]*Artifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var arts []*Artifact
	err := filepath.WalkDir(s.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".meta.json") {
			return nil
		}

		rel, err := filepath.Rel(s.dir, p)
		if err != nil {
			return nil
		}
		clean := filepath.ToSlash(rel)

		contentBytes, err := os.ReadFile(p)
		if err != nil {
			return nil
		}

		art := &Artifact{
			Name:    clean,
			Path:    p,
			Content: string(contentBytes),
		}

		mPath := s.metaPath(clean)
		if metaBytes, err := os.ReadFile(mPath); err == nil {
			_ = json.Unmarshal(metaBytes, art)
			art.Name = clean
			art.Path = p
			art.Content = string(contentBytes)
		}

		arts = append(arts, art)
		return nil
	})

	if err != nil {
		return nil, err
	}

	sort.Slice(arts, func(i, j int) bool {
		return arts[i].Name < arts[j].Name
	})

	return arts, nil
}
