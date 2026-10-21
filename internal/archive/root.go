package archive

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"holocron/internal/osx"
)

// syntax is the path grammar of an OS, applied lexically so a Windows table can be tested on Linux.
type syntax struct {
	win  bool // backslash and drive letters, UNC roots
	fold bool // case-insensitive comparison (Windows, macOS)
}

func syntaxFor(p osx.Platform) syntax {
	n := p.Name()
	return syntax{win: n == "windows", fold: n == "windows" || n == "darwin"}
}

type parsedPath struct {
	volume string   // "C:" or `\\server\share`; "" on unix
	elems  []string // components below the volume root
	kind   string   // "drive", "share" for a Windows volume; "" otherwise
}

// parse cleans an absolute path lexically ("." and ".." resolved, doubled separators collapsed).
// ok is false for a relative path or an unusable one (why says which).
func (s syntax) parse(path string) (pp parsedPath, why string) {
	if !s.win {
		if !strings.HasPrefix(path, "/") {
			return pp, "is not an absolute path"
		}
		pp.elems = clean(strings.Split(path, "/"))
		return pp, ""
	}
	p := strings.ReplaceAll(path, "/", `\`)
	switch {
	case strings.HasPrefix(p, `\\?\UNC\`):
		p = `\\` + p[len(`\\?\UNC\`):]
	case strings.HasPrefix(p, `\\?\`) || strings.HasPrefix(p, `\\.\`):
		p = p[4:]
	}
	if strings.HasPrefix(p, `\\`) {
		parts := strings.Split(strings.TrimLeft(p, `\`), `\`)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return pp, "is not a usable UNC path (it needs a server and a share)"
		}
		pp.volume, pp.kind = `\\`+parts[0]+`\`+parts[1], "share"
		pp.elems = clean(parts[2:])
		return pp, ""
	}
	if len(p) >= 3 && p[1] == ':' && p[2] == '\\' && isLetter(p[0]) {
		pp.volume, pp.kind = strings.ToUpper(p[:2]), "drive"
		pp.elems = clean(strings.Split(p[3:], `\`))
		return pp, ""
	}
	return pp, "is not an absolute path"
}

func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func clean(parts []string) []string {
	out := make([]string, 0, len(parts))
	for _, e := range parts {
		switch e {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, e)
		}
	}
	return out
}

func (s syntax) eq(a, b string) bool {
	if s.fold {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// within reports whether b lies under a (or is a): a's volume and elements are a prefix of b's.
func (s syntax) within(a, b parsedPath) (same bool, inside bool) {
	if !s.eq(a.volume, b.volume) || len(a.elems) > len(b.elems) {
		return false, false
	}
	for i, e := range a.elems {
		if !s.eq(e, b.elems[i]) {
			return false, false
		}
	}
	return len(a.elems) == len(b.elems), true
}

// pathProblem judges a path lexically against the protected set; "" means acceptable.
func (s syntax) pathProblem(path string, protected []osx.Protected) string {
	root, why := s.parse(path)
	if why != "" {
		return fmt.Sprintf("RootPath '%s' %s", path, why)
	}
	if len(root.elems) == 0 {
		switch root.kind {
		case "share":
			return fmt.Sprintf("RootPath '%s' is the root of the share %s", path, root.volume)
		case "drive":
			return fmt.Sprintf("RootPath '%s' is the root of the volume %s\\", path, root.volume)
		}
		return fmt.Sprintf("RootPath '%s' is the root of the filesystem", path)
	}
	for _, pr := range protected {
		if strings.TrimSpace(pr.Path) == "" {
			continue
		}
		pp, why := s.parse(pr.Path)
		if why != "" {
			continue
		}
		same, inside := s.within(root, pp)
		if same {
			return fmt.Sprintf("RootPath '%s' is %s", path, pr.Why)
		}
		if inside {
			return fmt.Sprintf("RootPath '%s' contains %s (%s)", path, pr.Why, pr.Path)
		}
	}
	return ""
}

// CheckRoot is the Get-CamArchiveRootProblem gate: it returns why path is unsafe to use as the
// archive root, or "" when it is acceptable. Refused: an empty or relative path; the root of a
// drive, a UNC share or the filesystem; a protected system folder, the user profile or the folder
// holding profiles, a mount point, the program's own folder (osx.Platform.ProtectedRoots) and any
// ANCESTOR of one of them; a root that is itself a symbolic link or reparse point, or that resolves
// (through a symlinked ancestor) to a refused place. A folder INSIDE a protected one (Videos\cameras)
// is accepted here: the ownership marker decides that case.
func CheckRoot(path string, p osx.Platform) string {
	if strings.TrimSpace(path) == "" {
		return "RootPath is empty"
	}
	s := syntaxFor(p)
	protected := p.ProtectedRoots()
	if problem := s.pathProblem(path, protected); problem != "" {
		return problem
	}
	if p.Name() != runtime.GOOS { // a foreign platform's table: lexical checks only
		return ""
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return "" // a root that does not exist yet is created later; claiming checks existence
	}
	if IsReparse(fi) {
		return fmt.Sprintf("RootPath '%s' is a symbolic link or reparse point; point RootPath at the real folder", path)
	}
	// A protected path is compared in its real form too: a resolved root (a long name, /private/var)
	// would not match a protected one spelled differently (an 8.3 name in %TEMP%, /var).
	protected = withRealPaths(protected)
	if problem := s.pathProblem(path, protected); problem != "" {
		return problem
	}
	if resolved, err := realPath(path); err == nil && resolved != filepath.Clean(path) {
		if problem := s.pathProblem(resolved, protected); problem != "" {
			return fmt.Sprintf("RootPath '%s' resolves to %s", path, strings.TrimPrefix(problem, "RootPath "))
		}
	}
	return ""
}

// withRealPaths adds the real form of each existing protected path where it differs. Mount points
// are left as they are (already real, and reading a stale network mount can hang).
func withRealPaths(protected []osx.Protected) []osx.Protected {
	out := append([]osx.Protected(nil), protected...)
	for _, pr := range protected {
		if pr.Why == osx.WhyMount || strings.TrimSpace(pr.Path) == "" {
			continue
		}
		if r, err := realPath(pr.Path); err == nil && r != filepath.Clean(pr.Path) {
			out = append(out, osx.Protected{Path: r, Why: pr.Why})
		}
	}
	return out
}

// IsReparse reports whether a file or folder (from os.Lstat or DirEntry.Info) is a symbolic link,
// a Windows junction or any other reparse point.
func IsReparse(fi fs.FileInfo) bool {
	return fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 || reparseAttr(fi)
}
