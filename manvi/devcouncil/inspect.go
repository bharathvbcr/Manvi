package devcouncil

import (
	"crypto/sha256"
	"path/filepath"
	"sync"
)

// contentDigest is what the set remembers of a file: a hash of exactly the
// bytes the agent was shown, or wrote.
type contentDigest = [sha256.Size]byte

// inspectedSet is the set of repository paths this registry has shown the
// agent in full or written itself, in this process, with a digest of the bytes
// the agent knows.
//
// It exists for one rule: an existing file is not overwritten wholesale by an
// agent that never looked at it, or looked at it before it changed.
// devcouncil_write_file replaces every byte, so
// a model that writes a file it has only guessed at — its name from a listing,
// its shape from memory — destroys whatever it did not guess. The patch tool
// needs no such check: it has to quote the text it replaces, and it reads the
// file itself to find it.
//
// The digest is why a read stays good only while the file does: another
// writer's addition after the read is exactly what a wholesale overwrite would
// drop, and a path-only set could not see it.
//
// The set is per registry and in memory. A resumed session starts empty, so
// the first overwrite after a resume asks for a read again; a sub-agent holds
// its own registry and so its own set. Both directions are deliberate: the
// check is cheap to satisfy and a stale "you read this" is exactly the
// assumption the rule is there to refuse.
type inspectedSet struct {
	mu    sync.Mutex
	paths map[string]contentDigest
}

// maxInspectedPaths bounds the set. Past it, new paths are still recorded by
// dropping the whole set and starting again: forgetting is the safe direction,
// since it can only cause a re-read and never permit an unread overwrite.
const maxInspectedPaths = 4096

func inspectedKey(p string) string { return filepath.ToSlash(filepath.Clean(p)) }

// mark records that the agent knows the bytes that hash to sum.
func (s *inspectedSet) mark(p string, sum contentDigest) {
	if p == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.paths == nil || len(s.paths) >= maxInspectedPaths {
		s.paths = make(map[string]contentDigest)
	}
	s.paths[inspectedKey(p)] = sum
}

// digest returns the digest of what the agent last read or wrote at p.
func (s *inspectedSet) digest(p string) (contentDigest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sum, ok := s.paths[inspectedKey(p)]
	return sum, ok
}

// has reports whether the agent has read or written p at all, whatever the
// file holds now.
func (s *inspectedSet) has(p string) bool {
	_, ok := s.digest(p)
	return ok
}
