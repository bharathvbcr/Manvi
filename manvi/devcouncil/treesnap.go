package devcouncil

import (
	"path"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// fsStamp is what a refusal judgement of a listed name depends on, read from
// one lstat: which object the name reaches (device and inode), how many names
// it has (nlink), what kind of object it is (mode), and when it last changed
// (mtime for a directory's entries, ctime for an inode's metadata and link
// count). Times are nanoseconds.
type fsStamp struct {
	dev, ino, nlink uint64
	mode            uint32
	mtime, ctime    int64
}

// listingStamps records, for the files a listing named and for every directory
// on the way to them up to the root, the stamps treeUnchanged compares after the
// search. fresh is set when any recorded time is within grepStampSlack of the
// start of the search, or when a stamp could not be read: the snapshot then
// proves nothing and the caller takes the full path.
type listingStamps struct {
	dirNames []string
	dirs     []fsStamp
	files    []fsStamp
	fresh    bool
}

// stampedName is one listed name's stamp as read by the judgement's own lstat,
// when it made one.
type stampedName struct {
	st fsStamp
	ok bool
}

// grepStampSlack is how close to the start of a search a recorded mtime or
// ctime may be before it is treated as possibly changed. A filesystem stamps
// with a coarse clock (a second on some, two on FAT), and a change in the same
// tick as the recording leaves the stamp equal to the one recorded.
const grepStampSlack = 2 * time.Second

// grepNow is the clock the slack is measured against; a test moves it.
var grepNow = time.Now

// grepFullRecheck, when set, is called each time treeUnchanged takes the full
// relist-and-judge path. It exists so a test can prove which path ran without
// timing it.
var grepFullRecheck func()

// parallelEach runs fn(0..n-1) on a bounded pool, the same shape as the
// refusal judgement: independent filesystem calls, so they overlap.
func parallelEach(n int, fn func(i int)) {
	workers := min(grepRefusalWorkers, n)
	var next atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				fn(i)
			}
		}()
	}
	wg.Wait()
}

// dirsAbove lists the root and every directory above the named files.
func dirsAbove(listed []string) []string {
	seen := map[string]struct{}{".": {}}
	names := []string{"."}
	for _, name := range listed {
		for dir := path.Dir(name); dir != "." && dir != "/"; dir = path.Dir(dir) {
			if _, ok := seen[dir]; ok {
				break
			}
			seen[dir] = struct{}{}
			names = append(names, dir)
		}
	}
	return names
}

// stampAll reads one lstat per name, in order. ok is false if any could not be
// read or the platform has no stamp.
func (r *Registry) stampAll(names []string) ([]fsStamp, bool) {
	out := make([]fsStamp, len(names))
	good := make([]bool, len(names))
	parallelEach(len(names), func(i int) {
		out[i], good[i] = lstatStamp(r.deps.Root + string(filepath.Separator) + filepath.FromSlash(names[i]))
	})
	for _, g := range good {
		if !g {
			return nil, false
		}
	}
	return out, true
}

// snapshotTree records the stamps of a listing. known holds the file stamps the
// judgement already read; the rest, and every directory, are read here. started
// is when the search that follows began: every change after it leaves a stamp no
// older than started, so a stamp within grepStampSlack of it marks the snapshot
// fresh. nil means no usable snapshot, and treeUnchanged then takes the full
// path.
func (r *Registry) snapshotTree(listed []string, known []stampedName, started time.Time) *listingStamps {
	dirNames := dirsAbove(listed)
	dirs, ok := r.stampAll(dirNames)
	if !ok {
		return nil
	}
	files := make([]fsStamp, len(listed))
	var missing []int
	for i := range listed {
		if i < len(known) && known[i].ok {
			files[i] = known[i].st
		} else {
			missing = append(missing, i)
		}
	}
	if len(missing) > 0 {
		names := make([]string, len(missing))
		for j, i := range missing {
			names[j] = listed[i]
		}
		got, ok := r.stampAll(names)
		if !ok {
			return nil
		}
		for j, i := range missing {
			files[i] = got[j]
		}
	}
	limit := started.Add(-grepStampSlack).UnixNano()
	snap := &listingStamps{dirNames: dirNames, dirs: dirs, files: files}
	for _, group := range [][]fsStamp{dirs, files} {
		for _, st := range group {
			snap.fresh = snap.fresh || st.mtime >= limit || st.ctime >= limit
		}
	}
	return snap
}

// holds reports whether every recorded entry still reads back the same.
func (s *listingStamps) holds(r *Registry, listed []string) bool {
	if s == nil || s.fresh || len(s.files) != len(listed) {
		return false
	}
	dirs, ok := r.stampAll(s.dirNames)
	if !ok {
		return false
	}
	for i := range dirs {
		if dirs[i] != s.dirs[i] {
			return false
		}
	}
	files, ok := r.stampAll(listed)
	if !ok {
		return false
	}
	for i := range files {
		if files[i] != s.files[i] {
			return false
		}
	}
	return true
}
