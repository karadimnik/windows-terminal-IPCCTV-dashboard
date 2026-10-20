package supervise

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"holocron/internal/config"
	"holocron/internal/logx"
	"holocron/internal/status"
)

// Control is the supervisor's local control endpoint (ctlapi.Server). The supervisor calls Start
// once, after its first successful status write, and Stop first in the ordered stop: Stop must not
// wait for requests in flight (a stop request is one of them and waits for this very stop).
type Control interface {
	Start() error
	Stop()
}

// ErrNotRunning: the supervisor is not running (not started yet, or stopping).
var ErrNotRunning = errors.New("the supervisor is not running")

// Snapshot is the running document as the next heartbeat would write it; nil while the
// supervisor is not running.
func (s *Supervisor) Snapshot() *status.Document {
	select {
	case <-s.running:
	default:
		return nil
	}
	if s.isStopping() {
		return nil
	}
	return s.document(status.StateRunning, status.StopKindNone, nil)
}

// Reload re-reads the configuration and the camera list now (instead of within ReconcileSeconds):
// LiveViewEnabled is applied at once (the live MediaMTX starts or stops); every other changed
// configuration value still needs a restart, as at a regular reconcile. It returns when the
// reconcile has run.
func (s *Supervisor) Reload(ctx context.Context) error {
	select {
	case <-s.running:
	default:
		return ErrNotRunning
	}
	if s.isStopping() {
		return ErrNotRunning
	}
	cfg, err := s.d.Source.Config()
	if err != nil {
		return fmt.Errorf("holocron.config.json cannot be read: %w", err)
	}
	s.setLive(cfg.LiveViewEnabled)
	done := make(chan struct{})
	select {
	case s.reloadCh <- done:
	case <-s.stopCh:
		return ErrNotRunning
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-s.stopCh:
		return ErrNotRunning
	case <-ctx.Done():
		return ctx.Err()
	}
}

// setLive starts or stops the live-view MediaMTX at run time. The live child keeps the other Live*
// values the supervisor started with.
func (s *Supervisor) setLive(on bool) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	s.mu.Lock()
	cur, stopping := s.liveM, s.stopping
	s.mu.Unlock()
	switch {
	case stopping:
	case on && cur == nil:
		m := &mtxChild{s: s, name: "liveview", label: "live view", live: true, log: s.llog, logPath: filepath.Join(s.cfg.RootPath, "_logs", "liveview.log")}
		s.mu.Lock()
		s.liveM = m
		s.mu.Unlock()
		m.start()
		s.llog.Info("live view switched on", logx.Action("live.enable"), logx.Result(logx.ResultOK))
	case !on && cur != nil:
		s.mu.Lock()
		s.liveM = nil
		s.mu.Unlock()
		cur.stop()
		select {
		case <-cur.done:
			s.llog.Info("live view switched off", logx.Action("live.disable"), logx.Result(logx.ResultOK))
		case <-s.clk.After(15 * time.Second):
			s.llog.Error("live view: its supervisor did not end within 15 s of the switch-off", logx.Action("live.disable"), logx.Result(logx.ResultFail))
		}
	}
}

// startControl starts the control endpoint after the first successful status write.
func (s *Supervisor) startControl() {
	if s.d.Control == nil {
		return
	}
	select {
	case <-s.firstWrite:
	case <-s.stopCh:
		return
	}
	s.ctlMu.Lock()
	defer s.ctlMu.Unlock()
	if s.ctlStopped {
		return
	}
	if err := s.d.Control.Start(); err != nil {
		s.log.Warn("the control endpoint did not start: "+err.Error()+". Recording goes on; holocron down falls back to the stop-request file.",
			logx.Action("ctlapi.start"), logx.Result(logx.ResultFail), logx.Err(err))
		s.ctlStopped = true
		return
	}
	s.ctlStarted = true
}

// stopControl is the first step of the ordered stop.
func (s *Supervisor) stopControl() {
	s.ctlMu.Lock()
	defer s.ctlMu.Unlock()
	if s.ctlStarted {
		s.d.Control.Stop()
	}
	s.ctlStarted, s.ctlStopped = false, true
}

// sameConfigIgnoringLive is sameConfig without LiveViewEnabled, which Reload applies at run time.
func sameConfigIgnoringLive(a, b config.Config) bool {
	a.LiveViewEnabled = b.LiveViewEnabled
	return sameConfig(a, b)
}
