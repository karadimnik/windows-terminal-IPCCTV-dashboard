package ffmpeg

import (
	"errors"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"holocron/internal/archive"
	"holocron/internal/config"
)

// Inputs is everything the argument builder needs for one camera start.
type Inputs struct {
	// URL is the -i value: the relay URL (RelayURL) or, for the direct input, the camera URL with
	// its credential.
	URL                 string
	RtspTransport       string // "tcp" by default
	TimeoutMicroseconds int64  // RTSP socket timeout; ffmpeg 5.0 or later only
	SegmentSeconds      int
	Profile             config.ContainerProfile
	// OutputPattern is the strftime pattern of the segment files, extension included (SegmentPattern).
	OutputPattern string
	// AllowCredentialArgs lets BuildArgs put a URL that carries a credential on the command line,
	// as the PowerShell product does. The golden test sets it; the recorder uses BuildArgsDirect.
	AllowCredentialArgs bool
}

var userinfoRE = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.\-]*://[^/\s@]*@`)

// hasCredential reports whether url has a userinfo part (scheme://user[:pass]@host).
func hasCredential(url string) bool { return userinfoRE.MatchString(url) }

// ErrCredentialArgs is returned by BuildArgs for a URL that carries a credential when
// AllowCredentialArgs is not set: the password would be visible in the process list.
var ErrCredentialArgs = errors.New("ffmpeg: the input URL carries a credential; use BuildArgsDirect (list-file input) so the password stays off the command line")

// BuildArgs returns the exact argument vector of Get-CamFfmpegArgs (IR 1.1): the argument order is
// the contract, golden-tested against testdata/golden/ffmpeg. The vector is an argv, not a quoted
// command line.
func BuildArgs(in Inputs) ([]string, error) {
	if err := in.check(); err != nil {
		return nil, err
	}
	if in.URL == "" {
		return nil, errors.New("ffmpeg: no input URL")
	}
	if hasCredential(in.URL) && !in.AllowCredentialArgs {
		return nil, ErrCredentialArgs
	}
	args := []string{
		"-hide_banner", "-loglevel", "warning", "-nostdin",
		"-rtsp_transport", in.RtspTransport,
		"-timeout", strconv.FormatInt(in.TimeoutMicroseconds, 10),
		"-fflags", "+genpts+discardcorrupt",
		"-i", in.URL,
	}
	return append(args, in.outputArgs()...), nil
}

// protocolWhitelist lets the concat demuxer open RTSP URLs from a list file (SP1b).
const protocolWhitelist = "file,rtsp,tcp,rtp,udp"

// BuildArgsDirect is the direct-input vector with the credential kept off the command line:
// the URL, the transport and the timeout live in the ffconcat list file listPath (WriteInputList),
// and `-f concat -safe 0 -protocol_whitelist ... -i listPath` replaces
// `-rtsp_transport T -timeout N -i URL`. Everything else is identical to BuildArgs. in.URL,
// in.RtspTransport and in.TimeoutMicroseconds are not used (they went into the list).
func BuildArgsDirect(in Inputs, listPath string) ([]string, error) {
	if err := in.checkOutput(); err != nil {
		return nil, err
	}
	if listPath == "" {
		return nil, errors.New("ffmpeg: no input list file")
	}
	args := []string{
		"-hide_banner", "-loglevel", "warning", "-nostdin",
		"-fflags", "+genpts+discardcorrupt",
		"-f", "concat", "-safe", "0", "-protocol_whitelist", protocolWhitelist,
		"-i", listPath,
	}
	return append(args, in.outputArgs()...), nil
}

func (in Inputs) check() error {
	if in.RtspTransport == "" {
		return errors.New("ffmpeg: no RTSP transport")
	}
	return in.checkOutput()
}

func (in Inputs) checkOutput() error {
	switch {
	case in.OutputPattern == "":
		return errors.New("ffmpeg: no output pattern")
	case in.Profile.SegmentFormat == "":
		return errors.New("ffmpeg: the container profile has no SegmentFormat")
	case in.SegmentSeconds <= 0:
		return errors.New("ffmpeg: SegmentSeconds must be positive")
	}
	return nil
}

// outputArgs is everything after -i: mapping, copy, the profile's arguments, the segment muxer.
func (in Inputs) outputArgs() []string {
	p := in.Profile
	args := []string{"-map", "0:v:0", "-map", "0:a:0?", "-c", "copy"}
	args = append(args, p.AudioArgs...)
	args = append(args, p.ExtraArgs...)
	args = append(args,
		"-f", "segment",
		"-segment_time", strconv.Itoa(in.SegmentSeconds),
		"-segment_atclocktime", "1",
		"-reset_timestamps", "1",
		"-strftime", "1",
		"-segment_format", p.SegmentFormat,
	)
	if strings.TrimSpace(p.SegmentFormatOptions) != "" {
		args = append(args, "-segment_format_options", p.SegmentFormatOptions)
	}
	return append(args, in.OutputPattern)
}

// RelayURL is the relay input: the base without a trailing slash, a slash and the path name.
func RelayURL(base, path string) string { return strings.TrimRight(base, "/") + "/" + path }

// SegmentPattern is the ffmpeg output pattern <root>/<safe>/<safe>_%Y%m%d_%H%M%S%z<ext> (the
// camera folder is <root>/<safe>). With -strftime 1 ffmpeg expands the WHOLE path, directories
// included (measured with 4.2.7: a folder "Cam%Y" is written as "Cam2026"), so every '%' in root and
// in the folder is doubled as well as the one in the file name (archive.FormatSegmentPattern).
func SegmentPattern(root, safe, ext string) string {
	esc := func(s string) string { return strings.ReplaceAll(s, "%", "%%") }
	return filepath.Join(esc(root), esc(safe), archive.FormatSegmentPattern(safe)+ext)
}
