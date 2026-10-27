package archive

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The layout guard: outside internal/archive, no non-test Go file lists a folder or joins an
// archive root with a further component other than one of the recorder's own "_" folders. Every
// camera folder and segment path comes from archive.Layout, so a change of layout stays inside this
// package. The exceptions below are listings of folders that are not camera folders, and the one
// string that is a pattern for ffmpeg rather than a path this program opens.
var guardAllowed = map[string]string{
	"internal/osx/procs_linux.go:ReadDir":   "lists /proc, not the archive",
	"internal/osx/mounts_darwin.go:ReadDir": "lists /Volumes, not the archive",
	"internal/ffmpeg/inputlist.go:ReadDir":  "lists <root>/_state/inputs, the recorder's own folder",
	"internal/web/admin.go:ReadDir":         "lists the TLS folder, not the archive",
	"internal/ffmpeg/args.go:Join":          "SegmentPattern: the strftime output pattern handed to ffmpeg (D20 keeps it), never opened here",
}

// listingCalls are the calls that enumerate a folder.
var listingCalls = map[string]bool{"ReadDir": true, "Readdir": true, "Readdirnames": true, "WalkDir": true, "Walk": true, "Glob": true}

// rootNames are the identifiers that hold an archive root in this code base.
var rootNames = map[string]bool{"root": true, "Root": true, "RootPath": true, "ArchiveRoot": true}

type guardHit struct {
	file, call string
	line       int
	text       string
}

// guardFile reports the listing calls and the root joins of one file's source.
func guardFile(fset *token.FileSet, name string, src any) ([]guardHit, error) {
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		return nil, err
	}
	var hits []guardHit
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pos := fset.Position(call.Pos())
		switch {
		case listingCalls[sel.Sel.Name]:
			hits = append(hits, guardHit{name, sel.Sel.Name, pos.Line, exprText(call.Fun)})
		case sel.Sel.Name == "Join" && len(call.Args) >= 2 && mentionsRoot(call.Args[0]):
			if lit, ok := call.Args[1].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if v, err := strconv.Unquote(lit.Value); err == nil && strings.HasPrefix(v, "_") {
					return true // <root>/_state, <root>/_logs: the recorder's own folders
				}
			}
			hits = append(hits, guardHit{name, "Join", pos.Line, exprText(call.Fun) + "(" + exprText(call.Args[0]) + ", ...)"})
		}
		return true
	})
	return hits, nil
}

func mentionsRoot(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			found = found || rootNames[x.Name]
		case *ast.SelectorExpr:
			found = found || rootNames[x.Sel.Name]
		}
		return !found
	})
	return found
}

func exprText(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprText(x.X) + "." + x.Sel.Name
	case *ast.CallExpr:
		return exprText(x.Fun) + "(...)"
	}
	return "?"
}

func TestNothingOutsideArchiveListsOrJoinsTheArchive(t *testing.T) {
	repo := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var bad []string
	used := map[string]bool{}
	checked := 0
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(repo, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel := filepath.ToSlash(strings.TrimPrefix(p, repo+string(filepath.Separator)))
			if d.IsDir() {
				if rel == "internal/archive" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
				return nil
			}
			hits, err := guardFile(fset, p, nil)
			if err != nil {
				return err
			}
			checked++
			for _, h := range hits {
				key := rel + ":" + h.call
				if _, ok := guardAllowed[key]; ok {
					used[key] = true
					continue
				}
				bad = append(bad, rel+":"+strconv.Itoa(h.line)+": "+h.text)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("%s: list and resolve camera folders and segments through archive.Layout", b)
	}
	for k := range guardAllowed {
		if !used[k] {
			t.Errorf("allow-list entry %s matches nothing any more: remove it", k)
		}
	}
	if checked < 100 {
		t.Errorf("only %d files checked: the walk is not seeing the code base", checked)
	}
}

// TestGuardCatchesTheOldShapes: the guard finds what the readers did before the layout library.
func TestGuardCatchesTheOldShapes(t *testing.T) {
	src := `package x
import ("os"; "path/filepath")
func f(root, cam string, e struct{ paths struct{ Root string } }) {
	os.ReadDir(filepath.Join(root, cam))
	_ = filepath.Join(e.paths.Root, cam, "x.mkv")
	_ = filepath.Join(esc(root), esc(cam))
	_ = filepath.Join(root, "_state", "status.json") // allowed: the recorder's own folder
	_ = filepath.Join(SystemRoot, "System32")         // allowed: not an archive root
	filepath.WalkDir(root, nil)
}`
	hits, err := guardFile(token.NewFileSet(), "x.go", src)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range hits {
		got = append(got, h.call+"@"+strconv.Itoa(h.line))
	}
	if strings.Join(got, " ") != "ReadDir@4 Join@4 Join@5 Join@6 WalkDir@9" {
		t.Errorf("hits %v", got)
	}
}
