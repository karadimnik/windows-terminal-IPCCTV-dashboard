package supervise

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"holocron/internal/archive"
	"holocron/internal/clock"
	"holocron/internal/config"
	"holocron/internal/ffmpeg"
	"holocron/internal/logx"
	"holocron/internal/relay"
)

// loopState is the reconcile loop's own bookkeeping (only the loop goroutine touches it).
type loopState struct {
	first         bool
	lastReconcile clock.Mono
	lastStatus    clock.Mono
	lastArchive   clock.Mono
	archiveDone   bool
	cams          []config.Camera
	camsLoaded    bool
	camsErr       string
	cfgNote       string
}

// loop is the reconcile goroutine: it owns the Manager. Every second it checks the stop-request
// file, reconciles the camera list every ReconcileSeconds, fetches ONE ready set, refreshes the scans
// and evaluates alert 110 every StatusIntervalSeconds, takes the archive scan every
// ArchiveScanSeconds, asks the Manager for actions and performs them, and publishes the status rows.
// Between ticks it answers the start and exit reports of the camera children.
func (s *Supervisor) loop(quit <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	defer s.recoverFault("the reconcile loop")
	ls := &loopState{first: true}
	s.tick(ls)
	timer := s.clk.After(time.Second)
	for {
		select {
		case <-quit:
			return
		case r := <-s.startCh:
			s.onStarted(r)
		case x := <-s.exitCh:
			s.onExited(x)
		case done := <-s.reloadCh:
			ls.lastReconcile = s.clk.Mono()
			s.reconcile(ls)
			s.publishRows()
			close(done)
		case <-timer:
			s.tick(ls)
			timer = s.clk.After(time.Second)
		}
	}
}

func (s *Supervisor) tick(ls *loopState) {
	now := s.clk.Mono()
	if _, err := os.Lstat(s.stopFile); err == nil {
		s.log.Info("stop requested through _state/stop.request", logx.Action("supervisor.stop"), logx.Result(logx.ResultOK))
		s.requestStop(StopRequested, "stop requested")
		return
	}
	if ls.first || now.Sub(ls.lastReconcile) >= seconds(s.cfg.ReconcileSeconds) {
		ls.lastReconcile = now
		s.reconcile(ls)
	}
	statusDue := ls.first || now.Sub(ls.lastStatus) >= seconds(s.cfg.StatusIntervalSeconds)
	rv := s.relayView()
	if statusDue {
		ls.lastStatus = now
		s.m.Refresh()
		s.m.Down()
		if !ls.archiveDone || now.Sub(ls.lastArchive) >= seconds(s.cfg.ArchiveScanSeconds) {
			ls.lastArchive, ls.archiveDone = now, true
			s.scanArchive(ls)
		}
	}
	for _, a := range s.m.Plan(rv) {
		s.perform(a)
	}
	s.publishRows()
	ls.first = false
}

// publishRows stores the Manager's rows for the heartbeat (the scan of a camera without a child
// comes from the last archive scan).
func (s *Supervisor) publishRows() {
	var dirs map[string]archive.DirScan
	s.mu.Lock()
	if s.archive != nil {
		dirs = s.archive.Dirs
	}
	s.mu.Unlock()
	rows := s.m.Rows(dirs)
	s.mu.Lock()
	s.rows = rows
	s.mu.Unlock()
}

func seconds(v float64) time.Duration { return time.Duration(v * float64(time.Second)) }

// reconcile re-reads the camera list (and notices a configuration change). A list that cannot be
// read keeps the current set (R:3871-3875); an unchanged list does nothing.
func (s *Supervisor) reconcile(ls *loopState) {
	if cfg, err := s.d.Source.Config(); err != nil || !sameConfigIgnoringLive(cfg, s.cfg) {
		note := "holocron.config.json changed; the running supervisor keeps the values it started with until it is restarted"
		if err != nil {
			note = fmt.Sprintf("holocron.config.json cannot be read now (%v); the running supervisor keeps the values it started with", err)
		}
		if note != ls.cfgNote {
			ls.cfgNote = note
			s.log.Warn(note, logx.Action("config.reload"), logx.Result(logx.ResultRefused))
		}
	} else {
		ls.cfgNote = ""
	}
	cams, err := s.d.Source.Cameras()
	if err != nil {
		msg := err.Error()
		if msg != ls.camsErr {
			ls.camsErr = msg
			s.log.Warn("reconcile failed, keeping the current camera set: "+msg, logx.Action("camera.reconcile"), logx.Result(logx.ResultFail), logx.Err(err))
		}
		return
	}
	ls.camsErr = ""
	if ls.camsLoaded && equalCameras(cams, ls.cams) {
		return
	}
	ls.cams, ls.camsLoaded = cams, true
	s.m.Reconcile(cams)
	recording := 0
	for _, c := range cams {
		if c.Record {
			if _, err := c.SafeName(); err == nil {
				recording++
			}
		}
	}
	s.log.Info(fmt.Sprintf("reconcile: %d of %d camera(s) set to record, %d child(ren) managed", recording, len(cams), len(s.m.entries)),
		logx.Action("camera.reconcile"), logx.Result(logx.ResultOK))
	if len(s.m.entries) == 0 {
		s.log.Warn("No camera has Record set to true, so nothing is being recorded. Waiting for an enable.")
	}
	s.publishCameras(cams)
}

func equalCameras(a, b []config.Camera) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].IP != b[i].IP || a[i].Quality != b[i].Quality || a[i].Record != b[i].Record {
			return false
		}
	}
	return true
}

func (s *Supervisor) scanArchive(ls *loopState) {
	scan, err := s.d.Scanner.ScanRoot(s.cfg.RootPath, s.clk.Now())
	if err != nil {
		s.log.Warn("archive scan failed: "+err.Error(), logx.Action("archive.scan"), logx.Result(logx.ResultFail), logx.Err(err))
		return
	}
	s.mu.Lock()
	s.archive = &scan
	s.mu.Unlock()
	s.log.Debug("archive scan", "files", scan.Files, "bytes", scan.Bytes, "folders", len(scan.Dirs))
}

// relayView fetches the ONE ready set of this cycle (IR 11 risk 8) and feeds the login watch
// (alert 130 once per episode). An external relay (RelayExternal) has no process and no API here: it
// is alive and serves every path, as a relay whose API denies the login (Denied counts as ready), so
// every camera with a relay path starts on the relay input; one that cannot read it exits and is
// restarted on it with the exit backoff.
func (s *Supervisor) relayView() RelayView {
	if s.relayM == nil {
		if s.cfg.RelayExternal {
			return RelayView{Alive: true, Ready: relay.DeniedReadySet()}
		}
		return RelayView{}
	}
	alive, startedMono := s.relayM.aliveSince()
	s.mu.Lock()
	api := s.api
	s.mu.Unlock()
	rv := RelayView{Alive: alive, StartedAt: startedMono}
	if api == nil {
		return rv
	}
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	set, err := api.Ready(ctx)
	cancel()
	if err != nil && set.State != relay.Unavailable {
		set = relay.UnavailableReadySet()
	}
	rv.Ready = set
	switch s.watch.Observe(set.State) {
	case relay.LoginRefused:
		title, msg := relay.LoginRefusedAlert(s.cfg.RelayApiBase)
		s.alert(s.rlog, Event{ID: relay.AlertLoginRefused, Severity: "Error", Title: title, Message: msg, Key: "130"})
	case relay.LoginAccepted:
		s.rlog.Info("the restreamer's control API accepts the product's login again", logx.Action("relay.login"), logx.Result(logx.ResultOK))
	}
	s.rlog.Debug("ready set", "state", set.State.String(), "paths", set.Names())
	return rv
}

// alert raises an event and writes the ALERT log line.
func (s *Supervisor) alert(log interface {
	Error(string, ...any)
}, ev Event) {
	if s.d.Alerts != nil {
		s.d.Alerts.Raise(ev)
	}
	log.Error("ALERT "+ev.Title+" - "+ev.Message, logx.Action("alert.raise"), "event_id", ev.ID)
}

// perform carries out one Manager action: a Start after its Delay on its own goroutine (the
// stagger never blocks the loop), a Kill at once.
func (s *Supervisor) perform(a Action) {
	switch a.Kind {
	case ActStart:
		s.startWG.Add(1)
		go s.startCamera(a)
	case ActKill:
		s.mu.Lock()
		child := s.registry[a.Entry]
		s.mu.Unlock()
		if child == nil {
			s.log.Error(fmt.Sprintf("kill %s: no child process is known for it", a.Entry.Name()), logx.Action("child.stop"), logx.Target(a.Entry.Name()), logx.Result(logx.ResultFail))
			return
		}
		if err := child.Kill(); err != nil {
			s.log.Warn(fmt.Sprintf("kill %s pid %d failed: %v", a.Entry.Name(), child.Pid(), err), logx.Action("child.stop"), logx.Target(a.Entry.Name()), logx.Err(err))
		}
	}
}

// startCamera waits the stagger delay, starts ffmpeg and reports the result to the loop; once the
// report is delivered a waiter goroutine reports the exit.
func (s *Supervisor) startCamera(a Action) {
	defer s.startWG.Done()
	defer s.recoverFault("a camera start")
	e := a.Entry
	if a.Delay > 0 {
		select {
		case <-s.clk.After(a.Delay):
		case <-s.stopCh:
			s.startCh <- camStarted{e: e, err: fmt.Errorf("the supervisor is stopping")}
			return
		}
	}
	if s.isStopping() {
		s.startCh <- camStarted{e: e, err: fmt.Errorf("the supervisor is stopping")}
		return
	}
	spec, err := s.cameraSpec(e, a.Input)
	if err != nil {
		s.startCh <- camStarted{e: e, err: err}
		return
	}
	child, err := ffmpeg.Start(context.Background(), spec)
	if err != nil {
		s.startCh <- camStarted{e: e, err: err}
		return
	}
	s.mu.Lock()
	s.registry[e] = child
	stopping := s.stopping
	s.mu.Unlock()
	if stopping {
		_ = child.Kill()
	}
	s.startCh <- camStarted{e: e, child: child}
	go func() {
		info := child.Wait()
		s.exitCh <- camExited{e: e, info: info} // before the registry entry goes: the teardown waits for both
		s.mu.Lock()
		if s.registry[e] == child {
			delete(s.registry, e)
		}
		s.mu.Unlock()
	}()
}

func (s *Supervisor) onStarted(r camStarted) {
	if r.err != nil {
		s.m.StartFailed(r.e, r.err)
		return
	}
	s.m.Started(r.e, int64(r.child.Pid()))
}

func (s *Supervisor) onExited(x camExited) { s.m.Exited(x.e, x.info) }

// cameraSpec builds the ffmpeg child of one start: the relay URL, or the direct input through an
// owner-only list file so the password is never on the command line (SP1b).
func (s *Supervisor) cameraSpec(e *Entry, input Input) (ffmpeg.Spec, error) {
	cfg := s.cfg
	if err := os.MkdirAll(e.Dir(), 0o755); err != nil {
		return ffmpeg.Spec{}, err
	}
	in := ffmpeg.Inputs{
		RtspTransport: cfg.RtspTransport, TimeoutMicroseconds: cfg.TimeoutMicroseconds, SegmentSeconds: cfg.SegmentSeconds,
		Profile: s.profile, OutputPattern: ffmpeg.SegmentPattern(cfg.RootPath, e.SafeName(), s.profile.Extension),
	}
	var args []string
	listCleanup := func() {}
	var err error
	if input == InputRelay {
		in.URL = ffmpeg.RelayURL(cfg.RelayBase, e.RelayPath())
		args, err = ffmpeg.BuildArgs(in)
	} else {
		cam := e.Camera()
		user, pass, cerr := credentials{s}.CameraCredential(cam)
		if cerr != nil {
			return ffmpeg.Spec{}, cerr
		}
		url := relay.SourceURL(cam, cam.Stream(), user, pass)
		s.register(url)
		var list string
		list, listCleanup, err = ffmpeg.WriteInputList(s.d.Platform, filepath.Join(cfg.RootPath, "_state", "inputs"), url, cfg.TimeoutMicroseconds, cfg.RtspTransport)
		if err != nil {
			return ffmpeg.Spec{}, err
		}
		args, err = ffmpeg.BuildArgsDirect(in, list)
	}
	if err != nil {
		listCleanup()
		return ffmpeg.Spec{}, err
	}
	safe := e.SafeName()
	return ffmpeg.Spec{
		Name: e.Name(), Exe: cfg.FfmpegPath, Args: args, Starter: s.starter, Redactor: s.d.Redactor,
		Log:   s.d.ChildLog("ffmpeg:"+safe, filepath.Join(cfg.RootPath, "_logs", safe+".log")),
		Clock: s.clk,
		Cleanup: func() {
			listCleanup()
			s.removeEmpty(e)
		},
	}, nil
}

// removeEmpty is the empty-segment cleanup after every exit and kill (IR 3.8), behind the root gate
// and, when a sandbox is in force, only inside it.
func (s *Supervisor) removeEmpty(e *Entry) {
	root := s.cfg.RootPath
	if s.sandboxRefuses(root) {
		return
	}
	res := ffmpeg.RemoveEmptySegment(e.Dir(), s.profile.Extension, ffmpeg.EmptyOptions{
		Root: root, Matcher: archive.MatcherFor(s.d.Platform),
		RootRefusal: func() string { return archive.VerifyRoot(root, s.d.Platform, s.profile.Extension, false) },
	})
	for _, m := range res.Messages {
		s.log.Info(m, logx.Action("segment.cleanup"), logx.Target(e.Name()))
	}
}
