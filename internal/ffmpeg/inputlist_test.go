package ffmpeg

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"holocron/internal/osx"
)

const (
	fakeUser = "fakeuser"
	fakePass = "fakepass"
	fakeURL  = "rtsp://fakeuser:fakepass@192.0.2.11/stream1"
)

func directInputs(t *testing.T) Inputs {
	t.Helper()
	g := loadGoldens(t)
	for _, v := range g {
		if v.ID == "mkv-direct-900" {
			return v.inputs()
		}
	}
	t.Fatal("mkv-direct-900 golden missing")
	return Inputs{}
}

func TestInputListContentAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run", "ffin")
	plat := &osx.Fake{}
	path, cleanup, err := WriteInputList(plat, dir, fakeURL, 10000000, "tcp")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "ffconcat version 1.0\nfile 'rtsp://fakeuser:fakepass@192.0.2.11/stream1'\noption rtsp_transport tcp\noption timeout 10000000\n"
	if string(b) != want {
		t.Fatalf("list:\n%q\nwant:\n%q", b, want)
	}
	if got := plat.OwnerOnlyCalls(); len(got) != 1 || got[0] != path {
		t.Fatalf("SetOwnerOnly calls %v, want [%s]", got, path)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", fi.Mode().Perm())
		}
		di, _ := os.Stat(dir)
		if di.Mode().Perm()&0o077 != 0 {
			t.Fatalf("directory mode %v", di.Mode().Perm())
		}
	}
	cleanup()
	cleanup() // idempotent
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("list still there: %v", err)
	}
}

func TestInputListFailsClosedWhenOwnerOnlyFails(t *testing.T) {
	dir := t.TempDir()
	_, _, err := WriteInputList(&osx.Fake{OwnerErr: errors.New("acl failed")}, dir, fakeURL, 1, "tcp")
	if err == nil {
		t.Fatal("no error")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
}

func TestInputListRejectsUnusableValues(t *testing.T) {
	dir := t.TempDir()
	for name, c := range map[string]struct {
		url, transport string
		timeout        int64
	}{
		"empty url":        {"", "tcp", 1},
		"newline in url":   {"rtsp://u:p@h/t\nfile '/etc/passwd'", "tcp", 1},
		"bad transport":    {fakeURL, "tcp\noption x y", 1},
		"empty transport":  {fakeURL, "", 1},
		"zero timeout":     {fakeURL, "tcp", 0},
		"negative timeout": {fakeURL, "tcp", -5},
	} {
		if _, _, err := WriteInputList(&osx.Fake{}, dir, c.url, c.timeout, c.transport); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestConcatQuote(t *testing.T) {
	for in, want := range map[string]string{
		"rtsp://h/t":           `'rtsp://h/t'`,
		"rtsp://u:it's@h/t":    `'rtsp://u:it'\''s@h/t'`,
		`rtsp://u:a\b@h/t`:     `'rtsp://u:a\b@h/t'`,
		"rtsp://u:''@h":        `'rtsp://u:'\'''\''@h'`,
		"rtsp://u:p ass@h/t t": `'rtsp://u:p ass@h/t t'`,
	} {
		if got := concatQuote(in); got != want {
			t.Errorf("%q: got %s want %s", in, got, want)
		}
		if back := unquoteToken(t, want); back != in {
			t.Errorf("%q: av_get_token model gives %q", in, back)
		}
	}
}

// unquoteToken models libavutil's av_get_token with a space terminator, to prove the quoting
// round-trips (the real ffmpeg is exercised by the opt-in test).
func unquoteToken(t *testing.T, s string) string {
	t.Helper()
	var out strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		i++
		switch {
		case c == '\\' && i < len(s):
			out.WriteByte(s[i])
			i++
		case c == '\'':
			for i < len(s) && s[i] != '\'' {
				out.WriteByte(s[i])
				i++
			}
			if i < len(s) {
				i++
			}
		case c == ' ':
			t.Fatalf("unquoted space in %s", s)
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

func TestPasswordAbsentFromDirectArgv(t *testing.T) {
	dir := t.TempDir()
	in := directInputs(t)
	list, cleanup, err := WriteInputList(&osx.Fake{}, dir, in.URL, in.TimeoutMicroseconds, in.RtspTransport)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	args, err := BuildArgsDirect(in, list)
	if err != nil {
		t.Fatal(err)
	}
	for i, a := range args {
		if strings.Contains(a, fakePass) || strings.Contains(a, fakeUser) || strings.Contains(a, "@") {
			t.Errorf("argument %d %q carries the credential", i, a)
		}
	}
	joined := " " + strings.Join(args, " ") + " "
	for _, must := range []string{" -f concat ", " -safe 0 ", " -protocol_whitelist file,rtsp,tcp,rtp,udp ", " -i " + list + " ", " -segment_format matroska "} {
		if !strings.Contains(joined, must) {
			t.Errorf("direct vector lacks %q: %q", must, args)
		}
	}
	for _, gone := range []string{" -rtsp_transport ", " -timeout "} {
		if strings.Contains(joined, gone) {
			t.Errorf("direct vector still has %q", gone)
		}
	}
	// The tail (mapping, copy, segment flags, pattern) is the golden's, byte for byte.
	golden, err := BuildArgs(in)
	if err != nil {
		t.Fatal(err)
	}
	iGold, iDirect := indexOf(golden, "-map"), indexOf(args, "-map")
	if iGold < 0 || iDirect < 0 || strings.Join(golden[iGold:], "\x00") != strings.Join(args[iDirect:], "\x00") {
		t.Errorf("tails differ:\n%q\n%q", golden[iGold:], args[iDirect:])
	}

	// Positive control: the same URL on the legacy path DOES put the password in the argv, and the
	// check above would have caught it; and the list file does hold it (that is the point).
	legacy, err := BuildArgs(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(legacy, " "), fakePass) {
		t.Fatal("positive control failed: the legacy vector should carry the password")
	}
	b, _ := os.ReadFile(list)
	if !strings.Contains(string(b), fakePass) {
		t.Fatal("positive control failed: the list file should carry the password")
	}
}

func indexOf(s []string, v string) int {
	for i := range s {
		if s[i] == v {
			return i
		}
	}
	return -1
}

func TestBuildArgsDirectRequiresList(t *testing.T) {
	if _, err := BuildArgsDirect(directInputs(t), ""); err == nil {
		t.Fatal("no error")
	}
}

func TestSweepInputLists(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	mk := func(name string, mtime time.Time) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, mtime, mtime)
		return p
	}
	stale := mk("ffin-0011223344556677.ffconcat", old)
	fresh := mk("ffin-8899aabbccddeeff.ffconcat", time.Now().Add(-10*time.Minute))
	foreignOld := mk("notes.txt", old)
	otherSuffix := mk("ffin-00.mkv", old)
	subdir := filepath.Join(dir, "ffin-dir.ffconcat")
	os.Mkdir(subdir, 0o700)
	os.Chtimes(subdir, old, old)
	var link string
	if runtime.GOOS != "windows" {
		link = filepath.Join(dir, "ffin-link.ffconcat")
		if err := os.Symlink(foreignOld, link); err == nil {
			os.Chtimes(link, old, old)
		} else {
			link = ""
		}
	}
	if n := SweepInputLists(dir, time.Hour); n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	for p, wantGone := range map[string]bool{stale: true, fresh: false, foreignOld: false, otherSuffix: false, subdir: false} {
		_, err := os.Lstat(p)
		if gone := os.IsNotExist(err); gone != wantGone {
			t.Errorf("%s: gone=%v want %v", filepath.Base(p), gone, wantGone)
		}
	}
	if link != "" {
		if _, err := os.Lstat(link); err != nil {
			t.Error("a symlink named like a list was removed")
		}
		if _, err := os.Stat(foreignOld); err != nil {
			t.Error("the symlink target was removed")
		}
	}
	// WriteInputList sweeps too.
	stale2 := mk("ffin-aaaaaaaaaaaaaaaa.ffconcat", old)
	if _, c, err := WriteInputList(&osx.Fake{}, dir, fakeURL, 1, "tcp"); err != nil {
		t.Fatal(err)
	} else {
		defer c()
	}
	if _, err := os.Stat(stale2); !os.IsNotExist(err) {
		t.Error("WriteInputList did not sweep")
	}
	if n := SweepInputLists(filepath.Join(dir, "missing"), time.Hour); n != 0 {
		t.Fatal(n)
	}
}
