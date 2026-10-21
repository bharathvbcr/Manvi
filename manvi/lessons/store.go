package lessons

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"

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
func DefaultLibrary(root string) (string, error) {
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

// Render is the Markdown a lesson is stored as.
func Render(sessionID string, r Result) string {
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
	return b.String()
}

// Save stores an accepted result, and only an accepted result with an entry in
// it: NO_ENTRY and the zero Result are errors, so a caller cannot store nothing
// by accident and report it as a lesson.
//
// A cue seen before revises its entry rather than starting a second one. It
// returns the artifact name and its revision.
func Save(store *artifacts.Store, library, sessionID string, r Result) (string, int, error) {
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
	content := Render(sessionID, r)

	if _, err := store.Get(name); err == nil {
		art, err := store.Update(name, content, &meta)
		if err != nil {
			return "", 0, err
		}
		return art.Name, art.Revision, nil
	}
	art, err := store.Create(name, content, meta)
	if err != nil {
		return "", 0, err
	}
	return art.Name, art.Revision, nil
}
