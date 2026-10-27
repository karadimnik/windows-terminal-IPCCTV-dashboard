package archive

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// fileView is what a caller can observe of a File (fs.FileInfo.Sys also holds the access time,
// which reading a file moves, so the infos are compared by their fields).
type fileView struct {
	Name, Path, Dir, Day string
	Size                 int64
	Mode                 os.FileMode
	MTime, Created       time.Time
}

func views(fs []File) []fileView {
	out := make([]fileView, len(fs))
	for i, f := range fs {
		out[i] = fileView{f.Name(), f.Path, f.Dir, f.Day, f.Info.Size(), f.Info.Mode(), f.Info.ModTime(), f.Created()}
	}
	return out
}

// simRig is one camera folder under a simulated clock: every time on disk (files and the folder)
// is set from the clock, so the cache's rules can be driven exactly.
type simRig struct {
	t     *testing.T
	root  string
	cam   string
	dir   string
	now   time.Time
	cache *Cache
	l     Layout
	ref   Layout // the same layout without a cache
}

func newSimRig(t *testing.T, cam string) *simRig {
	r := &simRig{t: t, root: filepath.Join(t.TempDir(), "archive"), cam: cam, now: time.Date(2026, 10, 25, 0, 0, 3, 0, time.UTC)}
	r.dir = filepath.Join(r.root, cam)
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r.cache = NewCache()
	r.cache.reuse = true // the Linux and macOS rules, on every OS
	r.cache.now = func() time.Time { return r.now }
	r.l = Layout{Root: r.root, Matcher: Matcher{}, Container: ".mkv", Cache: r.cache}
	r.ref = Layout{Root: r.root, Matcher: Matcher{}, Container: ".mkv"}
	r.touchDir()
	return r
}

func (r *simRig) seg(start time.Time, zone int) string {
	return r.cam + "_" + start.In(time.FixedZone("", zone*3600)).Format("20060102_150405-0700") + ".mkv"
}

// write creates or appends n bytes to name and sets its mtime to the clock.
func (r *simRig) write(name string, n int) {
	r.t.Helper()
	p := filepath.Join(r.dir, name)
	_, statErr := os.Lstat(p)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, n)); err != nil {
		r.t.Fatal(err)
	}
	f.Close()
	r.mtime(name, r.now)
	if statErr != nil {
		r.touchDir()
	}
}

func (r *simRig) mtime(name string, at time.Time) {
	r.t.Helper()
	if err := os.Chtimes(filepath.Join(r.dir, name), at, at); err != nil {
		r.t.Fatal(err)
	}
}

func (r *simRig) remove(name string) {
	r.t.Helper()
	if err := os.Remove(filepath.Join(r.dir, name)); err != nil {
		r.t.Fatal(err)
	}
	r.touchDir()
}

func (r *simRig) rename(from, to string) {
	r.t.Helper()
	if err := os.Rename(filepath.Join(r.dir, from), filepath.Join(r.dir, to)); err != nil {
		r.t.Fatal(err)
	}
	r.touchDir()
}

// touchDir sets the folder's mtime to the clock, as the file system does on a namespace change.
func (r *simRig) touchDir() {
	r.t.Helper()
	if err := os.Chtimes(r.dir, r.now, r.now); err != nil {
		r.t.Fatal(err)
	}
}

// check compares the cached Files and Scan with the uncached ones and the old Scan.
func (r *simRig) check(step string) {
	r.t.Helper()
	got, gerr := r.l.Files(r.cam)
	want, werr := r.ref.Files(r.cam)
	if (gerr == nil) != (werr == nil) || !reflect.DeepEqual(views(got), views(want)) {
		r.t.Fatalf("%s: Files\n cached   %+v %v\n uncached %+v %v", step, views(got), gerr, views(want), werr)
	}
	gs, gerr := r.l.Scan(r.cam, r.now, 900)
	ws, werr := oldScan(Matcher{}, r.dir, ".mkv", r.now, 900)
	if (gerr == nil) != (werr == nil) || !reflect.DeepEqual(gs, ws) {
		r.t.Fatalf("%s: Scan\n cached %+v %v\n old    %+v %v", step, gs, gerr, ws, werr)
	}
}

func (r *simRig) tick(d time.Duration) { r.now = r.now.Add(d) }

// fill writes n closed 15-minute segments ending at the clock and one open one.
func (r *simRig) fill(n int) (names []string) {
	start := r.now.Add(-time.Duration(n) * 15 * time.Minute)
	for i := 0; i <= n; i++ {
		st := start.Add(time.Duration(i) * 15 * time.Minute)
		name := r.seg(st, 3)
		p := filepath.Join(r.dir, name)
		if err := os.WriteFile(p, make([]byte, 1000+i), 0o644); err != nil {
			r.t.Fatal(err)
		}
		end := st.Add(15 * time.Minute)
		if i == n {
			end = r.now
		}
		r.mtime(name, end)
		names = append(names, name)
	}
	r.touchDir()
	return names
}

func TestCacheScenariosEqualUncached(t *testing.T) {
	t.Run("added, deleted, renamed, renamed over", func(t *testing.T) {
		r := newSimRig(t, "Cam_A")
		names := r.fill(40)
		r.check("first")
		for i := 0; i < 5; i++ {
			r.tick(5 * time.Second)
			r.check(fmt.Sprintf("idle %d", i))
		}
		r.tick(5 * time.Second)
		r.write("notes.txt", 3)
		r.check("added")
		r.tick(5 * time.Second)
		r.remove(names[0])
		r.check("deleted")
		r.tick(5 * time.Second)
		r.rename(names[1], "renamed.mkv")
		r.check("renamed")
		r.tick(5 * time.Second)
		r.rename(names[2], names[3]) // over a cold name with another size and time
		r.check("renamed over")
		r.tick(5 * time.Second)
		r.check("after")
	})
	t.Run("growing newest and the segment roll", func(t *testing.T) {
		r := newSimRig(t, "Cam_A")
		names := r.fill(40)
		open := names[len(names)-1]
		r.check("first")
		before := r.cache.Stats()
		for i := 0; i < 20; i++ {
			r.tick(5 * time.Second)
			r.write(open, 4096)
			r.check(fmt.Sprintf("grow %d", i))
		}
		if d := r.cache.Stats().Stats - before.Stats; d > 20*2*(2+1+2) { // Files and Scan per step: hot entries plus the folder
			t.Errorf("growing the newest file read %d infos in 20 steps: the cold files are re-read", d)
		}
		r.tick(5 * time.Second)
		next := r.seg(r.now, 3)
		r.write(next, 10)
		r.check("roll")
		for i := 0; i < 30; i++ {
			r.tick(5 * time.Second)
			r.write(next, 4096)
			r.check(fmt.Sprintf("after the roll %d", i))
		}
	})
	t.Run("directory replaced under the same name and time", func(t *testing.T) {
		r := newSimRig(t, "Cam_A")
		names := r.fill(10)
		r.check("first")
		r.tick(5 * time.Second)
		r.check("second")
		dirTime := r.now
		r.touchDir()
		r.tick(5 * time.Second)
		r.check("third")
		if err := os.Rename(r.dir, r.dir+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(r.dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for i, n := range names {
			if err := os.WriteFile(filepath.Join(r.dir, n), make([]byte, 7*i), 0o644); err != nil {
				t.Fatal(err)
			}
			r.mtime(n, dirTime.Add(-time.Hour))
		}
		if err := os.Chtimes(r.dir, dirTime, dirTime); err != nil { // the same folder time: only the identity differs
			t.Fatal(err)
		}
		r.check("replaced")
	})
	t.Run("clock steps back an hour while recording", func(t *testing.T) {
		r := newSimRig(t, "Cam_A")
		names := r.fill(20)
		open := names[len(names)-1]
		for i := 0; i < 4; i++ {
			r.tick(5 * time.Second)
			r.write(open, 100)
			r.check(fmt.Sprintf("before %d", i))
		}
		r.now = r.now.Add(-time.Hour) // the step: the next segment is named and written an hour earlier
		back := r.seg(r.now, 3)
		r.write(back, 10)
		r.check("stepped")
		for i := 0; i < 40; i++ {
			r.tick(5 * time.Second)
			r.write(back, 4096)
			r.check(fmt.Sprintf("after the step %d", i))
		}
	})
	t.Run("clock change night: +0300 and +0200 in one folder", func(t *testing.T) {
		r := newSimRig(t, "Cam_A")
		r.now = time.Date(2026, 10, 25, 0, 0, 3, 0, time.UTC) // 03:00 summer time = 02:00 winter time
		r.fill(8)
		r.check("summer")
		for i := 0; i < 3; i++ {
			r.tick(15 * time.Minute)
			name := r.seg(r.now, 2) // after the fall-back: names repeat the wall time with +0200
			r.write(name, 500)
			r.check(fmt.Sprintf("winter %d", i))
			r.tick(5 * time.Second)
			r.write(name, 500)
			r.check(fmt.Sprintf("winter grow %d", i))
		}
	})
	t.Run("type change under the same name", func(t *testing.T) {
		r := newSimRig(t, "Cam_A")
		names := r.fill(5)
		r.check("first")
		r.tick(5 * time.Second)
		r.remove(names[1])
		if err := os.Symlink(filepath.Join(r.dir, names[0]), filepath.Join(r.dir, names[1])); err != nil {
			t.Skip("no symbolic links here:", err)
		}
		r.touchDir()
		r.check("now a link")
	})
	t.Run("a missing folder, then one that appears", func(t *testing.T) {
		r := newSimRig(t, "Cam_A")
		if err := os.Remove(r.dir); err != nil {
			t.Fatal(err)
		}
		r.check("missing")
		if err := os.Mkdir(r.dir, 0o755); err != nil {
			t.Fatal(err)
		}
		r.fill(3)
		r.check("appeared")
	})
}

// TestCacheStalenessIsBoundedAndInvalidateClearsIt is the documented exception: a cold file
// changed in place (no name created, deleted or renamed) shows its old information for at most
// FullEvery-1 calls, and Invalidate ends it at once.
func TestCacheStalenessIsBoundedAndInvalidateClearsIt(t *testing.T) {
	r := newSimRig(t, "Cam_A")
	r.cache.FullEvery = 5
	names := r.fill(10)
	cold := names[2]
	r.check("first") // two calls each: Files and Scan
	r.tick(5 * time.Second)
	r.check("second") // the first listing saw a folder time of that instant (racy): re-read once more
	r.tick(5 * time.Second)
	r.write(cold, 9) // in place: the folder time does not move
	r.mtime(cold, r.now.Add(-time.Hour))
	stale := 0
	for i := 0; i < 5; i++ {
		got, _ := r.l.Files(r.cam)
		want, _ := r.ref.Files(r.cam)
		if !reflect.DeepEqual(views(got), views(want)) {
			stale++
		}
	}
	if stale == 0 || stale > 4 {
		t.Errorf("stale for %d of 5 calls; want between 1 and FullEvery-1", stale)
	}
	r.check("after a full re-read")
	r.write(cold, 9)
	r.mtime(cold, r.now.Add(-2*time.Hour))
	r.l.Invalidate(r.cam)
	r.check("after Invalidate")
	r.write(cold, 9)
	r.mtime(cold, r.now.Add(-3*time.Hour))
	r.cache.InvalidateAll()
	r.check("after InvalidateAll")
}

// TestCacheConcurrentReaders: readers of several folders at once get what an uncached listing gives
// (run with -race).
func TestCacheConcurrentReaders(t *testing.T) {
	r := newSimRig(t, "Cam_A")
	r.fill(30)
	cams := []string{"Cam_A", "Cam_B", "Cam_C"}
	for _, c := range cams[1:] {
		o := newSimRig(t, c)
		o.root = r.root
		o.dir = filepath.Join(r.root, c)
		if err := os.MkdirAll(o.dir, 0o755); err != nil {
			t.Fatal(err)
		}
		o.now = r.now
		o.fill(20)
	}
	for round := 0; round < 3; round++ {
		want := map[string][]fileView{}
		for _, c := range cams {
			f, err := r.ref.Files(c)
			if err != nil {
				t.Fatal(err)
			}
			want[c] = views(f)
		}
		var wg sync.WaitGroup
		errs := make(chan string, 64)
		for g := 0; g < 12; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := 0; i < 30; i++ {
					c := cams[(g+i)%len(cams)]
					got, err := r.l.Files(c)
					if err != nil || !reflect.DeepEqual(views(got), want[c]) {
						errs <- fmt.Sprintf("round %d reader %d %s: %v", round, g, c, err)
						return
					}
					if _, err := r.l.Scan(c, r.now, 900); err != nil {
						errs <- err.Error()
						return
					}
				}
			}(g)
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Error(e)
		}
		r.tick(5 * time.Second)
		r.write(r.seg(r.now, 3), 77)
	}
}

func TestCacheIsBounded(t *testing.T) {
	root := t.TempDir()
	c := NewCache()
	c.MaxDirs = 10
	l := Layout{Root: root, Cache: c}
	for i := 0; i < 40; i++ {
		cam := fmt.Sprintf("Cam_%02d", i)
		if err := os.Mkdir(filepath.Join(root, cam), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := l.Files(cam); err != nil {
			t.Fatal(err)
		}
		if _, err := l.Files("Missing"); err == nil {
			t.Fatal("a missing folder lists")
		}
	}
	c.mu.Lock()
	n := len(c.dirs)
	_, missing := c.dirs[filepath.Join(root, "Missing")]
	c.mu.Unlock()
	if n > 10 || missing {
		t.Errorf("%d folders remembered (missing one kept: %v), want at most 10", n, missing)
	}
}

func TestCacheOnWindowsReusesNothing(t *testing.T) {
	r := newSimRig(t, "Cam_A")
	r.cache.reuse = false
	names := r.fill(10)
	r.check("first")
	r.write(names[2], 9) // in place, cold: still fresh because nothing is reused
	r.mtime(names[2], r.now.Add(-time.Hour))
	r.check("in place")
	if s := r.cache.Stats(); s.FullRereads != 0 || s.Stats != 4*11 {
		t.Errorf("stats %+v: every entry is read from the listing on every call", s)
	}
}

// TestCacheStatCount is the benchmark of the brief: 360 segments in a camera folder, the status
// tick every 5 s (12 per minute), a new segment every 15 minutes and the oldest pruned, for an
// hour. At every tick the cached scan equals the old one; the entry stats per minute are counted
// for the old scan (an lstat per entry plus the live size of the newest) and the cached one.
func TestCacheStatCount(t *testing.T) {
	for _, every := range []int{30, 90, 180} {
		r := newSimRig(t, "Cam_A")
		r.cache.FullEvery = every
		names := r.fill(359) // 360 files
		live := 0
		newestSize = func(p string) (int64, error) { live++; return LiveSize(p) }
		t.Cleanup(func() { newestSize = LiveSize })
		const ticks = 12 * 60
		oldStats, base := 0, r.cache.Stats()
		for i := 0; i < ticks; i++ {
			r.tick(5 * time.Second)
			if i%180 == 97 { // every 15 minutes: the roll and the prune of the oldest, 3 s before the tick
				r.tick(-3 * time.Second)
				names = append(names, r.seg(r.now, 3))
				r.write(names[len(names)-1], 10)
				r.remove(names[0])
				names = names[1:]
				r.tick(3 * time.Second)
			} else {
				r.write(names[len(names)-1], 4096)
			}
			got, gerr := r.l.Scan(r.cam, r.now, 900)
			want, werr := oldScan(Matcher{}, r.dir, ".mkv", r.now, 900)
			if gerr != nil || werr != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("FullEvery %d tick %d:\n cached %+v %v\n old    %+v %v", every, i, got, gerr, want, werr)
			}
			oldStats += want.Files + 1
		}
		s := r.cache.Stats()
		newStats := int(s.Stats-base.Stats) + live // the old scan calls LiveSize itself, not newestSize
		perMinOld, perMinNew := oldStats/60, newStats/60
		t.Logf("FullEvery %d: per camera per minute, old %d stats, cached %d (%d full re-reads in the hour, %d listings)",
			every, perMinOld, perMinNew, s.FullRereads-base.FullRereads, s.Listings-base.Listings)
		if perMinOld < 4300 || perMinNew*15 > perMinOld {
			t.Errorf("FullEvery %d: %d -> %d stats per minute; want at least 15 times fewer", every, perMinOld, perMinNew)
		}
		if every == 180 && perMinNew >= 100 {
			t.Errorf("FullEvery 180: %d stats per minute, want below 100", perMinNew)
		}
	}
}
