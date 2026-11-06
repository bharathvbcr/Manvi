package catalog

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/internal/filelock"
	"github.com/bharathvbcr/Manvi/manvi/internal/syncdir"
	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

func (s Store) open(create bool) (*os.Root, error) {
	if strings.TrimSpace(s.Root) == "" {
		return nil, errors.New("catalog root required")
	}
	if create {
		if err := os.MkdirAll(s.Root, 0700); err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(s.Root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("catalog root must be a real directory")
	}
	root, err := os.OpenRoot(s.Root)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		root.Close()
		return nil, errors.New("catalog root changed during open")
	}
	return root, nil
}
func openChild(root *os.Root, name string, create bool) (*os.Root, error) {
	if !segment.MatchString(name) {
		return nil, errors.New("invalid catalog directory")
	}
	if create {
		if err := root.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("catalog identity must be a real directory")
	}
	child, err := root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := child.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		child.Close()
		return nil, errors.New("catalog directory changed during open")
	}
	return child, nil
}
func readRegular(root *os.Root, name string) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > workflow.MaxArtifactBytes {
		return nil, errors.New("catalog artifact must be a bounded regular file")
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, errors.New("catalog file changed during open")
	}
	raw, err := io.ReadAll(io.LimitReader(f, workflow.MaxArtifactBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > workflow.MaxArtifactBytes {
		return nil, errors.New("catalog artifact exceeds size limit")
	}
	return raw, nil
}
func directoryEntries(root *os.Root) ([]os.DirEntry, error) {
	f, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(4097)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > 4096 {
		return nil, errors.New("catalog directory exceeds 4096 entries")
	}
	return entries, nil
}

// lockWait bounds the wait for another writer of the same entry, so a wedged
// holder fails a caller with an error instead of hanging it. It is a variable so
// a test can shorten it.
var lockWait = 10 * time.Second

// currentLockName is the lock file in an identity directory. It is not a
// revision (no ".json" suffix), so listing skips it, and it is never removed: a
// removed lock file lets two processes lock two inodes of one name.
const currentLockName = ".current.lock"

// identityLocks holds one single-slot semaphore per identity directory. Store is
// a value, and callers (Jarvis builds one per request) make many of them for one
// directory, so the lock cannot live on the Store. A channel rather than a
// sync.Mutex so that waiting for it can be bounded.
var identityLocks sync.Map

// lockCurrent serialises every read-modify-write of an identity's current.json.
// The in-process semaphore orders this process's writers without polling; the
// file lock orders them against other processes, because the catalog is shared:
// the Jarvis daemon and CLI each write the same directory through this package.
// One lockWait bounds the wait for both together, so N writers behind a wedged
// holder fail after one bound, not N. On a platform with no cross-process lock
// this package knows it refuses rather than write unlocked.
func (s Store) lockCurrent(dir *os.Root, id string) (func(), error) {
	key, err := filepath.Abs(filepath.Join(s.Root, id))
	if err != nil {
		return nil, err
	}
	sem, _ := identityLocks.LoadOrStore(key, make(chan struct{}, 1))
	slot := sem.(chan struct{}) //nolint:errcheck,forcetypeassert // the map only ever holds chan struct{}
	ctx, cancel := context.WithTimeout(context.Background(), lockWait)
	defer cancel()
	waiting := func(err error) error { return fmt.Errorf("catalog: waiting for another writer of %s: %w", id, err) }
	select {
	case slot <- struct{}{}:
	case <-ctx.Done():
		return nil, waiting(ctx.Err())
	}
	// Opened through dir, not by path, so a directory swapped after openChild
	// cannot redirect the lock file.
	f, err := dir.OpenFile(currentLockName, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		<-slot
		return nil, err
	}
	unlock, err := filelock.LockFile(ctx, f)
	if err != nil {
		<-slot
		if errors.Is(err, filelock.ErrUnsupported) {
			return nil, errors.New("catalog: no cross-process file lock on this platform; the catalog cannot be written safely")
		}
		return nil, waiting(err)
	}
	return func() { unlock(); <-slot }, nil
}

// updateCurrent is the one place that reads, edits and replaces an identity's
// current.json, under lockCurrent. The pointer is edited as a raw object, never
// rebuilt from a decoded Entry, so a key another writer stored (Jarvis shares
// this file, and a newer catalog revision may add keys) is written back as found
// unless fn replaces it. The file is still decoded into Entry first, so a
// pointer with a wrong type for a key this build owns is refused rather than
// overwritten. When the file is absent, create decides: true runs fn on an empty
// object, false returns the read error.
//
// Keys and values are decoded and re-encoded, so they come back as valid UTF-8
// (an invalid byte in a key becomes U+FFFD), output keys are sorted, and a
// repeated key collapses to its last value, which is the value every decoder
// reads. HTML characters and U+2028/2029 are not escaped on the way out: a
// value that fit under MaxArtifactBytes must not grow past it for being
// rewritten.
//
// If the encoded pointer would still exceed MaxArtifactBytes and shrink is set,
// the foreign keys are dropped and only the owned ones written; their names are
// returned. Without shrink the write is refused and the file left untouched.
func (s Store) updateCurrent(dir *os.Root, id string, create, shrink bool, fn func(prior Entry, exists bool, fields map[string]json.RawMessage) error) (dropped []string, err error) {
	unlock, err := s.lockCurrent(dir, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	var prior Entry
	fields := map[string]json.RawMessage{}
	raw, err := readRegular(dir, "current.json")
	exists := err == nil
	switch {
	case exists:
		if err := json.Unmarshal(raw, &prior); err != nil {
			return nil, err
		}
		normalizeStatus(&prior)
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return nil, err
		}
		if decoded != nil {
			fields = decoded
		}
	case !errors.Is(err, os.ErrNotExist) || !create:
		return nil, err
	}
	if err := fn(prior, exists, fields); err != nil {
		return nil, err
	}
	metadata, err := encodeFields(fields)
	if err != nil {
		return nil, err
	}
	if len(metadata) > workflow.MaxArtifactBytes {
		if !shrink {
			return nil, fmt.Errorf("catalog: current.json would be %d bytes, over the %d byte limit; refusing to write it", len(metadata), workflow.MaxArtifactBytes)
		}
		var kept map[string]json.RawMessage
		if kept, dropped, err = ownedOnly(fields); err != nil {
			return nil, err
		}
		if metadata, err = encodeFields(kept); err != nil {
			return nil, err
		}
		if len(metadata) > workflow.MaxArtifactBytes {
			return nil, errors.New("catalog: current.json over the size limit even without foreign keys")
		}
	}
	return dropped, publish(dir, "current.json", metadata, true)
}

// encodeFields encodes the pointer without HTML escaping. json.Marshal would turn
// each <, >, & and U+2028/2029 inside a foreign value into a six byte escape.
func encodeFields(fields map[string]json.RawMessage) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(fields); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// jsonKeys lists the JSON key names a struct type's tags own.
func jsonKeys(t reflect.Type) []string {
	var names []string
	for i := 0; i < t.NumField(); i++ {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); name != "" && name != "-" {
			names = append(names, name)
		}
	}
	return names
}

// matchesOwned reports whether encoding/json would decode key into a field named
// by one of owned. The decoder matches a key to a field by exact name or by
// folding (Unicode simple case folding: "\u017f" matches s, "\u212a" matches k),
// and when several keys match one field the last in document order wins, so a
// folded variant that sorts after the real key can override it for every reader.
// encoding/json's foldName is documented as equivalent to bytes.EqualFold.
func matchesOwned(key string, owned []string) bool {
	for _, name := range owned {
		if strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

// dropNearDuplicates removes from fields every key that matches an owned name
// without being it, so no stored key can shadow an owned one. A conflicting
// near-duplicate of an owned key is not a foreign field worth preserving, and
// dropping it is the safe choice. With keepExact the exact owned keys stay.
func dropNearDuplicates(fields map[string]json.RawMessage, owned []string, keepExact bool) {
	for k := range fields {
		if matchesOwned(k, owned) && !(keepExact && slices.Contains(owned, k)) {
			delete(fields, k)
		}
	}
}

// overlayOwned makes fields carry exactly the keys v's type owns, taken from v.
// Every key that decodes into one of v's fields is removed first (including
// case-folded variants, see matchesOwned), then the ones v marshals to are set
// (omitempty ones may not be). Keys the type does not match are left alone. The
// owned set comes from the struct tags, so it cannot drift from the type.
func overlayOwned(fields map[string]json.RawMessage, v any) error {
	dropNearDuplicates(fields, jsonKeys(reflect.TypeOf(v)), false)
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var owned map[string]json.RawMessage
	if err := json.Unmarshal(raw, &owned); err != nil {
		return err
	}
	for k, val := range owned {
		fields[k] = val
	}
	return nil
}

// ownedOnly returns the pointer reduced to the keys this build owns, with the
// names of what was dropped ("stability.<key>" for keys inside stability).
func ownedOnly(fields map[string]json.RawMessage) (kept map[string]json.RawMessage, dropped []string, err error) {
	entryKeys := jsonKeys(reflect.TypeOf(Entry{}))
	stabilityKeys := jsonKeys(reflect.TypeOf(Stability{}))
	kept = map[string]json.RawMessage{}
	for k, v := range fields {
		if !slices.Contains(entryKeys, k) {
			dropped = append(dropped, k)
			continue
		}
		if k == "stability" {
			var inner map[string]json.RawMessage
			if err := json.Unmarshal(v, &inner); err != nil {
				return nil, nil, err
			}
			for ik := range inner {
				if !slices.Contains(stabilityKeys, ik) {
					dropped = append(dropped, "stability."+ik)
					delete(inner, ik)
				}
			}
			if v, err = encodeFields(inner); err != nil {
				return nil, nil, err
			}
		}
		kept[k] = v
	}
	sort.Strings(dropped)
	return kept, dropped, nil
}

// Publish only after the full bytes have reached a synchronized staging file.
// Link provides atomic non-replacement for immutable revisions on every OS.
func publish(root *os.Root, name string, raw []byte, replace bool) error {
	staged := ".staged-" + rand.Text()
	f, err := root.OpenFile(staged, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(staged)
	n, writeErr := f.Write(raw)
	if writeErr == nil && n != len(raw) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = f.Sync()
	}
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return err
	}
	if replace {
		err = root.Rename(staged, name)
	} else {
		err = root.Link(staged, name)
	}
	if err != nil {
		return err
	}
	// Syncing the staged file put the bytes on the disk; syncing the directory
	// is what puts the name there. Without it a power loss can leave the
	// revision (or the current pointer) published as an unreferenced inode.
	if err := syncDir(root); err != nil {
		return fmt.Errorf("catalog: syncing directory after publishing %s: %w", name, err)
	}
	return nil
}

// syncDir flushes a catalog directory's own entries. It is a variable so a
// test can prove publish calls it and fails when it fails; a crash cannot be
// reproduced in a test.
var syncDir = syncdir.Root
