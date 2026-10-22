package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"holocron/internal/app"
	"holocron/internal/archive"
	"holocron/internal/config"
	"holocron/internal/osx"
	"holocron/internal/shadow"
	"holocron/internal/status"
)

var recordingTable = []doctorCheck{
	{id: "recording.heartbeat", group: "recording", run: recHeartbeat},
	{id: "recording.camera", group: "recording", run: recCameras},
	{id: "recording.growth", group: "recording", concurrent: true, outsideBudget: true, timeout: growthTimeout, run: recGrowth},
	{id: "recording.gaps", group: "recording", run: recGaps},
	{id: "recording.segment", group: "recording", timeout: 30 * time.Second, run: recSegments},
}

func init() { doctorTable = append(doctorTable, recordingTable...) }

// Thresholds of the recording group.
const (
	restartsWarn  = 3                // restarts in the last hour
	restartsFail  = 10               // restarts in the last hour
	growthWait    = 2 * time.Second  // between two size samples
	growthRounds  = 5                // a file that has not grown is sampled again up to this many times: growthWait * growthRounds is the window
	growthTimeout = 13 * time.Second // the window plus a margin
	closedMargin  = 60 * time.Second // a closed segment younger than SegmentSeconds plus this is recent (the archive scan's open grace)
	gapLimit      = 2 * time.Second  // a hole between consecutive segments larger than this is a gap
	gapEventSlack = 10 * time.Second // a log record this close to a gap explains it
)

// camFix is the fix of a camera that does not record.
func camFix(cam config.Camera, safe string) string {
	comp := "ffmpeg:" + safe
	if strings.ContainsAny(comp, " \t") {
		comp = `"` + comp + `"`
	}
	return fmt.Sprintf(`holocron cam test "%s" ; holocron logs --component %s --since 10m`, cam.Name, comp)
}

// enabled is cameras.json's cameras with Record on.
func (r *drun) enabled() ([]config.Camera, error) {
	type res struct {
		cams []config.Camera
		err  error
	}
	v := r.once("cameras", func() any {
		cs, err := app.LoadCameras(r.e.paths.Cameras)
		if err != nil {
			return res{err: err}
		}
		var out []config.Camera
		for _, c := range cs.List {
			if c.Record {
				out = append(out, c)
			}
		}
		return res{cams: out}
	}).(res)
	return v.cams, v.err
}

func (r *drun) row(name string) *status.Camera {
	if r.doc == nil {
		return nil
	}
	for i := range r.doc.Cameras {
		if strings.EqualFold(r.doc.Cameras[i].Name, name) {
			return &r.doc.Cameras[i]
		}
	}
	return nil
}

// segFile is one strict-pattern segment of a camera.
type segFile struct {
	Name  string
	Start time.Time // from the name
	MTime time.Time // the last write: the end of a closed segment
	Size  int64
}

// camFiles is one camera folder: its segments started within the last 25 h, oldest first.
type camFiles struct {
	Safe, Dir string
	Segs      []segFile
	Err       error
}

func (r *drun) ext() string {
	if p, ok := r.e.cfg.Profile(); ok && p.Extension != "" {
		return p.Extension
	}
	return "." + strings.TrimPrefix(r.e.cfg.Container, ".")
}

func (r *drun) files(cam config.Camera) camFiles {
	return r.once("files:"+cam.Name, func() any {
		safe, err := archive.SafeName(cam.Name)
		if err != nil {
			return camFiles{Err: err}
		}
		cf := camFiles{Safe: safe, Dir: filepath.Join(r.e.paths.Root, safe)}
		entries, err := os.ReadDir(cf.Dir)
		if err != nil {
			cf.Err = err
			return cf
		}
		m := archive.MatcherFor(r.e.opts.Platform)
		since := r.now.Add(-25 * time.Hour)
		for _, en := range entries {
			if en.IsDir() || !m.IsSegmentOf(en.Name(), safe, r.ext()) {
				continue
			}
			_, start, ok := archive.ParseSegment(en.Name())
			info, err := en.Info()
			if !ok || err != nil || start.Before(since) {
				continue
			}
			cf.Segs = append(cf.Segs, segFile{Name: en.Name(), Start: start, MTime: info.ModTime(), Size: info.Size()})
		}
		sort.Slice(cf.Segs, func(i, j int) bool { return cf.Segs[i].Start.Before(cf.Segs[j].Start) })
		return cf
	}).(camFiles)
}

// newest is the segment being written: the newest by modification time (later name on a tie), as
// the archive scan decides; the rest are closed.
func (cf camFiles) newest() (segFile, []segFile, bool) {
	if len(cf.Segs) == 0 {
		return segFile{}, nil, false
	}
	n := 0
	for i, s := range cf.Segs {
		if !s.MTime.Before(cf.Segs[n].MTime) {
			n = i
		}
	}
	closed := append(append([]segFile{}, cf.Segs[:n]...), cf.Segs[n+1:]...)
	return cf.Segs[n], closed, true
}

func secs(d time.Duration) string { return fmt.Sprintf("%.0f s", d.Seconds()) }

func recHeartbeat(_ context.Context, r *drun) []check {
	iv := r.e.cfg.StatusIntervalSeconds
	if iv <= 0 {
		iv = 5
	}
	age := r.now.Sub(r.doc.UpdatedAt.Time())
	n := age.Seconds() / iv
	host := "no holocron host process (this supervisor was not started by holocron up: see the system group)"
	if h, ok := app.ReadHostRecord(r.e.paths); ok {
		if alive, _ := osx.Verify(h, r.e.inspector, goos()); alive {
			host = fmt.Sprintf("host pid %d", h.Pid)
		}
	}
	data := map[string]any{"ageSeconds": age.Seconds(), "intervalSeconds": iv, "supervisorPid": r.doc.SupervisorPid}
	text := fmt.Sprintf("supervisor pid %d, heartbeat %.1f s old (%.1f status interval(s) of %g s), %s", r.doc.SupervisorPid, age.Seconds(), n, iv, host)
	const fix = "holocron logs --component supervisor --since 10m (a supervisor that stops writing status.json is stuck; holocron restart --detach starts it again)"
	switch {
	case n > 6:
		return []check{mk(lvFail, "heartbeat", "%s", text).fix(fix).data(data)}
	case n > 3:
		return []check{mk(lvWarn, "heartbeat", "%s", text).fix(fix).data(data)}
	}
	return []check{mk(lvOK, "heartbeat", "%s", text).data(data)}
}

// recCameras: per enabled camera, what the supervisor says: alive, input mode, restarts.
func recCameras(_ context.Context, r *drun) []check {
	cams, err := r.enabled()
	if err != nil {
		return []check{mk(lvFail, "cameras", "%s: %v", r.e.paths.Cameras, err).fix("holocron config validate, then correct %s", r.e.paths.Cameras)}
	}
	if len(cams) == 0 {
		return []check{mk(lvWarn, "cameras", "no camera is set to record in %s", r.e.paths.Cameras).fix(`holocron cam enable "<camera>" (or holocron cam add)`)}
	}
	var cs []check
	for _, cam := range cams {
		safe, _ := archive.SafeName(cam.Name)
		row := r.row(cam.Name)
		if row == nil {
			cs = append(cs, mk(lvFail, cam.Name, "set to record, but the supervisor reports no such camera").fix("holocron status ; holocron logs --component supervisor --since 10m (the supervisor reads cameras.json within ReconcileSeconds)"))
			continue
		}
		data := map[string]any{"camera": cam.Name, "alive": row.Alive, "inputMode": row.InputMode, "restartsLastHour": row.RestartsLastHour, "pid": row.Pid}
		if !row.Alive {
			text := "DOWN"
			if !row.DownSince.IsNull() {
				text += fmt.Sprintf(" since %s (%s)", row.DownSince.Time().Format(time.RFC3339), secs(r.now.Sub(row.DownSince.Time())))
			}
			if row.LastError != "" {
				text += ": " + row.LastError
			}
			if row.ConfigError != "" {
				text += ": " + row.ConfigError
			}
			cs = append(cs, mk(lvFail, cam.Name, "%s", text).fix("%s", camFix(cam, safe)).data(data))
			continue
		}
		level, fixes := lvOK, []string{}
		text := fmt.Sprintf("alive%s, input %s, %d restart(s) in the last hour", pidText(row.Pid), orDash(row.InputMode), row.RestartsLastHour)
		switch {
		case row.RestartsLastHour >= restartsFail:
			level = lvFail
		case row.RestartsLastHour >= restartsWarn:
			level = lvWarn
		}
		if level != lvOK {
			if row.LastError != "" {
				text += ", last error: " + row.LastError
			}
			fixes = append(fixes, camFix(cam, safe))
		}
		if row.InputMode == "direct" {
			if level == lvOK {
				level = lvWarn
			}
			text += " - records DIRECTLY from the camera (the relay does not serve it), which takes the camera's second session"
			fixes = append(fixes, "holocron doctor --group connections ; holocron logs --component relay --since 10m")
		}
		c := mk(level, cam.Name, "%s", text).data(data)
		if len(fixes) > 0 {
			c = c.fix("%s", strings.Join(fixes, " ; "))
		}
		cs = append(cs, c)
	}
	return cs
}

// growthSample is the newest segment's live size at the start and after each growthWait until it
// grew (the muxer writes in bursts, so one quiet 2 s sample is not a stall).
type growthSample struct {
	Newest       segFile
	Found        bool
	Before       int64
	After        int64
	ClosedMTime  time.Time // the newest closed segment's last write
	Err          error
	SampleMillis int64
	Rounds       int // samples taken after the first
}

// window is how long the file was watched without growing.
func (g growthSample) window() time.Duration { return time.Duration(g.Rounds) * growthWait }

func (g growthSample) grew() bool { return g.Found && g.Err == nil && g.After > g.Before }

// growth samples every enabled camera once per run: one wait for all of them.
func (r *drun) growth(ctx context.Context) map[string]growthSample {
	return r.once("growth", func() any {
		cams, _ := r.enabled()
		out := map[string]growthSample{}
		for _, cam := range cams {
			cf := r.files(cam)
			g := growthSample{Err: cf.Err}
			if n, closed, ok := cf.newest(); ok {
				g.Newest, g.Found = n, true
				g.Before, g.Err = r.d.liveSize(filepath.Join(cf.Dir, n.Name))
				for _, c := range closed {
					if c.MTime.After(g.ClosedMTime) {
						g.ClosedMTime = c.MTime
					}
				}
			}
			out[cam.Name] = g
		}
		start := r.d.now()
		// Every camera shares each wait, so six stalled cameras cost growthRounds waits, not six times that.
		for round := 1; round <= growthRounds; round++ {
			pending := false
			for _, cam := range cams {
				if g := out[cam.Name]; g.Found && g.Err == nil && !g.grew() {
					pending = true
				}
			}
			if !pending {
				break
			}
			err := r.d.sleep(ctx, growthWait)
			for _, cam := range cams {
				g := out[cam.Name]
				if !g.Found || g.Err != nil || g.grew() {
					continue
				}
				if err != nil {
					g.Err = err
				} else {
					g.After, g.Err = r.d.liveSize(filepath.Join(r.files(cam).Dir, g.Newest.Name))
					g.Rounds = round
					g.SampleMillis = r.d.now().Sub(start).Milliseconds()
				}
				out[cam.Name] = g
			}
		}
		return out
	}).(map[string]growthSample)
}

// recGrowth: never by the supervisor's word alone - the newest segment must grow on disk.
func recGrowth(ctx context.Context, r *drun) []check {
	cams, _ := r.enabled()
	samples := r.growth(ctx)
	seg := time.Duration(r.e.cfg.SegmentSeconds) * time.Second
	var cs []check
	for _, cam := range cams {
		g := samples[cam.Name]
		safe, _ := archive.SafeName(cam.Name)
		fix := camFix(cam, safe)
		data := map[string]any{"camera": cam.Name, "segment": g.Newest.Name, "bytesBefore": g.Before, "bytesAfter": g.After}
		switch {
		case g.grew():
			cs = append(cs, mk(lvOK, cam.Name, "recording: %s grew %s in %.1f s", g.Newest.Name, kb(g.After-g.Before), float64(g.SampleMillis)/1000).data(data))
		case !g.Found && g.Err != nil && !os.IsNotExist(g.Err):
			cs = append(cs, mk(lvFail, cam.Name, "the camera folder cannot be read: %v", g.Err).fix("%s", fix).data(data))
		case !g.Found:
			cs = append(cs, mk(lvFail, cam.Name, "not recording: no segment of the last 25 h in %s", filepath.Join(r.e.paths.Root, safe)).fix("%s", fix).data(data))
		case g.Err != nil:
			cs = append(cs, mk(lvWarn, cam.Name, "%s: the size could not be sampled: %v", g.Newest.Name, g.Err).fix("run holocron doctor --group recording again").data(data))
		case !g.ClosedMTime.IsZero() && r.now.Sub(g.ClosedMTime) <= seg+closedMargin:
			cs = append(cs, mk(lvWarn, cam.Name, "%s did not grow in %s; the newest closed segment ended %s ago", g.Newest.Name, secs(g.window()), secs(r.now.Sub(g.ClosedMTime))).
				fix("run holocron doctor --group recording again in a minute; if it repeats: %s", fix).data(data))
		default:
			last := g.Newest.MTime
			if g.ClosedMTime.After(last) {
				last = g.ClosedMTime
			}
			cs = append(cs, mk(lvFail, cam.Name, "not recording: no new data for %s (%s did not grow in %s)", secs(r.now.Sub(last)), g.Newest.Name, secs(g.window())).fix("%s", fix).data(data))
		}
	}
	return cs
}

func kb(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%d KB", n/1024)
}

// logLines is holocron.log (and its .1) parsed once per run.
func (r *drun) logLines() []logLine {
	return r.once("log", func() any {
		var out []logLine
		path := r.e.logFile()
		for _, p := range []string{path + ".1", path} {
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			for _, s := range strings.Split(string(b), "\n") {
				if l, ok := parseLogLine(strings.TrimRight(s, "\r")); ok {
					out = append(out, l)
				}
			}
		}
		return out
	}).([]logLine)
}

// explained: a supervisor or host record about this camera, a restart or a start or stop, close to
// the hole between from and to.
func explained(lines []logLine, cam string, from, to time.Time) bool {
	for _, l := range lines {
		if l.Time.Before(from.Add(-gapEventSlack)) || l.Time.After(to.Add(gapEventSlack)) {
			continue
		}
		c := strings.ToLower(l.Component)
		if c != "supervisor" && c != "host" && !strings.HasPrefix(c, "ffmpeg:") {
			continue
		}
		t := strings.ToLower(l.Text)
		if strings.Contains(t, strings.ToLower(cam)) || strings.Contains(t, "start") || strings.Contains(t, "stop") {
			return true
		}
	}
	return false
}

type gap struct {
	After  string    `json:"after"`
	At     time.Time `json:"at"`
	Length float64   `json:"seconds"`
	Logged bool      `json:"logged"`
}

// recGaps: holes larger than gapLimit between consecutive segments of the last 24 h (the end of a
// closed segment is its last write).
func recGaps(_ context.Context, r *drun) []check {
	cams, _ := r.enabled()
	var cs []check
	since := r.now.Add(-24 * time.Hour)
	for _, cam := range cams {
		cf := r.files(cam)
		var gaps []gap
		var segs []segFile
		for _, s := range cf.Segs {
			if !s.Start.Before(since) {
				segs = append(segs, s)
			}
		}
		for i := 1; i < len(segs); i++ {
			a, b := segs[i-1], segs[i]
			if d := b.Start.Sub(a.MTime); d > gapLimit {
				gaps = append(gaps, gap{After: a.Name, At: a.MTime, Length: d.Seconds(), Logged: explained(r.logLines(), cam.Name, a.MTime, b.Start)})
			}
		}
		if len(gaps) == 0 {
			cs = append(cs, mk(lvOK, cam.Name, "no gap larger than %s in the last 24 h (%d segment(s))", gapLimit, len(segs)))
			continue
		}
		longest, logged := gaps[0], 0
		for _, g := range gaps {
			if g.Length > longest.Length {
				longest = g
			}
			if g.Logged {
				logged++
			}
		}
		data := map[string]any{"camera": cam.Name, "gaps": gaps}
		text := fmt.Sprintf("%d gap(s) larger than %s in the last 24 h, longest %.0f s after %s", len(gaps), gapLimit, longest.Length, longest.At.Format("2006-01-02 15:04:05"))
		if logged == len(gaps) {
			cs = append(cs, mk(lvInfo, cam.Name, "%s; each at a logged restart or start", text).data(data))
			continue
		}
		safe, _ := archive.SafeName(cam.Name)
		cs = append(cs, mk(lvWarn, cam.Name, "%s; %d of them at a logged restart or start", text, logged).
			fix("holocron logs --component supervisor --since 24h ; %s", camFix(cam, safe)).data(data))
	}
	return cs
}

// recSegments: the newest closed segment against the camera's closed segments of the last 2 h, by
// bytes per second (a segment cut short by a restart is short, not broken); with --deep, ffprobe
// says whether it holds video and pcm_alaw audio.
func recSegments(ctx context.Context, r *drun) []check {
	cams, _ := r.enabled()
	var cs []check
	for _, cam := range cams {
		cf := r.files(cam)
		safe, _ := archive.SafeName(cam.Name)
		_, closed, ok := cf.newest()
		if !ok || len(closed) == 0 {
			cs = append(cs, mk(lvInfo, cam.Name, "no closed segment yet to compare"))
			continue
		}
		last := closed[0]
		for _, c := range closed {
			if c.MTime.After(last.MTime) {
				last = c
			}
		}
		var rates []float64
		for _, c := range closed {
			if d := c.MTime.Sub(c.Start).Seconds(); d >= 10 && r.now.Sub(c.MTime) <= 2*time.Hour {
				rates = append(rates, float64(c.Size)/d)
			}
		}
		dur := last.MTime.Sub(last.Start).Seconds()
		data := map[string]any{"camera": cam.Name, "segment": last.Name, "bytes": last.Size, "compared": len(rates)}
		var c check
		switch {
		case len(rates) < 3 || dur < 10:
			c = mk(lvOK, cam.Name, "%s %s; fewer than 3 closed segments of the last 2 h to compare", last.Name, kb(last.Size))
		default:
			sort.Float64s(rates)
			ratio := float64(last.Size) / dur / rates[len(rates)/2]
			data["ratio"] = ratio
			c = mk(lvOK, cam.Name, "%s %s, %.0f%% of the median rate of %d closed segment(s) of the last 2 h", last.Name, kb(last.Size), ratio*100, len(rates))
			if ratio < 0.5 || ratio > 1.5 {
				c.Level = lvWarn
				c = c.fix("holocron doctor --deep --group recording ; %s", camFix(cam, safe))
			}
		}
		if r.deep {
			c = deepProbe(ctx, r, cam, filepath.Join(cf.Dir, last.Name), c, safe)
		}
		cs = append(cs, c.data(data))
	}
	return cs
}

func deepProbe(ctx context.Context, r *drun, cam config.Camera, path string, c check, safe string) check {
	p, err := r.d.prober(r.e.cfg).Probe(ctx, path)
	fix := camFix(cam, safe)
	switch {
	case err != nil:
		c.Level, c.Detail = lvFail, c.Detail+"; ffprobe: "+err.Error()
		return c.fix("check FfprobePath (holocron config get FfprobePath) ; %s", fix)
	case p.Video == nil:
		c.Level, c.Detail = lvFail, c.Detail+"; ffprobe: NO video stream"
		return c.fix("%s", fix)
	case p.Audio == nil || p.Audio.Codec != "pcm_alaw":
		audio := "no audio stream"
		if p.Audio != nil {
			audio = "audio " + p.Audio.Codec
		}
		if c.Level == lvOK {
			c.Level = lvWarn
		}
		c.Detail += fmt.Sprintf("; ffprobe: video %s, %s (pcm_alaw expected)", p.Video.Codec, audio)
		return c.fix("check the camera's audio setting ; %s", fix)
	}
	c.Detail += fmt.Sprintf("; ffprobe: video %s, audio pcm_alaw, %.0f s", p.Video.Codec, p.Duration)
	return c
}

// shadowProber is the production --deep prober.
func shadowProber(cfg config.Config) shadow.Prober {
	path := cfg.FfprobePath
	if path == "" {
		path = "ffprobe"
	}
	return shadow.FFprobe{Path: path, Timeout: 10 * time.Second}
}
