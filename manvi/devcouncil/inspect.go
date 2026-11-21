package devcouncil

import (
	"path/filepath"
	"sync"
)

// inspectedSet is the set of repository paths this registry has shown the
// agent in full or written itself, in this process.
//
// It exists for one rule: an existing file is not overwritten wholesale by an
// agent that never looked at it. devcouncil_write_file replaces every byte, so
// a model that writes a file it has only guessed at — its name from a listing,
// its shape from memory — destroys whatever it did not guess. The patch tool
// needs no such check: it has to quote the text it replaces, and it reads the
// file itself to find it.
//
// The set is per registry and in memory. A resumed session starts empty, so
// the first overwrite after a resume asks for a read again; a sub-agent holds
// its own registry and so its own set. Both directions are deliberate: the
// check is cheap to satisfy and a stale "you read this" is exactly the
// assumption the rule is there to refuse.
type inspectedSet struct {
	mu    sync.Mutex
	paths map[string]struct{}
}

// maxInspectedPaths bounds the set. Past it, new paths are still recorded by
// dropping the whole set and starting again: forgetting is the safe direction,
// since it can only cause a re-read and never permit an unread overwrite.
const maxInspectedPaths = 4096

func inspectedKey(p string) string { return filepath.ToSlash(filepath.Clean(p)) }

func (s *inspectedSet) mark(p string) {
	if p == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.paths == nil || len(s.paths) >= maxInspectedPaths {
		s.paths = make(map[string]struct{})
	}
	s.paths[inspectedKey(p)] = struct{}{}
}

func (s *inspectedSet) has(p string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.paths[inspectedKey(p)]
	return ok
}
