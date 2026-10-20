package web

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadStatusFileBehaviourIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	api := NewAPI(Config{StatusFile: filepath.Join(dir, "status.json")}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, _, err := api.readStatus(); err == nil || !strings.Contains(err.Error(), "status file not readable") {
		t.Errorf("missing file: %v", err)
	}
	write := func(s string) {
		if err := os.WriteFile(api.cfg.StatusFile, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("")
	if _, info, err := api.readStatus(); err == nil || !strings.Contains(err.Error(), "parse status file") || !info.Present {
		t.Errorf("empty file: %v", err)
	}
	write(`{"State": "runn`)
	if _, _, err := api.readStatus(); err == nil || !strings.Contains(err.Error(), "parse status file") {
		t.Errorf("invalid file: %v", err)
	}
	write("\xef\xbb\xbf" + `{"State": "running", "SupervisorPid": 7}`)
	got, _, err := api.readStatus()
	if err != nil || got["State"] != "running" {
		t.Errorf("BOM file: %v %v", got, err)
	}
}

func TestReadStatusFileReturnsTheBytes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "status.json")
	if err := os.WriteFile(p, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if raw, err := readStatusFile(p); err != nil || string(raw) != "abc" {
		t.Errorf("%q %v", raw, err)
	}
	if _, err := readStatusFile(p + ".missing"); !os.IsNotExist(err) {
		t.Errorf("missing: %v", err)
	}
}
