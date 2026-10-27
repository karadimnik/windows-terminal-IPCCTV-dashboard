package archive

import (
	"io/fs"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Cache makes Layout.Files incremental: a folder is still listed on every call, but the file
// information of an entry seen before is reused instead of stat'ed again, except where it can
// still change. One Cache per process (the supervisor, the web role); it is safe for concurrent use
// and keeps nothing on disk.
//
// Invariants (what a cached call returns, compared with an uncached one):
//
//  1. The set of names is always exact: every call lists the folder, so a deleted file is never
//     returned (no ghost) and a new file always is, stat'ed on first sight. An entry whose stat
//     fails is left out and forgotten, as uncached.
//  2. Nothing is reused across a change of the folder's namespace: when the folder is not the same
//     folder as at the previous call (os.SameFile), or its own modification time differs in any
//     direction (a file was created, deleted or renamed, including renamed over another name, or
//     the clock stepped), or that time was within racyWindow of the previous listing (a change in
//     the same tick of a coarse clock would not move it), every entry is re-read.
//  3. Otherwise the file information of a name is re-read (DirEntry.Info) when the name is new or
//     its type bits in the listing changed, when it is among the Hot newest files by modification
//     time or the Hot last names in name order, or when it was modified less than Fresh before it
//     was last read (a file being written, also under a clock that stepped back). Every
//     FullEvery-th call, and the call after Invalidate, re-reads everything.
//
// So the one possible staleness is a file changed IN PLACE (same name, same folder namespace)
// that is not among the newest and was untouched for Fresh when last read: its size and times can
// lag for at most FullEvery-1 calls. Recorder segments are written once and never touched after
// they close, which is why this is safe; the pruner does not use a Cache at all.
//
// On Windows the listing itself carries every entry's information (os.ReadDir reads the directory
// records; DirEntry.Info makes no system call), so nothing is reused there and a cached call
// returns exactly what an uncached one does; the reuse rules apply on Linux and macOS, where every
// DirEntry.Info is an lstat.
//
// Memory is bounded: at most MaxDirs folders are remembered (the least recently listed is dropped
// first) and a folder holds only the entries of its last listing.
type Cache struct {
	// Set these before the first use; zero means the default.
	FullEvery int           // every FullEvery-th call of a folder re-reads every entry; default 30
	Hot       int           // newest-by-mtime and last-by-name entries re-read on every call; default 2
	Fresh     time.Duration // an entry modified within Fresh of its last stat is re-read; default 2 min
	MaxDirs   int           // folders remembered; default 256

	reuse bool             // false on Windows, see invariant 3
	now   func() time.Time // the clock entries are stamped with

	mu   sync.Mutex
	dirs map[string]*dirState
	lru  uint64

	listings, stats, full atomic.Int64
}

// CacheStats counts what a Cache did: folder listings, entry information reads (each DirEntry.Info
// and the folder's own fstat: one system call each on Linux and macOS, none on Windows, where Info
// is read from the listing and the folder is not stat'ed), and full re-reads (Linux and macOS).
type CacheStats struct {
	Listings, Stats, FullRereads int64
}

type dirState struct {
	mu    sync.Mutex
	files map[string]cachedFile
	calls int
	dir   fs.FileInfo // the folder at the last listing; nil: unknown
	racy  bool        // the folder changed within racyWindow of the last listing
	used  uint64
}

type cachedFile struct {
	typ  fs.FileMode
	info fs.FileInfo
	seen time.Time // when info was read
}

// NewCache is a Cache with the defaults.
func NewCache() *Cache {
	return &Cache{reuse: runtime.GOOS != "windows", now: time.Now, dirs: map[string]*dirState{}}
}

// Stats are the counters so far.
func (c *Cache) Stats() CacheStats {
	return CacheStats{Listings: c.listings.Load(), Stats: c.stats.Load(), FullRereads: c.full.Load()}
}

// Invalidate forgets the folder dir (as Layout.CameraDir names it): its next listing re-reads every entry.
func (c *Cache) Invalidate(dir string) {
	c.mu.Lock()
	delete(c.dirs, dir)
	c.mu.Unlock()
}

// InvalidateAll forgets every folder.
func (c *Cache) InvalidateAll() {
	c.mu.Lock()
	c.dirs = map[string]*dirState{}
	c.mu.Unlock()
}

// Invalidate forgets the camera's folders in the layout's Cache (a no-op without one).
func (l Layout) Invalidate(camera string) {
	if l.Cache != nil {
		l.Cache.Invalidate(l.CameraDir(camera))
	}
}

func orDefault[T int | time.Duration](v, d T) T {
	if v <= 0 {
		return d
	}
	return v
}

func (c *Cache) state(dir string) *dirState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dirs == nil {
		c.dirs = map[string]*dirState{}
	}
	c.lru++
	st, ok := c.dirs[dir]
	if !ok {
		st = &dirState{}
		c.dirs[dir] = st
		if max := orDefault(c.MaxDirs, 256); len(c.dirs) > max {
			var oldest string
			var at uint64
			for d, s := range c.dirs {
				if d != dir && (oldest == "" || s.used < at) {
					oldest, at = d, s.used
				}
			}
			delete(c.dirs, oldest)
		}
	}
	st.used = c.lru
	return st
}

func (c *Cache) forget(dir string, st *dirState) {
	c.mu.Lock()
	if c.dirs[dir] == st {
		delete(c.dirs, dir)
	}
	c.mu.Unlock()
}

// files is Layout.Files of one folder through the cache.
func (c *Cache) files(dir string) ([]File, error) {
	st := c.state(dir)
	st.mu.Lock()
	defer st.mu.Unlock()

	// os.ReadDir is Open, ReadDir(-1), Close and a sort by name; the open handle also gives the
	// folder's own information for the identity and mtime checks.
	f, err := os.Open(dir)
	if err != nil {
		c.forget(dir, st)
		return nil, err
	}
	var di fs.FileInfo
	if c.reuse {
		di, _ = f.Stat()
		c.stats.Add(1)
	}
	ents, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		c.forget(dir, st)
		return nil, err
	}
	slices.SortFunc(ents, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	c.listings.Add(1)

	full := !c.reuse || st.files == nil || di == nil || st.dir == nil || st.racy || st.calls%orDefault(c.FullEvery, 30) == 0 ||
		!os.SameFile(st.dir, di) || !di.ModTime().Equal(st.dir.ModTime())
	var hot map[string]bool
	if full {
		if c.reuse {
			c.full.Add(1)
		}
	} else {
		hot = c.hot(st.files, ents)
	}

	now := c.now
	if now == nil {
		now = time.Now
	}
	out := make([]File, 0, len(ents))
	next := make(map[string]cachedFile, len(ents))
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		cf, ok := st.files[name]
		if full || !ok || cf.typ != e.Type() || hot[name] {
			info, err := e.Info()
			c.stats.Add(1)
			if err != nil { // vanished between listing and stat
				continue
			}
			cf = cachedFile{typ: e.Type(), info: info, seen: now()}
		}
		next[name] = cf
		out = append(out, File{Entry: entryOf(dir, e), Info: cf.info})
	}
	st.files, st.dir = next, di
	st.racy = di != nil && now().Sub(di.ModTime()) < racyWindow
	st.calls++
	return out, nil
}

// racyWindow covers file systems whose folder times are coarse (FAT: 2 s): a folder modified this
// close to its listing may change again without its time moving.
const racyWindow = 2 * time.Second

// hot are the cached names that are re-read even though they were seen before (invariant 3).
func (c *Cache) hot(cached map[string]cachedFile, ents []os.DirEntry) map[string]bool {
	k := orDefault(c.Hot, 2)
	fresh := orDefault(c.Fresh, 2*time.Minute)
	hot := map[string]bool{}
	for i, n := len(ents)-1, 0; i >= 0 && n < k; i-- { // the last names
		if !ents[i].IsDir() {
			hot[ents[i].Name()] = true
			n++
		}
	}
	type cand struct {
		name string
		m    time.Time
	}
	top := make([]cand, 0, k+1) // the newest by mtime, later name on a tie
	for name, cf := range cached {
		m := cf.info.ModTime()
		if cf.seen.Sub(m) < fresh {
			hot[name] = true
		}
		i := len(top)
		for i > 0 && (m.After(top[i-1].m) || (m.Equal(top[i-1].m) && name > top[i-1].name)) {
			i--
		}
		if i < k {
			top = slices.Insert(top, i, cand{name, m})
			if len(top) > k {
				top = top[:k]
			}
		}
	}
	for _, x := range top {
		hot[x.name] = true
	}
	return hot
}
