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
	"syscall"
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

	cPath, err := s.containedPath(clean)
	if err != nil {
		return nil, err
	}
	// Lstat, not Stat: a name that is a symbolic link "exists" for the purpose
	// of refusing to clobber it, and saying so plainly is better than telling
	// the caller to use Update — which would refuse for a different reason and
	// leave them guessing which of the two answers was the real one.
	if info, err := os.Lstat(cPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("artifacts: refusing %q: that name is already a symbolic link", clean)
		}
		return nil, fmt.Errorf("artifacts: artifact %q already exists; use Update to modify", clean)
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

	if err := writeContained(cPath, []byte(content), 0o644); err != nil {
		return nil, err
	}

	metaBytes, _ := json.MarshalIndent(art, "", "  ")
	metaFull, err := s.containedPath(clean + ".meta.json")
	if err != nil {
		_ = os.Remove(cPath)
		return nil, err
	}
	if err := writeContained(metaFull, metaBytes, 0o644); err != nil {
		_ = os.Remove(cPath)
		return nil, err
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

	cPath, err := s.containedPath(clean)
	if err != nil {
		return nil, err
	}
	mPath, err := s.containedPath(clean + ".meta.json")
	if err != nil {
		return nil, err
	}

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

	if err := writeContained(cPath, []byte(content), 0o644); err != nil {
		return nil, err
	}

	metaBytes, _ := json.MarshalIndent(existing, "", "  ")
	if err := writeContained(mPath, metaBytes, 0o644); err != nil {
		return nil, err
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

// --- containment ---
//
// sanitizeName refuses traversal, and traversal is not the only way out of a
// directory. It rejects "../x" and an absolute path, then hands the result to
// os.WriteFile, which follows symbolic links — so an artifact named "notes.md"
// whose name already exists in the store as a link to somewhere else wrote
// there instead. A symlinked *directory* component did the same for
// "sub/notes.md". Neither carries a ".." for sanitizeName to find.
//
// That mattered more than it looks: the create_artifact tool reaches this store
// without going through the policy gate at all, so the store's own containment
// was the only thing standing between a model-chosen name and an arbitrary
// file write.
//
// The rule here is the one safefs.go states for the write gate: a component
// this code cannot identify is a component it will not walk through. Every
// directory under the store root is created and then inspected — os.Mkdir
// answers EEXIST for a symlink, so creating without looking proves nothing —
// and the leaf is opened with O_NOFOLLOW so the kernel refuses the link rather
// than resolving it.

// containedPath returns the physical path for a cleaned artifact name,
// creating the directories beneath it, and refuses if any component is a
// symbolic link or is not a directory.
func (s *Store) containedPath(clean string) (string, error) {
	root, err := filepath.Abs(s.dir)
	if err != nil {
		return "", fmt.Errorf("artifacts: resolving store dir %s: %w", s.dir, err)
	}
	current := root
	parts := strings.Split(filepath.ToSlash(clean), "/")
	for _, part := range parts[:len(parts)-1] {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		if err := os.Mkdir(current, 0o755); err != nil && !os.IsExist(err) {
			return "", fmt.Errorf("artifacts: creating %s: %w", current, err)
		}
		// Lstat, not Stat: Stat follows the link and would report the directory
		// at the far end as though this component were one.
		info, err := os.Lstat(current)
		if err != nil {
			return "", fmt.Errorf("artifacts: inspecting %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("artifacts: refusing %q: path component %q is a symbolic link", clean, part)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("artifacts: refusing %q: path component %q is not a directory", clean, part)
		}
	}
	return filepath.Join(current, parts[len(parts)-1]), nil
}

// writeContained writes data at full, refusing to follow a symbolic link at the
// final component.
//
// O_NOFOLLOW is what makes this a decision the kernel enforces rather than a
// check this code makes and then races against: an Lstat followed by an
// ordinary open is two operations with a window between them, and the window is
// the whole attack.
func writeContained(full string, data []byte, perm os.FileMode) error {
	fd, err := syscall.Open(full,
		syscall.O_WRONLY|syscall.O_CREAT|syscall.O_TRUNC|syscall.O_NOFOLLOW|syscall.O_CLOEXEC,
		uint32(perm))
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return fmt.Errorf("artifacts: refusing to write %s: it is a symbolic link", filepath.Base(full))
		}
		return fmt.Errorf("artifacts: opening %s: %w", filepath.Base(full), err)
	}
	f := os.NewFile(uintptr(fd), full)
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("artifacts: inspecting %s: %w", filepath.Base(full), err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("artifacts: refusing to write %s: not a regular file", filepath.Base(full))
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("artifacts: writing %s: %w", filepath.Base(full), err)
	}
	return nil
}
