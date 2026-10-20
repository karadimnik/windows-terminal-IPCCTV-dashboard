package relay

import (
	_ "embed"
	"fmt"
	"regexp"
	"strings"

	"holocron/internal/config"
)

// template is a copy of mediamtx.template.yml (repository root); a test keeps the two identical.
//
//go:embed mediamtx.template.yml
var template string

// RelayInputs is everything the recording relay's config depends on. Config supplies RelayBase and
// RelayApiBase; API is the product's control-API login, of which the file holds the user and the
// hash of the secret only.
type RelayInputs struct {
	Config  config.Config
	Cameras []config.Camera
	API     Login
}

// RelayConfig is Get-CamRelayConfigText: the template with five plain-text replacements, CRLF line
// endings, byte for byte what PowerShell renders. It never contains a camera credential: every path
// keeps "source: publisher" and its URL arrives in MTX_PATHS_<NAME>_SOURCE (SourceEnv).
func RelayConfig(in RelayInputs) ([]byte, error) {
	paths, err := RelayPaths(in.Cameras)
	if err != nil {
		return nil, err
	}
	rtsp, err := config.HostPort(in.Config.RelayBase)
	if err != nil {
		return nil, err
	}
	api, err := config.HostPort(in.Config.RelayApiBase)
	if err != nil {
		return nil, err
	}
	if err := checkLogin(in.API); err != nil {
		return nil, err
	}
	for _, v := range []string{rtsp, api} {
		if err := checkScalar("address", v); err != nil {
			return nil, err
		}
	}

	lines := make([]string, 0, 3*len(paths))
	for _, p := range paths {
		lines = append(lines,
			"  "+p.Name+":",
			fmt.Sprintf("    # %s at %s - source supplied at launch by %s", oneLine(p.Camera.Name), oneLine(p.Camera.IP), SourceEnvName(p.Name)),
			"    source: publisher")
	}

	text := template
	text = strings.ReplaceAll(text, "__RTSP_ADDRESS__", rtsp)
	text = strings.ReplaceAll(text, "__API_ADDRESS__", api)
	text = strings.ReplaceAll(text, "__API_USER__", in.API.User)
	text = strings.ReplaceAll(text, "__API_PASS__", in.API.PassHash())
	text = strings.ReplaceAll(text, "__PATHS__", strings.Join(lines, "\r\n"))
	return []byte(crlf(text)), nil
}

var (
	scalarRE   = regexp.MustCompile(`^[A-Za-z0-9_.:\[\]\-]+$`)
	userRE     = regexp.MustCompile(`^[A-Za-z0-9_.\-]+$`)
	newlinesRE = regexp.MustCompile("\r\n|\n")
)

// crlf is the normalisation PowerShell applies last: every CRLF or LF becomes CRLF.
func crlf(s string) string { return newlinesRE.ReplaceAllString(s, "\r\n") }

// oneLine keeps a camera name or address inside the YAML comment it is written into: a line break
// (or any control character) would end the comment and let the name write configuration. PowerShell
// does not guard this; for every name that has no control character the output is identical.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == 0x85 || r == 0x2028 || r == 0x2029 {
			return ' '
		}
		return r
	}, s)
}

// checkScalar refuses a value that could not sit in a YAML scalar position (a quote, a line break, a
// comment mark, a space). It guards the text, it is not the address rule: config.Load owns that.
func checkScalar(what, v string) error {
	if !scalarRE.MatchString(v) {
		return fmt.Errorf("relay: %s %q cannot be written into the MediaMTX config", what, v)
	}
	return nil
}

func checkLogin(l Login) error {
	if !userRE.MatchString(l.User) {
		return fmt.Errorf("relay: login user %q cannot be written into the MediaMTX config", l.User)
	}
	if l.Secret == "" {
		return fmt.Errorf("relay: the login for %s has no secret", l.User)
	}
	return nil
}
