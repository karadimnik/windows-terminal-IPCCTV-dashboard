package archive

import (
	"runtime"
	"strings"
	"time"

	"holocron/internal/osx"
)

// Segment file names are <safe>_YYYYMMDD_HHMMSS+HHMM<ext>: local date and time plus the numeric
// offset in force, written by ffmpeg's strftime ("%Y%m%d_%H%M%S%z").
//
// DECISION (advisor, W2): the Go allowlist is STRICTER than Test-CamSegmentName. PowerShell matches
// with an unanchored-at-the-end "$" (a trailing newline passes) and .NET \d (any Unicode digit
// passes); here only ASCII digits are accepted, the name must end exactly at the extension, and
// the digits must form a real date, time and offset. A stricter allowlist can only reduce what the
// pruner may delete. Case: see Matcher (insensitive on Windows only).

const stampLen = len("20060102_150405+0300") // 20

// FormatSegmentPattern is the ffmpeg strftime pattern of a camera's segments, without the
// extension (append the container's, for example ".mkv"). A "%" in the safe name is doubled so
// strftime writes it literally; the PowerShell product passes it through, so a camera called
// "Cam%z" gets the offset spliced into its file names there (IR 2.2 latent defects). The doubling
// is unverified against a real ffmpeg: check it in the ffmpeg-runner task.
func FormatSegmentPattern(safe string) string {
	return strings.ReplaceAll(safe, "%", "%%") + "_%Y%m%d_%H%M%S%z"
}

// FormatSegmentName is the file name ffmpeg writes for a segment that starts at start, using the
// offset of start's own location, and ext such as ".mkv". It is what the pattern expands to.
func FormatSegmentName(safe string, start time.Time, ext string) string {
	return safe + "_" + start.Format("20060102_150405-0700") + ext
}

// ParseSegment splits a segment file name into its camera prefix and start instant. The extension
// can be anything that is 1-8 ASCII letters or digits; use IsSegmentName or IsSegmentOf to demand
// the configured container. start carries the offset written in the name. The date, time and
// offset must be real.
func ParseSegment(name string) (camera string, start time.Time, ok bool) {
	dot := strings.LastIndexByte(name, '.')
	if dot < 0 {
		return "", time.Time{}, false
	}
	return parseSegmentBase(name[:dot], name[dot+1:])
}

func parseSegmentBase(base, ext string) (string, time.Time, bool) {
	if len(ext) < 1 || len(ext) > 8 {
		return "", time.Time{}, false
	}
	for i := 0; i < len(ext); i++ {
		if !isAlnum(ext[i]) {
			return "", time.Time{}, false
		}
	}
	if len(base) < stampLen+2 || base[len(base)-stampLen-1] != '_' {
		return "", time.Time{}, false
	}
	camera, stamp := base[:len(base)-stampLen-1], base[len(base)-stampLen:]
	if strings.ContainsAny(camera, "/\\\x00") { // a file name never holds a separator
		return "", time.Time{}, false
	}
	start, ok := parseStamp(stamp)
	if !ok {
		return "", time.Time{}, false
	}
	return camera, start, true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isAlnum(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// digits reads exactly n ASCII digits.
func digits(s string) (int, bool) {
	v := 0
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return 0, false
		}
		v = v*10 + int(s[i]-'0')
	}
	return v, true
}

// parseStamp reads "YYYYMMDD_HHMMSS+HHMM".
func parseStamp(s string) (time.Time, bool) {
	if len(s) != stampLen || s[8] != '_' || (s[15] != '+' && s[15] != '-') {
		return time.Time{}, false
	}
	var v [7]int
	for i, part := range []string{s[0:4], s[4:6], s[6:8], s[9:11], s[11:13], s[13:15], s[16:20]} {
		n, ok := digits(part)
		if !ok {
			return time.Time{}, false
		}
		v[i] = n
	}
	year, month, day, hour, min, sec, off := v[0], v[1], v[2], v[3], v[4], v[5], v[6]
	if hour > 23 || min > 59 || sec > 59 || off/100 > 14 || off%100 > 59 {
		return time.Time{}, false
	}
	secs := (off/100*60 + off%100) * 60
	if s[15] == '-' {
		secs = -secs
	}
	t := time.Date(year, time.Month(month), day, hour, min, sec, 0, time.FixedZone("", secs))
	if t.Year() != year || int(t.Month()) != month || t.Day() != day { // 30 February and the like
		return time.Time{}, false
	}
	return t, true
}

func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if x == y {
			continue
		}
		if x >= 0x80 || y >= 0x80 {
			return false
		}
		if x|0x20 != y|0x20 || x|0x20 < 'a' || x|0x20 > 'z' {
			return false
		}
	}
	return true
}

func normExt(container string) string {
	if container != "" && container[0] != '.' {
		return "." + container
	}
	return container
}

// Matcher decides segment names. FoldCase makes the extension and the camera prefix compare
// case-insensitively (ASCII only), which is right on Windows (NTFS, as PowerShell's -match did)
// and wrong on Linux and macOS, where ffmpeg writes exactly the pattern and a differently-cased
// file is foreign. Everything else in the allowlist is the same on every OS.
type Matcher struct{ FoldCase bool }

// DefaultMatcher folds case on Windows only.
var DefaultMatcher = Matcher{FoldCase: runtime.GOOS == "windows"}

// MatcherFor is the matcher of a platform (osx.Platform.Name()), so tables for another OS can be tested.
func MatcherFor(p osx.Platform) Matcher { return Matcher{FoldCase: p.Name() == "windows"} }

func (m Matcher) eq(a, b string) bool {
	if m.FoldCase {
		return asciiEqualFold(a, b)
	}
	return a == b
}

// IsSegmentName reports whether name has the shape of a segment of the given container, using
// DefaultMatcher. See Matcher.IsSegmentName.
func IsSegmentName(name, container string) bool { return DefaultMatcher.IsSegmentName(name, container) }

// IsSegmentOf is Matcher.IsSegmentOf with DefaultMatcher.
func IsSegmentOf(name, safe, container string) bool {
	return DefaultMatcher.IsSegmentOf(name, safe, container)
}

// IsSegmentName reports whether name has the shape of a segment of the given container (an
// extension such as ".mkv", with or without the dot). It does not know the camera; IsSegmentOf adds that.
func (m Matcher) IsSegmentName(name, container string) bool {
	ext := normExt(container)
	if len(ext) < 2 || len(name) <= len(ext) || !m.eq(name[len(name)-len(ext):], ext) {
		return false
	}
	_, _, ok := parseSegmentBase(name[:len(name)-len(ext)], ext[1:])
	return ok
}

// IsSegmentOf is the Test-CamSegmentName predicate: name is a segment of the camera whose safe name
// (its folder name) is safe, in the configured container.
func (m Matcher) IsSegmentOf(name, safe, container string) bool {
	if safe == "" || !m.IsSegmentName(name, container) {
		return false
	}
	ext := normExt(container)
	camera := name[:len(name)-len(ext)-stampLen-1]
	return m.eq(camera, safe)
}
