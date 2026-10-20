package supervise

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"holocron/internal/clock"
	"holocron/internal/ffmpeg"
	"holocron/internal/logx"
	"holocron/internal/relay"
	"holocron/internal/status"
)

// mtxChild is one MediaMTX child (the recording relay or the live view) and its supervising
// goroutine: start, readiness wait (relay), restart with the camera backoff (IS 4, R:2279-2337,
// R:2480-2548), hard kill on stop.
type mtxChild struct {
	s       *Supervisor
	name    string // restreamer | liveview
	label   string // restreamer | live view (log text)
	live    bool
	log     *slog.Logger
	logPath string

	stopOnce sync.Once
	stopCh   chan struct{}
	done     chan struct{}

	// guarded by s.mu
	child       *ffmpeg.Child
	pid         int64
	lastPid     int64
	alive       bool
	startedWall time.Time
	startedMono clock.Mono
	restarts    int64
	paths       []string
	liveCams    []status.LiveCamera
	lastError   string
	lastErrorAt time.Time
	backoff     int
	reasonSaid  string
}

func (m *mtxChild) start() {
	m.stopCh, m.done = make(chan struct{}), make(chan struct{})
	go m.run()
}

// stop ends the goroutine and kills the child (hard), once.
func (m *mtxChild) stop() {
	m.stopOnce.Do(func() { close(m.stopCh) })
}

func (m *mtxChild) aliveSince() (bool, clock.Mono) {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	return m.alive, m.startedMono
}

func (m *mtxChild) running() *ffmpeg.Child {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	return m.child
}

// sleep waits d on the supervisor clock; false when the child is to stop.
func (m *mtxChild) sleep(d time.Duration) bool {
	select {
	case <-m.s.clk.After(d):
		return true
	case <-m.stopCh:
		return false
	}
}

// say logs a reason once until it changes (Invoke-CamLiveTick's ReasonLogged).
func (m *mtxChild) say(level slog.Level, reason string) {
	m.s.mu.Lock()
	same := m.reasonSaid == reason
	m.reasonSaid = reason
	m.s.mu.Unlock()
	if !same {
		m.log.Log(context.Background(), level, reason, logx.Target(m.name))
	}
}

func (m *mtxChild) run() {
	defer close(m.done)
	defer m.s.recoverFault("the " + m.label + " supervisor")
	s := m.s
	if !m.waitCameras() {
		return
	}
	next := s.clk.Mono()
	for {
		if d := next.Sub(s.clk.Mono()); d > 0 && !m.sleep(d) {
			return
		}
		select {
		case <-m.stopCh:
			return
		default:
		}
		if m.live {
			if s.relayM == nil {
				m.say(slog.LevelWarn, "live view needs the recording relay: RestreamerEnabled is false, so it is not started")
				if !m.sleep(time.Second) {
					return
				}
				continue
			}
			if alive, _ := s.relayM.aliveSince(); !alive {
				m.say(slog.LevelInfo, "waiting for the recording relay before starting live view")
				if !m.sleep(time.Second) {
					return
				}
				continue
			}
		}
		child, err := m.launch()
		if err != nil {
			msg := s.d.Redactor.String(err.Error())
			s.mu.Lock()
			m.lastError, m.lastErrorAt = "failed to start: "+msg, s.clk.Now()
			s.mu.Unlock()
			wait := m.setBackoff(0)
			if m.live {
				m.say(slog.LevelWarn, fmt.Sprintf("failed to start live view: %s; retrying with backoff (recording is not affected)", msg))
			} else {
				m.log.Warn(fmt.Sprintf("failed to start the restreamer: %s; retry in %ds. Consumers stay on the direct camera inputs until it is up.", msg, wait),
					logx.Action("relay.start"), logx.Result(logx.ResultFail), logx.Err(err))
			}
			next = s.clk.Mono() + clock.Mono(time.Duration(wait)*time.Second)
			continue
		}
		recount, stopped := m.watch(child)
		if stopped {
			return
		}
		info := child.Wait()
		now, nowMono := s.clk.Now(), s.clk.Mono()
		s.mu.Lock()
		ranFor := nowMono.Sub(m.startedMono)
		m.alive, m.pid, m.child = false, 0, nil
		s.mu.Unlock()
		if !recount {
			m.log.Info(fmt.Sprintf("%s pid %d stopped to load its rewritten config; starting it again", m.label, m.lastPid), logx.Action("relay.restart"), logx.PID(int(m.lastPid)))
			next = nowMono
			continue
		}
		s.mu.Lock()
		m.restarts++
		n := m.restarts
		m.lastError, m.lastErrorAt = relay.ExitMessage(info.Code, ranFor), now
		s.mu.Unlock()
		wait := m.setBackoff(ranFor.Seconds())
		m.log.Warn(fmt.Sprintf("%s exit %d after %ds; restart #%d in %ds", m.label, info.Code, roundInt(ranFor.Seconds()), n, wait),
			logx.Action("relay.restart"), logx.Target(m.name), logx.Result(logx.ResultOK), logx.PID(int(m.lastPid)))
		next = nowMono + clock.Mono(time.Duration(wait)*time.Second)
	}
}

// waitCameras holds the first start until the loop has read the camera list (the relay serves
// every camera of cameras.json).
func (m *mtxChild) waitCameras() bool {
	select {
	case <-m.s.camsReady:
		return true
	case <-m.stopCh:
		return false
	}
}

// watch waits for the child to exit, for the stop, or (relay) for its config to lose the stored
// login, in which case the file is rewritten and the child restarted without counting a failure.
// recount is false for that restart; stopped is true when the child was stopped for good.
func (m *mtxChild) watch(child *ffmpeg.Child) (recount, stopped bool) {
	s := m.s
	if !m.live {
		m.waitAPI(child)
	}
	check := seconds(s.cfg.StatusIntervalSeconds)
	for {
		var tick <-chan time.Time
		if !m.live {
			tick = s.clk.After(check)
		}
		select {
		case <-child.Done():
			return true, false
		case <-m.stopCh:
			m.kill(child, "supervisor shutdown")
			return false, true
		case <-tick:
			if m.rewriteIfStale() {
				m.kill(child, "its config was rewritten to the stored control API login")
				<-child.Done()
				return false, false
			}
		}
	}
}

// kill is Stop-CamRelayChild / Stop-CamLiveChild: hard kill and a bounded wait.
func (m *mtxChild) kill(child *ffmpeg.Child, reason string) {
	select {
	case <-child.Done():
		return
	default:
	}
	m.log.Info(fmt.Sprintf("kill %s pid %d: %s", m.label, child.Pid(), reason), logx.Action("relay.stop"), logx.Target(m.name), logx.PID(child.Pid()))
	if err := child.Kill(); err != nil {
		m.log.Warn(fmt.Sprintf("kill %s pid %d failed: %v", m.label, child.Pid(), err), logx.Err(err))
	}
	select {
	case <-child.Done():
		m.s.mu.Lock()
		m.alive, m.pid = false, 0
		m.s.mu.Unlock()
		m.log.Info(fmt.Sprintf("stopped %s (pid %d)", m.label, child.Pid()), logx.Action("relay.stop"), logx.Result(logx.ResultOK), logx.PID(child.Pid()))
	case <-m.s.clk.After(10 * time.Second):
		m.log.Error(fmt.Sprintf("%s pid %d did not exit within 10 s of the kill", m.label, child.Pid()), logx.Action("relay.stop"), logx.Result(logx.ResultFail), logx.PID(child.Pid()))
	}
}

// waitAPI is the bounded startup wait of Start-CamRelayChild (R:2180-2217): until the control API
// answers at all (a refused login is an answer), not until every path serves. It blocks only this
// goroutine; the loop holds the cameras meanwhile (the relay is young) and the heartbeat goes on.
func (m *mtxChild) waitAPI(child *ffmpeg.Child) {
	s := m.s
	policy := relay.NewReadinessPolicy(s.cfg)
	s.mu.Lock()
	api, paths := s.api, len(m.paths)
	s.mu.Unlock()
	if api == nil || !policy.ShouldWaitAtStart(paths) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-m.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	set, answered := api.WaitStarted(ctx, policy, child.Done())
	waited := s.clk.Since(m.startedMono).Seconds()
	switch {
	case answered && set.State == relay.Denied:
		m.log.Warn(fmt.Sprintf("restreamer control API refused the product's login after %.1fs: readiness is not visible, so every consumer is kept on the relay", waited), logx.Action("relay.ready"))
	case answered:
		m.log.Info(fmt.Sprintf("restreamer control API answered after %.1fs, serving %d of %d path(s): %s", waited, len(set.Names()), paths, strings.Join(set.Names(), ", ")), logx.Action("relay.ready"), logx.Result(logx.ResultOK))
	default:
		m.log.Warn(fmt.Sprintf("restreamer control API did not answer after %.1fs, serving 0 of %d path(s)", waited, paths), logx.Action("relay.ready"), logx.Result(logx.ResultFail))
	}
}

// launch renders and writes the config (temp then rename, only when it changed), builds the
// environment (camera credentials only there) and starts the child through the supervisor's starter.
func (m *mtxChild) launch() (*ffmpeg.Child, error) {
	s := m.s
	cfg := s.cfg
	cams := s.camerasNow()
	var spec relay.ChildSpec
	var paths []string
	var liveCams []status.LiveCamera
	var configPath string
	if m.live {
		lp, err := relay.LivePaths(cams, cfg)
		if err != nil {
			return nil, err
		}
		login, err := s.d.LiveLogin()
		if err != nil {
			return nil, err
		}
		s.register(login.Secret)
		text, err := relay.LiveConfig(relay.LiveInputs{Config: cfg, Cameras: cams, Login: login})
		if err != nil {
			return nil, err
		}
		configPath = cfg.LiveConfigPath
		if changed, err := relay.WriteConfigIfChanged(configPath, text); err != nil {
			return nil, err
		} else if changed {
			m.log.Info(fmt.Sprintf("wrote %s for live view (%d path(s))", configPath, len(lp)), logx.Action("live.config"), logx.Result(logx.ResultOK))
		}
		env, err := relay.LiveSourceEnv(cams, cfg, credentials{s})
		if err != nil {
			return nil, err
		}
		if spec, err = relay.LiveChildSpec(s.d.Platform, cfg, m.logPath, os.Environ(), env); err != nil {
			return nil, err
		}
		paths, liveCams = livePathView(lp)
	} else {
		rp, err := relay.RelayPaths(cams)
		if err != nil {
			return nil, err
		}
		login, err := s.d.APILogin()
		if err != nil {
			return nil, err
		}
		s.register(login.Secret)
		in := relay.RelayInputs{Config: cfg, Cameras: cams, API: login}
		text, err := relay.RelayConfig(in)
		if err != nil {
			return nil, err
		}
		configPath = cfg.RestreamerConfigPath
		if changed, err := relay.WriteConfigIfChanged(configPath, text); err != nil {
			return nil, err
		} else if changed {
			m.log.Info(fmt.Sprintf("wrote %s for the restreamer (%d path(s))", configPath, len(rp)), logx.Action("relay.config"), logx.Result(logx.ResultOK))
		}
		if cur := relay.ConfigCurrent(configPath, in); !cur.Current {
			m.log.Warn(fmt.Sprintf("WARNING: %s Starting the restreamer with the file as it is.", cur.Detail), logx.Action("relay.config"))
		}
		env, err := relay.SourceEnv(cams, credentials{s})
		if err != nil {
			return nil, err
		}
		if spec, err = relay.RelayChildSpec(s.d.Platform, cfg, m.logPath, os.Environ(), env); err != nil {
			return nil, err
		}
		for _, p := range rp {
			paths = append(paths, p.Name)
		}
		s.mu.Lock()
		s.api = s.d.NewRelayAPI(cfg, login)
		s.mu.Unlock()
	}
	if paths == nil {
		paths = []string{}
	}
	if s.isStopping() {
		return nil, errors.New("the supervisor is stopping")
	}
	child, err := ffmpeg.Start(context.Background(), ffmpeg.Spec{
		Name: m.name, Exe: spec.Exe, Args: spec.Args, Dir: spec.Dir, Env: spec.Env, Starter: s.starter,
		Redactor: s.d.Redactor, Log: s.d.ChildLog(m.name, spec.LogPath), Clock: s.clk,
	})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	m.child, m.pid, m.lastPid, m.alive = child, int64(child.Pid()), int64(child.Pid()), true
	m.startedWall, m.startedMono = s.clk.Now(), s.clk.Mono()
	m.paths, m.liveCams, m.reasonSaid = paths, liveCams, ""
	s.mu.Unlock()
	if m.live {
		m.log.Info(fmt.Sprintf("start live view pid %d: %s serving %d path(s)", child.Pid(), spec.Exe, len(paths)),
			logx.Action("live.start"), logx.Result(logx.ResultOK), logx.PID(child.Pid()))
	} else {
		m.log.Info(fmt.Sprintf("start restreamer pid %d: %s serving %d path(s): %s", child.Pid(), spec.Exe, len(paths), strings.Join(paths, ", ")),
			logx.Action("relay.start"), logx.Result(logx.ResultOK), logx.PID(child.Pid()))
	}
	return child, nil
}

// rewriteIfStale rewrites the relay config when its control-API login is missing or not the stored
// one (Test-CamRelayConfigCurrent AuthCurrent); true when it wrote, so the caller restarts the child.
func (m *mtxChild) rewriteIfStale() bool {
	s := m.s
	login, err := s.d.APILogin()
	if err != nil {
		return false
	}
	in := relay.RelayInputs{Config: s.cfg, Cameras: s.camerasNow(), API: login}
	cur := relay.ConfigCurrent(s.cfg.RestreamerConfigPath, in)
	if !cur.NeedsRewrite() {
		return false
	}
	text, err := relay.RelayConfig(in)
	if err != nil {
		return false
	}
	if _, err := relay.WriteConfigIfChanged(s.cfg.RestreamerConfigPath, text); err != nil {
		m.log.Error("could not rewrite the restreamer config: "+err.Error(), logx.Action("relay.config"), logx.Result(logx.ResultFail), logx.Err(err))
		return false
	}
	m.log.Warn(relay.RewriteMessage(s.cfg.RestreamerConfigPath), logx.Action("relay.config"), logx.Result(logx.ResultOK))
	return true
}

// setBackoff is Set-CamChildBackoff for a MediaMTX child.
func (m *mtxChild) setBackoff(ranFor float64) int {
	cfg := m.s.cfg
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	if ranFor >= float64(cfg.HealthyResetSeconds) {
		m.backoff = cfg.BackoffStartSeconds
	} else {
		m.backoff = min(max(m.backoff*2, cfg.BackoffStartSeconds), cfg.BackoffMaxSeconds)
	}
	return m.backoff
}

// livePathView is the Live.Paths and Live.Cameras view of the live path map.
func livePathView(lp []relay.LivePath) ([]string, []status.LiveCamera) {
	paths := []string{}
	cams := []status.LiveCamera{}
	idx := map[string]int{}
	for _, p := range lp {
		paths = append(paths, p.Name)
		i, ok := idx[p.Camera.Name]
		if !ok {
			i = len(cams)
			idx[p.Camera.Name] = i
			cams = append(cams, status.LiveCamera{Name: p.Camera.Name})
		}
		switch p.Kind {
		case relay.LiveSub:
			cams[i].Sub, cams[i].SubPath = true, p.Name
		case relay.LiveMain:
			cams[i].Main, cams[i].MainPath = true, p.Name
		}
	}
	return paths, cams
}
