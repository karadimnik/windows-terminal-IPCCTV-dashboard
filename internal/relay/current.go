package relay

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// CurrentResult is Test-CamRelayConfigCurrent's answer.
type CurrentResult struct {
	Current     bool // paths and login both match
	AuthCurrent bool // the authentication block is the one rendered for the stored login
	Readable    bool
	Expected    []string
	Found       []string
	Missing     []string
	Extra       []string
	Detail      string
}

// NeedsRewrite says the control-API login in the file is missing or not the stored one, so the
// product would be locked out of its own API: rewrite the config before MediaMTX reads it.
func (r CurrentResult) NeedsRewrite() bool { return !r.AuthCurrent }

// RewriteMessage is the log line written when NeedsRewrite led to a rewrite.
func RewriteMessage(configPath string) string {
	return fmt.Sprintf("rewrote %s: its control API login was missing or not the stored one; the API now requires the product's login", configPath)
}

// ConfigCurrent is Test-CamRelayConfigCurrent over the file at path. "Current" means the SET of path
// names equals the camera list and the authentication block (authMethod: through the end of
// authInternalUsers:) equals the one rendered for in.API, compared case-sensitively. The rest of the
// text is not compared: the comment holding a camera's IP changes with every DHCP lease and a
// whole-text compare would cry wolf about a working file. A zero in.API (no stored login yet) can
// never be current.
func ConfigCurrent(path string, in RelayInputs) CurrentResult {
	actual, err := os.ReadFile(path)
	if err != nil {
		res := CurrentResult{Detail: "could not be read at " + path}
		if paths, perr := RelayPaths(in.Cameras); perr == nil {
			for _, p := range paths {
				res.Expected = append(res.Expected, p.Name)
			}
		}
		return res
	}
	return configCurrent(path, string(actual), in)
}

func configCurrent(path, actual string, in RelayInputs) CurrentResult {
	res := CurrentResult{Readable: true}
	paths, err := RelayPaths(in.Cameras)
	if err != nil {
		res.Detail = err.Error()
		return res
	}
	for _, p := range paths {
		res.Expected = append(res.Expected, p.Name)
	}
	actual = strings.TrimPrefix(actual, "\ufeff")
	res.Found = configPathNames(actual)
	res.Missing = difference(res.Expected, res.Found)
	res.Extra = difference(res.Found, res.Expected)
	pathsCurrent := len(res.Missing) == 0 && len(res.Extra) == 0

	haveLogin := !in.API.Zero()
	if haveLogin {
		if want, err := RelayConfig(in); err == nil {
			res.AuthCurrent = configAuthBlock(actual) == configAuthBlock(string(want))
		}
	}
	res.Current = pathsCurrent && res.AuthCurrent

	if res.Current {
		found := append([]string(nil), res.Found...)
		sort.Strings(found)
		res.Detail = fmt.Sprintf("%s has a path for every camera in cameras.json and the control API login: %s", path, strings.Join(found, ", "))
		return res
	}
	var parts []string
	if len(res.Missing) > 0 {
		parts = append(parts, "no path for "+strings.Join(sorted(res.Missing), ", "))
	}
	if len(res.Extra) > 0 {
		parts = append(parts, "a path for no camera: "+strings.Join(sorted(res.Extra), ", "))
	}
	if !haveLogin {
		parts = append(parts, "no control API login exists yet")
	} else if !res.AuthCurrent {
		parts = append(parts, "the control API login is missing or not the stored one")
	}
	res.Detail = fmt.Sprintf("%s is out of date - %s. The restreamer start rewrites it", path, strings.Join(parts, "; "))
	return res
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func difference(a, b []string) []string {
	in := map[string]bool{}
	for _, s := range b {
		in[s] = true
	}
	var out []string
	for _, s := range a {
		if !in[s] {
			out = append(out, s)
		}
	}
	return out
}

var (
	pathsKeyRE  = regexp.MustCompile(`^paths:\s*$`)
	pathNameRE  = regexp.MustCompile(`^  ([A-Za-z0-9_]+):\s*$`)
	topLevelRE  = regexp.MustCompile(`^\S`)
	authUsersRE = regexp.MustCompile(`^authInternalUsers:`)
	authKeyRE   = regexp.MustCompile(`^authMethod:`)
)

func splitLines(text string) []string { return newlinesRE.Split(text, -1) }

// configPathNames is Get-CamRelayConfigPathNames: only the paths: block is read (pathDefaults: has
// children at the same indent), in file order.
func configPathNames(text string) []string {
	var names []string
	in := false
	for _, line := range splitLines(text) {
		if !in {
			in = pathsKeyRE.MatchString(line)
			continue
		}
		if topLevelRE.MatchString(line) {
			break
		}
		if m := pathNameRE.FindStringSubmatch(line); m != nil {
			names = append(names, m[1])
		}
	}
	return names
}

// configAuthBlock is Get-CamRelayConfigAuthBlock: from the authMethod: line through the end of the
// authInternalUsers: list, line endings normalised and trailing blanks trimmed; empty when the file
// has none (every config written before the API needed a login).
func configAuthBlock(text string) string {
	var block []string
	in := false
	for _, line := range splitLines(text) {
		if !in {
			if authKeyRE.MatchString(line) {
				in = true
				block = append(block, strings.TrimRight(line, " \t\r\n"))
			}
			continue
		}
		if topLevelRE.MatchString(line) && !authUsersRE.MatchString(line) {
			break
		}
		block = append(block, strings.TrimRight(line, " \t\r\n"))
	}
	return strings.TrimRight(strings.Join(block, "\r\n"), " \t\r\n")
}

