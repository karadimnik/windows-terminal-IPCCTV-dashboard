package supervise

import (
	"fmt"
	"time"

	"holocron/internal/archive"
	"holocron/internal/clock"
	"holocron/internal/logx"
)

// Refresh takes a directory scan of every recording camera (the PowerShell status-tick scan,
// R:3936) and records when the open segment was first seen (archive.Tracker). Plan and Down use the
// stored scan until the next Refresh. A failed scan keeps the previous one.
func (m *Manager) Refresh() {
	nowWall, _ := m.now()
	for _, e := range m.sorted() {
		s, err := m.d.Scanner.Scan(e.dir, nowWall)
		if err != nil {
			m.log.Debug("scan failed, keeping the previous one", logx.Target(e.cam.Name), logx.Err(err))
			continue
		}
		first := m.tracker.Observe(s)
		e.scan, e.hasScan = s, true
		e.openFirstSeen = time.Time{}
		if s.Recent {
			e.openFirstSeen = first
		}
		m.log.Debug("scan", logx.Target(e.cam.Name), "files", s.Files, "bytes", s.Bytes, "newest", s.Newest, "recent", s.Recent)
	}
}

// fileProgress is the latest file progress of the last scan: the newest closed write or the
// first-seen time of the open (recent) segment; zero when there is none.
func (e *Entry) fileProgress() time.Time {
	if !e.hasScan {
		return time.Time{}
	}
	p := e.scan.NewestClosedWrite
	if e.openFirstSeen.After(p) {
		p = e.openFirstSeen
	}
	return p
}

// stalled is Test-CamChildStalled (R:1871-1914, caller R:3981-4010): age = now - max(StartedAt,
// newest closed write, open-segment first-seen), stalled iff age > SegmentSeconds x StallFactor.
// The StartedAt part is measured on the monotonic clock, file times on the wall clock, and time
// the supervisor was blind (not ticked) is not counted.
func (m *Manager) stalled(e *Entry, nowWall time.Time, nowMono clock.Mono) (age, thr float64, ok bool) {
	thr = m.s.SegmentSeconds * m.s.StallFactor
	d := nowMono.Sub(e.startedMono) - m.blindMono(e.startedMono, nowMono)
	if p := e.fileProgress(); !p.IsZero() {
		if f := nowWall.Sub(p) - m.blindWall(p, nowWall); f < d {
			d = f
		}
	}
	age = d.Seconds()
	return age, thr, age > thr
}

// tick records a blind interval when Plan was not called for more than BlindGapFactor x Tick on
// either clock (a suspended or hidden OS, a wall-clock step): deadlines measured across it are
// extended by it, so it cannot fire the watchdog or alert 110.
func (m *Manager) tick(nowWall time.Time, nowMono clock.Mono) {
	if m.ticked {
		gap := max(nowMono.Sub(m.lastTickMono), nowWall.Sub(m.lastTickWall))
		if gap > time.Duration(m.s.BlindGapFactor*float64(m.d.Tick)) {
			m.blind = append(m.blind, blindInterval{m.lastTickWall, nowWall, m.lastTickMono, nowMono})
			if len(m.blind) > m.s.BlindIntervalsCap {
				m.blind = m.blind[len(m.blind)-m.s.BlindIntervalsCap:]
			}
			m.log.Warn(fmt.Sprintf("the supervisor was not ticked for %v; stall and camera-down deadlines are extended by it", gap.Round(time.Millisecond)))
		}
	}
	m.ticked, m.lastTickWall, m.lastTickMono = true, nowWall, nowMono
}

func (m *Manager) blindWall(from, to time.Time) time.Duration {
	var sum time.Duration
	for _, b := range m.blind {
		lo, hi := b.fromWall, b.toWall
		if from.After(lo) {
			lo = from
		}
		if to.Before(hi) {
			hi = to
		}
		if hi.After(lo) {
			sum += hi.Sub(lo)
		}
	}
	return sum
}

func (m *Manager) blindMono(from, to clock.Mono) time.Duration {
	var sum time.Duration
	for _, b := range m.blind {
		lo, hi := max(b.fromMono, from), min(b.toMono, to)
		if hi > lo {
			sum += hi.Sub(lo)
		}
	}
	return sum
}

// ArchiveScanner is the production Scanner: archive's Scan with the configured container.
type ArchiveScanner struct {
	Matcher        archive.Matcher
	Container      string
	SegmentSeconds int
}

func (a ArchiveScanner) Scan(dir string, now time.Time) (archive.DirScan, error) {
	return a.Matcher.Scan(dir, a.Container, now, a.SegmentSeconds)
}
