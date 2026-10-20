package prune

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"holocron/internal/archive"
	"holocron/internal/osx"
)

// Every test in this package runs with the sandbox guard on: the executor refuses any root or
// file outside the OS temporary directory, where t.TempDir lives.
func TestMain(m *testing.M) {
	if os.Getenv(SandboxEnv) == "" {
		os.Setenv(SandboxEnv, "1")
	}
	os.Exit(m.Run())
}

// Test files are small: unit physical bytes stand for one GB (as the PowerShell checks do), so
// sizes are multiplied by scale wherever the pruner sees them.
const (
	unit  = 65536
	scale = gib / unit
)

var (
	testNow  = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	testZone = time.FixedZone("", 3*3600)
)

func segName(cam string, start time.Time, ext string) string {
	return archive.FormatSegmentName(cam, start.In(testZone), ext)
}

// scaledFS is the real filesystem with sizes multiplied by scale and a free-space model: Base
// bytes plus every byte removed since Reset (or Base alone when Fixed).
type scaledFS struct {
	OSFS
	mu      sync.Mutex
	Base    uint64
	Fixed   bool
	removed uint64
	Removed []string
	FailOn  map[string]bool // Remove of these paths fails
	Before  func(path string)
	Scale   uint64 // physical to logical bytes; 0 means scale
}

func (s *scaledFS) factor() uint64 {
	if s.Scale == 0 {
		return scale
	}
	return s.Scale
}

func (s *scaledFS) Reset(baseGB float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Base = uint64(baseGB * gib)
	s.removed = 0
	s.Removed = nil
}

func (s *scaledFS) Stat(path string) (Info, error) {
	i, err := s.OSFS.Stat(path)
	i.Size *= int64(s.factor())
	return i, err
}

func (s *scaledFS) Remove(path string) error {
	if s.Before != nil {
		s.Before(path)
	}
	if s.FailOn[path] {
		return fmt.Errorf("simulated failure")
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if err := s.OSFS.Remove(path); err != nil {
		return err
	}
	s.mu.Lock()
	s.removed += uint64(fi.Size()) * s.factor()
	s.Removed = append(s.Removed, path)
	s.mu.Unlock()
	return nil
}

func (s *scaledFS) FreeBytes(string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Fixed {
		return s.Base, nil
	}
	return s.Base + s.removed, nil
}

// freePlatform is the running OS with the free space taken from the model.
type freePlatform struct {
	osx.Platform
	fs *scaledFS
}

func (p freePlatform) DiskFree(path string) (uint64, error) { return p.fs.FreeBytes(path) }

// recorder keeps every log record.
type recorder struct {
	mu   sync.Mutex
	recs []rec
}

type rec struct {
	Level slog.Level
	Msg   string
	Attrs map[string]string
}

func (r *recorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *recorder) Handle(_ context.Context, x slog.Record) error {
	m := map[string]string{}
	x.Attrs(func(a slog.Attr) bool { m[a.Key] = a.Value.String(); return true })
	r.mu.Lock()
	r.recs = append(r.recs, rec{x.Level, x.Message, m})
	r.mu.Unlock()
	return nil
}
func (r *recorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *recorder) WithGroup(string) slog.Handler      { return r }

func (r *recorder) count(match func(rec) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, x := range r.recs {
		if match(x) {
			n++
		}
	}
	return n
}

func msgLike(sub string) func(rec) bool {
	return func(x rec) bool { return strings.Contains(x.Msg, sub) }
}

// rig is one sandbox archive under t.TempDir.
type rig struct {
	t     *testing.T
	dir   string // the sandbox: root and outside live here
	root  string
	now   time.Time
	fs    *scaledFS
	rules Rules
	st    State
	log   *recorder
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "archive")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return &rig{
		t: t, dir: dir, root: root, now: testNow,
		fs:    &scaledFS{OSFS: OSFS{Platform: osx.Current()}},
		rules: Rules{Container: ".mkv", SegmentSeconds: 900, StallFactor: 2.5, MaxAgePruneFraction: 0.5},
		log:   &recorder{},
	}
}

// seg writes a segment of cam named for start, written at start, of phys bytes (default unit).
func (r *rig) seg(cam string, start time.Time, phys ...int) string {
	return r.segExt(cam, cam, start, ".mkv", phys...)
}

func (r *rig) segExt(folder, cam string, start time.Time, ext string, phys ...int) string {
	n := unit / 64
	if len(phys) > 0 {
		n = phys[0]
	}
	return r.put(filepath.Join(r.root, folder, segName(cam, start, ext)), n, start)
}

// put writes a file of n bytes (content unique to its path) with that write time.
func (r *rig) put(path string, n int, mtime time.Time) string {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = path[i%len(path)]
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.touch(path, mtime)
	if err := setCreated(path, mtime); err != nil {
		r.t.Fatal(err)
	}
	return path
}

func (r *rig) touch(path string, mtime time.Time) {
	r.t.Helper()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) claim() {
	r.t.Helper()
	res, err := archive.ClaimRoot(r.root, osx.Current(), r.rules.Container, r.now)
	if err != nil || !res.Created {
		r.t.Fatalf("claim: %+v %v", res, err)
	}
}

func (r *rig) platform() osx.Platform { return freePlatform{osx.Current(), r.fs} }

func (r *rig) inputs(dry bool) Inputs {
	r.t.Helper()
	in, err := Collect(r.root, r.platform(), r.rules, r.now, dry)
	if err != nil {
		r.t.Fatal(err)
	}
	for i := range in.Cameras {
		for j := range in.Cameras[i].Files {
			in.Cameras[i].Files[j].Size *= scale
		}
	}
	return in
}

func (r *rig) exec(dry bool) *Executor {
	return &Executor{FS: r.fs, Log: slog.New(r.log), DryRun: dry}
}

// run is one prune cycle with the rig's state (as Invoke-CamPrune -State).
func (r *rig) run(dry bool) Result {
	r.t.Helper()
	res, err := r.exec(dry).Run(r.inputs(dry), &r.st)
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

// manifest maps every entry under the sandbox (links recorded, never followed) to its content hash.
func (r *rig) manifest() map[string]string {
	r.t.Helper()
	m := map[string]string{}
	err := filepath.WalkDir(r.dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case archive.IsReparse(info):
			m[p] = "link" // WalkDir never follows it
		case info.IsDir():
			m[p] = "dir"
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			h := sha256.Sum256(b)
			m[p] = hex.EncodeToString(h[:]) + "|" + info.ModTime().UTC().String()
		}
		return nil
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return m
}

func diffManifest(before, after map[string]string) (removed, changed, added []string) {
	for k, v := range before {
		w, ok := after[k]
		switch {
		case !ok:
			removed = append(removed, k)
		case w != v:
			changed = append(changed, k)
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			added = append(added, k)
		}
	}
	sort.Strings(removed)
	sort.Strings(changed)
	sort.Strings(added)
	return
}

func paths(ds []Deletion) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Path
	}
	return out
}

func sorted(s []string) []string {
	c := append([]string(nil), s...)
	sort.Strings(c)
	return c
}

func eqList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func ptr(v float64) *float64 { return &v }

func base(ps []string) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = filepath.Base(p)
	}
	return out
}
