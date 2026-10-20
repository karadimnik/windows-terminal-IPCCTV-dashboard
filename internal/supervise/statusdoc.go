package supervise

import (
	"context"
	"math"
	"time"

	"holocron/internal/clock"
	"holocron/internal/logx"
	"holocron/internal/status"
)

const gib = 1 << 30

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.RoundToEven(v*p) / p
}

func (s *Supervisor) ts(t time.Time) status.Time {
	if t.IsZero() {
		return status.Time{}
	}
	return status.NewTime(t.In(s.d.Location))
}

func ptrv[T any](v T) *T { return &v }

// heartbeat writes the running document every StatusIntervalSeconds on its own goroutine
// (status.Heartbeater): it reads only the state the other goroutines publish under s.mu, so a relay
// start, a child start or a prune run never delays it (IS 1.1 defect not reproduced).
func (s *Supervisor) heartbeat(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	defer s.recoverFault("the heartbeat")
	h := status.Heartbeater{
		Writer: s.writer,
		Snapshot: func() *status.Document {
			doc := s.document(status.StateRunning, status.StopKindNone, nil)
			if s.onDoc != nil {
				s.onDoc(doc)
			}
			return doc
		},
		Interval: seconds(s.cfg.StatusIntervalSeconds),
		OnError: func(err error) {
			s.log.Warn("status write failed: "+err.Error(), logx.Action("status.write"), logx.Result(logx.ResultFail), logx.Err(err))
		},
		NewTicker: func(d time.Duration) (<-chan time.Time, func()) { return clockTicker(s.clk, d) },
	}
	h.Run(ctx)
}

// clockTicker is a ticker on the supervisor clock (a fake clock in tests).
func clockTicker(clk clock.Clock, d time.Duration) (<-chan time.Time, func()) {
	ch := make(chan time.Time, 1)
	quit := make(chan struct{})
	go func() {
		for {
			select {
			case t := <-clk.After(d):
				select {
				case ch <- t:
				default:
				}
			case <-quit:
				return
			}
		}
	}()
	return ch, func() { close(quit) }
}

// document assembles status.json from the published state (Write-CamStatusFile, R:2726-3027). Free
// space is measured on every write; the archive figures come from the last archive scan.
func (s *Supervisor) document(state, kind string, unexpected *status.UnexpectedStop) *status.Document {
	now := s.clk.Now()
	cfg := s.cfg
	free, ferr := s.d.Platform.DiskFree(cfg.RootPath)
	uptime := round(s.clk.Since(s.startedMono).Seconds(), 1)
	doc := &status.Document{
		Schema: 1, State: state, StopKind: kind, SupervisorPid: s.d.Pid,
		StartedAt: s.ts(s.startedWall), UpdatedAt: s.ts(now), UptimeSeconds: &uptime,
		RootPath: cfg.RootPath, Container: cfg.Container, SegmentSeconds: cfg.SegmentSeconds,
		Events: s.d.Events.Newest(cfg.StatusEventCount),
	}
	disk := &status.Disk{ReserveGB: cfg.ReserveGB, PruneHysteresisGB: cfg.PruneHysteresisGB}
	if ferr == nil {
		disk.FreeGB = round(float64(free)/gib, 2)
	}
	s.mu.Lock()
	if a := s.archive; a != nil {
		disk.ArchiveGB = ptrv(round(float64(a.Bytes)/gib, 2))
		disk.ArchiveBytes, disk.ArchiveFiles = ptrv(a.Bytes), ptrv(a.Files)
		disk.OldestSegment, disk.NewestSegment, disk.ScannedAt = s.ts(a.Oldest), s.ts(a.Newest), s.ts(a.ScannedAt)
		if !a.Oldest.IsZero() && !a.Newest.IsZero() {
			if span := a.Newest.Sub(a.Oldest).Seconds(); span >= 600 {
				rate := round((float64(a.Bytes)/gib)/(span/86400), 2)
				disk.MeasuredGBPerDay = &rate
				if rate > 0 {
					disk.ProjectedDaysRemaining = ptrv(round(math.Max(disk.FreeGB-cfg.ReserveGB, 0)/rate, 2))
				}
			}
		}
	}
	doc.Disk = disk
	pv := s.pruneView
	doc.Prune = &pv
	doc.Cameras = append([]status.Camera{}, s.rows...)
	doc.Restreamer = &status.Restreamer{Enabled: false, RelayBase: cfg.RelayBase, Paths: []string{}}
	if m := s.relayM; m != nil {
		r := doc.Restreamer
		r.Enabled, r.Alive, r.RestartsTotal = true, m.alive, m.restarts
		if m.alive {
			r.Pid = ptrv(m.pid)
		}
		if m.lastPid != 0 {
			r.LastPid = ptrv(m.lastPid)
		}
		r.StartedAt, r.LastError, r.LastErrorAt = s.ts(m.startedWall), m.lastError, s.ts(m.lastErrorAt)
		if m.paths != nil {
			r.Paths = append([]string{}, m.paths...)
		}
	}
	if m := s.liveM; m != nil {
		l := &status.Live{Enabled: true, Alive: m.alive, RestartsTotal: m.restarts, Paths: []string{}, Cameras: []status.LiveCamera{}}
		if m.alive {
			l.Pid = ptrv(m.pid)
		}
		if m.lastPid != 0 {
			l.LastPid = ptrv(m.lastPid)
		}
		l.StartedAt, l.LastError, l.LastErrorAt = s.ts(m.startedWall), m.lastError, s.ts(m.lastErrorAt)
		if m.paths != nil {
			l.Paths = append([]string{}, m.paths...)
		}
		if m.liveCams != nil {
			l.Cameras = append([]status.LiveCamera{}, m.liveCams...)
		}
		doc.Live = l
	}
	s.mu.Unlock()
	if unexpected != nil {
		doc.LastUnexpectedStop = status.MaybeStop{Present: true, Stop: unexpected}
	}
	doc.CarryFrom(s.prev)
	return doc
}
