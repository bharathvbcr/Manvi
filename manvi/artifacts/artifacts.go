// Package artifacts manages persistent, structured project and session artifacts
// (implementation plans, walkthroughs, research notes, architecture designs, UI mockups)
// stored under .devcouncil/artifacts/ or a custom directory.
package artifacts

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/safefile"
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

// NewStore returns an artifact store over dir. The directory is created on the
// first write, not here: the store is built for every tool surface, 'manvi
// tools' listing included, and a surface that never writes an artifact must
// not leave a directory behind in the checkout it was built over.
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		dir = ".devcouncil/artifacts"
	}
	if info, err := os.Stat(dir); err == nil && !info.IsDir() {
		return nil, fmt.Errorf("artifacts: store dir %s is not a directory", dir)
	}
	return &Store{dir: dir}, nil
}

// syncFile is the durability step of a write, a variable so a test can make it
// fail. Nothing else assigns it.
var syncFile = func(f *os.File) error { return f.Sync() }

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
	// "<name>.meta.json" is where the metadata of <name> is kept. A name in that
	// namespace would clobber another artifact's metadata, or have its own
	// content read back as one, so no artifact may be called that.
	if strings.HasSuffix(clean, metaSuffix) {
		return "", fmt.Errorf("artifacts: invalid artifact name %q: names ending in %s are reserved for metadata", name, metaSuffix)
	}
	return clean, nil
}

// metaSuffix is appended to an artifact's name to give its metadata file.
const metaSuffix = ".meta.json"

func (s *Store) metaPath(cleanName string) string {
	return filepath.Join(s.dir, cleanName+metaSuffix)
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

	metaBytes, err := json.MarshalIndent(art, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("artifacts: encoding metadata for %q: %w", clean, err)
	}
	metaFull, err := s.containedPath(clean + metaSuffix)
	if err != nil {
		return nil, err
	}

	// Exclusive: the existence check and the write are one operation, so two
	// creators (two processes over one directory) cannot both succeed and the
	// loser cannot overwrite the winner. A name that is a symbolic link is
	// refused outright rather than reported as merely existing.
	if err := writeContained(cPath, []byte(content), 0o644, true); err != nil {
		return nil, err
	}
	if err := writeContained(metaFull, metaBytes, 0o644, false); err != nil {
		// This call created the content, so taking it away leaves the store as
		// it was found.
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
	mPath, err := s.containedPath(clean + metaSuffix)
	if err != nil {
		return nil, err
	}

	// Update modifies an artifact that exists; it never creates one, and it
	// never invents a history for one whose record it cannot read. Resetting
	// to revision 1 over a damaged sidecar would be a silent loss of the real
	// revision count and created_at.
	if info, err := os.Lstat(cPath); err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("artifacts: artifact %q not found; use Create to make it", clean)
	}
	metaData, err := os.ReadFile(mPath)
	if err != nil {
		return nil, fmt.Errorf("artifacts: the metadata of %q is unreadable, so its revision history cannot be continued: %w", clean, err)
	}
	var existing Artifact
	if err := json.Unmarshal(metaData, &existing); err != nil {
		return nil, fmt.Errorf("artifacts: the metadata of %q is corrupt, so its revision history cannot be continued: %w", clean, err)
	}
	if existing.Revision < 1 || existing.CreatedAt.IsZero() {
		return nil, fmt.Errorf("artifacts: the metadata of %q has no revision history to continue", clean)
	}

	existing.Name = clean
	existing.Path = cPath
	existing.Content = content
	existing.UpdatedAt = time.Now()
	existing.Revision++
	if meta != nil {
		existing.Metadata = *meta
	}

	metaBytes, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("artifacts: encoding metadata for %q: %w", clean, err)
	}
	// Content first, then the record of it: a failure between the two leaves
	// the new content under the previous revision number, which is the
	// direction that still tells the truth about which revision was last
	// recorded.
	if err := writeContained(cPath, []byte(content), 0o644, false); err != nil {
		return nil, err
	}
	if err := writeContained(mPath, metaBytes, 0o644, false); err != nil {
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
		if strings.HasSuffix(d.Name(), metaSuffix) {
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
// creating the store directory and the directories beneath it, and refuses if
// any component below the store directory is a symbolic link or is not a
// directory. Only the two writes call it, which is what keeps a store that is
// never written from ever touching the disk.
func (s *Store) containedPath(clean string) (string, error) {
	root, err := filepath.Abs(s.dir)
	if err != nil {
		return "", fmt.Errorf("artifacts: resolving store dir %s: %w", s.dir, err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("artifacts: creating store dir %s: %w", s.dir, err)
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

// writeContained writes data at full without ever leaving a half-written file
// at that name: the bytes go to a temporary sibling, are synced, and are then
// published by rename (replacing) or by link (exclusive, failing if the name is
// taken). A symbolic link at the final component is refused, and the directory
// is synced so the name survives a crash.
//
// The sibling is created with O_EXCL and O_NOFOLLOW, so nothing planted at its
// name is followed; the final name is never opened for writing, so a link
// swapped in at it cannot redirect a write — rename replaces the link itself.
func writeContained(full string, data []byte, perm os.FileMode, exclusive bool) (result error) {
	base := filepath.Base(full)
	if info, err := os.Lstat(full); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("artifacts: refusing %q: that name is a symbolic link", base)
		}
		if exclusive {
			return fmt.Errorf("artifacts: artifact %q already exists; use Update to modify", base)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("artifacts: refusing to write %s: not a regular file", base)
		}
	}

	dir := filepath.Dir(full)
	var (
		tmp  string
		f    *os.File
		open error
	)
	for attempt := 0; attempt < 16; attempt++ {
		var nonce [8]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return fmt.Errorf("artifacts: naming a temporary file for %s: %w", base, err)
		}
		tmp = filepath.Join(dir, "."+base+"."+hex.EncodeToString(nonce[:])+".tmp")
		f, open = safefile.OpenNoFollow(nil, tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if !errors.Is(open, fs.ErrExist) {
			break
		}
	}
	if open != nil {
		return fmt.Errorf("artifacts: creating a temporary file for %s: %w", base, open)
	}
	defer func() {
		// Rename consumed the temp name; after a link it is still there.
		if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
			result = errors.Join(result, err)
		}
	}()

	if _, err := f.Write(data); err != nil {
		return errors.Join(fmt.Errorf("artifacts: writing %s: %w", base, err), f.Close())
	}
	if err := syncFile(f); err != nil {
		return errors.Join(fmt.Errorf("artifacts: syncing %s: %w", base, err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("artifacts: closing %s: %w", base, err)
	}

	if exclusive {
		if err := os.Link(tmp, full); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("artifacts: artifact %q already exists; use Update to modify", base)
			}
			return fmt.Errorf("artifacts: publishing %s: %w", base, err)
		}
	} else if err := os.Rename(tmp, full); err != nil {
		return fmt.Errorf("artifacts: publishing %s: %w", base, err)
	}

	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("artifacts: opening the directory of %s to sync it: %w", base, err)
	}
	return errors.Join(d.Sync(), d.Close())
}
