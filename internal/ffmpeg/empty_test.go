package ffmpeg

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"holocron/internal/archive"
)

const (
	segA = "Front_20261012_105730+0300.mkv" // the camera "Front"
	segB = "Front_20261012_110000+0300.mkv"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func TestRemoveEmptySegment(t *testing.T) {
	type tc struct {
		name        string
		files       map[string]string // name -> content ("" = empty)
		dirs        []string
		links       map[string]string // link name -> target file name (relative, created in dir)
		wantLeft    []string
		wantRemoved []string
		foreign     int
	}
	cases := []tc{
		{name: "empty newest segment goes", files: map[string]string{segA: "data", segB: ""},
			wantLeft: []string{segA}, wantRemoved: []string{segB}},
		{name: "non-empty segments never touched", files: map[string]string{segA: "data", segB: "x"},
			wantLeft: []string{segA, segB}},
		{name: "every empty segment goes, not only the newest", files: map[string]string{segA: "", segB: ""},
			wantRemoved: []string{segA, segB}},
		{name: "foreign empty files stay", files: map[string]string{
			"notes.txt": "", "Front_20261012_105730+0300.mp4": "", "Front_20261012_105730+0300.mkv.tmp": "",
			"Other_20261012_105730+0300.mkv": "", "Front_export.mkv": "", "Front_2026101_105730+0300.mkv": "", segB: ""},
			wantLeft:    []string{"Front_20261012_105730+0300.mkv.tmp", "Front_20261012_105730+0300.mp4", "Front_2026101_105730+0300.mkv", "Front_export.mkv", "Other_20261012_105730+0300.mkv", "notes.txt"},
			wantRemoved: []string{segB}, foreign: 6},
		{name: "foreign names are case-sensitive on this matcher", files: map[string]string{"Front_20261012_105730+0300.MKV": "", "front_20261012_105730+0300.mkv": ""},
			wantLeft: []string{"Front_20261012_105730+0300.MKV", "front_20261012_105730+0300.mkv"}, foreign: 2},
		{name: "a folder named like a segment is not a file", dirs: []string{segA}, files: map[string]string{segB: ""},
			wantLeft: []string{segA}, wantRemoved: []string{segB}},
		{name: "nothing empty", files: map[string]string{segA: "data"}, wantLeft: []string{segA}},
		{name: "no files", wantLeft: nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "Front")
			os.MkdirAll(dir, 0o755)
			for n, content := range c.files {
				writeFile(t, filepath.Join(dir, n), content)
			}
			for _, d := range c.dirs {
				os.MkdirAll(filepath.Join(dir, d), 0o755)
			}
			r := RemoveEmptySegment(dir, ".mkv", EmptyOptions{Root: root})
			sort.Strings(r.Removed)
			if !reflect.DeepEqual(r.Removed, c.wantRemoved) && !(len(r.Removed) == 0 && len(c.wantRemoved) == 0) {
				t.Errorf("removed %v want %v", r.Removed, c.wantRemoved)
			}
			if got := names(t, dir); !reflect.DeepEqual(got, c.wantLeft) && !(len(got) == 0 && len(c.wantLeft) == 0) {
				t.Errorf("left %v want %v", got, c.wantLeft)
			}
			if r.Foreign != c.foreign {
				t.Errorf("foreign %d want %d", r.Foreign, c.foreign)
			}
		})
	}
}

func TestRemoveEmptySegmentIgnoresLinks(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Front")
	outside := filepath.Join(root, "_state", "precious") // an empty file the recorder did not write
	writeFile(t, outside, "")
	writeFile(t, filepath.Join(dir, segA), "data")
	if err := os.Symlink(outside, filepath.Join(dir, segB)); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	// a symlink to a NON-empty file with a segment name, and a dangling one
	writeFile(t, filepath.Join(root, "other.bin"), "payload")
	linkNonEmpty := "Front_20261012_120000+0300.mkv"
	os.Symlink(filepath.Join(root, "other.bin"), filepath.Join(dir, linkNonEmpty))
	dangling := "Front_20261012_130000+0300.mkv"
	os.Symlink(filepath.Join(root, "nowhere"), filepath.Join(dir, dangling))

	r := RemoveEmptySegment(dir, ".mkv", EmptyOptions{Root: root})
	if len(r.Removed) != 0 || r.Foreign != 1 {
		t.Fatalf("removed %v foreign %d (want none, 1: only the link that reads as empty)", r.Removed, r.Foreign)
	}
	for _, p := range []string{filepath.Join(dir, segB), filepath.Join(dir, linkNonEmpty), filepath.Join(dir, dangling), outside} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s is gone", p)
		}
	}
}

func TestRemoveEmptySegmentFolderGates(t *testing.T) {
	m := archive.Matcher{}
	t.Run("folder not directly under the root", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "nested", "Front")
		writeFile(t, filepath.Join(dir, segA), "")
		r := RemoveEmptySegment(dir, ".mkv", EmptyOptions{Root: root, Matcher: m})
		if len(r.Removed) != 0 || r.Foreign != 1 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("underscore folder", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "_Front")
		writeFile(t, filepath.Join(dir, "_Front_20261012_105730+0300.mkv"), "")
		r := RemoveEmptySegment(dir, ".mkv", EmptyOptions{Root: root, Matcher: m})
		if len(r.Removed) != 0 || r.Foreign != 1 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("prefix must be the folder name", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "Back")
		writeFile(t, filepath.Join(dir, segA), "") // a Front segment inside Back
		r := RemoveEmptySegment(dir, ".mkv", EmptyOptions{Root: root, Matcher: m})
		if len(r.Removed) != 0 || r.Foreign != 1 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("the folder is a symlink", func(t *testing.T) {
		root := t.TempDir()
		real := filepath.Join(t.TempDir(), "Front")
		writeFile(t, filepath.Join(real, segA), "")
		link := filepath.Join(root, "Front")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("no symlinks here: %v", err)
		}
		r := RemoveEmptySegment(link, ".mkv", EmptyOptions{Root: root, Matcher: m})
		if len(r.Removed) != 0 {
			t.Fatalf("%+v", r)
		}
		if _, err := os.Stat(filepath.Join(real, segA)); err != nil {
			t.Fatal("the file behind a linked folder was removed")
		}
	})
	t.Run("case-insensitive root comparison only with FoldCase", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("filesystem is case-insensitive")
		}
		root := t.TempDir()
		dir := filepath.Join(root, "Front")
		writeFile(t, filepath.Join(dir, segA), "")
		upper := strings.ToUpper(root)
		if r := RemoveEmptySegment(dir, ".mkv", EmptyOptions{Root: upper, Matcher: archive.Matcher{}}); len(r.Removed) != 0 {
			t.Fatal("different case matched without FoldCase")
		}
		if r := RemoveEmptySegment(dir, ".mkv", EmptyOptions{Root: upper, Matcher: archive.Matcher{FoldCase: true}}); len(r.Removed) != 1 {
			t.Fatal("FoldCase did not match")
		}
	})
}

func TestRemoveEmptySegmentRootGate(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Front")
	writeFile(t, filepath.Join(dir, segA), "")
	r := RemoveEmptySegment(dir, ".mkv", EmptyOptions{Root: root, RootRefusal: func() string { return "no ownership marker" }})
	if len(r.Removed) != 0 || r.Refusal != "no ownership marker" || len(r.Messages) != 1 ||
		!strings.Contains(r.Messages[0], "pruning is disabled - no ownership marker") {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, segA)); err != nil {
		t.Fatal("removed despite the root gate")
	}
	// the gate is not consulted when there is nothing empty
	called := false
	writeFile(t, filepath.Join(dir, segA), "data")
	RemoveEmptySegment(dir, ".mkv", EmptyOptions{RootRefusal: func() string { called = true; return "x" }})
	if called {
		t.Error("gate consulted with nothing empty")
	}
	// an open gate removes
	writeFile(t, filepath.Join(dir, segB), "")
	r = RemoveEmptySegment(dir, ".mkv", EmptyOptions{Root: root, RootRefusal: func() string { return "" }})
	if len(r.Removed) != 1 || len(r.Messages) != 1 || !strings.HasPrefix(r.Messages[0], "removed empty segment "+segB) {
		t.Fatalf("%+v", r)
	}
}

func TestRemoveEmptySegmentMissingDir(t *testing.T) {
	r := RemoveEmptySegment(filepath.Join(t.TempDir(), "nope"), ".mkv", EmptyOptions{})
	if len(r.Removed) != 0 || r.Foreign != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestRemoveEmptySegmentWindowsCaseFold(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Front")
	writeFile(t, filepath.Join(dir, "Front_20261012_105730+0300.MKV"), "")
	if r := RemoveEmptySegment(dir, ".mkv", EmptyOptions{Root: root, Matcher: archive.Matcher{FoldCase: true}}); len(r.Removed) != 1 {
		t.Fatalf("%+v", r)
	}
}
