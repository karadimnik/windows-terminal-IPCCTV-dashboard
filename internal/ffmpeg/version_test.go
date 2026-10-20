package ffmpeg

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// Real first lines collected on this PC (2026-10-20), plus labelled synthetic ones for release
// numbering that no installed build shows.
var versionCases = []struct {
	name, line string
	ok         bool
	major      int
	git        bool
	errPart    string
}{
	{"windows gyan git build (real)", "ffmpeg version 2026-07-06-git-c6498178bb-essentials_build-www.gyan.dev Copyright (c) 2000-2026 the FFmpeg developers", true, 0, true, ""},
	{"linux ubuntu 20.04 apt 4.2.7 (real)", "ffmpeg version 4.2.7-0ubuntu0.1 Copyright (c) 2000-2022 the FFmpeg developers", false, 4, false, "too old"},
	{"linux static master build (real)", "ffmpeg version N-127579-g5cb9fcd211-20261016 Copyright (c) 2000-2026 the FFmpeg developers", true, 0, true, ""},
	{"synthetic 8.0.1", "ffmpeg version 8.0.1 Copyright (c) 2000-2026 the FFmpeg developers", true, 8, false, ""},
	{"synthetic 5.0 exactly", "ffmpeg version 5.0 Copyright", true, 5, false, ""},
	{"synthetic 4.4.2 ubuntu 22.04", "ffmpeg version 4.4.2-0ubuntu0.22.04.1 Copyright", false, 4, false, "too old"},
	{"synthetic 5.1.6 debian", "ffmpeg version 5.1.6-0+deb12u1 Copyright", true, 5, false, ""},
	{"synthetic 6.1.1 ubuntu", "ffmpeg version 6.1.1-3ubuntu5 Copyright", true, 6, false, ""},
	{"synthetic gyan release", "ffmpeg version 7.1.1-essentials_build-www.gyan.dev Copyright", true, 7, false, ""},
	{"synthetic git tag n7.0.2", "ffmpeg version n7.0.2 Copyright", true, 7, false, ""},
	{"synthetic 3.4.8", "ffmpeg version 3.4.8-0ubuntu0.2 Copyright", false, 3, false, "too old"},
	{"synthetic old nightly", "ffmpeg version 2021-12-01-git-abcdef0123-full_build-www.gyan.dev Copyright", false, 0, true, "before the 5.0 release"},
	{"synthetic nightly after 5.0", "ffmpeg version 2022-01-17-git-abcdef0123 Copyright", true, 0, true, ""},
	{"synthetic old git- prefix form", "ffmpeg version git-2020-05-01-abcdef0 Copyright", false, 0, true, "before the 5.0 release"},
	{"synthetic UNKNOWN", "ffmpeg version UNKNOWN Copyright", false, 0, false, "cannot recognise"},
	{"synthetic HEAD build", "ffmpeg version HEAD-abc123 Copyright", false, 0, false, "cannot recognise"},
	{"synthetic empty", "", false, 0, false, "cannot find a version line"},
	{"synthetic not ffmpeg", "ffprobe version 7.0 Copyright", false, 0, false, "cannot find a version line"},
}

func TestParseVersion(t *testing.T) {
	for _, c := range versionCases {
		t.Run(c.name, func(t *testing.T) {
			v, err := ParseVersion(c.line + "\nbuilt with gcc\nconfiguration: --enable-x\n")
			if c.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !c.ok {
				if err == nil {
					t.Fatalf("accepted %v", v)
				}
				if !strings.Contains(err.Error(), c.errPart) {
					t.Fatalf("error %q lacks %q", err, c.errPart)
				}
			}
			if v.Major != c.major || v.Git != c.git {
				t.Fatalf("got major %d git %v, want %d %v", v.Major, v.Git, c.major, c.git)
			}
			if c.ok && c.git && v.Warning == "" {
				t.Fatal("an assumed-ok development build must carry a warning")
			}
			if c.ok && !c.git && v.Warning != "" {
				t.Fatalf("release build with a warning: %s", v.Warning)
			}
		})
	}
}

func TestParseVersionRefusalNamesTheProblem(t *testing.T) {
	_, err := ParseVersion("ffmpeg version 4.2.7-0ubuntu0.1 Copyright")
	for _, want := range []string{"4.2.7", "5.0 or later", "-timeout"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("message %v lacks %q", err, want)
		}
	}
}

func TestParseVersionNumbers(t *testing.T) {
	v, err := ParseVersion("ffmpeg version 6.1.2-x Copyright")
	if err != nil || v.Major != 6 || v.Minor != 1 || v.Patch != 2 {
		t.Fatalf("%+v %v", v, err)
	}
}

func fakeFfmpeg(t *testing.T, firstLine string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake")
	}
	p := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho '"+firstLine+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckVersionRunsTheBinary(t *testing.T) {
	v, err := CheckVersion(fakeFfmpeg(t, "ffmpeg version 7.1 Copyright"))
	if err != nil || v.Major != 7 {
		t.Fatalf("%+v %v", v, err)
	}
	if _, err := CheckVersion(fakeFfmpeg(t, "ffmpeg version 4.2.7-0ubuntu0.1 Copyright")); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("4.2.7 not refused: %v", err)
	}
	if _, err := CheckVersion(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing binary is an error")
	}
}

func TestChildEnvTZ(t *testing.T) {
	base := []string{"PATH=/bin", "TZ=Europe/Bucharest", "HOME=/h", "TZX=keep", "tz=lower"}
	cases := []struct {
		goos string
		want []string
	}{
		{"windows", []string{"PATH=/bin", "HOME=/h", "TZX=keep"}},
		{"linux", base},
		{"darwin", base},
	}
	for _, c := range cases {
		in := append([]string(nil), base...)
		got := ChildEnv(in, c.goos)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %q want %q", c.goos, got, c.want)
		}
		if !reflect.DeepEqual(in, base) {
			t.Errorf("%s: the input slice was modified", c.goos)
		}
	}
	if got := ChildEnv(nil, "windows"); len(got) != 0 {
		t.Fatal(got)
	}
	// "TZ" with no '=' and an empty TZ value
	if got := ChildEnv([]string{"TZ", "TZ="}, "windows"); !reflect.DeepEqual(got, []string{"TZ"}) {
		t.Fatalf("got %q", got)
	}
}
