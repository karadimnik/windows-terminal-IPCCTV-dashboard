package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"holocron/internal/archive"
	"holocron/internal/shadow"
)

// textProber reads "<seconds>[ noaudio]" from the start of a file (the rest is zero padding that
// only gives the file its size), so the command tests need no ffmpeg.
type textProber struct{}

func (textProber) Probe(_ context.Context, path string) (shadow.Probe, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return shadow.Probe{}, err
	}
	head := string(bytes.SplitN(b, []byte{0}, 2)[0])
	f := strings.Fields(head)
	if len(f) == 0 {
		return shadow.Probe{}, os.ErrInvalid
	}
	d, _ := strconv.ParseFloat(f[0], 64)
	p := shadow.Probe{Duration: d, Video: &shadow.Stream{Codec: "h264", Width: 2560, Height: 1440}, Videos: 1}
	if !(len(f) > 1 && f[1] == "noaudio") {
		p.Audio, p.Audios = &shadow.Stream{Codec: "pcm_alaw", SampleRate: 8000, Channels: 1}, 1
	}
	return p, nil
}

type cmpRig struct {
	t        *testing.T
	old, new string
	ffprobe  string
}

func newCmpRig(t *testing.T) *cmpRig {
	t.Helper()
	orig := newProber
	newProber = func(string) shadow.Prober { return textProber{} }
	t.Cleanup(func() { newProber = orig })
	base := t.TempDir()
	r := &cmpRig{t: t, old: filepath.Join(base, "old archive"), new: filepath.Join(base, "new archive"), ffprobe: filepath.Join(base, "ffprobe-stand-in")}
	os.WriteFile(r.ffprobe, []byte("x"), 0o755)
	return r
}

// put writes a closed segment of the given length and head text, a day ago.
func (r *cmpRig) put(root, folder string, start time.Time, head string, size int64) {
	r.t.Helper()
	p := filepath.Join(root, folder, archive.FormatSegmentName(folder, start, ".mkv"))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(head+"\x00"), 0o644); err != nil {
		r.t.Fatal(err)
	}
	os.Truncate(p, size)
	end := start.Add(60 * time.Second)
	os.Chtimes(p, end, end)
}

func (r *cmpRig) series(root, folder string, n int, head string, size int64) {
	day := time.Now().Add(-24 * time.Hour).Truncate(time.Hour)
	for i := 0; i < n; i++ {
		r.put(root, folder, day.Add(time.Duration(i)*time.Minute), head, size)
	}
}

func (r *cmpRig) run(args ...string) (string, string, int) {
	var out, errw bytes.Buffer
	code := run(context.Background(), args, &out, &errw, &seams{})
	return out.String(), errw.String(), code
}

func (r *cmpRig) cmp(extra ...string) (string, string, int) {
	return r.run(append([]string{"archive", "compare", "--old", r.old, "--new", r.new, "--ffprobe", r.ffprobe, "--segment-seconds", "60"}, extra...)...)
}

func TestArchiveCompareExitCodes(t *testing.T) {
	r := newCmpRig(t)
	r.series(r.old, "Front_Door", 6, "60", 1000)
	r.series(r.new, "Front_Door", 6, "60", 1000)

	out, errw, code := r.cmp()
	if code != 0 || !strings.Contains(out, "EQUAL: 1 camera(s) match") || errw != "" {
		t.Fatalf("equal: %d\n%s\n%s", code, out, errw)
	}

	// a different size in the new archive
	r.put(r.new, "Front_Door", time.Now().Add(-24*time.Hour).Truncate(time.Hour).Add(2*time.Minute), "60", 2000)
	out, errw, code = r.cmp()
	if code != 1 || !strings.Contains(out, "DIFFERENT: 1 difference(s)") || !strings.Contains(out, "size ratio new/old 2.000") || errw != "" {
		t.Fatalf("differences: %d\n%s\n%s", code, out, errw)
	}

	for name, args := range map[string][]string{
		"no old":            {"archive", "compare", "--new", r.new},
		"no new":            {"archive", "compare", "--old", r.old},
		"a bad since":       {"archive", "compare", "--old", r.old, "--new", r.new, "--ffprobe", r.ffprobe, "--since", "lastweek"},
		"a missing root":    {"archive", "compare", "--old", filepath.Join(r.old, "nope"), "--new", r.new, "--ffprobe", r.ffprobe},
		"the same root":     {"archive", "compare", "--old", r.old, "--new", r.old, "--ffprobe", r.ffprobe},
		"a missing ffprobe": {"archive", "compare", "--old", r.old, "--new", r.new, "--ffprobe", filepath.Join(r.old, "no-ffprobe")},
		"a stray argument":  {"archive", "compare", "--old", r.old, "--new", r.new, "--ffprobe", r.ffprobe, "extra"},
		"an unknown flag":   {"archive", "compare", "--old", r.old, "--new", r.new, "--nope"},
		"a negative gap":    {"archive", "compare", "--old", r.old, "--new", r.new, "--ffprobe", r.ffprobe, "--gap-tolerance", "-1s"},
	} {
		if _, errw, code := r.run(args...); code != 2 || errw == "" {
			t.Errorf("%s: exit %d (want 2), stderr %q", name, code, errw)
		}
	}
}

func TestArchiveCompareJSONIsStable(t *testing.T) {
	r := newCmpRig(t)
	r.series(r.old, "Front_Door", 6, "60", 1000)
	r.series(r.new, "Front_Door", 6, "60 noaudio", 1000)
	out, _, code := r.cmp("--json")
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, k := range []string{"schema", "old", "new", "checked_at", "params", "equal", "differences", "cameras"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("top-level key %q missing", k)
		}
	}
	cam := doc["cameras"].([]any)[0].(map[string]any)
	for _, k := range []string{"camera", "equal", "window", "old", "new", "pairs", "partials", "unmatched_old", "unmatched_new", "size_ratio", "differences", "notes"} {
		if _, ok := cam[k]; !ok {
			t.Errorf("camera key %q missing", k)
		}
	}
	pair := cam["pairs"].([]any)[0].(map[string]any)
	for _, k := range []string{"old", "new", "kind", "start_diff_seconds", "old_bytes", "new_bytes", "size_ratio", "old_duration_seconds", "new_duration_seconds", "duration_diff_seconds", "old_layout", "new_layout", "flags"} {
		if _, ok := pair[k]; !ok {
			t.Errorf("pair key %q missing", k)
		}
	}
	if doc["schema"].(float64) != 1 || doc["equal"].(bool) || !strings.Contains(out, "has no audio stream") {
		t.Errorf("%s", out)
	}
	// empty lists are [] and never null
	if strings.Contains(out, "null,") && strings.Contains(out, `"gaps": null`) {
		t.Error("gaps must be [] not null")
	}
}

func TestArchiveCompareCameraFilterSinceAndSpaces(t *testing.T) {
	r := newCmpRig(t)
	r.series(r.old, "Front_Door", 6, "60", 1000)
	r.series(r.new, "Front_Door", 6, "60", 1000)
	r.series(r.old, "Back_Door", 6, "60", 1000)
	r.series(r.new, "Back_Door", 6, "60", 2000) // the Back Door pair is wrong

	if out, _, code := r.cmp("--camera", "Front Door"); code != 0 || strings.Contains(out, "Back_Door") {
		t.Fatalf("--camera with a space: %d\n%s", code, out)
	}
	if out, _, code := r.cmp(); code != 1 || !strings.Contains(out, "== Back_Door") || !strings.Contains(out, "== Front_Door") {
		t.Fatalf("all cameras of the new root: %d\n%s", code, out)
	}
	if _, errw, code := r.cmp("--camera", "Front Door", "--camera", "Back Door", "--since", "1000h"); code != 1 {
		t.Fatalf("since: %d %s", code, errw)
	}
	if _, _, code := r.cmp("--camera", "Back Door", "--since", "2h"); code != 1 {
		t.Errorf("a since after the files leaves nothing to compare, which is not equal: %d", code)
	}
}

func TestArchiveCompareWritesNothing(t *testing.T) {
	r := newCmpRig(t)
	r.series(r.old, "Front_Door", 6, "60", 1000)
	r.series(r.new, "Front_Door", 6, "60", 1500)
	before := tree(t, filepath.Dir(r.old))
	r.cmp()
	r.cmp("--json")
	if after := tree(t, filepath.Dir(r.old)); after != before {
		t.Fatalf("the command changed the tree (logs, markers, anything):\n%s\n---\n%s", before, after)
	}
}

func tree(t *testing.T, root string) string {
	var b strings.Builder
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil {
			b.WriteString(p + " " + strconv.FormatInt(fi.Size(), 10) + " " + fi.ModTime().String() + "\n")
		}
		return nil
	})
	return b.String()
}

func TestArchiveCompareIsAnAction(t *testing.T) {
	c, rest := matchCommand([]string{"archive", "compare", "--old", "x"})
	if c == nil || c.action != "archive.compare" || len(rest) != 2 || !c.noRoot || c.mutating {
		t.Fatalf("%+v %v", c, rest)
	}
	if c2, _ := matchCommand([]string{"archive", "stats"}); c2 == nil || c2.action != "archive.stats" {
		t.Error("archive stats is still reachable")
	}
}
