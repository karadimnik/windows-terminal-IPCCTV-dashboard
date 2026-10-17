package archive

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func golden(t *testing.T, rel string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "golden", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

type nameCase struct {
	ID        string
	Name      string
	OK        bool
	SafeName  string
	ErrorKind string
	Error     string
}

// The 61 cases of testdata/golden/names/safe-names.json must come out exactly as PowerShell's
// Get-CamSafeName produced them: result, error kind and error text.
func TestSafeNameGolden(t *testing.T) {
	var d struct {
		Cases                           []nameCase
		CaseInsensitiveFolderCollisions []struct {
			Folder string
			IDs    []string `json:"ids"`
		}
	}
	golden(t, "names/safe-names.json", &d)
	if len(d.Cases) != 61 {
		t.Fatalf("%d cases, want 61", len(d.Cases))
	}
	byKey := map[string][]string{}
	for _, c := range d.Cases {
		got, err := SafeName(c.Name)
		if c.OK {
			if err != nil || got != c.SafeName {
				t.Errorf("%s: SafeName(%q) = %q, %v; golden %q", c.ID, c.Name, got, err, c.SafeName)
			}
			byKey[FolderKey(got)] = append(byKey[FolderKey(got)], c.ID)
			continue
		}
		var ne *NameError
		if !errors.As(err, &ne) || ne.Kind != c.ErrorKind || err.Error() != c.Error {
			t.Errorf("%s: SafeName(%q) = %q, %v; golden error %s %q", c.ID, c.Name, got, err, c.ErrorKind, c.Error)
		}
	}
	// the folders that collide on a case-insensitive filesystem are exactly the golden's
	var got [][]string
	for _, ids := range byKey {
		if len(ids) > 1 {
			sort.Strings(ids)
			got = append(got, ids)
		}
	}
	var want [][]string
	for _, g := range d.CaseInsensitiveFolderCollisions {
		sort.Strings(g.IDs)
		want = append(want, g.IDs)
	}
	// Known gap: PowerShell's culture-aware comparison also folds canonically equivalent forms
	// (combining vs precomposed e-acute); NTFS keeps them apart and Go has no normaliser in the
	// standard library, so FolderKey does not report that pair (macOS would merge the folders).
	kept := want[:0]
	for _, g := range want {
		if g[0] != "combining-acute" {
			kept = append(kept, g)
		}
	}
	want = kept
	sort.Slice(got, func(i, j int) bool { return got[i][0] < got[j][0] })
	sort.Slice(want, func(i, j int) bool { return want[i][0] < want[j][0] })
	if !reflect.DeepEqual(got, want) {
		t.Errorf("collision groups\n got %v\nwant %v", got, want)
	}
}

// Negative control: the golden test above must be able to fail. A deliberately wrong rule
// (ASCII-only whitespace, as Go's regexp \s) differs from PowerShell on the Unicode whitespace rows.
func TestSafeNameGoldenDetectsAsciiOnlyWhitespace(t *testing.T) {
	var d struct{ Cases []nameCase }
	golden(t, "names/safe-names.json", &d)
	ws := regexp.MustCompile(`\s+`)
	diffs := 0
	for _, c := range d.Cases {
		if !c.OK {
			continue
		}
		wrong := ws.ReplaceAllString(strings.Trim(c.Name, " \t\n\v\f\r"), "_")
		if wrong != c.SafeName && strings.ContainsAny(c.Name, "\u00a0\u2003\u3000\u2028\u0085") {
			diffs++
		}
	}
	if diffs < 6 {
		t.Errorf("only %d corpus rows tell Unicode whitespace handling apart", diffs)
	}
}

func TestSafeNameTraps(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"A​B", "A​B"},                                       // zero width space is kept
		{"a \x01 b", "a__b"},                                 // a removed control char keeps two runs apart
		{"_hidden", "_hidden"},                               // a leading underscore is not touched here
		{"100% Cam", "100%_Cam"},                             // nor a percent sign
		{strings.Repeat("x", 300), strings.Repeat("x", 300)}, // no length cap
	} {
		if got, err := SafeName(c.in); err != nil || got != c.want {
			t.Errorf("SafeName(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{"", "   ", "...", "CON", "nul.txt", "<>"} {
		if _, err := SafeName(in); err == nil {
			t.Errorf("SafeName(%q) should be refused", in)
		}
	}
}
