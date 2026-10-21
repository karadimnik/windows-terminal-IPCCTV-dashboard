package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"holocron/internal/archive"
	"holocron/internal/config"
	"holocron/internal/shadow"
)

// stringList is a repeatable flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// newProber is the seam tests replace: given the ffprobe path it returns the prober.
var newProber = func(path string) shadow.Prober { return shadow.FFprobe{Path: path} }

// findFfprobe: --ffprobe, else the FfprobePath of the configuration (the --config file, else the
// platform defaults) when it exists, else ffprobe on PATH, else next to the configured ffmpeg.
func findFfprobe(e *env, flagValue string, cfg config.Config) (string, error) {
	if flagValue != "" {
		if _, err := os.Stat(flagValue); err != nil {
			return "", usageErr("--ffprobe %s: %v", flagValue, err)
		}
		return flagValue, nil
	}
	if p := cfg.FfprobePath; p != "" {
		if strings.ContainsAny(p, `/\`) {
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return p, nil
			}
		} else if r, err := exec.LookPath(p); err == nil {
			return r, nil
		}
	}
	if r, err := exec.LookPath("ffprobe"); err == nil {
		return r, nil
	}
	if cfg.FfmpegPath != "" && strings.ContainsAny(cfg.FfmpegPath, `/\`) {
		sib := filepath.Join(filepath.Dir(cfg.FfmpegPath), filepath.Base(cfg.FfprobePath))
		if fi, err := os.Stat(sib); err == nil && !fi.IsDir() {
			return sib, nil
		}
	}
	return "", usageErr("ffprobe not found: set FfprobePath in the configuration (--config), put ffprobe on PATH, or pass --ffprobe PATH")
}

// cmdArchiveCompare is `holocron archive compare`: read-only on both roots.
func cmdArchiveCompare(ctx context.Context, e *env) error {
	fs := newFlags(e, "archive compare")
	var oldRoot, newRoot, since, ffprobePath, audio string
	var cams stringList
	var segSec int
	var startTol, durTol, gapTol time.Duration
	fs.StringVar(&oldRoot, "old", "", "the existing archive root (read only)")
	fs.StringVar(&newRoot, "new", "", "the shadow archive root (read only)")
	fs.Var(&cams, "camera", "a camera name or folder to compare (repeatable; default every camera folder of --new)")
	fs.StringVar(&since, "since", "", "compare only from this time (RFC 3339, \"2006-01-02 15:04\", or a duration such as 36h ago)")
	fs.StringVar(&ffprobePath, "ffprobe", "", "ffprobe to use (default: FfprobePath of --config, then PATH)")
	fs.StringVar(&audio, "audio", "pcm_alaw:8000:1", "expected audio codec:rate:channels, or any (the two sides must still agree)")
	fs.IntVar(&segSec, "segment-seconds", 0, "segment length in seconds (default: SegmentSeconds of --config, else 900)")
	fs.DurationVar(&startTol, "start-tolerance", 5*time.Second, "starts this close are a pair; also the clock boundary jitter")
	fs.DurationVar(&durTol, "duration-tolerance", 2*time.Second, "ffprobe durations of a pair may differ by this much")
	fs.DurationVar(&gapTol, "gap-tolerance", 2*time.Second, "a gap between segments is longer than this")
	pos, err := parseFlags(fs, e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("archive compare", pos, 0, 0); err != nil {
		return err
	}
	if oldRoot == "" || newRoot == "" {
		return usageErr("holocron archive compare: --old and --new are required")
	}
	if segSec < 0 || startTol < 0 || durTol < 0 || gapTol < 0 {
		return usageErr("holocron archive compare: a negative number")
	}
	cfg := config.Defaults(e.opts.Platform)
	if e.opts.ConfigPath != "" {
		c, _, err := config.Load(e.opts.ConfigPath, e.opts.Platform)
		if err != nil {
			return usageErr("--config %s: %v", e.opts.ConfigPath, err)
		}
		cfg = c
		if segSec == 0 {
			segSec = c.SegmentSeconds
		}
	}
	container := ".mkv"
	if p, ok := cfg.Profile(); ok && p.Extension != "" {
		container = p.Extension
	}
	o := shadow.Options{Old: oldRoot, New: newRoot, SegmentSeconds: segSec, StartTolerance: startTol, DurationTolerance: durTol,
		GapTolerance: gapTol, Container: container, Audio: audio, Matcher: archive.MatcherFor(e.opts.Platform), Now: time.Now()}
	if since != "" {
		if o.Since, err = shadow.ParseSince(since, o.Now, time.Local); err != nil {
			return usageErr("%v", err)
		}
	}
	if len(cams) > 0 {
		if o.Cameras, err = shadow.ResolveCameras(cams); err != nil {
			return usageErr("%v", err)
		}
	}
	ff, err := findFfprobe(e, ffprobePath, cfg)
	if err != nil {
		return err
	}
	o.Prober = newProber(ff)
	if !e.json {
		o.Progress = func(s string) { fmt.Fprintln(e.errw, s) }
	}
	rep, err := shadow.Compare(ctx, o)
	if err != nil {
		return usageErr("archive compare: %v", err)
	}
	if e.json {
		if err := printJSON(e.out, rep); err != nil {
			return err
		}
	} else {
		shadow.Render(e.out, rep)
	}
	if !rep.Equal {
		return quietFail("%d difference(s) between %s and %s", rep.Differences, oldRoot, newRoot)
	}
	return nil
}
