// Package artifacts manages persistent, structured project and session artifacts
// (implementation plans, walkthroughs, research notes, architecture designs, UI mockups)
// stored under .devcouncil/artifacts/ or a custom directory.
package artifacts

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bharathvbcr/Manvi/manvi/internal/filelock"
	"github.com/bharathvbcr/Manvi/manvi/internal/syncdir"
	"github.com/bharathvbcr/Manvi/manvi/internal/treefile"
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
//
// The store directory is never trusted as a path string. Every operation opens
// it afresh through treefile, which refuses a directory that is, or passes
// through, a symbolic link below the store's anchor, and every read, write, lock
// and sync then goes through that handle.
type Store struct {
	mu sync.RWMutex
	// dir is the store directory as a path, for the Path an artifact reports.
	dir string
	// anchor is the directory the caller vouches for, and rel the path below it
	// that is checked component by component. Both are empty on a zero Store,
	// which resolves each write against the directory of the file it is given.
	anchor, rel string
	// dirSync flushes the directory a write was just published into. Nil means
	// syncdir.Root, the one shared owner of the platform rule.
	dirSync func(dir string) error
}

// NewStore returns an artifact store over dir. The directory is created on the
// first write, not here: the store is built for every tool surface, 'manvi
// tools' listing included, and a surface that never writes an artifact must
// not leave a directory behind in the checkout it was built over.
//
// A relative dir is checked from the working directory down, component by
// component; an absolute dir is anchored at its parent, so its last component
// is checked and the path above it is the caller's to vouch for. A store over
// a directory that is a symbolic link is refused. NewStoreUnder is the form for
// a caller that knows the project root.
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		dir = ".devcouncil/artifacts"
	}
	clean := filepath.Clean(dir)
	if !filepath.IsAbs(clean) && filepath.IsLocal(clean) {
		return newStore(".", clean)
	}
	// The anchor is the deepest ancestor that exists: what the store will make
	// below it is checked as it is made, and what is above it is the caller's.
	anchor := filepath.Dir(clean)
	for {
		if info, err := os.Stat(anchor); err == nil && info.IsDir() {
			break
		}
		parent := filepath.Dir(anchor)
		if parent == anchor {
			break
		}
		anchor = parent
	}
	rel, err := filepath.Rel(anchor, clean)
	if err != nil {
		return newStore(clean, ".")
	}
	return newStore(anchor, rel)
}

// NewStoreUnder returns an artifact store over rel below anchor. The anchor is
// the project root, which the caller vouches for; every component of rel is
// checked, so a state directory that is, or passes through, a symbolic link is
// refused rather than written through.
func NewStoreUnder(anchor, rel string) (*Store, error) {
	rel = filepath.Clean(rel)
	if !filepath.IsLocal(rel) && rel != "." {
		return nil, fmt.Errorf("artifacts: store dir %q does not stay below %s", rel, anchor)
	}
	return newStore(anchor, rel)
}

func newStore(anchor, rel string) (*Store, error) {
	dir := filepath.Join(anchor, rel)
	// Lstat, not Stat: the store must not be built over a link, whatever it
	// points at. The walk at every operation repeats this for each component.
	if info, err := os.Lstat(dir); err == nil { // #nosec G703 -- dir is the configured store root plus a component already validated; Lstat reads it without following a link
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: artifacts: store dir %s is a symbolic link; refusing to write through it", treefile.ErrRefused, dir)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("artifacts: store dir %s is not a directory", dir)
		}
	}
	return &Store{dir: dir, anchor: anchor, rel: rel}, nil
}

// syncFile is the durability step of a write, a variable so a test can make it
// fail. Nothing else assigns it.
var syncFile = func(f *os.File) error { return f.Sync() }

// NewStoreWithDirSync is NewStore with the directory flush supplied: sync is
// called with the directory of every write after it is published, in place of
// syncdir.Dir, and its error is what a write's ErrPublishedUnsynced wraps. It
// exists so a caller above this package can make a write publish without
// confirming durability, which no real filesystem will do on demand; a nil sync
// is NewStore.
func NewStoreWithDirSync(dir string, sync func(dir string) error) (*Store, error) {
	s, err := NewStore(dir)
	if err != nil {
		return nil, err
	}
	s.dirSync = sync
	return s, nil
}

// sanitizeName ensures an artifact name contains no traversal elements.
func (s *Store) sanitizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("artifacts: name cannot be empty")
	}
	clean := filepath.Clean(name)
	// A name is relative to the store on every platform. filepath.IsAbs is not
	// enough on Windows, where a leading slash or backslash is rooted without
	// being absolute (no drive) and "C:x" is drive-relative; a rooted name
	// would be accepted and joined under the store. Both separators and any
	// volume are refused on every platform, so the same name means the same
	// thing everywhere.
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) || filepath.VolumeName(clean) != "" ||
		strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, `\`) {
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
	// The name is slash-separated whatever the platform: it is what List reports
	// (relName) and what a caller passed, so Get and Create must not hand it
	// back as "lessons\\a.md" on Windows.
	return filepath.ToSlash(clean), nil
}

// metaSuffix is appended to an artifact's name to give its metadata file.
const metaSuffix = ".meta.json"

// isSidecarName reports whether a single path component is in the metadata
// namespace under Unicode simple case folding, the folding the file system
// applies, not lower-casing: the long s (U+017F) is an s to APFS, so
// "x.meta.j\u017fon" is the sidecar of x there, and ToLower, which leaves it
// alone, let it through as a name of its own.
func isSidecarName(component string) bool {
	runes := []rune(component)
	n := utf8.RuneCountInString(metaSuffix)
	return len(runes) >= n && strings.EqualFold(string(runes[len(runes)-n:]), metaSuffix)
}

// checkAliases refuses a new artifact name any component of which would alias an
// entry already in the store under a different spelling, so that Create cannot
// land in another artifact's directory or beside its file and report a name that
// Get then does not find. An entry spelled exactly as asked is not an alias; it
// is reached, or refused as existing, by the write. Nothing is renamed.
func (s *Store) checkAliases(clean string) error {
	root, err := s.openRoot(false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // an empty store has nothing to alias
	}
	if err != nil {
		return err
	}
	current := root
	defer func() { _ = current.Close() }()
	parts := strings.Split(filepath.ToSlash(clean), "/")
	for i, part := range parts {
		if err := treefile.CheckAlias(current, part); err != nil {
			return fmt.Errorf("artifacts: refusing %q: %w", clean, err)
		}
		if i == len(parts)-1 {
			return nil
		}
		next, err := treefile.Child(current, part, 0)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, treefile.ErrRefused) {
			// Nothing below this component exists, or what is there is not a
			// directory, which the write itself refuses with its own reason.
			return nil
		}
		if err != nil {
			return fmt.Errorf("artifacts: checking %q: %w", clean, err)
		}
		if err := current.Close(); err != nil {
			_ = next.Close()
			return err
		}
		current = next
	}
	return nil
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

func (s *Store) contentPath(cleanName string) string {
	return filepath.Join(s.dir, cleanName)
}

// Create creates a new artifact, returning an error if it already exists.
//
// An error that matches ErrPublishedUnsynced is not a failure: the artifact is
// written and readable, and the returned *Artifact is non-nil beside it. See
// that sentinel.
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

	if err := s.checkAliases(clean); err != nil {
		return nil, err
	}
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
	//
	// A write that published but could not confirm its directory sync carries
	// on to the sidecar: the content is already there, and stopping would leave
	// it without its record, which Get reads as revision 0 (another writer
	// mid-create) and which lessons.Save would retry until it gave up.
	contentErr := s.writeContained(cPath, []byte(content), 0o644, true)
	if contentErr != nil && !errors.Is(contentErr, ErrPublishedUnsynced) {
		return nil, contentErr
	}
	metaErr := s.writeContained(metaFull, metaBytes, 0o644, false)
	if metaErr != nil && !errors.Is(metaErr, ErrPublishedUnsynced) {
		// This call created the content, so taking it away leaves the store as
		// it was found.
		_ = s.removeContained(cPath)
		return nil, metaErr
	}

	// Both are published. The error, if any, is the unconfirmed sync of one or
	// both, and it travels with the artifact, never instead of it.
	return art, errors.Join(contentErr, metaErr)
}

// Sentinels a caller can branch on. Their text is unchanged from the messages
// they were added to, so nothing that matched on the words stops matching.
var (
	// ErrExists is Create's answer when the name is taken.
	ErrExists = errors.New("already exists")
	// ErrNotFound is Get's answer for a name with no readable artifact.
	ErrNotFound = errors.New("not found")
	// ErrRevisionConflict is UpdateIfRevision's answer when the artifact is no
	// longer at the revision the caller read.
	ErrRevisionConflict = errors.New("revision conflict")
	// ErrLocked is an update's answer when another writer held the entry's lock
	// for longer than lockWait.
	ErrLocked = errors.New("locked by another writer")
	// ErrPublishedUnsynced reports a write that is committed whose durability
	// is not confirmed: the content and its record were published, and the
	// sync of their directory failed, so after a crash the new name might not
	// be there.
	//
	// It is a distinct sentinel because the distinction is the whole point. A
	// caller that reads it as "not written" and retries gets ErrExists from
	// Create, or a second revision from Update; one that reads it as plain
	// success hides a durability risk. Nothing is rolled back: the artifact is
	// readable by Get and List now, and taking it away would trade an
	// unconfirmed write for a certain loss.
	//
	// Create, Update and UpdateIfRevision return it WITH a non-nil *Artifact,
	// the one that was written, which departs from the usual error-or-value
	// shape on purpose. The error text names the cause.
	ErrPublishedUnsynced = treefile.ErrPublishedUnsynced
)

// Update updates an existing artifact's content and metadata, incrementing its
// revision. An error that matches ErrPublishedUnsynced is not a failure: the
// new revision is written, and the *Artifact returned beside it is non-nil.
func (s *Store) Update(name, content string, meta *Metadata) (*Artifact, error) {
	return s.update(name, content, meta, 0)
}

// UpdateIfRevision is Update as a compare-and-swap: it writes only if the
// artifact is still at revision expected, and returns ErrRevisionConflict if
// another writer, in this process or another, got there first. A caller that
// derives the new content from what it read (a revision that appends to a
// history) must use it, because Update would write its derivation over a
// revision it never saw.
func (s *Store) UpdateIfRevision(name, content string, meta *Metadata, expected int) (*Artifact, error) {
	if expected < 1 {
		return nil, fmt.Errorf("artifacts: UpdateIfRevision needs the revision the caller read (>= 1), got %d", expected)
	}
	return s.update(name, content, meta, expected)
}

// lockNonce makes an entry's lock file take the shape of the store's own
// temporary siblings (".<base>.<16 hex>.tmp"), which List skips and sanitizeName
// reserves, so the lock is neither listed as an artifact nor nameable as one.
const lockNonce = "0000000000000000"

// lockWait bounds how long a live contender queues for an entry's lock. The
// lock is held for the few milliseconds an update takes. It is a variable so a
// test can shorten it.
var lockWait = 10 * time.Second

// lockEntry takes the cross-process lock for the entry at full: an exclusive
// kernel lock (internal/filelock, through treefile) on a lock file beside it.
// The kernel drops the lock when its holder exits, so a crashed writer never
// wedges the others and the lock file's age is never consulted; the file is
// never removed, because removing it lets two writers lock two inodes of one
// name. The in-process mutex cannot serve two processes, and two Stores in one
// process do not share it. The revision compare in update is the second line of
// defence against a write over a revision the writer did not read.
func (s *Store) lockEntry(full string) (func(), error) {
	dir, leaf, err := s.dirFor(full, false)
	if err != nil {
		return nil, fmt.Errorf("artifacts: locking %s: %w", filepath.Base(full), err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), lockWait)
	defer cancel()
	unlock, err := treefile.Lock(ctx, dir, "."+leaf+"."+lockNonce+".tmp")
	if err != nil {
		_ = dir.Close()
		switch {
		case errors.Is(err, filelock.ErrUnsupported):
			return nil, fmt.Errorf("artifacts: no cross-process file lock on this platform; refusing to update %q unlocked: %w", leaf, err)
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("artifacts: %q is %w for more than %s", leaf, ErrLocked, lockWait)
		}
		return nil, fmt.Errorf("artifacts: locking %s: %w", leaf, err)
	}
	return func() { unlock(); _ = dir.Close() }, nil
}

func (s *Store) update(name, content string, meta *Metadata, expected int) (*Artifact, error) {
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
	root, err := s.openRoot(false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("artifacts: artifact %q not found; use Create to make it", clean)
		}
		return nil, err
	}
	defer func() { _ = root.Close() }()
	notFound := fmt.Errorf("artifacts: artifact %q not found; use Create to make it", clean)

	// Update modifies what exists, so the entry's directory exists whenever
	// there is anything to lock; when it does not, the existence check below
	// answers and no lock file is made.
	if info, statErr := root.Lstat(filepath.FromSlash(clean)); statErr == nil && info.Mode().IsRegular() {
		unlock, lockErr := s.lockEntry(cPath)
		if lockErr != nil {
			return nil, lockErr
		}
		defer unlock()
	}

	// Update modifies an artifact that exists; it never creates one, and it
	// never invents a history for one whose record it cannot read. Resetting
	// to revision 1 over a damaged sidecar would be a silent loss of the real
	// revision count and created_at.
	if info, err := root.Lstat(filepath.FromSlash(clean)); err != nil || !info.Mode().IsRegular() {
		return nil, notFound
	}
	metaData, err := readContained(root, clean+metaSuffix, maxSidecarBytes, true)
	var nameErr error
	if err == nil {
		nameErr = requireOnDiskName(root, clean)
	}
	if nameErr != nil {
		return nil, notFound
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
	if expected > 0 && existing.Revision != expected {
		return nil, fmt.Errorf("artifacts: %q is at revision %d, not the %d the caller read: %w",
			clean, existing.Revision, expected, ErrRevisionConflict)
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
	//
	// As in Create, content published without its directory sync confirmed goes
	// on to the record, so the revision on disk is the revision of the content.
	contentErr := s.writeContained(cPath, []byte(content), 0o644, false)
	if contentErr != nil && !errors.Is(contentErr, ErrPublishedUnsynced) {
		return nil, contentErr
	}
	metaErr := s.writeContained(mPath, metaBytes, 0o644, false)
	if metaErr != nil && !errors.Is(metaErr, ErrPublishedUnsynced) {
		return nil, metaErr
	}

	return &existing, errors.Join(contentErr, metaErr)
}

// Get retrieves an artifact by name.
func (s *Store) Get(name string) (*Artifact, error) {
	clean, err := s.sanitizeName(name)
	if err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	root, err := s.openRoot(false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("artifacts: artifact %q %w", clean, ErrNotFound)
		}
		return nil, err
	}
	defer func() { _ = root.Close() }()

	art, err := readArtifact(root, clean, s.contentPath(clean), true)
	if err != nil {
		return nil, notFoundOr(clean, err)
	}
	if err := requireOnDiskName(root, clean); err != nil {
		return nil, notFoundOr(clean, err)
	}
	return art, nil
}

// notFoundOr words a missing artifact as ErrNotFound and passes every other
// error through unchanged.
func notFoundOr(clean string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("artifacts: artifact %q %w", clean, ErrNotFound)
	}
	return err
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

// readContained reads the regular file rel beneath root through treefile: no
// component below the root is followed if it is a link, the leaf is opened
// O_NOFOLLOW and O_NONBLOCK (so a FIFO planted at the name cannot hang the
// read), the handle itself is checked to be a regular file, and a file with
// more than one hard link is refused: O_NOFOLLOW does not see a hard link to a
// file outside the store, and a published artifact has exactly one name. A file
// over limit bytes is refused. The error names the path; only a name that is
// absent altogether wraps fs.ErrNotExist.
func readContained(root *os.Root, rel string, limit int64, load bool) ([]byte, error) {
	opts := treefile.ReadOptions{Limit: limit, SingleLink: true}
	var (
		data []byte
		err  error
	)
	if load {
		data, err = treefile.ReadFile(root, rel, opts)
	} else {
		err = treefile.Check(root, rel, opts)
	}
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !errors.Is(err, treefile.ErrRefused) {
			return nil, err
		}
		return nil, fmt.Errorf("artifacts: refusing to read %q: %w", rel, err)
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

	root, err := s.openRoot(false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = root.Close() }()

	var (
		arts    []*Artifact
		skipped []EntryError
	)
	// Walked through the root's own file system, so the walk cannot leave the
	// directory the root was opened on and shows a link as an entry, not as what
	// it points at.
	err = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			if name == "." {
				return fmt.Errorf("artifacts: walking %s: %w", s.dir, err)
			}
			skipped = append(skipped, EntryError{Name: name, Reason: err.Error()})
			return nil
		}
		if d.IsDir() || isSidecarName(d.Name()) || isStoreTemp(d.Name()) {
			return nil
		}

		art, err := readArtifact(root, name, s.contentPath(name), false)
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

// openRoot opens the store directory afresh and returns it as a root, after
// treefile has checked every component below the store's anchor. With create
// the directory and its missing parents are made through the same handles, which
// is what keeps a store that is never written from ever touching the disk. A
// directory that is not there is fs.ErrNotExist; one that is, or passes through,
// a link is a refusal.
func (s *Store) openRoot(create bool) (*os.Root, error) {
	var mode fs.FileMode
	if create {
		mode = 0o755
		if err := s.ensureAnchor(); err != nil {
			return nil, err
		}
	}
	root, err := treefile.OpenDir(s.anchor, s.rel, mode)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("artifacts: opening store dir %s: %w", s.dir, err)
	}
	return root, nil
}

// ensureAnchor makes the anchor itself on a write when it is not there yet. The
// anchor is the one path the caller vouches for (the project root, or an
// absolute MANVI_STATE_DIR an operator named, which on a fresh machine has not
// been created), so it is made by path, as it always was; everything below it is
// made and checked through handles.
func (s *Store) ensureAnchor() error {
	if s.anchor == "" {
		return nil
	}
	if err := os.MkdirAll(s.anchor, 0o755); err != nil { // #nosec G703 -- s.anchor is the store root the caller configured
		return fmt.Errorf("artifacts: creating %s: %w", s.anchor, err)
	}
	return nil
}

// dirFor opens the directory that holds the physical path full, which lies
// within the store, and names its leaf. With create the directories below the
// store are made, each inspected after it is made (os.Mkdir answers EEXIST for a
// link, so creating without looking proves nothing). A zero Store, which has no
// anchor, treats the directory of full as the one it vouches for.
func (s *Store) dirFor(full string, create bool) (dir *os.Root, leaf string, err error) {
	var mode fs.FileMode
	if create {
		mode = 0o755
		if err := s.ensureAnchor(); err != nil {
			return nil, "", err
		}
	}
	if s.anchor == "" {
		dir, err = treefile.OpenDir(filepath.Dir(full), ".", mode)
		return dir, filepath.Base(full), err
	}
	// Both sides absolute: a relative store dir (the default is one) and the
	// absolute path containedPath builds from it are the same place, and Rel
	// refuses to compare a relative path with an absolute one.
	absDir, err := filepath.Abs(s.dir)
	if err != nil {
		return nil, "", fmt.Errorf("artifacts: resolving store dir %s: %w", s.dir, err)
	}
	absFull, err := filepath.Abs(full)
	if err != nil {
		return nil, "", fmt.Errorf("artifacts: resolving %s: %w", full, err)
	}
	rel, err := filepath.Rel(absDir, absFull)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, "", fmt.Errorf("artifacts: %q is not inside the store", full)
	}
	dir, err = openDirNamed(s.anchor, s.rel, filepath.Dir(rel), mode)
	return dir, filepath.Base(rel), err
}

// dirLockName is the lock file in a directory that serialises the creation of its
// subdirectories, so two spellings of one name cannot both be made. It has the
// shape of the store's own temporary files, which List skips and no artifact can
// be named, like the per-entry lock files.
const dirLockName = "." + "dirs." + lockNonce + ".tmp"

// openDirNamed opens the store directory storeRel below anchor and then sub below
// it, making the missing directories when mode is non-zero. Those below the store
// are made alias-safe: the check that no entry aliases the new name and the
// creation are one step under the directory's lock, across processes
// (treefile.ChildAliasSafe). The store directory and what leads to it are not:
// they are the operator's, and no lock file belongs in the project root.
func openDirNamed(anchor, storeRel, sub string, mode fs.FileMode) (*os.Root, error) {
	store, err := treefile.OpenDir(anchor, storeRel, mode)
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()
	return treefile.DescendAliasSafe(store, sub, mode, dirLockName)
}

// containedPath returns the physical path for a cleaned artifact name, creating
// the store directory and the directories beneath it, and refuses if any
// component below the store directory is a symbolic link or is not a directory.
// Only the two writes call it, which is what keeps a store that is never
// written from ever touching the disk.
func (s *Store) containedPath(clean string) (string, error) {
	root, err := filepath.Abs(s.dir)
	if err != nil {
		return "", fmt.Errorf("artifacts: resolving store dir %s: %w", s.dir, err)
	}
	full := filepath.Join(root, filepath.FromSlash(clean))
	dir, _, err := s.dirFor(full, true)
	if err != nil {
		return "", fmt.Errorf("artifacts: refusing %q: %w", clean, err)
	}
	_ = dir.Close()
	return full, nil
}

// removeContained removes the file at the physical path full through the
// store's root, never through the path.
func (s *Store) removeContained(full string) error {
	dir, leaf, err := s.dirFor(full, false)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Remove(leaf)
}

// writeContained writes data at the physical path full without ever leaving a
// half-written file at that name: treefile stages it in a synced temporary
// sibling and publishes it by rename (replacing) or by link (exclusive, failing
// if the name is taken). A symbolic link at the final component is refused, and
// the directory is synced so the name survives a crash.
//
// A failure of that last step comes back wrapping ErrPublishedUnsynced: by then
// the name is published, so the write is neither undone nor reported as not
// written.
func (s *Store) writeContained(full string, data []byte, perm os.FileMode, exclusive bool) error {
	dir, base, err := s.dirFor(full, true)
	if err != nil {
		return fmt.Errorf("artifacts: refusing to write %q: %w", filepath.Base(full), err)
	}
	defer func() { _ = dir.Close() }()
	if info, err := dir.Lstat(base); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("artifacts: refusing %q: that name is a symbolic link", base)
		}
		if exclusive {
			return fmt.Errorf("artifacts: artifact %q %w; use Update to modify", base, ErrExists)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("artifacts: refusing to write %s: not a regular file", base)
		}
	}
	err = treefile.Publish(dir, base, data, treefile.PublishOptions{
		Perm:     perm,
		Replace:  !exclusive,
		SyncFile: syncFile,
		SyncDir: func(d *os.Root) error {
			if s.dirSync != nil {
				return s.dirSync(filepath.Dir(full))
			}
			return syncdir.Root(d)
		},
	})
	switch {
	case err == nil:
		return nil
	case exclusive && errors.Is(err, fs.ErrExist):
		return fmt.Errorf("artifacts: artifact %q %w; use Update to modify", base, ErrExists)
	}
	return fmt.Errorf("artifacts: %w", err)
}
