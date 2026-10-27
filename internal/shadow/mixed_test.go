package shadow

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"holocron/internal/archive"
	"holocron/internal/archive/archivetest"
)

// TestCompareListingOnMixedTrees: archive compare lists a camera's segments from the spool and its
// day folders, and everything else (misfiled, foreign, other folders, links) as other, as a
// brute-force walk of the tree says.
func TestCompareListingOnMixedTrees(t *testing.T) {
	n, others := 0, 0
	for _, tr := range archivetest.Mixed(20261031, 60) {
		root, _ := tr.Build(t, t.TempDir())
		for _, fold := range []bool{false, true} {
			truth := archivetest.Walk(t, root, tr.Container, fold)
			o := &Options{Old: root, New: root, Now: tr.Now, Container: tr.Container, Matcher: archive.Matcher{FoldCase: fold}}
			o.Defaults()
			for _, c := range truth.Cameras {
				var wantSegs, wantOther []string
				foot := map[string]bool{}
				for _, s := range c.Footage {
					foot[s.Path] = true
				}
				for _, f := range c.AllFiles() {
					rel := f.Name
					if f.Day != "" {
						rel = f.Day + "/" + f.Name
					}
					if foot[f.Path] {
						wantSegs = append(wantSegs, f.Name)
					} else {
						wantOther = append(wantOther, rel)
					}
				}
				ents, _ := os.ReadDir(c.Dir)
				for _, e := range ents {
					fi, _ := os.Lstat(filepath.Join(c.Dir, e.Name()))
					if fi.IsDir() && !archivetest.IsDay(e.Name()) {
						wantOther = append(wantOther, e.Name())
					}
				}
				l, err := list(o, root, c.Name)
				if err != nil {
					t.Fatal(err)
				}
				var gotSegs []string
				for _, s := range l.closed {
					gotSegs = append(gotSegs, s.name)
				}
				gotSegs = append(gotSegs, l.open...)
				sort.Strings(gotSegs)
				sort.Strings(wantSegs)
				sort.Strings(wantOther)
				if strings.Join(gotSegs, ",") != strings.Join(wantSegs, ",") || strings.Join(l.other, ",") != strings.Join(wantOther, ",") {
					t.Errorf("%s/%s fold=%v:\n segs  %v\n want  %v\n other %v\n want  %v", tr.Name, c.Name, fold, gotSegs, wantSegs, l.other, wantOther)
				}
				n += len(gotSegs)
				others += len(l.other)
			}
		}
	}
	t.Logf("%d segments listed, %d others", n, others)
	if n < 500 {
		t.Error("the corpus is too thin")
	}
}
