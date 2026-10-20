package relay

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"holocron/internal/config"
	"holocron/internal/osx"
)

func TestRelayChildSpecWindowsDefaults(t *testing.T) {
	cfg := config.Defaults(&osx.Fake{OS: "windows"})
	spec, err := RelayChildSpec(&osx.Fake{OS: "windows"}, cfg, `C:\logs\restreamer.log`, []string{"PATH=x", "MTX_STALE=1"}, []string{"MTX_PATHS_A_SOURCE=rtsp://u:p@h/s"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "restreamer" || spec.Exe != `C:\mediamtx\mediamtx.exe` || !reflect.DeepEqual(spec.Args, []string{`C:\mediamtx\mediamtx.yml`}) {
		t.Errorf("%+v", spec)
	}
	if spec.Dir != `C:\mediamtx` || spec.LogPath != `C:\logs\restreamer.log` || spec.ConfigPath != `C:\mediamtx\mediamtx.yml` {
		t.Errorf("%+v", spec)
	}
	if !reflect.DeepEqual(spec.Env, []string{"PATH=x", "MTX_PATHS_A_SOURCE=rtsp://u:p@h/s"}) {
		t.Errorf("env %v", spec.Env)
	}
	want := RestartPolicy{3 * time.Second, 60 * time.Second, 300 * time.Second, 10 * time.Second}
	if spec.Restart != want {
		t.Errorf("restart %+v", spec.Restart)
	}
	// the arguments never hold a credential
	for _, a := range spec.Args {
		if strings.Contains(a, "@") {
			t.Error(a)
		}
	}
}

func TestLiveChildSpec(t *testing.T) {
	cfg := config.Defaults(&osx.Fake{OS: "windows"})
	spec, err := LiveChildSpec(&osx.Fake{OS: "windows"}, cfg, "l", nil, nil)
	if err != nil || spec.Name != "liveview" || spec.Exe != `C:\mediamtx\mediamtx.exe` || spec.Dir != `C:\mediamtx` || spec.Args[0] != `C:\mediamtx\mediamtx-live.yml` {
		t.Errorf("%+v %v", spec, err)
	}
	cfg.LiveExePath = `D:\other\mediamtx.exe`
	cfg.LiveConfigPath = `D:\live\mediamtx-live.yml`
	spec, _ = LiveChildSpec(&osx.Fake{OS: "windows"}, cfg, "l", nil, nil)
	if spec.Exe != `D:\other\mediamtx.exe` || spec.Dir != `D:\live` {
		t.Errorf("%+v", spec)
	}
}

func TestWorkingDirectoryIsLocalAndAbsolute(t *testing.T) {
	cases := []struct {
		goos, path, dir string
		bad             bool
	}{
		{"windows", `C:\mediamtx\mediamtx.yml`, `C:\mediamtx`, false},
		{"windows", `c:/mediamtx/m.yml`, `c:/mediamtx`, false},
		{"windows", `C:\m.yml`, `C:\`, false},
		{"windows", `\\wsl.localhost\Ubuntu\home\x\mediamtx.yml`, "", true},
		{"windows", `\\server\share\mediamtx.yml`, "", true},
		{"windows", `//server/share/m.yml`, "", true},
		{"windows", `mediamtx.yml`, "", true},
		{"windows", `\mediamtx\mediamtx.yml`, "", true},
		{"linux", "/etc/holocron/mediamtx.yml", "/etc/holocron", false},
		{"linux", "/mediamtx.yml", "/", false},
		{"linux", "mediamtx.yml", "", true},
		{"darwin", "relative/m.yml", "", true},
	}
	for _, c := range cases {
		dir, err := localDir(c.goos, c.path)
		if c.bad != (err != nil) || dir != c.dir {
			t.Errorf("%s %s: %q, %v", c.goos, c.path, dir, err)
		}
	}
	cfg := config.Defaults(&osx.Fake{OS: "windows"})
	cfg.RestreamerConfigPath = `\\wsl.localhost\Ubuntu\x\mediamtx.yml`
	if _, err := RelayChildSpec(&osx.Fake{OS: "windows"}, cfg, "l", nil, nil); err == nil || !strings.Contains(err.Error(), "local working directory") {
		t.Error(err)
	}
	cfg.RestreamerConfigPath = ""
	if _, err := RelayChildSpec(&osx.Fake{OS: "windows"}, cfg, "l", nil, nil); err == nil {
		t.Error("empty config path accepted")
	}
}

func TestReadinessPolicy(t *testing.T) {
	cfg := config.Defaults(&osx.Fake{})
	p := NewReadinessPolicy(cfg)
	if p.StartupWait != 20*time.Second || p.ReadyWait != 15*time.Second || p.Poll != 500*time.Millisecond || p.ProbeTimeout != 1500*time.Millisecond {
		t.Errorf("%+v", p)
	}
	cfg.RelayProbeTimeoutMs = 10
	cfg.RelayStartupWaitSeconds = 0
	p = NewReadinessPolicy(cfg)
	if p.ProbeTimeout != 100*time.Millisecond || p.ShouldWaitAtStart(3) {
		t.Errorf("%+v", p)
	}
	if !NewReadinessPolicy(config.Defaults(&osx.Fake{})).ShouldWaitAtStart(1) || NewReadinessPolicy(config.Defaults(&osx.Fake{})).ShouldWaitAtStart(0) {
		t.Error("ShouldWaitAtStart")
	}

	p = NewReadinessPolicy(config.Defaults(&osx.Fake{}))
	start := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	hold := []struct {
		after          time.Duration
		alive, serving bool
		want           bool
	}{
		{2 * time.Second, true, false, true},   // relay up, path not serving yet: hold
		{2 * time.Second, true, true, false},   // serving: go
		{2 * time.Second, false, false, false}, // relay dead: go direct now
		{14 * time.Second, true, false, true},  // still inside 15 s
		{15 * time.Second, true, false, false}, // budget spent: direct
		{time.Minute, true, false, false},
	}
	for _, h := range hold {
		if got := p.HoldChild(start.Add(h.after), start, h.alive, h.serving); got != h.want {
			t.Errorf("%+v: got %v", h, got)
		}
	}
}

func TestExitMessage(t *testing.T) {
	for _, c := range []struct {
		ran  time.Duration
		want string
	}{
		{7400 * time.Millisecond, "after 7s"},
		{7600 * time.Millisecond, "after 8s"}, // truncation would say 7
		{2500 * time.Millisecond, "after 2s"}, // half to even
		{3500 * time.Millisecond, "after 4s"},
		{0, "after 0s"},
	} {
		if got := ExitMessage(1, c.ran); got != "mediamtx exited with code 1 "+c.want {
			t.Errorf("%v: %s", c.ran, got)
		}
	}
}
