package main

import (
	"os"
	"sync"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/repomap"
)

// graphCache holds the code graph last loaded, with the file it came from.
//
// Every session builds a gate, and every gate, the map command and the refresh
// report each loaded the graph again — a bounded but potentially large JSON
// document decoded per new tab. A Map is never modified once built, so one copy
// serves them all. It is reused only while the file is the same file at the
// same size and modification time: a rebuilt graph is a different file or a
// different stamp, and is loaded fresh, which is what "open a new session to
// navigate against the rebuilt graph" relies on.
var graphCache struct {
	mu   sync.Mutex
	path string
	info os.FileInfo
	m    *repomap.Map
}

// loadGraph is repomap.LoadIfPresent, answered from the cache when the file
// has not changed since it was last read.
func loadGraph(path string) (*repomap.Map, error) {
	info, err := os.Stat(path)
	if err != nil {
		graphCache.mu.Lock()
		graphCache.path, graphCache.info, graphCache.m = "", nil, nil
		graphCache.mu.Unlock()
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	graphCache.mu.Lock()
	defer graphCache.mu.Unlock()
	if c := graphCache.info; c != nil && graphCache.path == path && os.SameFile(c, info) &&
		c.Size() == info.Size() && c.ModTime().Equal(info.ModTime()) {
		return graphCache.m, nil
	}
	// The stat is taken before the read, so a file replaced in between is
	// cached under the older stamp and simply read again next time.
	m, err := repomap.LoadIfPresent(path)
	if err != nil {
		graphCache.path, graphCache.info, graphCache.m = "", nil, nil
		return nil, err
	}
	graphCache.path, graphCache.info, graphCache.m = path, info, m
	return m, nil
}
