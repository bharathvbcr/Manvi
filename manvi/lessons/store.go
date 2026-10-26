package lessons

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/artifacts"
)

// Where lessons live: .devcouncil/artifacts/lessons/<library>/<slug>.md, in the
// artifact store every other artifact uses. There is deliberately nothing else —
// no table, no index file — because the store under .devcouncil/ that holds
// state.sqlite is DevCouncil's and is read by other applications at a schema
// version this package would have to track. A directory of Markdown needs no
// schema, and a person can read, edit and promote an entry with the tools they
// already have.

// libraryRoot is the artifact-store prefix every lesson is under.
const libraryRoot = "lessons"

// Bounds on the names the store is handed.
const (
	MaxLibraryLen = 64
	MaxSlugLen    = 48
)

var libraryPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ValidateLibrary refuses a library name that is anything but one plain path
// component of lowercase letters, digits, underscore and hyphen. The library is
// the host application or repository the lessons are about, chosen by a person
// at the command line, so it is checked as a boundary rather than trusted: a
// name with a separator, a dot-dot or a leading dot is how a lesson would land
// outside lessons/.
func ValidateLibrary(library string) error {
	if library == "" || len(library) > MaxLibraryLen || !libraryPattern.MatchString(library) {
		return fmt.Errorf("lessons: library %q is not allowed: use 1-%d characters from a-z, 0-9, _ and -, "+
			"starting with a letter or digit", library, MaxLibraryLen)
	}
	return nil
}

// DefaultLibrary derives the library for a repository from the name of its
// root directory, made to fit ValidateLibrary. A root with no usable name is an
// error rather than a guess: a lesson filed under a made-up library is one
// nobody will look for.
//
// The name comes from the main repository, not from the checkout: a linked git
// worktree is a directory named for a branch or an agent, and lessons filed
// under it would be scattered across one library per worktree. Where root is a
// linked worktree, its main repository is the parent of the common git dir.
func DefaultLibrary(root string) (string, error) {
	root = mainRepositoryRoot(root)
	base := strings.ToLower(filepath.Base(filepath.Clean(root)))
	var b strings.Builder
	lastHyphen := true
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			lastHyphen = false
		default:
			if !lastHyphen {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	name := strings.Trim(b.String(), "-_")
	if len(name) > MaxLibraryLen {
		name = strings.Trim(name[:MaxLibraryLen], "-_")
	}
	if err := ValidateLibrary(name); err != nil {
		return "", fmt.Errorf("lessons: the repository root %q does not give a usable library name; pass --library", root)
	}
	return name, nil
}

// maxGitFileBytes bounds the two small git files read to find the main
// repository; a real one is a line long.
const maxGitFileBytes = 4096

// mainRepositoryRoot returns the root of the repository root belongs to, or root
// itself when it is not a linked worktree or cannot be told. In a linked
// worktree ".git" is a file reading "gitdir: <main>/.git/worktrees/<name>", and
// that directory's "commondir" names the common git dir (usually "../.."),
// whose parent is the main checkout. Done by reading those two files rather
// than running git, so it works where git does not and cannot be made to
// execute anything.
func mainRepositoryRoot(root string) string {
	gitFile := filepath.Join(root, ".git")
	info, err := os.Lstat(gitFile)
	if err != nil || !info.Mode().IsRegular() {
		return root // a normal checkout (a directory), or not a repository
	}
	line, ok := readSmallFile(gitFile)
	if !ok {
		return root
	}
	gitdir, found := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !found {
		return root
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(root, gitdir)
	}
	common := filepath.Join(gitdir, "..", "..") // <main>/.git/worktrees/<name> -> <main>/.git
	if rel, ok := readSmallFile(filepath.Join(gitdir, "commondir")); ok {
		if rel = strings.TrimSpace(rel); filepath.IsAbs(rel) {
			common = rel
		} else if rel != "" {
			common = filepath.Join(gitdir, rel)
		}
	}
	common = filepath.Clean(common)
	if filepath.Base(common) != ".git" {
		return common // a bare repository: its own directory names it
	}
	return filepath.Dir(common)
}

func readSmallFile(p string) (string, bool) {
	f, err := os.Open(p) // #nosec G304 -- a file inside the checkout's own git metadata.
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, maxGitFileBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", false
	}
	return string(buf[:n]), true
}

// EntryName is the artifact name of the lesson with this cue: deterministic, so
// the same cue is the same entry, and always a single file directly under
// lessons/<library>/. The slug is only ever lowercase letters, digits and
// hyphens, so no cue can introduce a separator or a dot-dot; the hash suffix is
// what keeps two cues that slug alike from sharing a file.
func EntryName(library, cue string) string {
	var b strings.Builder
	lastHyphen := true
	for _, r := range strings.ToLower(cue) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastHyphen = false
		} else if !lastHyphen {
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > MaxSlugLen {
		slug = strings.Trim(slug[:MaxSlugLen], "-")
	}
	if slug == "" {
		slug = "entry"
	}
	sum := sha256.Sum256([]byte(cue))
	return path.Join(libraryRoot, library, slug+"-"+hex.EncodeToString(sum[:])[:12]+".md")
}

// Render is the Markdown a lesson is stored as, for a first revision. A revision
// of an existing entry goes through render with the history it carries.
func Render(sessionID string, r Result) string { return render(sessionID, r, nil) }

const (
	historyHeading   = "## History"
	historyRowPrefix = "- revision "
	// maxHistoryRows bounds how many superseded revisions an entry carries; the
	// oldest fall off first, and the cap is what keeps an entry that is revised
	// for years under the store's size limit.
	maxHistoryRows    = 200
	maxHistoryRowRune = 700
	maxSaveAttempts   = 32
)

func render(sessionID string, r Result, history []string) string {
	e := r.Experience
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", e.Cue)
	fmt.Fprintf(&b, "- confidence: %s\n- session: %s\n\n", e.Confidence, oneLine(sessionID, 80))
	fmt.Fprintf(&b, "## Lesson\n\n%s\n\n## Evidence\n\n%s\n\n## Attribution\n\n", e.Lesson, e.Evidence)
	for _, ed := range r.Edits {
		fmt.Fprintf(&b, "- %s: %s", ed.Verdict, ed.Change)
		if ed.Note != "" {
			fmt.Fprintf(&b, " (%s)", ed.Note)
		}
		b.WriteString("\n")
	}
	if len(history) > 0 {
		fmt.Fprintf(&b, "\n%s\n\n", historyHeading)
		for _, row := range history {
			b.WriteString(row)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// historyOf is the history an entry carries after it is revised: the rows its
// previous content already held, then one row for the revision being
// superseded. A revision that replaced the evidence would leave only a counter
// to say a session had ever drawn this lesson; this keeps who, when, how the
// attribution read and what the evidence was.
func historyOf(prev *artifacts.Artifact) []string {
	var rows []string
	if i := strings.Index(prev.Content, "\n"+historyHeading+"\n"); i >= 0 {
		for line := range strings.Lines(prev.Content[i+len(historyHeading)+2:]) {
			line = strings.TrimRight(line, "\n")
			if strings.HasPrefix(line, historyRowPrefix) {
				rows = append(rows, oneLine(line, maxHistoryRowRune))
			}
		}
	}
	rows = append(rows, supersededRow(prev))
	if len(rows) > maxHistoryRows {
		rows = rows[len(rows)-maxHistoryRows:]
	}
	return rows
}

// supersededRow summarises the revision being replaced, from the stored
// Markdown (which a person may have edited; every piece is read defensively and
// bounded).
func supersededRow(prev *artifacts.Artifact) string {
	session, confidence := "unknown", "unknown"
	counts := map[Verdict]int{}
	var evidence strings.Builder
	where := ""
	for line := range strings.Lines(prev.Content) {
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "## "):
			where = line
			continue
		case where == "" && strings.HasPrefix(line, "- session: "):
			session = oneLine(strings.TrimPrefix(line, "- session: "), 80)
		case where == "" && strings.HasPrefix(line, "- confidence: "):
			confidence = oneLine(strings.TrimPrefix(line, "- confidence: "), 16)
		case where == "## Evidence":
			evidence.WriteString(line + " ")
		case where == "## Attribution":
			for _, v := range []Verdict{VerdictHelped, VerdictHurt, VerdictNoEffect, VerdictConfounded} {
				if strings.HasPrefix(line, "- "+string(v)+":") {
					counts[v]++
				}
			}
		}
	}
	var verdicts []string
	for _, v := range []Verdict{VerdictHelped, VerdictHurt, VerdictNoEffect, VerdictConfounded} {
		if counts[v] > 0 {
			verdicts = append(verdicts, fmt.Sprintf("%s=%d", strings.ReplaceAll(string(v), " ", "-"), counts[v]))
		}
	}
	if len(verdicts) == 0 {
		verdicts = []string{"none recorded"}
	}
	return oneLine(fmt.Sprintf("%s%d, %s, session %s, confidence %s, verdicts: %s; evidence: %s",
		historyRowPrefix, prev.Revision, prev.UpdatedAt.UTC().Format(time.RFC3339), session, confidence,
		strings.Join(verdicts, " "), oneLine(evidence.String(), 300)), maxHistoryRowRune)
}

// stored is the outcome of a write the store reported as published: the
// artifact's name and revision, with err (nil or ErrPublishedUnsynced) beside
// them. The store's contract is a non-nil artifact in both cases; a nil one is
// refused as an error here, not dereferenced, so a store that broke the
// contract cannot panic a save.
func stored(name string, art *artifacts.Artifact, err error) (string, int, error) {
	if art == nil {
		return "", 0, fmt.Errorf("lessons: %s was reported as published but the store returned no artifact (%v)", name, err)
	}
	return art.Name, art.Revision, err
}

// Save stores an accepted result, and only an accepted result with an entry in
// it: NO_ENTRY and the zero Result are errors, so a caller cannot store nothing
// by accident and report it as a lesson.
//
// A cue seen before revises its entry rather than starting a second one, and
// the revision keeps the earlier one as a History row. It is safe against
// another process doing the same: a fresh cue that loses the exclusive create
// becomes a revision, and a revision is a compare-and-swap on the revision it
// read, retried (bounded) when someone else got there first, so no concurrent
// reflection is lost or overwritten. It returns the artifact name and its
// revision.
//
// An error matching artifacts.ErrPublishedUnsynced comes back WITH the name and
// revision: the entry is stored and readable, only its durability is
// unconfirmed. Retrying it would file a second revision of a lesson already
// filed.
func Save(store *artifacts.Store, library, sessionID string, r Result) (string, int, error) {
	return save(store, library, sessionID, r)
}

// entryStore is the part of *artifacts.Store that Save uses: the seam through
// which a test makes a write publish with its directory sync unconfirmed,
// which no real filesystem will do on demand.
type entryStore interface {
	Get(name string) (*artifacts.Artifact, error)
	Create(name, content string, meta artifacts.Metadata) (*artifacts.Artifact, error)
	UpdateIfRevision(name, content string, meta *artifacts.Metadata, expected int) (*artifacts.Artifact, error)
}

func save(store entryStore, library, sessionID string, r Result) (string, int, error) {
	if err := ValidateLibrary(library); err != nil {
		return "", 0, err
	}
	if r.NoEntry || r.Experience == nil {
		return "", 0, errors.New("lessons: there is no entry to store")
	}
	name := EntryName(library, r.Experience.Cue)
	// Containment, checked on the result rather than assumed from how it was
	// built: whatever the store is asked to write is directly under
	// lessons/<library>/ or it is not written.
	if path.Dir(name) != path.Join(libraryRoot, library) {
		return "", 0, fmt.Errorf("lessons: %q is not directly under %s/%s/", name, libraryRoot, library)
	}
	meta := artifacts.Metadata{Summary: "lesson: " + oneLine(r.Experience.Cue, 120)}

	var last error
	for attempt := 0; attempt < maxSaveAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(min(attempt, 10)) * 3 * time.Millisecond)
		}
		prev, err := store.Get(name)
		switch {
		case err == nil && prev.Revision < 1:
			// Create publishes the content and then its record; a read between
			// the two sees an entry with no revision yet. That is another
			// writer mid-create, not a revision to build on.
			last = errors.New("the entry has no recorded revision yet")
		case err == nil:
			art, err := store.UpdateIfRevision(name, render(sessionID, r, historyOf(prev)), &meta, prev.Revision)
			if err == nil || errors.Is(err, artifacts.ErrPublishedUnsynced) {
				// A published write whose directory sync was not confirmed is
				// stored: it comes back with its name and revision and with
				// the error, for the caller to report beside them, not retry.
				return stored(name, art, err)
			}
			if !errors.Is(err, artifacts.ErrRevisionConflict) {
				return "", 0, err
			}
			last = err
		case errors.Is(err, artifacts.ErrNotFound):
			art, err := store.Create(name, render(sessionID, r, nil), meta)
			if err == nil || errors.Is(err, artifacts.ErrPublishedUnsynced) {
				// A published write whose directory sync was not confirmed is
				// stored: it comes back with its name and revision and with
				// the error, for the caller to report beside them, not retry.
				return stored(name, art, err)
			}
			if !errors.Is(err, artifacts.ErrExists) {
				return "", 0, err
			}
			// Another writer created it between the Get and the Create. It is
			// a revision now.
			last = err
		default:
			// A read that refuses is, for the instant another process's Create
			// holds the new file under its temporary and its final name or has
			// published the content and not yet its record, indistinguishable
			// from a store that is damaged. Both are retried within the same
			// bound, and a store that really is damaged exhausts it and fails
			// with the last refusal — about a second, fail closed.
			last = err
		}
	}
	return "", 0, fmt.Errorf("lessons: %s kept changing under this save (%d attempts): %w", name, maxSaveAttempts, last)
}
