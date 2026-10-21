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
	"io"
	"io/fs"
	"os"
	"path"
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
	// content read back as one, so no artifact may be called that. The test is
	// case-blind and covers every path component: on a case-insensitive
	// filesystem (APFS and NTFS by default) "x.META.JSON" is the sidecar of "x",
	// and a directory called "x.meta.json" occupies the sidecar's name.
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if isSidecarName(part) {
			return "", fmt.Errorf("artifacts: invalid artifact name %q: names ending in %s are reserved for metadata", name, metaSuffix)
		}
		if isStoreTemp(part) {
			return "", fmt.Errorf("artifacts: invalid artifact name %q: that shape is reserved for the store's temporary files", name)
		}
	}
	return clean, nil
}

// metaSuffix is appended to an artifact's name to give its metadata file.
const metaSuffix = ".meta.json"

// isSidecarName reports whether a single path component is in the metadata
// namespace, ignoring case.
func isSidecarName(component string) bool {
	return strings.HasSuffix(strings.ToLower(component), metaSuffix)
}

// isStoreTemp reports whether name is a temporary sibling left by writeContained
// (".<base>.<16 hex>.tmp"): a crash between create and publish leaves one, and
// it is not an artifact.
func isStoreTemp(name string) bool {
	if !strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".tmp") {
		return false
	}
	stem := strings.TrimSuffix(name, ".tmp")
	i := strings.LastIndex(stem, ".")
	if i <= 0 || len(stem)-i-1 != 16 {
		return false
	}
	_, err := hex.DecodeString(stem[i+1:])
	return err == nil
}

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
	if err := checkSize(clean, content); err != nil {
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
	if err := checkSize(clean, content); err != nil {
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
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return nil, fmt.Errorf("artifacts: opening store dir %s: %w", s.dir, err)
	}
	metaData, err := readContained(root, clean+metaSuffix, maxSidecarBytes, true)
	var nameErr error
	if err == nil {
		nameErr = requireOnDiskName(root, clean)
	}
	_ = root.Close()
	if nameErr != nil {
		return nil, fmt.Errorf("artifacts: artifact %q not found; use Create to make it", clean)
	}
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

	root, err := os.OpenRoot(s.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("artifacts: artifact %q not found", clean)
		}
		return nil, fmt.Errorf("artifacts: opening store dir %s: %w", s.dir, err)
	}
	defer func() { _ = root.Close() }()

	art, err := readArtifact(root, clean, s.contentPath(clean), true)
	if err == nil {
		err = requireOnDiskName(root, clean)
	}
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("artifacts: artifact %q not found", clean)
		}
		return nil, err
	}
	return art, nil
}

// requireOnDiskName fails with fs.ErrNotExist unless every component of clean
// is spelled on disk exactly as requested. On a case-insensitive filesystem
// (APFS and NTFS by default) "PLAN.md" opens plan.md, and an artifact reported
// under a name List never returns is a second identity for one file. The store
// already treats the sidecar namespace as case-blind; it does not treat names
// as case-blind, so a spelling that differs is not that artifact.
func requireOnDiskName(root *os.Root, clean string) error {
	dir := "."
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		d, err := root.Open(filepath.FromSlash(dir))
		if err != nil {
			return err
		}
		entries, err := d.ReadDir(-1)
		_ = d.Close()
		if err != nil {
			return err
		}
		found := false
		for _, e := range entries {
			if e.Name() == part {
				found = true
				break
			}
		}
		if !found {
			return fs.ErrNotExist
		}
		dir = path.Join(dir, part)
	}
	return nil
}

// maxArtifactBytes bounds one artifact's content, on write and on read.
// Artifacts are plans, notes and lessons read whole into memory by tools; the
// store had no bound, so a sparse or planted file could be read to exhaustion.
//
// maxSidecarBytes bounds a sidecar on read. A sidecar embeds the artifact's
// content as JSON, which escaping can expand up to six-fold, so it is not
// bounded by the content limit itself.
const (
	maxArtifactBytes = 4 << 20
	maxSidecarBytes  = 8 * maxArtifactBytes
)

// checkSize refuses content the store would then refuse to read back.
func checkSize(clean, content string) error {
	if len(content) > maxArtifactBytes {
		return fmt.Errorf("artifacts: refusing %q: %d bytes exceeds the %d-byte artifact limit", clean, len(content), maxArtifactBytes)
	}
	return nil
}

// EntryError names one store entry List could not return and why.
type EntryError struct {
	Name   string
	Reason string
}

// PartialListError is what List returns beside the artifacts it could read when
// it had to leave entries out: a link, a corrupt sidecar, an oversized or
// unreadable file. One bad entry must not hide every good one (a repository can
// commit a symlink under the artifacts directory), and it must not be hidden
// either, so each skipped entry is carried here with its reason.
type PartialListError struct{ Skipped []EntryError }

func (e *PartialListError) Error() string {
	parts := make([]string, len(e.Skipped))
	for i, s := range e.Skipped {
		parts[i] = s.Name + ": " + s.Reason
	}
	return fmt.Sprintf("artifacts: %d entries could not be listed: %s", len(e.Skipped), strings.Join(parts, "; "))
}

// readArtifact reads an artifact's content and sidecar through root. A missing
// sidecar is an artifact with no recorded history (revision 0); a sidecar that
// is present but cannot be trusted (a link, not a regular file, unreadable,
// corrupt) is an error, as it is for Update, never revision 0. A missing
// content file is returned as fs.ErrNotExist for the caller to word.
//
// With withContent false the content file is still opened and checked (so a
// link, a hard link, a non-regular or oversized file is refused) but not read,
// and Content is empty.
func readArtifact(root *os.Root, clean, fullPath string, withContent bool) (*Artifact, error) {
	contentBytes, err := readContained(root, clean, maxArtifactBytes, withContent)
	if err != nil {
		return nil, err
	}
	art := &Artifact{}
	metaBytes, err := readContained(root, clean+metaSuffix, maxSidecarBytes, true)
	switch {
	case err == nil:
		if err := json.Unmarshal(metaBytes, art); err != nil {
			return nil, fmt.Errorf("artifacts: the metadata of %q is corrupt: %w", clean, err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("artifacts: the metadata of %q is unreadable: %w", clean, err)
	}
	art.Name = clean
	art.Path = fullPath
	art.Content = string(contentBytes)
	return art, nil
}

// readContained reads the regular file rel beneath root through the same
// no-follow open the writes use (safefile.OpenNoFollow): no component below the
// root is followed if it is a link, the leaf is opened O_NOFOLLOW (and
// O_NONBLOCK, so a FIFO planted at the name cannot hang the read), and the
// handle itself, not a separate lstat, is checked to be a regular file. The
// A file with more than one hard link is refused too (as the DevCouncil
// untracked-diff reader does): O_NOFOLLOW does not see a hard link to a file
// outside the store, and a published artifact has exactly one name. A file over
// limit bytes is refused. The error names the path; only a name that is absent
// altogether wraps fs.ErrNotExist.
func readContained(root *os.Root, rel string, limit int64, load bool) ([]byte, error) {
	f, err := safefile.OpenNoFollow(root, filepath.FromSlash(rel), os.O_RDONLY, 0)
	if err != nil {
		// os.Root can answer "does not exist" for a dangling link it resolved
		// itself; a name that is present as anything is a refusal, not an
		// absence, so that case must not wrap fs.ErrNotExist.
		if errors.Is(err, fs.ErrNotExist) {
			if _, lerr := root.Lstat(filepath.FromSlash(rel)); errors.Is(lerr, fs.ErrNotExist) {
				return nil, err
			}
			return nil, fmt.Errorf("artifacts: refusing to read %q (a dangling symbolic link): %s", rel, err.Error())
		}
		return nil, fmt.Errorf("artifacts: refusing to read %q (a symbolic link, or not an ordinary path): %w", rel, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("artifacts: inspecting %q: %w", rel, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("artifacts: refusing to read %q: not a regular file", rel)
	}
	links, err := safefile.LinkCount(f)
	if err != nil {
		return nil, fmt.Errorf("artifacts: counting the links of %q: %w", rel, err)
	}
	if links != 1 {
		return nil, fmt.Errorf("artifacts: refusing to read %q: it has %d hard links, so it may be a file outside the store", rel, links)
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("artifacts: refusing to read %q: %d bytes exceeds the %d-byte limit", rel, info.Size(), limit)
	}
	if !load {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("artifacts: reading %q: %w", rel, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("artifacts: refusing to read %q: it grew past the %d-byte limit", rel, limit)
	}
	return data, nil
}

// List returns the artifacts in the store, sorted by name, with their metadata
// but not their content (Content is empty: the one production caller discards
// it, and loading every artifact whole to list them is the cost the read cap
// exists to bound; use Get for content). Every entry is still opened and
// checked as Get would.
//
// An entry that cannot be returned (a link, a hard link, a corrupt sidecar, an
// oversized or unreadable file) does not hide the others: the artifacts that
// could be read are returned together with a *PartialListError naming each
// skipped entry and why. A failure on the store directory itself returns no
// artifacts and a plain error.
func (s *Store) List() ([]*Artifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	root, err := os.OpenRoot(s.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("artifacts: opening store dir %s: %w", s.dir, err)
	}
	defer func() { _ = root.Close() }()

	var (
		arts    []*Artifact
		skipped []EntryError
	)
	err = filepath.WalkDir(s.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if p == s.dir {
				return fmt.Errorf("artifacts: walking %s: %w", p, err)
			}
			skipped = append(skipped, EntryError{Name: relName(s.dir, p), Reason: err.Error()})
			return nil
		}
		if d.IsDir() || isSidecarName(d.Name()) || isStoreTemp(d.Name()) {
			return nil
		}

		name := relName(s.dir, p)
		art, err := readArtifact(root, name, p, false)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // removed while the walk was running
			}
			skipped = append(skipped, EntryError{Name: name, Reason: err.Error()})
			return nil
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
	if len(skipped) > 0 {
		sort.Slice(skipped, func(i, j int) bool { return skipped[i].Name < skipped[j].Name })
		return arts, &PartialListError{Skipped: skipped}
	}
	return arts, nil
}

// relName is p's slash-separated name relative to the store directory dir.
func relName(dir, p string) string {
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(rel)
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
