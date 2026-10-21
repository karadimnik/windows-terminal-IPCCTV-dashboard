//go:build !windows

package app

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"holocron/internal/status"
)

// The shadow rig (docs/shadow.md) runs Holocron with RestreamerEnabled false and every camera
// entry pointing at the Operator's existing relay: IP 127.0.0.1:8554, stream = the relay path. This
// runs the production supervisor on that configuration with a stand-in ffmpeg and shows (1) what
// the stand-in is asked to read, (2) that a relay that refuses is retried with the backoff, not in
// a tight loop, with the supervisor staying up, and (3) that recording resumes by itself when the
// relay is back.
func TestShadowRigSurvivesARelayThatIsGoneAndRecovers(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "shadow root")
	cfgDir := filepath.Join(root, "_config")
	os.MkdirAll(cfgDir, 0o755)
	ffmpeg := filepath.Join(dir, "ffmpeg")
	starts, captured, up := filepath.Join(dir, "starts"), filepath.Join(dir, "captured"), filepath.Join(dir, "relay-is-up")
	// -version answers like ffmpeg; any other call logs itself and, until `relay-is-up` exists, fails
	// at once like ffmpeg does when the RTSP connection is refused; afterwards it records (sleeps)
	script := `#!/bin/sh
case "$1" in -version) echo "ffmpeg version 7.1"; exit 0;; esac
echo "$$ $(date +%s)" >> ` + starts + `
prev=
for a in "$@"; do if [ "$prev" = "-i" ]; then cat "$a" >> ` + captured + `; fi; prev="$a"; done
if [ -e ` + up + ` ]; then exec sleep 300; fi
exit 1
`
	if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"FfmpegPath":` + strconv.Quote(ffmpeg) + `,"RestreamerEnabled":false,"SegmentSeconds":60,"ReconcileSeconds":0.5,"StatusIntervalSeconds":0.5,
"BackoffStartSeconds":1,"BackoffMaxSeconds":2,"StopTimeoutSeconds":5,"alerts":{"desktop":{"enabled":false},"eventlog":{"enabled":false}},"web":{"enabled":false}}`
	cams := `[{"Name":"Front Door","IP":"127.0.0.1:8554","Quality":"frontdoor","Record":true}]`
	cfgPath := filepath.Join(cfgDir, "holocron.config.json")
	os.WriteFile(cfgPath, []byte(cfg), 0o644)
	os.WriteFile(filepath.Join(cfgDir, "cameras.json"), []byte(cams), 0o644)

	r, err := NewSupervisor(Options{Root: root, ConfigPath: cfgPath, Platform: plat, Getenv: func(k string) string {
		return map[string]string{EnvCameraUser: "shadow", EnvCameraPassword: "shadow-NOT-A-SECRET"}[k]
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	count := func() int { b, _ := os.ReadFile(starts); return strings.Count(string(b), "\n") }
	time.Sleep(7 * time.Second)
	n := count()
	if n < 2 || n > 6 {
		t.Fatalf("a refused relay: %d start(s) in 7 s, want a few (backoff 1 s, then 2 s), not a loop", n)
	}
	t.Logf("%d start(s) of the recorder in 7 s against a refused relay", n)
	doc, err := status.Read(filepath.Join(root, "_state", "status.json"))
	if err != nil || doc.State != status.StateRunning {
		t.Fatalf("the supervisor must stay up: %v %+v", err, doc)
	}
	if doc.Restreamer != nil && doc.Restreamer.Enabled {
		t.Errorf("no relay of its own may be started: %+v", doc.Restreamer)
	}
	list, _ := os.ReadFile(captured)
	if !strings.Contains(string(list), "rtsp://shadow:") || !strings.Contains(string(list), "@127.0.0.1:8554/frontdoor'") || !strings.Contains(string(list), "option rtsp_transport tcp") {
		t.Errorf("the recorder should read the relay path: %q", list)
	}

	// the relay is back: the next start stays up and the camera is reported alive
	os.WriteFile(up, nil, 0o644)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if d, err := status.Read(filepath.Join(root, "_state", "status.json")); err == nil && len(d.Cameras) == 1 && d.Cameras[0].Alive {
			cancel()
			if code := <-done; code != 0 {
				t.Errorf("exit %d", code)
			}
			done <- 0
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("recording did not resume when the relay came back")
}
