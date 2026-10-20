package supervise

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"holocron/internal/archive"
	"holocron/internal/clock"
	"holocron/internal/config"
	"holocron/internal/osx"
)

var t0 = time.Date(2026, 10, 17, 8, 30, 1, 0, time.UTC)

type fakeScanner struct{ scans map[string]archive.DirScan }

func (f *fakeScanner) Scan(dir string, now time.Time) (archive.DirScan, error) {
	s, ok := f.scans[dir]
	if !ok {
		return archive.DirScan{Dir: dir, ScannedAt: now}, nil
	}
	s.Dir, s.ScannedAt = dir, now
	return s, nil
}

type fakeAlerts struct{ events []Event }

func (f *fakeAlerts) Raise(e Event) { f.events = append(f.events, e) }

type fakeReady map[string]bool

func (f fakeReady) Has(p string) bool { return f[p] }

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type rig struct {
	t      *testing.T
	clk    *clock.Fake
	scan   *fakeScanner
	alerts *fakeAlerts
	logs   *syncBuf
	m      *Manager
	s      Settings
	nextID int64
}

func defaultSettings() Settings {
	s := SettingsFrom(config.Defaults(osx.Current()))
	s.RootPath = "/arch"
	return s
}

func newRig(t *testing.T, s Settings) *rig {
	t.Helper()
	r := &rig{t: t, clk: clock.NewFake(t0), scan: &fakeScanner{scans: map[string]archive.DirScan{}}, alerts: &fakeAlerts{}, logs: &syncBuf{}, s: s, nextID: 100}
	log := slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})).With("component", "supervisor")
	r.m = New(s, Deps{Clock: r.clk, Scanner: r.scan, Alerts: r.alerts, Log: log})
	return r
}

func cam(name string, record bool) config.Camera {
	return config.Camera{Name: name, IP: "192.0.2.10", Quality: "stream1", Record: record}
}

func (r *rig) dir(safe string) string { return filepath.Join(r.s.RootPath, safe) }

type cameraT = config.Camera

func newRigAt(t *testing.T, s Settings, start time.Time) *rig {
	t.Helper()
	r := newRig(t, s)
	r.clk = clock.NewFake(start)
	r.m = New(s, Deps{Clock: r.clk, Scanner: r.scan, Alerts: r.alerts, Log: r.m.log})
	return r
}
