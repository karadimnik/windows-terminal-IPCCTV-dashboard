package web

// Footage browsing: the index, and one segment remuxed for the browser.
//
// Three things here are load-bearing.
//
// The index is parsed from FILENAMES, in the camera's spool and its day folders
// (archive.Layout, decision D20: a file in a day folder counts only under the
// date in its name). "Front_Entrance_20261013_190147+0300.mkv"
// already carries the camera, the date, the time and the UTC offset, so there is
// no database and nothing reads a segment's contents. The %z suffix is what
// keeps a date unambiguous across the DST change: every segment is placed in
// absolute time by its OWN offset, so the hour that repeats on 25 October does
// not collapse two segments onto one clock time.
//
// A segment's duration comes from its last-write time. ffmpeg stops writing when
// the segment ends, so mtime is the end of the recording. Measured against
// ffprobe on three real 15-minute segments: 900.047 vs 900.050, 900.018 vs
// 900.018, 900.047 vs 900.050 seconds. That is what makes gaps visible without
// opening a single file - a segment the camera cut short ends early, and the
// silence until the next one starts is the gap.
//
// A camera is addressed by NAME and resolved by MATCHING that name against the
// folders actually present under the archive root. Nothing the browser sends is
// ever joined onto a path before it has matched something enumerated from disk,
// which is what makes traversal structurally impossible rather than filtered.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"holocron/internal/archive"
)

const (
	// gapThresholdSeconds is the silence that counts as a gap. A clean segment
	// rollover lands within a few hundredths of a second of the next start
	// (measured: -0.05 s), so anything past a few seconds is a camera that
	// actually stopped delivering.
	gapThresholdSeconds = 5

	// inProgressWindow is how recently the newest segment must have been
	// written to count as the one being recorded now. The recorder writes
	// continuously, so a newest file untouched for longer than this means the
	// camera has stopped delivering.
	inProgressWindow = 90 * time.Second

	// clipTimeout bounds one remux. A 15-minute segment remuxes in about 10 s
	// at the measured 97x realtime, so this is a backstop against an ffmpeg
	// that hangs rather than a working limit.
	clipTimeout = 10 * time.Minute

	// clipProbeBytes is read from ffmpeg before the response status is
	// committed, so a failure can still be answered with an error instead of a
	// truncated video.
	clipProbeBytes = 64 << 10

	// clipStderrLimit caps what is kept from ffmpeg's stderr. At -loglevel
	// error there are only ever a few lines.
	clipStderrLimit = 4 << 10
)

// segmentSuffix matches what follows the camera prefix in a segment filename:
// _YYYYMMDD_HHMMSS±HHMM.ext. Fully anchored, so a name carrying a separator, a
// drive letter or ".." cannot match at all.
var segmentSuffix = regexp.MustCompile(`^_(\d{8})_(\d{6})([+-]\d{4})\.(?:mkv|mp4)$`)

// datePattern is the only shape the date parameter may take.
var datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

var (
	errNoSuchCamera  = errors.New("no camera of that name has footage on this machine")
	errNoSuchSegment = errors.New("no segment of that name exists for that camera")
)

// FootageCamera is one camera in the footage index.
type FootageCamera struct {
	Name     string `json:"name"`
	Known    bool   `json:"known"`
	Segments int    `json:"segments"`
	NewestAt string `json:"newestAt"`
}

// FootageDate is one day a camera has footage for.
type FootageDate struct {
	Date     string  `json:"date"`
	Segments int     `json:"segments"`
	Bytes    int64   `json:"bytes"`
	Seconds  float64 `json:"seconds"`
}

// FootageSegment is one recorded segment.
type FootageSegment struct {
	Segment    string  `json:"segment"`
	StartAt    string  `json:"startAt"`
	Start      string  `json:"start"`
	Seconds    float64 `json:"seconds"`
	Bytes      int64   `json:"bytes"`
	InProgress bool    `json:"inProgress"`
}

// FootageGap is a stretch of a day with no footage. Open means nothing has been
// recorded since, so the gap is still growing.
type FootageGap struct {
	From    string  `json:"from"`
	To      string  `json:"to"`
	Seconds float64 `json:"seconds"`
	Open    bool    `json:"open"`
}

// FootageResponse is the body of GET /api/footage in all three of its shapes:
// the camera list, one camera's dates, and one date's segments.
type FootageResponse struct {
	Schema   int              `json:"schema"`
	Camera   string           `json:"camera,omitempty"`
	Date     string           `json:"date,omitempty"`
	Cameras  []FootageCamera  `json:"cameras,omitempty"`
	Dates    []FootageDate    `json:"dates,omitempty"`
	Segments []FootageSegment `json:"segments,omitempty"`
	Gaps     []FootageGap     `json:"gaps,omitempty"`
	Seconds  float64          `json:"seconds,omitempty"`
	Bytes    int64            `json:"bytes,omitempty"`
}

// archiveCamera is a camera folder found under the archive root, paired with
// the display name it has in cameras.json when it still has a row there.
type archiveCamera struct {
	Dir   string
	Name  string
	Known bool
}

// segmentFile is one parsed segment filename plus what os.Stat says about it.
type segmentFile struct {
	Name       string
	Start      time.Time
	Date       string
	Seconds    float64
	Bytes      int64
	InProgress bool

	// writtenAt is the file's last-write time: the end of the recording, and
	// the only thing that says whether the newest segment is still growing.
	writtenAt time.Time
}

func (a *API) handleFootage(w http.ResponseWriter, r *http.Request) {
	camera := strings.TrimSpace(r.URL.Query().Get("camera"))
	date := strings.TrimSpace(r.URL.Query().Get("date"))

	if camera == "" {
		if date != "" {
			writeAPIError(w, http.StatusBadRequest, "a date needs a camera: ask for ?camera=<name>&date=YYYY-MM-DD")
			return
		}
		cameras, err := a.footageCameras()
		if err != nil {
			a.log.Error("footage index", "error", err)
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, FootageResponse{Schema: 1, Cameras: cameras})
		return
	}

	cam, err := a.resolveCamera(camera)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}

	if date != "" && !datePattern.MatchString(date) {
		writeAPIError(w, http.StatusBadRequest, "a date must be written YYYY-MM-DD")
		return
	}
	var segments []segmentFile
	if date == "" {
		segments, err = a.listSegments(cam)
	} else {
		segments, err = a.daySegments(cam, date)
	}
	if err != nil {
		a.log.Error("footage index", "camera", cam.Dir, "error", err)
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if date == "" {
		writeJSON(w, http.StatusOK, FootageResponse{
			Schema: 1,
			Camera: cam.Name,
			Dates:  footageDates(segments),
		})
		return
	}

	day := make([]segmentFile, 0, 96)
	for _, segment := range segments {
		if segment.Date == date {
			day = append(day, segment)
		}
	}

	body := FootageResponse{Schema: 1, Camera: cam.Name, Date: date, Segments: []FootageSegment{}}
	for _, segment := range day {
		body.Seconds += segment.Seconds
		body.Bytes += segment.Bytes
	}
	body.Gaps = segmentGaps(day, time.Now())
	// Newest first, which is the order the Operator looks for footage in.
	for i := len(day) - 1; i >= 0; i-- {
		segment := day[i]
		body.Segments = append(body.Segments, FootageSegment{
			Segment:    segment.Name,
			StartAt:    segment.Start.Format(time.RFC3339),
			Start:      segment.Start.Format("15:04:05"),
			Seconds:    roundTo(segment.Seconds, 1),
			Bytes:      segment.Bytes,
			InProgress: segment.InProgress,
		})
	}
	body.Seconds = roundTo(body.Seconds, 1)
	writeJSON(w, http.StatusOK, body)
}

// footageCameras lists every camera with footage, in cameras.json order, with
// any folder whose camera row has gone appended after them. A removed camera's
// footage stays on disk until the pruner reaches it, and footage nobody can
// find is footage nobody can check.
func (a *API) footageCameras() ([]FootageCamera, error) {
	cameras, err := a.archiveCameras()
	if err != nil {
		return nil, err
	}

	out := make([]FootageCamera, 0, len(cameras))
	for _, cam := range cameras {
		segments, err := a.listSegments(cam)
		if err != nil {
			return nil, err
		}
		entry := FootageCamera{Name: cam.Name, Known: cam.Known, Segments: len(segments)}
		if len(segments) > 0 {
			entry.NewestAt = segments[len(segments)-1].Start.Format(time.RFC3339)
		}
		out = append(out, entry)
	}
	return out, nil
}

// archiveCameras enumerates the camera folders under the archive root. This
// enumeration is the allow-list every camera parameter is matched against;
// folders beginning with an underscore are the recorder's own (_state, _logs).
func (a *API) archiveCameras() ([]archiveCamera, error) {
	entries, err := a.archive().Folders()
	if err != nil {
		return nil, fmt.Errorf("read the archive root: %w", scrubErr(err))
	}

	// cameras.json supplies the display name and the order. It is read for
	// presentation only: a camera missing from it still has its footage listed.
	rows, _, rowErr := a.readCameras()
	if rowErr != nil {
		a.log.Warn("footage index is naming cameras from their folders", "error", rowErr)
	}

	byDir := make(map[string]archiveCamera, len(entries))
	for _, entry := range entries { // the recorder's own "_" folders are not listed
		if !entry.IsDir() {
			continue
		}
		byDir[strings.ToLower(entry.Name())] = archiveCamera{Dir: entry.Name(), Name: entry.Name()}
	}

	out := make([]archiveCamera, 0, len(byDir))
	for _, row := range rows {
		key := strings.ToLower(safeCameraName(row.Name))
		cam, ok := byDir[key]
		if !ok {
			continue
		}
		cam.Name = row.Name
		cam.Known = true
		out = append(out, cam)
		delete(byDir, key)
	}

	orphans := make([]archiveCamera, 0, len(byDir))
	for _, cam := range byDir {
		orphans = append(orphans, cam)
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].Dir < orphans[j].Dir })
	return append(out, orphans...), nil
}

// resolveCamera turns a requested name into a folder that exists, by matching
// it against the enumerated folders. The request's string never reaches a path.
func (a *API) resolveCamera(requested string) (archiveCamera, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return archiveCamera{}, errNoSuchCamera
	}
	cameras, err := a.archiveCameras()
	if err != nil {
		return archiveCamera{}, err
	}
	// Both shapes are accepted because both are the camera's name: the display
	// name the page shows ("Front Entrance") and the folder name the recorder
	// derives from it ("Front_Entrance").
	folder := safeCameraName(requested)
	for _, cam := range cameras {
		if strings.EqualFold(cam.Name, requested) || (folder != "" && strings.EqualFold(cam.Dir, folder)) {
			return cam, nil
		}
	}
	return archiveCamera{}, errNoSuchCamera
}

// listSegments returns one camera's segments, oldest first: the spool's and
// every day folder's (one listing per day folder; the index is not polled).
// Files that do not match the recorder's naming scheme are ignored rather than
// guessed at.
func (a *API) listSegments(cam archiveCamera) ([]segmentFile, error) {
	files, err := a.archive().Files(cam.Dir)
	if err != nil {
		return nil, fmt.Errorf("read the folder for camera %q: %w", cam.Name, scrubErr(err))
	}
	return segmentsOf(cam, files), nil
}

// daySegments returns the segments a date's page needs: the spool's and those
// of the one day folder named date (two listings, however many days are
// filed), oldest first, so the newest of them is the one in progress exactly
// as in the whole index (the spool holds the newest segment).
func (a *API) daySegments(cam archiveCamera, date string) ([]segmentFile, error) {
	l := a.archive()
	files, days, err := l.Spool(cam.Dir)
	if err != nil {
		return nil, fmt.Errorf("read the folder for camera %q: %w", cam.Name, scrubErr(err))
	}
	for _, d := range days {
		if d.Name() == date {
			if df, err := l.DayFiles(d); err == nil {
				files = append(files, df...)
			}
			break
		}
	}
	return segmentsOf(cam, files), nil
}

// segmentsOf parses listed files into segments, oldest first, the newest in
// progress when it was written within inProgressWindow. A file in a day folder
// counts only when the day is the date in its name (archive.DayName); a
// misfiled one is not footage.
func segmentsOf(cam archiveCamera, files []archive.File) []segmentFile {
	// A segment that vanished between the listing and the stat is the pruner
	// doing its job, not an error worth failing the index for: the listing leaves it out.
	out := make([]segmentFile, 0, len(files))
	for _, file := range files {
		start, ok := parseSegmentName(cam.Dir, file.Name())
		if !ok || (file.Day != "" && archive.DayName(start) != file.Day) {
			continue
		}
		info := file.Info
		seconds := info.ModTime().Sub(start).Seconds()
		if seconds < 0 {
			seconds = 0
		}
		out = append(out, segmentFile{
			Name:      file.Name(),
			Start:     start,
			Date:      start.Format("2006-01-02"),
			Seconds:   seconds,
			Bytes:     info.Size(),
			writtenAt: info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })

	// ffmpeg writes exactly one segment per camera at a time and it is always
	// the newest, so only that one can be in progress. Asking the question of
	// every file instead flags the segment that rolled over a minute ago as
	// still recording, which it is not.
	if last := len(out) - 1; last >= 0 && time.Since(out[last].writtenAt) < inProgressWindow {
		out[last].InProgress = true
	}
	return out
}

// archive is the archive root's layout. The cache lives as long as the API, so
// the footage index stats only what changed since the last click (on Linux and
// macOS; on Windows the listing already carries every file's size and times).
// Folding case when resolving a clip keeps what the file systems of Windows
// and macOS do with a name that differs only in case.
func (a *API) archive() archive.Layout {
	a.footageOnce.Do(func() { a.footageCache = archive.NewCache() })
	fold := runtime.GOOS == "windows" || runtime.GOOS == "darwin"
	return archive.Layout{Root: a.cfg.ArchiveRoot, Matcher: archive.Matcher{FoldCase: fold}, Cache: a.footageCache}
}

// parseSegmentName returns the start time a segment filename encodes. The time
// is returned in the fixed zone the filename names, so formatting it gives back
// the local clock time the recorder wrote - across a DST change too.
func parseSegmentName(dir, name string) (time.Time, bool) {
	if len(name) <= len(dir) || !strings.EqualFold(name[:len(dir)], dir) {
		return time.Time{}, false
	}
	match := segmentSuffix.FindStringSubmatch(name[len(dir):])
	if match == nil {
		return time.Time{}, false
	}
	start, err := time.Parse("20060102_150405-0700", match[1]+"_"+match[2]+match[3])
	if err != nil {
		return time.Time{}, false
	}
	return start, true
}

// footageDates groups segments by the date in their filename, newest first.
func footageDates(segments []segmentFile) []FootageDate {
	order := make([]string, 0, 16)
	byDate := make(map[string]*FootageDate, 16)
	for _, segment := range segments {
		entry, ok := byDate[segment.Date]
		if !ok {
			entry = &FootageDate{Date: segment.Date}
			byDate[segment.Date] = entry
			order = append(order, segment.Date)
		}
		entry.Segments++
		entry.Bytes += segment.Bytes
		entry.Seconds += segment.Seconds
	}

	out := make([]FootageDate, 0, len(order))
	for i := len(order) - 1; i >= 0; i-- {
		entry := byDate[order[i]]
		entry.Seconds = roundTo(entry.Seconds, 1)
		out = append(out, *entry)
	}
	return out
}

// segmentGaps returns the stretches of one day with no footage, newest first.
// day must be one date's segments, oldest first.
//
// The day's own boundaries are taken from the offsets the first and last
// segments were written with, so on the day the clocks change the window is the
// 23 or 25 hours that day actually had.
func segmentGaps(day []segmentFile, now time.Time) []FootageGap {
	if len(day) == 0 {
		return nil
	}

	first := day[0].Start
	dayStart := time.Date(first.Year(), first.Month(), first.Day(), 0, 0, 0, 0, first.Location())
	last := day[len(day)-1].Start
	dayEnd := time.Date(last.Year(), last.Month(), last.Day(), 0, 0, 0, 0, last.Location()).AddDate(0, 0, 1)

	// A day still in progress ends now, so the silence since the last segment
	// is reported as what it is rather than as the rest of the day.
	open := false
	if now.Before(dayEnd) {
		dayEnd = now
		open = true
	}

	gaps := make([]FootageGap, 0, 4)
	add := func(from, to time.Time, stillOpen bool) {
		seconds := to.Sub(from).Seconds()
		if seconds < gapThresholdSeconds {
			return
		}
		gaps = append(gaps, FootageGap{
			From:    from.In(first.Location()).Format("15:04:05"),
			To:      to.In(last.Location()).Format("15:04:05"),
			Seconds: roundTo(seconds, 1),
			Open:    stillOpen,
		})
	}

	add(dayStart, first, false)
	for i := 0; i+1 < len(day); i++ {
		add(day[i].Start.Add(time.Duration(day[i].Seconds*float64(time.Second))), day[i+1].Start, false)
	}
	end := last.Add(time.Duration(day[len(day)-1].Seconds * float64(time.Second)))
	add(end, dayEnd, open)

	for i, j := 0, len(gaps)-1; i < j; i, j = i+1, j-1 {
		gaps[i], gaps[j] = gaps[j], gaps[i]
	}
	return gaps
}

// ---------------------------------------------------------------------------
// GET /api/clip
// ---------------------------------------------------------------------------

// handleClip streams one segment as fragmented MP4. The video is copied and
// only the 8 kHz G.711 audio is transcoded, which is why this costs about 1 % of
// realtime: measured 7.3 s for an 11.8-minute segment.
//
// MKV cannot be handed to a browser at all - no engine decodes A-law - so the
// remux is not an optimisation, it is the only way this plays.
func (a *API) handleClip(w http.ResponseWriter, r *http.Request) {
	cam, err := a.resolveCamera(r.URL.Query().Get("camera"))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, errNoSuchCamera.Error())
		return
	}

	// A segment name carries the UTC offset the recorder wrote it with, so it
	// contains a "+", and an unescaped "+" in a query string decodes to a
	// space. A real segment name can never contain a space - Get-CamSafeName
	// turns every whitespace run into an underscore - so a space here is only
	// ever that lost escape, and repairing it is kinder than a 404 nobody can
	// explain. Everything after this is still matched, never joined.
	segment := strings.ReplaceAll(strings.TrimSpace(r.URL.Query().Get("segment")), " ", "+")
	path, err := a.resolveSegment(cam, segment)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, errNoSuchSegment.Error())
		return
	}

	a.streamClip(w, r, path, segment)
}

// resolveSegment turns a requested segment name into a path on disk, or refuses.
// Four independent checks, because each would be sufficient and none is
// trusted alone:
//
//  1. the name must match the recorder's naming scheme for THIS camera, which
//     no name holding a separator, a drive letter or ".." can do;
//  2. the name must MATCH a name the camera's folder lists (archive.Layout's
//     Resolve): the path is the listed entry's, the request is never joined;
//  3. that path must resolve inside the archive root, the same test
//     Test-CamPathInside makes before the pruner deletes anything;
//  4. the result must already exist as a regular file.
func (a *API) resolveSegment(cam archiveCamera, segment string) (string, error) {
	if _, ok := parseSegmentName(cam.Dir, segment); !ok {
		return "", errNoSuchSegment
	}

	entry, err := a.archive().Resolve(cam.Dir, segment)
	if err != nil {
		return "", errNoSuchSegment
	}
	full := entry.Path
	if !pathInside(a.cfg.ArchiveRoot, full) {
		return "", errNoSuchSegment
	}

	info, err := os.Stat(full)
	if err != nil || !info.Mode().IsRegular() {
		return "", errNoSuchSegment
	}
	return full, nil
}

// pathInside mirrors Test-CamPathInside in cam.common.ps1: true only when path
// resolves inside root. The trailing separator on the root is what stops
// "E:\cameras_old\x.mkv" counting as inside "E:\cameras", and Abs collapses any
// ".." before the comparison. The comparison is case-insensitive because the
// filesystem this guards is.
func pathInside(root, path string) bool {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(path) == "" {
		return false
	}
	rootFull, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	separator := string(filepath.Separator)
	if !strings.HasSuffix(rootFull, separator) {
		rootFull += separator
	}
	full, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	if len(full) < len(rootFull) {
		return false
	}
	return strings.EqualFold(full[:len(rootFull)], rootFull)
}

// streamClip runs the remux and copies its stdout to the response.
//
// Nothing is buffered: a 15-minute segment is about 170 MB and arrives as it is
// produced. The first bytes are read before the status is committed, so an
// ffmpeg that cannot open the file is answered with an error rather than with a
// 200 and an empty video. The request's own context kills ffmpeg when the client
// goes away, which is what stops an abandoned player leaving a remux running.
func (a *API) streamClip(w http.ResponseWriter, r *http.Request, path, segment string) {
	ctx, cancel := context.WithTimeout(r.Context(), clipTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, a.ffmpegPath(),
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-i", path,
		"-map", "0:v:0", "-map", "0:a:0?",
		"-c:v", "copy",
		"-c:a", "aac", "-b:a", "64k",
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof",
		"-f", "mp4", "pipe:1",
	)
	stderr := &limitedBuffer{limit: clipStderrLimit}
	cmd.Stderr = stderr
	// WaitDelay bounds Wait after the context is cancelled: without it a kill
	// still leaves Wait blocked on a stdout pipe nobody is reading.
	cmd.WaitDelay = 5 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		a.log.Error("clip pipe", "segment", segment, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "the clip could not be prepared")
		return
	}
	if err := cmd.Start(); err != nil {
		a.log.Error("clip start", "segment", segment, "error", err)
		writeAPIError(w, http.StatusBadGateway,
			"ffmpeg could not be started, so footage cannot be played: "+ScrubText(err.Error()))
		return
	}

	// ffmpeg writes the fMP4 header as soon as the input opens, so one byte of
	// stdout is the difference between a working remux and a failed one.
	head := make([]byte, clipProbeBytes)
	read, readErr := io.ReadAtLeast(stdout, head, 1)
	if readErr != nil {
		cancel()
		_ = cmd.Wait()
		detail := strings.ReplaceAll(firstLine(stderr.String()), path, segment)
		a.log.Warn("clip produced nothing", "segment", segment, "ffmpeg", stderr.String())
		writeAPIError(w, http.StatusBadGateway, strings.TrimSpace("the clip could not be prepared. "+ScrubText(detail)))
		return
	}

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Honest, because this body is produced on the fly and has no length: a
	// byte range cannot be served, and the player must not be told otherwise.
	w.Header().Set("Accept-Ranges", "none")
	w.WriteHeader(http.StatusOK)

	_, writeErr := w.Write(head[:read])
	if writeErr == nil {
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		_, writeErr = io.Copy(w, stdout)
	}

	// Cancel before Wait either way. On a clean finish ffmpeg has already
	// exited and this changes nothing; on a client that walked away it is what
	// ends the remux instead of leaving it to run to the end of the segment.
	cancel()
	waitErr := cmd.Wait()

	switch {
	case writeErr != nil:
		// The client disconnected. Expected whenever a player is closed
		// mid-clip, and not a fault.
		a.log.Info("clip abandoned by the client", "segment", segment, "sentBytes", read)
	case waitErr != nil:
		a.log.Warn("clip remux ended badly", "segment", segment, "error", waitErr, "ffmpeg", stderr.String())
	case stderr.Len() > 0:
		// An in-progress segment ends mid-cluster, so ffmpeg says "File ended
		// prematurely" and exits 0. The clip is good up to that point.
		a.log.Info("clip remuxed with a warning", "segment", segment, "ffmpeg", stderr.String())
	}
}

// ffmpegPath is the recorder's own ffmpeg, so playback and recording can never
// disagree about which build is installed. The bare name is the fallback, and
// resolves through PATH (and PATHEXT on Windows).
func (a *API) ffmpegPath() string {
	raw, err := os.ReadFile(a.cfg.RecorderConfigFile())
	if err == nil {
		var parsed map[string]any
		if json.Unmarshal(stripBOM(raw), &parsed) == nil {
			if configured := strings.TrimSpace(jsonString(parsed["FfmpegPath"])); configured != "" {
				return configured
			}
		}
	}
	return "ffmpeg"
}

// limitedBuffer keeps the first limit bytes written to it and drops the rest, so
// an ffmpeg that complains once per frame cannot grow the server's memory.
type limitedBuffer struct {
	limit int
	buf   []byte
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.limit - len(l.buf); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		l.buf = append(l.buf, p[:room]...)
	}
	return len(p), nil
}

func (l *limitedBuffer) String() string { return strings.TrimSpace(string(l.buf)) }
func (l *limitedBuffer) Len() int       { return len(l.buf) }
