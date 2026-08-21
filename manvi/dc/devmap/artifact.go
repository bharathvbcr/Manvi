package devmap

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// This file owns one question: what to do when the paths `devmap manifest`
// writes already hold a file some other producer wrote.
//
// The producer refuses that write rather than performing it — `refuse to
// overwrite a non-devmap-rust repo map at <path> (pass --force to replace)` —
// and the refusal is right. `.devcouncil/` is shared: DevCouncil's Python
// implementation writes `repo_map.json` there under a different schema, and a
// Rust map landing on top of it silently would leave every Python consumer
// parsing fields that no longer exist.
//
// What was wrong was this boundary's answer to the refusal, which was to have
// none. `manvi map build`, and the background refresh every session start runs,
// both failed with the producer's message and stopped. Neither passed --force
// and neither could: the flag replaces the file, and destroying another tool's
// artifact to get a session started is not a decision a background task makes.
// So in any repository that had ever been indexed by the Python producer the
// harness was wedged permanently — navigation unavailable, the gate's neighbour
// rule reporting `repo_map.unavailable` on every unplanned write, and the one
// remedy the transcript offered (`run 'manvi map build'`) reproducing the same
// failure. A dead end that repeats its own advice is worse than a hard failure,
// because it reads like something the operator has not tried yet.
//
// The answer here is neither to force nor to stop. The foreign file is moved
// aside under a name that says what it is, which leaves its bytes intact and
// leaves the path empty — and the producer's guard is `path.exists() && !force
// && is_foreign(path)`, so an empty path passes it. Nothing is overwritten,
// --force is never passed (so a devmap too old to accept the flag is not broken
// by this), and the operator is told which file moved and where. It happens
// once per artifact: what devmap writes afterwards carries its own engine
// marker, so the next run finds nothing foreign and does nothing.

// consumerMapEngine is the name devmap-rust stamps into the artifacts it
// writes, and the whole of the identity test.
//
// It is duplicated from the producer rather than derived, because there is
// nothing to derive it from: the value crosses a process boundary as JSON. The
// consequence of it drifting is bounded and safe in the direction that matters
// — a renamed engine would make this boundary read its own artifacts as foreign
// and preserve them, which is noisy and lossless, rather than read a foreign one
// as its own and destroy it. TestTheLiveProducerRefusalIsRecoverable asserts the live
// binary still writes this exact string, at both of the positions below.
const consumerMapEngine = "devmap-rust"

// preserveLimit bounds how many preserved copies of one artifact may pile up
// before this boundary refuses to make another.
//
// Reaching it means the other producer has rewritten the path this many times
// since the harness first took it over, which is a repository where two tools
// are fighting over one file every build. Preserving a seventeenth copy would
// answer that by filling the state directory; refusing says it out loud while
// every earlier copy is still on disk.
const preserveLimit = 16

// Adoption records one foreign artifact this boundary moved aside so devmap
// could write the path.
//
// It is carried out of the manifest rather than logged and dropped because it
// is a change to files the operator did not ask for in this session. The rule
// the rest of this package follows — that a summary which could be read as
// complete coverage must be complete — applies to actions as much as to counts.
type Adoption struct {
	// Path is the artifact devmap writes.
	Path string
	// PreservedAs is where the previous file's bytes are now.
	PreservedAs string
}

// String is the line an operator reads about it.
func (a Adoption) String() string {
	return fmt.Sprintf(
		"%s held a repo map written by a producer other than %s; it was preserved as %s and the "+
			"path is now the harness's, so a tool still reading the older schema there will read this one",
		a.Path, consumerMapEngine, a.PreservedAs)
}

// restore puts a preserved artifact back, for a manifest that then failed.
//
// A rename rather than a copy, and it overwrites: if the failed attempt left a
// partial file at Path, the file that was there before this boundary touched
// anything is the one that belongs there.
func (a Adoption) restore() error { return os.Rename(a.PreservedAs, a.Path) }

// destination is one file `devmap manifest` writes, and where that file carries
// the name of the producer that wrote it.
//
// The two artifacts stamp it in different places — the repo map at the top
// level, the code graph under `meta` — which is why this is a table rather than
// one key. Reading the wrong key would find nothing and call every artifact
// foreign, and a boundary that preserves its own files on every build is a
// boundary nobody leaves switched on.
type destination struct {
	path   string
	marker []string
}

// manifestDestinations pairs the paths a manifest run writes with their markers.
func manifestDestinations(mapPath, graphPath string) []destination {
	return []destination{
		{path: mapPath, marker: []string{"map_engine"}},
		{path: graphPath, marker: []string{"meta", "map_engine"}},
	}
}

// foreign reports whether the file at this destination was written by a
// producer other than devmap-rust.
//
// Absent is not foreign: there is nothing there to protect and nothing to
// preserve. A document that does not parse *is* foreign, and that is not a
// judgement call — it is the producer's own rule, and the two verdicts have to
// agree on every file. If this boundary called a malformed artifact its own, it
// would leave the file in place, devmap would refuse it, and the manifest would
// fail exactly as it did before any of this existed.
//
// An unreadable *file*, though, is neither: it is a check that did not run, and
// it errors rather than guessing. Treating it as foreign would move a file this
// boundary could not read; treating it as ours would hand it to devmap to
// overwrite. Both are decisions made without evidence, and the difference
// between them is which one destroys something.
func (d destination) foreign() (bool, error) {
	file, err := os.Open(d.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, d.unreadable(err)
	}
	defer file.Close()

	source := &trackingReader{r: bufio.NewReaderSize(file, markerBuffer)}
	engine, wellFormed := scanMarker(source, d.marker)
	if source.err != nil {
		return false, d.unreadable(source.err)
	}
	if !wellFormed {
		return true, nil
	}
	return engine != consumerMapEngine, nil
}

// unreadable is the one sentence for a check that could not run.
func (d destination) unreadable(err error) error {
	return fmt.Errorf(
		"%s could not be read, so whether it was written by %s is unknown and it will "+
			"neither be preserved nor overwritten: %w", d.path, consumerMapEngine, err)
}

// markerBuffer is how much of an artifact is held while scanning for its
// engine marker. It is a read buffer, not a bound on the file: see scanMarker.
const markerBuffer = 64 << 10

// trackingReader remembers a read failure so a decode that stopped can say
// which kind of stop it was.
//
// json.Decoder returns the reader's error verbatim, so a truncated file and an
// I/O error arrive at the caller looking the same — and they are not the same
// answer at all. One means the file is not a devmap artifact; the other means
// nobody knows what it is.
type trackingReader struct {
	r   io.Reader
	err error
}

func (t *trackingReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		t.err = err
	}
	return n, err
}

// scanMarker streams a JSON document and reports the string at marker, and
// whether the document parsed cleanly to its end.
//
// It streams rather than unmarshalling because of what these files are. The
// code graph for this repository is 8.9 MB and grows with the tree, the marker
// it carries is one word buried past the node list, and this runs at every
// session start on a file written by a process outside this program. Holding
// the whole of it — the bytes and the decoded tree at once, four and a half
// times the file in the measurement next door — to compare one string is the
// payload bound this package applies everywhere else, unapplied; and the
// version that did it also handed a deeply nested document straight to a
// recursive unmarshaller. What is held here is one token and a 64 KiB buffer,
// whatever the artifact's size.
//
// The trade is honest rather than free: boxing every token allocates more in
// total than one bulk decode does. It keeps none of it, and this runs once per
// build, so churn is the cheap half and retention is the expensive one.
//
// Parsing to the end is not optional even though the marker is usually found
// long before it. The producer decides foreignness by parsing the whole
// document, so a file that this boundary accepted on its prefix and devmap
// rejected on its tail would be left in place for devmap to refuse — the
// original wedge, reached by a different road.
func scanMarker(r io.Reader, marker []string) (string, bool) {
	dec := json.NewDecoder(r)
	engine := ""
	if err := scanObject(dec, marker, &engine); err != nil {
		return "", false
	}
	// Trailing content after the document is a parse failure to serde_json, and
	// the two verdicts have to agree.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", false
	}
	return engine, true
}

// scanObject consumes one value, capturing marker's target if it is inside it.
//
// A value that is not an object is consumed and ignored: the producer looks the
// key up on it, finds nothing, and calls the file foreign — which is what an
// empty capture yields here too.
func scanObject(dec *json.Decoder, marker []string, engine *string) error {
	first, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := first.(json.Delim); !ok || delim != '{' {
		return consumeValue(dec, first)
	}
	for {
		key, err := dec.Token()
		if err != nil {
			return err
		}
		if delim, ok := key.(json.Delim); ok && delim == '}' {
			return nil
		}
		name, _ := key.(string)
		switch {
		case name != marker[0]:
			err = skipValue(dec)
		case len(marker) > 1:
			err = scanObject(dec, marker[1:], engine)
		default:
			var value json.Token
			if value, err = dec.Token(); err == nil {
				// Cleared first so a repeated key resolves to its last
				// occurrence, as serde_json's object does.
				*engine = ""
				if text, ok := value.(string); ok {
					*engine = text
				}
				err = consumeValue(dec, value)
			}
		}
		if err != nil {
			return err
		}
	}
}

// skipValue reads the next value and discards it.
func skipValue(dec *json.Decoder) error {
	first, err := dec.Token()
	if err != nil {
		return err
	}
	return consumeValue(dec, first)
}

// consumeValue finishes a value whose first token has already been read.
//
// Iterative rather than recursive, and that is the point: the input is a file
// this program did not write, and nesting is the cheapest way to turn a parser
// into a crash. Depth costs an int here.
func consumeValue(dec *json.Decoder, first json.Token) error {
	delim, ok := first.(json.Delim)
	if !ok {
		return nil
	}
	for depth := 1; depth > 0; {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		if delim, ok = token.(json.Delim); ok {
			if delim == '{' || delim == '[' {
				depth++
			} else {
				depth--
			}
		}
	}
	return nil
}

// adopt moves aside every foreign artifact among the manifest's destinations.
//
// It is all-or-nothing. A run that preserves the repo map and then cannot read
// the code graph puts the repo map back before returning, because the caller's
// next act is to fail, and a failure that has already half-rearranged the state
// directory leaves the operator worse off than the error they were about to be
// shown.
func adopt(mapPath, graphPath string) ([]Adoption, error) {
	var adopted []Adoption
	for _, d := range manifestDestinations(mapPath, graphPath) {
		isForeign, err := d.foreign()
		if err != nil {
			return nil, abandon(adopted, err)
		}
		if !isForeign {
			continue
		}
		preserved, err := preserve(d.path)
		if err != nil {
			return nil, abandon(adopted, err)
		}
		adopted = append(adopted, Adoption{Path: d.path, PreservedAs: preserved})
	}
	return adopted, nil
}

// abandon rolls an adoption back and folds any failure to do so into the error
// that caused it.
//
// A restore that fails is not allowed to replace the original error: the
// operator needs the reason the manifest is not happening, and separately needs
// to know that a file is not where they left it. Reporting only the second
// would send them to tidy up a state directory without ever learning why.
func abandon(adopted []Adoption, cause error) error {
	var stranded []string
	for _, a := range adopted {
		if err := a.restore(); err != nil {
			stranded = append(stranded, fmt.Sprintf("%s could not be put back from %s: %v",
				a.Path, a.PreservedAs, err))
		}
	}
	if len(stranded) == 0 {
		return cause
	}
	return fmt.Errorf("%w; and the artifact(s) moved aside before the attempt are still aside: %s",
		cause, strings.Join(stranded, "; "))
}

// preserve moves a foreign artifact to a free name beside itself and returns it.
//
// The link-then-remove is a rename that cannot overwrite. os.Rename replaces
// whatever is at the destination, so a check that the name is free followed by
// a rename is a window in which a second process — a concurrent `manvi map
// build`, another session's background refresh — can take that name and have
// its copy destroyed. os.Link fails with ErrExist instead, which turns the race
// into the loop's next iteration. The source is removed only once its bytes are
// safely under the new name, so an interruption anywhere in here leaves the
// artifact readable under one name or both, never neither.
func preserve(path string) (string, error) {
	dir, base := filepath.Split(path)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	for n := 1; n <= preserveLimit; n++ {
		name := stem + ".foreign" + ext
		if n > 1 {
			name = fmt.Sprintf("%s.foreign-%d%s", stem, n, ext)
		}
		candidate := filepath.Join(dir, name)

		switch err := os.Link(path, candidate); {
		case err == nil:
		case errors.Is(err, fs.ErrExist):
			continue
		default:
			return "", fmt.Errorf(
				"%s was written by a producer other than %s and could not be preserved as %s, so it "+
					"was left alone and the artifacts the scope rung reads were not rewritten: %w",
				path, consumerMapEngine, candidate, err)
		}

		if err := os.Remove(path); err != nil {
			// The bytes are under both names now. Dropping the copy is the only
			// way back to the state this function was called in; if that fails
			// too, both names are reported so neither is a surprise later.
			if undo := os.Remove(candidate); undo != nil {
				return "", fmt.Errorf(
					"%s could not be moved aside and a copy of it was left at %s: %w (removing the copy: %v)",
					path, candidate, err, undo)
			}
			return "", fmt.Errorf("%s could not be moved aside, so it was left alone: %w", path, err)
		}
		return candidate, nil
	}

	return "", fmt.Errorf(
		"%s was written by a producer other than %s and %d preserved copies of it already exist "+
			"beside it, so it was left alone rather than adding another; the two producers are "+
			"overwriting one path every build and one of them has to be pointed elsewhere",
		path, consumerMapEngine, preserveLimit)
}
