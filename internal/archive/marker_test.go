package archive

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"holocron/internal/clock"
	"holocron/internal/osx"
)

func plat() *osx.Fake { return &osx.Fake{OS: runtime.GOOS} }

func mk(t *testing.T, root string, files ...string) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if strings.HasSuffix(f, "/") {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestClaimProblem(t *testing.T) {
	caseWant := "not a recorder segment" // case-sensitive hosts
	if runtime.GOOS == "windows" {
		caseWant = ""
	}
	seg := func(cam string) string { return cam + "/" + cam + "_20261017_120000+0300.mkv" }
	tests := []struct {
		name  string
		files []string
		want  string // substring; "" = claimable
	}{
		{"empty root", nil, ""},
		{"only recorder folders", []string{"_state/status.json", "_logs/recorder.log", "_selftest/x"}, ""},
		{"camera folders with segments", []string{seg("Cam_A"), seg("Cam_B"), "Cam_C/"}, ""},
		{"different case: Windows only", []string{"Cam_A/cam_a_20261017_120000+0300.MKV"}, caseWant},
		{"a file at the top level", []string{seg("Cam_A"), "notes.txt"}, "notes.txt (a file at the top level)"},
		{"a foreign file in a camera folder", []string{"Cam_A/desktop.ini"}, "Cam_A/desktop.ini (not a recorder segment)"},
		{"another camera's segment in the folder", []string{"Cam_A/Cam_B_20261017_120000+0300.mkv"}, "not a recorder segment"},
		{"another container's segment", []string{"Cam_A/Cam_A_20261017_120000+0300.mp4"}, "not a recorder segment"},
		{"a tmp file", []string{"Cam_A/Cam_A_20261017_120000+0300.mkv.tmp"}, "not a recorder segment"},
		{"a unicode-digit lookalike", []string{"Cam_A/Cam_A_٢٠٢٦" + "1017_120000+0300.mkv"}, "not a recorder segment"},
		{"a folder inside a camera folder", []string{"Cam_A/sub/"}, "Cam_A/sub (a folder inside a camera folder)"},
		{"an old-style folder tree", []string{"Photos/2024/img.jpg"}, "Photos/2024 (a folder inside a camera folder)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			mk(t, root, tc.files...)
			got := ClaimProblem(root, plat(), ".mkv")
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("ClaimProblem = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("more than five blockers are summarised", func(t *testing.T) {
		root := t.TempDir()
		for i := 0; i < 8; i++ {
			mk(t, root, fmt.Sprintf("f%d.txt", i))
		}
		got := ClaimProblem(root, plat(), ".mkv")
		if !strings.Contains(got, "holds 8 top-level entries") || strings.Contains(got, "f5.txt") || !strings.Contains(got, "f4.txt") {
			t.Errorf("got %q", got)
		}
	})
	t.Run("missing and unsafe roots", func(t *testing.T) {
		if got := ClaimProblem(filepath.Join(t.TempDir(), "nope"), plat(), ".mkv"); !strings.Contains(got, "does not exist") {
			t.Errorf("missing: %q", got)
		}
		root := t.TempDir()
		p := &osx.Fake{OS: runtime.GOOS, Protected: []osx.Protected{{Path: root, Why: "a protected folder"}}}
		if got := ClaimProblem(root, p, ".mkv"); !strings.Contains(got, "is a protected folder") {
			t.Errorf("protected: %q", got)
		}
	})
	t.Run("a symlink at the top level blocks", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Symlink(t.TempDir(), filepath.Join(root, "Cam_L")); err != nil {
			t.Skip(err)
		}
		if got := ClaimProblem(root, plat(), ".mkv"); !strings.Contains(got, "Cam_L (a reparse point)") {
			t.Errorf("got %q", got)
		}
	})
	t.Run("a symlinked segment blocks", func(t *testing.T) {
		root := t.TempDir()
		mk(t, root, "Cam_A/")
		target := filepath.Join(t.TempDir(), "x")
		os.WriteFile(target, []byte("x"), 0o644)
		if err := os.Symlink(target, filepath.Join(root, "Cam_A", "Cam_A_20261017_120000+0300.mkv")); err != nil {
			t.Skip(err)
		}
		if got := ClaimProblem(root, plat(), ".mkv"); !strings.Contains(got, "not a recorder segment") {
			t.Errorf("got %q", got)
		}
	})
}

func TestClaimRootWritesThePowerShellMarker(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "Cam_A/Cam_A_20261017_120000+0300.mkv", "_logs/recorder.log")
	p := plat()
	now := time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC)

	res, err := ClaimRoot(root, p, ".mkv", now)
	if err != nil || !res.Owned || !res.Created || res.Problem != "" {
		t.Fatalf("claim: %+v, %v", res, err)
	}
	if res.Path != filepath.Join(root, "_state", "archive-owner.json") {
		t.Errorf("path %s", res.Path)
	}
	raw, _ := os.ReadFile(res.Path)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, raw)
	}
	if doc["Owner"] != "IPCCTV recorder" || doc["Schema"] != float64(1) || doc["RootPath"] != root ||
		doc["Note"] != "The recorder prunes footage only under a root holding this file. It wrote it because the root held nothing but its own files. Do not copy it into another folder." {
		t.Errorf("marker content: %s", raw)
	}
	if at, err := clock.ParseTimestamp(doc["CreatedAt"].(string)); err != nil || !at.Equal(now) {
		t.Errorf("CreatedAt %v: %v", doc["CreatedAt"], err)
	}
	if strings.Index(string(raw), `"Owner"`) > strings.Index(string(raw), `"Schema"`) || strings.Index(string(raw), `"Schema"`) > strings.Index(string(raw), `"RootPath"`) {
		t.Errorf("key order differs from the PowerShell marker: %s", raw)
	}
	if raw[len(raw)-1] != '\n' || len(raw) >= 3 && raw[0] == 0xEF {
		t.Error("PowerShell writes UTF-8 without BOM and with a trailing newline")
	}
	if calls := p.OwnerOnlyCalls(); len(calls) != 1 || filepath.Dir(calls[0]) != filepath.Dir(res.Path) {
		t.Errorf("owner-only was requested on %v", calls)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(res.Path); fi.Mode().Perm() != 0o600 {
			t.Errorf("marker mode %v (the fake platform does not chmod, the temp file is 0600 from CreateTemp)", fi.Mode().Perm())
		}
	}
	if left, _ := filepath.Glob(filepath.Join(root, "_state", "*.tmp")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}

	// valid now; a second claim is a no-op that leaves the file untouched
	if ok, why := MarkerValid(root, p); !ok {
		t.Errorf("MarkerValid: %s", why)
	}
	res2, err := ClaimRoot(root, p, ".mkv", now.Add(time.Hour))
	if err != nil || !res2.Owned || res2.Created {
		t.Fatalf("second claim: %+v, %v", res2, err)
	}
	if again, _ := os.ReadFile(res.Path); string(again) != string(raw) {
		t.Error("second claim rewrote the marker")
	}
	if got := VerifyRoot(root, p, ".mkv", false); got != "" {
		t.Errorf("VerifyRoot on a claimed root: %q", got)
	}
}

func TestClaimRootRefusesWithoutWriting(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "Cam_A/Cam_A_20261017_120000+0300.mkv", "holiday.jpg")
	res, err := ClaimRoot(root, plat(), ".mkv", time.Now())
	if err != nil || res.Owned || res.Created || !strings.Contains(res.Problem, "holiday.jpg") {
		t.Fatalf("%+v, %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(root, "_state")); err == nil {
		t.Error("a refused claim must write nothing, not even _state")
	}
	if got := VerifyRoot(root, plat(), ".mkv", false); !strings.Contains(got, "cannot claim the root") || !strings.Contains(got, "holiday.jpg") {
		t.Errorf("VerifyRoot: %q", got)
	}
}

// A root with files in it but no marker; dry run previews the next start.
func TestVerifyRootStates(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "Cam_A/Cam_A_20261017_120000+0300.mkv")
	p := plat()
	if got := VerifyRoot(root, p, ".mkv", false); !strings.Contains(got, "no ownership marker") || !strings.Contains(got, "The root qualifies") {
		t.Errorf("unmarked claimable root: %q", got)
	}
	if got := VerifyRoot(root, p, ".mkv", true); got != "" {
		t.Errorf("dry run of a claimable root must not be refused: %q", got)
	}
	if got := VerifyRoot(t.TempDir()+"/../..", &osx.Fake{OS: runtime.GOOS, Protected: []osx.Protected{{Path: root, Why: "x"}}}, ".mkv", false); got == "" {
		t.Error("an unsafe root must be refused whatever the marker says")
	}
	if _, err := ClaimRoot(root, p, ".mkv", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := VerifyRoot(root, p, ".mkv", false); got != "" {
		t.Errorf("claimed: %q", got)
	}
}

// Markers that must not count: junk, wrong owner, wrong shape, and (unlike PowerShell) one copied from another root.
func TestMarkerValidityNegativeControls(t *testing.T) {
	p := plat()
	write := func(root, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, "_state"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(MarkerPath(root), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	psStyle := func(rootPath string) string { // what ConvertTo-Json produced in PowerShell 5.1, with a BOM-less file
		b, _ := json.Marshal(rootPath)
		return "{\r\n    \"Owner\":  \"IPCCTV recorder\",\r\n    \"Schema\":  1,\r\n    \"RootPath\":  " + string(b) + ",\r\n    \"CreatedAt\":  \"2026-10-17T12:00:00.0000000+03:00\",\r\n    \"Note\":  \"x\"\r\n}\r\n"
	}
	write(root, psStyle(root))
	if ok, why := MarkerValid(root, p); !ok {
		t.Fatalf("a PowerShell-written marker must be accepted: %s", why)
	}
	write(root, "\xef\xbb\xbf"+psStyle(root))
	if ok, why := MarkerValid(root, p); !ok {
		t.Errorf("BOM tolerated: %s", why)
	}
	other := filepath.Join(t.TempDir(), "elsewhere")
	for name, body := range map[string]string{
		"not json":                 "Owner: IPCCTV recorder",
		"empty":                    "",
		"wrong owner":              strings.Replace(psStyle(root), "IPCCTV recorder", "someone else", 1),
		"no owner":                 `{"Schema":1}`,
		"array":                    `[]`,
		"copied from another root": psStyle(other),
		"relative root":            psStyle("cameras"),
	} {
		write(root, body)
		if ok, _ := MarkerValid(root, p); ok {
			t.Errorf("%s: accepted", name)
		}
		// and a refused marker is replaced when the root qualifies
		res, err := ClaimRoot(root, p, ".mkv", time.Now())
		if err != nil || !res.Created {
			t.Errorf("%s: re-claim %+v, %v", name, res, err)
		} else if ok, why := MarkerValid(root, p); !ok {
			t.Errorf("%s: after re-claim: %s", name, why)
		}
	}
	// the root's own spelling does not matter
	write(root, psStyle(filepath.Join(root, "x", "..")+string(filepath.Separator)))
	if ok, why := MarkerValid(root, p); !ok {
		t.Errorf("equivalent spelling of the root: %s", why)
	}
	// a marker that is a directory or a symlink is not a marker
	_ = os.Remove(MarkerPath(root))
	if err := os.Mkdir(MarkerPath(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, _ := MarkerValid(root, p); ok {
		t.Error("a folder named archive-owner.json is accepted")
	}
	if got := VerifyRoot(t.TempDir(), p, ".mkv", false); got == "" { // an empty root with no marker is still refused for pruning
		t.Error("empty unmarked root must be refused until claimed")
	}
}

func TestClaimRootFailsWhenOwnerOnlyFails(t *testing.T) {
	root := t.TempDir()
	p := &osx.Fake{OS: runtime.GOOS, OwnerErr: fmt.Errorf("icacls missing")}
	res, err := ClaimRoot(root, p, ".mkv", time.Now())
	if err == nil || res.Owned {
		t.Fatalf("a marker that cannot be made owner-only must not be written: %+v, %v", res, err)
	}
	if _, serr := os.Stat(MarkerPath(root)); serr == nil {
		t.Error("marker exists")
	}
	if left, _ := filepath.Glob(filepath.Join(root, "_state", "*")); len(left) != 0 {
		t.Errorf("leftovers: %v", left)
	}
}
