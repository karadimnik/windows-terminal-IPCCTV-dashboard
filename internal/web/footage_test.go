package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// newFootageAPI returns an API over a throwaway archive root holding the two
// cameras of liveCamerasJSON, plus a folder whose camera row has gone.
func newFootageAPI(t *testing.T) (*API, string) {
	t.Helper()
	api, _, _ := newTestAPI(t)

	root := filepath.Join(t.TempDir(), "cameras")
	for _, dir := range []string{"Front_Entrance", "Workshop", "Old_Gate", "_state", "_logs"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	api.cfg.ArchiveRoot = root
	api.cfg.StatusFile = filepath.Join(root, "_state", "status.json")
	return api, root
}

// addSegment writes a segment file named the way the recorder names them and
// backdates its last-write time to the end of the recording, which is what the
// index derives the duration from.
func addSegment(t *testing.T, root, folder, stamp string, seconds float64, bytes int64) string {
	t.Helper()
	name := folder + "_" + stamp + ".mkv"
	path := filepath.Join(root, folder, name)
	if err := os.WriteFile(path, make([]byte, 1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, bytes); err != nil {
		t.Fatal(err)
	}
	start, err := time.Parse("20060102_150405-0700", stamp)
	if err != nil {
		t.Fatalf("test stamp %q: %v", stamp, err)
	}
	end := start.Add(time.Duration(seconds * float64(time.Second)))
	if err := os.Chtimes(path, end, end); err != nil {
		t.Fatal(err)
	}
	return name
}

func getFootage(t *testing.T, api *API, query string) FootageResponse {
	t.Helper()
	response := request(t, api, "GET", "/api/footage"+query, "")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/footage%s returned %d: %s", query, response.Code, response.Body.String())
	}
	var body FootageResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

// ---------------------------------------------------------------------------
// The index
// ---------------------------------------------------------------------------

func TestCameraListIsInCamerasJSONOrderWithOrphansAfter(t *testing.T) {
	api, root := newFootageAPI(t)
	addSegment(t, root, "Front_Entrance", "20261013_190000+0300", 900, 180*1024*1024)
	addSegment(t, root, "Front_Entrance", "20261012_190000+0300", 900, 180*1024*1024)
	addSegment(t, root, "Workshop", "20261013_120000+0300", 900, 170*1024*1024)
	addSegment(t, root, "Old_Gate", "20261001_120000+0300", 900, 170*1024*1024)

	body := getFootage(t, api, "")
	var got []string
	for _, cam := range body.Cameras {
		got = append(got, fmt.Sprintf("%s/%d/%t", cam.Name, cam.Segments, cam.Known))
	}
	want := []string{"Front Entrance/2/true", "Workshop/1/true", "Old_Gate/1/false"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("camera list\n got %v\nwant %v", got, want)
	}
	// _state and _logs are the recorder's own folders, not cameras.
	for _, cam := range body.Cameras {
		if strings.HasPrefix(cam.Name, "_") {
			t.Fatalf("the recorder's own folder %q was listed as a camera", cam.Name)
		}
	}
}

func TestDatesAreNewestFirstWithCountsAndDurations(t *testing.T) {
	api, root := newFootageAPI(t)
	addSegment(t, root, "Front_Entrance", "20261011_223650+0300", 130, 20*1024*1024)
	addSegment(t, root, "Front_Entrance", "20261013_000000+0300", 900, 180*1024*1024)
	addSegment(t, root, "Front_Entrance", "20261013_001500+0300", 900, 180*1024*1024)

	body := getFootage(t, api, "?camera=Front%20Entrance")
	if body.Camera != "Front Entrance" {
		t.Fatalf("camera %q", body.Camera)
	}
	if len(body.Dates) != 2 {
		t.Fatalf("dates %+v", body.Dates)
	}
	if body.Dates[0].Date != "2026-10-13" || body.Dates[0].Segments != 2 || body.Dates[0].Seconds != 1800 {
		t.Fatalf("newest date wrong: %+v", body.Dates[0])
	}
	if body.Dates[1].Date != "2026-10-11" || body.Dates[1].Segments != 1 || body.Dates[1].Seconds != 130 {
		t.Fatalf("older date wrong: %+v", body.Dates[1])
	}
	if body.Dates[0].Bytes != 2*180*1024*1024 {
		t.Fatalf("bytes %d", body.Dates[0].Bytes)
	}
}

func TestSegmentsAreNewestFirstAndDatedByTheirOwnOffset(t *testing.T) {
	api, root := newFootageAPI(t)
	addSegment(t, root, "Front_Entrance", "20261013_190000+0300", 36, 7*1024*1024)
	addSegment(t, root, "Front_Entrance", "20261013_184500+0300", 900, 180*1024*1024)

	body := getFootage(t, api, "?camera=Front%20Entrance&date=2026-10-13")
	if len(body.Segments) != 2 {
		t.Fatalf("segments %+v", body.Segments)
	}
	if body.Segments[0].Start != "19:00:00" || body.Segments[1].Start != "18:45:00" {
		t.Fatalf("not newest first: %+v", body.Segments)
	}
	if body.Segments[0].Seconds != 36 || body.Segments[1].Seconds != 900 {
		t.Fatalf("durations wrong: %+v", body.Segments)
	}
	if body.Segments[0].StartAt != "2026-10-13T19:00:00+03:00" {
		t.Fatalf("startAt %q must keep the filename's own offset", body.Segments[0].StartAt)
	}
	if body.Segments[0].InProgress {
		t.Fatal("a segment last written in the past is not in progress")
	}
}

func TestTheSegmentBeingRecordedNowIsFlagged(t *testing.T) {
	api, root := newFootageAPI(t)
	start := time.Now().Add(-5 * time.Minute)
	addSegment(t, root, "Workshop", start.Format("20060102_150405-0700"), 300, 60*1024*1024)

	body := getFootage(t, api, "?camera=Workshop&date="+start.Format("2006-01-02"))
	if len(body.Segments) != 1 {
		t.Fatalf("segments %+v", body.Segments)
	}
	if !body.Segments[0].InProgress {
		t.Fatalf("the newest segment of a recording camera must be flagged in progress: %+v", body.Segments[0])
	}
}

// TestOnlyTheNewestSegmentCanBeInProgress is the defect a live run found: the
// window alone flags the segment that rolled over seconds ago as still
// recording. ffmpeg writes one file per camera at a time and it is always the
// newest, so only that one can be growing.
func TestOnlyTheNewestSegmentCanBeInProgress(t *testing.T) {
	api, root := newFootageAPI(t)
	// The stamps are fixed so the day never depends on the wall clock (a
	// fixture built from "now minus 20 minutes" splits across two dates
	// between 00:05 and 00:20). Only the write times are recent: both files
	// were written within the in-progress window, so the window alone would
	// flag both.
	first := addSegment(t, root, "Workshop", "20261013_120000+0300", 900, 1024)
	second := addSegment(t, root, "Workshop", "20261013_121500+0300", 300, 1024)
	now := time.Now()
	for name, at := range map[string]time.Time{first: now.Add(-30 * time.Second), second: now} {
		if err := os.Chtimes(filepath.Join(root, "Workshop", name), at, at); err != nil {
			t.Fatal(err)
		}
	}

	body := getFootage(t, api, "?camera=Workshop&date=2026-10-13")
	if len(body.Segments) != 2 {
		t.Fatalf("segments %+v", body.Segments)
	}
	if !body.Segments[0].InProgress {
		t.Fatalf("the newest segment is the one being written: %+v", body.Segments[0])
	}
	if body.Segments[1].InProgress {
		t.Fatalf("a segment that rolled over seconds ago is finished: %+v", body.Segments[1])
	}
}

func TestFilesThatAreNotSegmentsAreIgnored(t *testing.T) {
	api, root := newFootageAPI(t)
	addSegment(t, root, "Workshop", "20261013_120000+0300", 900, 1024)
	for _, name := range []string{
		"Workshop.mkv",                        // no stamp
		"Workshop_20261013_120000.mkv",        // no offset
		"Workshop_20261013_120000+0300.txt",   // not a recording
		"Other_20261013_120000+0300.mkv",      // another camera's prefix
		"Workshop_2026101_120000+0300.mkv",    // short date
		"Workshop_20261013_120000+0300.mkv.t", // a half-written copy
	} {
		if err := os.WriteFile(filepath.Join(root, "Workshop", name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	body := getFootage(t, api, "?camera=Workshop")
	if len(body.Dates) != 1 || body.Dates[0].Segments != 1 {
		t.Fatalf("only the one real segment should be indexed: %+v", body.Dates)
	}
}

// ---------------------------------------------------------------------------
// Gaps
// ---------------------------------------------------------------------------

func TestGapsAreVisibleNotHidden(t *testing.T) {
	api, root := newFootageAPI(t)
	// A day that starts late, stops for 75 minutes in the middle, and ends
	// before midnight. All three silences are the Operator's to see.
	addSegment(t, root, "Workshop", "20261012_090000+0300", 900, 1024)
	addSegment(t, root, "Workshop", "20261012_091500+0300", 900, 1024)
	addSegment(t, root, "Workshop", "20261012_114500+0300", 900, 1024)

	body := getFootage(t, api, "?camera=Workshop&date=2026-10-12")
	if len(body.Gaps) != 3 {
		t.Fatalf("expected the leading, middle and trailing gaps, got %+v", body.Gaps)
	}
	// Newest first, matching the segment list.
	if body.Gaps[0].From != "12:00:00" || body.Gaps[0].To != "00:00:00" {
		t.Fatalf("trailing gap wrong: %+v", body.Gaps[0])
	}
	if body.Gaps[1].From != "09:30:00" || body.Gaps[1].To != "11:45:00" || body.Gaps[1].Seconds != 8100 {
		t.Fatalf("middle gap wrong: %+v", body.Gaps[1])
	}
	if body.Gaps[2].From != "00:00:00" || body.Gaps[2].To != "09:00:00" {
		t.Fatalf("leading gap wrong: %+v", body.Gaps[2])
	}
	for _, gap := range body.Gaps {
		if gap.Open {
			t.Fatalf("a day that is over has no open gap: %+v", gap)
		}
	}
}

func TestAContinuousDayHasNoGaps(t *testing.T) {
	api, root := newFootageAPI(t)
	start := time.Date(2026, 10, 12, 0, 0, 0, 0, time.FixedZone("", 3*3600))
	for i := 0; i < 96; i++ {
		addSegment(t, root, "Workshop", start.Add(time.Duration(i)*15*time.Minute).Format("20060102_150405-0700"), 900, 1024)
	}
	body := getFootage(t, api, "?camera=Workshop&date=2026-10-12")
	if len(body.Segments) != 96 {
		t.Fatalf("segments %d", len(body.Segments))
	}
	if len(body.Gaps) != 0 {
		t.Fatalf("a full day of clean rollovers has no gaps: %+v", body.Gaps)
	}
}

// TestTheGapSinceTheLastSegmentOfTodayIsOpen asks segmentGaps directly, with
// the clock supplied, because a day still in progress is the one case whose
// answer depends on what time it is.
func TestTheGapSinceTheLastSegmentOfTodayIsOpen(t *testing.T) {
	zone := time.FixedZone("", 3*3600)
	start := time.Date(2026, 10, 13, 9, 0, 0, 0, zone)
	day := []segmentFile{{Name: "Workshop_20261013_090000+0300.mkv", Start: start, Seconds: 900}}

	gaps := segmentGaps(day, start.Add(3*time.Hour))
	if len(gaps) != 2 {
		t.Fatalf("expected the leading gap and the open one, got %+v", gaps)
	}
	if !gaps[0].Open || gaps[0].From != "09:15:00" || gaps[0].To != "12:00:00" || gaps[0].Seconds != 9900 {
		t.Fatalf("open gap wrong: %+v", gaps[0])
	}
	if gaps[1].Open || gaps[1].From != "00:00:00" || gaps[1].To != "09:00:00" {
		t.Fatalf("leading gap wrong: %+v", gaps[1])
	}

	// Once the day is over the same silence is bounded by midnight instead.
	gaps = segmentGaps(day, start.AddDate(0, 0, 2))
	if gaps[0].Open || gaps[0].To != "00:00:00" {
		t.Fatalf("a finished day has no open gap: %+v", gaps[0])
	}
}

// TestTheRepeatedHourOnTheDSTChangeIsNotAGap is why %z is in the filename. On
// 25 October 2026 the clocks go back at 04:00 +0300, so 03:00-03:59 happens
// twice: once at +0300 and once at +0200. Read as local clock times the second
// hour looks like a 3600 s overlap; read with each segment's own offset it is
// one continuous recording.
func TestTheRepeatedHourOnTheDSTChangeIsNotAGap(t *testing.T) {
	api, root := newFootageAPI(t)
	for _, stamp := range []string{
		"20261025_030000+0300", "20261025_031500+0300", "20261025_033000+0300", "20261025_034500+0300",
		"20261025_030000+0200", "20261025_031500+0200", "20261025_033000+0200", "20261025_034500+0200",
	} {
		addSegment(t, root, "Workshop", stamp, 900, 1024)
	}

	body := getFootage(t, api, "?camera=Workshop&date=2026-10-25")
	if len(body.Segments) != 8 {
		t.Fatalf("both passes through the repeated hour belong to the day: %d", len(body.Segments))
	}
	// Newest first means the +0200 pass comes first: it is an hour later in
	// absolute time despite reading the same on the clock.
	if body.Segments[0].StartAt != "2026-10-25T03:45:00+02:00" {
		t.Fatalf("newest segment is the second pass: %+v", body.Segments[0])
	}
	if body.Segments[7].StartAt != "2026-10-25T03:00:00+03:00" {
		t.Fatalf("oldest segment is the first pass: %+v", body.Segments[7])
	}
	for _, gap := range body.Gaps {
		if gap.From == "04:00:00" || gap.Seconds == 3600 {
			t.Fatalf("the repeated hour was read as a gap: %+v", gap)
		}
	}
}

// ---------------------------------------------------------------------------
// Path traversal
// ---------------------------------------------------------------------------

func TestPathInsideMirrorsTestCamPathInside(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cameras")
	cases := []struct {
		path string
		want bool
	}{
		{filepath.Join(root, "Workshop", "a.mkv"), true},
		{filepath.Join(root, "a.mkv"), true},
		{root + "_old" + string(filepath.Separator) + "a.mkv", false},
		{filepath.Join(root, "..", "elsewhere", "a.mkv"), false},
		{filepath.Join(root, "Workshop", "..", "..", "secret.txt"), false},
		{filepath.Dir(root), false},
		{"", false},
	}
	for _, test := range cases {
		if got := pathInside(root, test.path); got != test.want {
			t.Errorf("pathInside(%q, %q) = %t, want %t", root, test.path, got, test.want)
		}
	}
}

func TestClipRefusesEverythingThatIsNotASegmentOfThatCamera(t *testing.T) {
	api, root := newFootageAPI(t)
	real := addSegment(t, root, "Workshop", "20261013_120000+0300", 900, 1024)

	// A sibling directory sharing the archive root's prefix, and a file in it
	// that a traversal would be aiming for.
	sibling := root + "_old"
	if err := os.MkdirAll(filepath.Join(sibling, "Workshop"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "Workshop", real), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(root), "secret.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	refused := []struct {
		what  string
		query string
	}{
		{"a relative escape in the segment", "?camera=Workshop&segment=../../secret.txt"},
		{"a backslash escape in the segment", `?camera=Workshop&segment=..\..\secret.txt`},
		{"an absolute path as the segment", "?camera=Workshop&segment=" + filepath.Join(filepath.Dir(root), "secret.txt")},
		{"an absolute Windows path as the segment", `?camera=Workshop&segment=C:\Windows\win.ini`},
		{"a UNC path as the segment", `?camera=Workshop&segment=\\127.0.0.1\c$\win.ini`},
		{"another camera's segment", "?camera=Workshop&segment=Front_Entrance_20261013_120000+0300.mkv"},
		{"a relative escape in the camera", "?camera=../" + filepath.Base(sibling) + "&segment=" + real},
		{"the sibling root as the camera", "?camera=" + filepath.Base(sibling) + "&segment=" + real},
		{"an absolute path as the camera", "?camera=" + sibling + "&segment=" + real},
		{"a camera that is not a camera", "?camera=Nonesuch&segment=Nonesuch_20261013_120000+0300.mkv"},
		{"the recorder's own state folder", "?camera=_state&segment=status.json"},
		{"no camera at all", "?segment=" + real},
		{"no segment at all", "?camera=Workshop"},
		{"a segment that does not exist", "?camera=Workshop&segment=Workshop_20260101_000000+0300.mkv"},
	}
	for _, test := range refused {
		response := request(t, api, "GET", "/api/clip"+test.query, "")
		if response.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404 (%s)", test.what, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "secret") || strings.Contains(response.Body.String(), "win.ini") {
			t.Errorf("%s: the refusal echoed the request back: %s", test.what, response.Body.String())
		}
	}

	// The same table, asked of the resolver directly, so a path that escapes
	// is caught even if a later handler stops calling it.
	cam, err := api.resolveCamera("Workshop")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range refused {
		segment := strings.TrimPrefix(test.query, "?camera=Workshop&segment=")
		if resolved, err := api.resolveSegment(cam, segment); err == nil {
			t.Errorf("resolveSegment accepted %q -> %q", segment, resolved)
		}
	}
}

func TestFootageIndexRefusesACameraThatIsNotACamera(t *testing.T) {
	api, _ := newFootageAPI(t)
	for _, query := range []string{"?camera=Nonesuch", "?camera=_state", "?camera=../cameras", "?camera=" + `C:\Windows`} {
		response := request(t, api, "GET", "/api/footage"+query, "")
		if response.Code != http.StatusNotFound {
			t.Errorf("GET /api/footage%s returned %d, want 404", query, response.Code)
		}
	}
	// A date without a camera is a request that cannot be answered.
	if response := request(t, api, "GET", "/api/footage?date=2026-10-13", ""); response.Code != http.StatusBadRequest {
		t.Errorf("a date with no camera returned %d, want 400", response.Code)
	}
	// A malformed date is refused rather than matched against nothing.
	api2, root := newFootageAPI(t)
	addSegment(t, root, "Workshop", "20261013_120000+0300", 900, 1024)
	if response := request(t, api2, "GET", "/api/footage?camera=Workshop&date=13-10-2026", ""); response.Code != http.StatusBadRequest {
		t.Errorf("a malformed date returned %d, want 400", response.Code)
	}
}

// ---------------------------------------------------------------------------
// No credential, no address
// ---------------------------------------------------------------------------

func TestFootageResponsesCarryNoAddressOrCredential(t *testing.T) {
	api, root := newFootageAPI(t)
	if err := os.WriteFile(api.cfg.StatusFile, []byte(leakyStatus), 0o600); err != nil {
		t.Fatal(err)
	}
	addSegment(t, root, "Front_Entrance", "20261013_120000+0300", 900, 1024)

	for _, query := range []string{"", "?camera=Front%20Entrance", "?camera=Front%20Entrance&date=2026-10-13"} {
		body := request(t, api, "GET", "/api/footage"+query, "").Body.String()
		if hits := scan(body); len(hits) != 0 {
			t.Errorf("GET /api/footage%s leaked %v: %s", query, hits, body)
		}
	}
}

// ---------------------------------------------------------------------------
// Streaming
// ---------------------------------------------------------------------------

// fakeFfmpeg replaces ffmpeg with a script, so the streaming behaviour can be
// tested without a camera, a real segment or an ffmpeg build. The script writes
// its own pid where the test can read it, which is how "no orphan is left
// behind" is asserted rather than assumed.
func fakeFfmpeg(t *testing.T, api *API, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake ffmpeg is a shell script")
	}
	path := filepath.Join(t.TempDir(), "fake-ffmpeg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := map[string]any{"FfmpegPath": path, "StatusIntervalSeconds": 5}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(api.cfg.RecorderConfigFile(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClipStreamsWhatFfmpegWrites(t *testing.T) {
	api, root := newFootageAPI(t)
	segment := addSegment(t, root, "Workshop", "20261013_120000+0300", 900, 1024)
	fakeFfmpeg(t, api, `printf 'fragmented-mp4-bytes'`)

	response := request(t, api, "GET", "/api/clip?camera=Workshop&segment="+url.QueryEscape(segment), "")
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "video/mp4" {
		t.Fatalf("content type %q", got)
	}
	if got := response.Header().Get("Accept-Ranges"); got != "none" {
		t.Fatalf("a stream with no length must not advertise ranges, got %q", got)
	}
	if response.Body.String() != "fragmented-mp4-bytes" {
		t.Fatalf("body %q", response.Body.String())
	}
}

func TestClipAnswersAnFfmpegThatProducesNothingWithAnError(t *testing.T) {
	api, root := newFootageAPI(t)
	segment := addSegment(t, root, "Workshop", "20261013_120000+0300", 900, 1024)
	fakeFfmpeg(t, api, `echo "Invalid data found when processing input" >&2; exit 1`)

	response := request(t, api, "GET", "/api/clip?camera=Workshop&segment="+url.QueryEscape(segment), "")
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "Invalid data") {
		t.Fatalf("the reason ffmpeg gave should reach the page: %s", response.Body.String())
	}
}

// TestClientDisconnectLeavesNoFfmpegBehind is acceptance criterion S5 in unit
// form: the remux must die with the request, not run on to the end of a segment
// nobody is watching.
func TestClientDisconnectLeavesNoFfmpegBehind(t *testing.T) {
	api, root := newFootageAPI(t)
	segment := addSegment(t, root, "Workshop", "20261013_120000+0300", 900, 1024)
	pidFile := filepath.Join(t.TempDir(), "pid")
	fakeFfmpeg(t, api, "echo $$ > "+pidFile+"\nprintf 'head'\nwhile true; do sleep 1; done")

	mux := http.NewServeMux()
	api.Routes(mux)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/api/clip?camera=Workshop&segment="+url.QueryEscape(segment), nil).WithContext(ctx)

	done := make(chan struct{})
	writer := &blockingWriter{header: http.Header{}, wrote: make(chan struct{}, 1)}
	go func() {
		mux.ServeHTTP(writer, req)
		close(done)
	}()

	// Wait for the stream to be under way, so there is something to orphan.
	select {
	case <-writer.wrote:
	case <-time.After(10 * time.Second):
		t.Fatal("the clip never started streaming")
	}
	pid := readPid(t, pidFile)
	if !processAlive(pid) {
		t.Fatalf("the fake ffmpeg %d should be running while the clip streams", pid)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler did not return after the client disconnected")
	}

	deadline := time.Now().Add(10 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("ffmpeg %d outlived the request it was serving", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func readPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			var pid int
			if _, err := fmt.Sscan(strings.TrimSpace(string(raw)), &pid); err == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pid in %s", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// blockingWriter is a ResponseWriter that reports the first body write and then
// accepts bytes for ever, standing in for a browser that is still connected.
type blockingWriter struct {
	header http.Header
	wrote  chan struct{}
}

func (b *blockingWriter) Header() http.Header { return b.header }
func (b *blockingWriter) WriteHeader(int)     {}
func (b *blockingWriter) Write(p []byte) (int, error) {
	select {
	case b.wrote <- struct{}{}:
	default:
	}
	return len(p), nil
}
