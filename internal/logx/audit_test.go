package logx

import (
	"strings"
	"testing"
)

func TestAuditRouting(t *testing.T) {
	main, audit := NewMemorySink(), NewMemorySink()
	l, _ := New(Options{Sink: main, Audit: audit, Getenv: func(string) string { return "" }})
	log := l.For("auth")
	log.Info("plain event")
	log.Info("user added", Action("user.add"), Audit())
	log.With(Audit()).Info("via with")
	log.WithGroup("g").Info("grouped audit is not top-level", "audit", true)
	log.Info("audit false", "audit", false)
	log.Debug("below level", Audit())

	if got := len(main.Lines()); got != 5 {
		t.Errorf("main sink has %d lines, want 5: %v", got, main.Lines())
	}
	got := audit.Lines()
	if len(got) != 2 || !strings.Contains(got[0], "user added") || !strings.Contains(got[1], "via with") {
		t.Errorf("audit sink = %q, want exactly the two audit=true records", got)
	}
	if !strings.Contains(got[0], "audit=true") {
		t.Errorf("audit line lacks audit=true: %s", got[0])
	}
}

func TestAuditHasOwnRotation(t *testing.T) {
	dir := t.TempDir()
	mainF := NewRotatingFile(FileOptions{Path: dir + "/holocron.log", MaxBytes: 1 << 20})
	auditF := NewRotatingFile(FileOptions{Path: dir + "/audit.log", MaxBytes: 120})
	l, _ := New(Options{Sink: mainF, Audit: auditF, Getenv: func(string) string { return "" }})
	for i := 0; i < 10; i++ {
		l.For("auth").Info("login", Audit())
	}
	if _, err := fileSize(dir + "/audit.log.1"); err != nil {
		t.Error("audit log should have rotated at its own limit")
	}
	if _, err := fileSize(dir + "/holocron.log.1"); err == nil {
		t.Error("main log must not rotate at the audit limit")
	}
}
