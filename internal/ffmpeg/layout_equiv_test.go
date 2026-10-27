package ffmpeg

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"holocron/internal/archive"
	"holocron/internal/archive/archivetest"
)

// oldRemoveEmptySegment is RemoveEmptySegment as it was before the layout library.
func oldRemoveEmptySegment(dir, container string, o EmptyOptions) EmptyResult {
	var r EmptyResult
	entries, err := os.ReadDir(dir)
	if err != nil {
		return r
	}
	var empty []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fi, err := os.Lstat(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if archive.IsReparse(fi) {
			if ti, err := os.Stat(filepath.Join(dir, e.Name())); err == nil && ti.Size() == 0 {
				empty = append(empty, "")
			}
			continue
		}
		if fi.Mode().IsRegular() && fi.Size() == 0 {
			empty = append(empty, e.Name())
		}
	}
	if len(empty) == 0 {
		return r
	}
	if o.RootRefusal != nil {
		if why := o.RootRefusal(); why != "" {
			r.Refusal = why
			r.Messages = append(r.Messages, fmt.Sprintf("left %d empty file(s) in %s alone: pruning is disabled - %s", len(empty), dir, why))
			return r
		}
	}
	folderOK := folderGate(dir, o)
	safe := filepath.Base(dir)
	for _, name := range empty {
		if name == "" || !folderOK || !o.Matcher.IsSegmentOf(name, safe, container) {
			r.Foreign++
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			r.Failures = append(r.Failures, name+": "+err.Error())
			r.Messages = append(r.Messages, fmt.Sprintf("could not remove empty segment %s: %v", name, err))
			continue
		}
		r.Removed = append(r.Removed, name)
		r.Messages = append(r.Messages, fmt.Sprintf("removed empty segment %s: the child was stopped before its header reached disk", name))
	}
	if r.Foreign > 0 {
		r.Messages = append(r.Messages, fmt.Sprintf("%d empty foreign file(s) left alone in %s: not recorder segments", r.Foreign, dir))
	}
	return r
}

// tree lists every path under base, relative, with its size, so two copies can be compared.
func tree(t *testing.T, base string) []string {
	var out []string
	filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(base, p)
		out = append(out, fmt.Sprintf("%s %d %v", rel, fi.Size(), fi.Mode().Type()))
		return nil
	})
	sort.Strings(out)
	return out
}

// TestRemoveEmptySegmentEqualsTheOld runs the old and the new cleanup on two copies of every tree,
// for every top-level entry and three option sets, and compares the results and what is left.
func TestRemoveEmptySegmentEqualsTheOld(t *testing.T) {
	trees := archivetest.All(t, "../../testdata/golden", 120)
	removed := 0
	for _, tr := range trees {
		for k, opt := range []func(root string) EmptyOptions{
			func(root string) EmptyOptions { return EmptyOptions{Root: root, Matcher: archive.DefaultMatcher} },
			func(root string) EmptyOptions { return EmptyOptions{Matcher: archive.Matcher{FoldCase: true}} },
			func(root string) EmptyOptions {
				return EmptyOptions{Root: root, RootRefusal: func() string { return "no ownership marker" }}
			},
		} {
			baseA, baseB := t.TempDir(), t.TempDir()
			rootA, _ := tr.Build(t, baseA)
			rootB, _ := tr.Build(t, baseB)
			ents, err := os.ReadDir(rootA)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range append(ents, nil) {
				name := "Missing"
				if e != nil {
					name = e.Name()
				}
				want := oldRemoveEmptySegment(filepath.Join(rootA, name), tr.Container, opt(rootA))
				got := RemoveEmptySegment(filepath.Join(rootB, name), tr.Container, opt(rootB))
				for i := range got.Messages {
					got.Messages[i] = strings.ReplaceAll(got.Messages[i], rootB, rootA)
				}
				if !reflect.DeepEqual(want, got) {
					t.Fatalf("%s/%s options %d:\n old %+v\n new %+v", tr.Name, name, k, want, got)
				}
				removed += len(got.Removed)
			}
			if a, b := tree(t, baseA), tree(t, baseB); !reflect.DeepEqual(a, b) {
				t.Fatalf("%s options %d: the trees differ after the cleanup\n old %v\n new %v", tr.Name, k, a, b)
			}
		}
	}
	if removed == 0 {
		t.Error("no tree had an empty segment to remove: the corpus does not exercise the cleanup")
	}
	t.Logf("%d trees x 3 option sets: identical results, %d empty segments removed by each", len(trees), removed)
}
