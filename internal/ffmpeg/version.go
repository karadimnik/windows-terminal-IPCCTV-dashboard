package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MinMajor is the oldest supported ffmpeg major version (decision from SP1): the -timeout option
// (RTSP socket timeout, microseconds) exists only from 5.0; on 4.x the same flag means "listen
// timeout" and turns the RTSP demuxer into a server that fails at once.
const MinMajor = 5

// Version is a parsed `ffmpeg -version` first line.
type Version struct {
	Raw                 string // the version token, e.g. "7.1.1-essentials_build-www.gyan.dev"
	Major, Minor, Patch int    // zero for a git/nightly build
	// Git is true for a development build (N-12345-gabc, 2026-07-06-git-abc-..., git-2020-...): its
	// release number is unknown, so it is assumed to be recent enough.
	Git bool
	// Warning is set when the version was assumed to be acceptable rather than checked.
	Warning string
}

func (v Version) String() string {
	if v.Git {
		return v.Raw + " (development build)"
	}
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

var (
	firstLineRE = regexp.MustCompile(`(?m)^ffmpeg version (\S+)`)
	// release: 5.1, 6.0.1, n7.1 (git tag), 4.2.7-0ubuntu0.1, 7.1.1-essentials_build-www.gyan.dev
	releaseRE = regexp.MustCompile(`^[nN]?(\d+)\.(\d+)(?:\.(\d+))?(?:[-+~.]\S*)?$`)
	// development builds: N-127579-g5cb9fcd211-20261016, 2026-07-06-git-c6498178bb-essentials_build,
	// git-2020-05-01-abcdef0
	devRE  = regexp.MustCompile(`^(?:[nN]-\d+-g[0-9a-f]+|\d{4}-\d{2}-\d{2}-git-[0-9a-f]+|git-\d{4}-\d{2}-\d{2}-[0-9a-f]+)`)
	dateRE = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
)

// release5 is the date of the ffmpeg 5.0 release; a dated development build older than that is
// refused (it predates the -timeout change).
var release5 = time.Date(2022, 1, 17, 0, 0, 0, 0, time.UTC)

// ParseVersion reads the version out of `ffmpeg -version` output (only the first "ffmpeg version"
// line matters) and applies the minimum. The result of a refusal is the parsed Version together with
// an error; an unrecognised string is an error. A development build whose number cannot be known is
// accepted with Version.Warning set.
func ParseVersion(output string) (Version, error) {
	m := firstLineRE.FindStringSubmatch(output)
	if m == nil {
		return Version{}, fmt.Errorf("ffmpeg: cannot find a version line in the output of -version (starts: %q)", head(output))
	}
	tok := m[1]
	if r := releaseRE.FindStringSubmatch(tok); r != nil {
		v := Version{Raw: tok}
		v.Major, _ = strconv.Atoi(r[1])
		v.Minor, _ = strconv.Atoi(r[2])
		if r[3] != "" {
			v.Patch, _ = strconv.Atoi(r[3])
		}
		if v.Major < MinMajor {
			return v, fmt.Errorf("ffmpeg %s is too old: Holocron needs ffmpeg %d.0 or later. Before 5.0 the -timeout option is the RTSP listen timeout and makes the demuxer a server that fails at once; install a current build", v, MinMajor)
		}
		return v, nil
	}
	if devRE.MatchString(tok) {
		if d := dateRE.FindString(tok); d != "" {
			if t, err := time.Parse("2006-01-02", d); err == nil && t.Before(release5) {
				return Version{Raw: tok, Git: true}, fmt.Errorf("ffmpeg: the development build %s dates from before the 5.0 release (%s); Holocron needs ffmpeg %d.0 or later", tok, release5.Format("2006-01-02"), MinMajor)
			}
		}
		return Version{Raw: tok, Git: true,
			Warning: fmt.Sprintf("ffmpeg version %s is a development build; its release number is unknown, assuming it is %d.0 or later", tok, MinMajor)}, nil
	}
	return Version{Raw: tok}, fmt.Errorf("ffmpeg: cannot recognise the version %q; Holocron needs ffmpeg %d.0 or later", tok, MinMajor)
}

func head(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

// CheckVersion runs `<path> -version` and applies ParseVersion. It refuses an ffmpeg older than 5.0.
// Log Version.Warning when it is not empty.
func CheckVersion(path string) (Version, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "-version")
	cmd.Stdin = nil
	cmd.SysProcAttr = hideWindow()
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return Version{}, fmt.Errorf("ffmpeg: %s -version failed: %v", path, err)
		}
		return Version{}, fmt.Errorf("ffmpeg: cannot run %s: %w", path, err)
	}
	return ParseVersion(string(out))
}

// ChildEnv is the environment for the ffmpeg child, derived from base (os.Environ form). On Windows
// TZ is removed: that build honours a POSIX-style TZ but silently produces a wrong UTC offset for
// an IANA name such as Europe/Bucharest, which would put a wrong %z in every segment name (SP1).
// Elsewhere it is left inherited, so ffmpeg and Go's time.Local agree. The result is a new slice.
func ChildEnv(base []string, goos string) []string {
	out := make([]string, 0, len(base))
	for _, e := range base {
		if goos == "windows" && isTZ(e) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// isTZ reports an entry TZ=... ; Windows names are case-insensitive.
func isTZ(entry string) bool {
	return len(entry) >= 3 && strings.EqualFold(entry[:2], "TZ") && entry[2] == '='
}
