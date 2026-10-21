package autostart

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"time"

	"holocron/internal/osx"
)

// execTimeout bounds one OS tool run (schtasks, systemctl, launchctl, loginctl).
const execTimeout = 30 * time.Second

// RealExec runs the tool with standard input from the null device (a tool that asks a question
// fails instead of waiting) and returns its combined output.
func RealExec(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

// RealEnv is this machine.
func RealEnv() Env {
	e := Env{GOOS: runtime.GOOS, Exec: RealExec, SCM: osx.Services(), SystemRoot: os.Getenv("SystemRoot"), ConfigHome: os.Getenv("XDG_CONFIG_HOME")}
	e.Home, _ = os.UserHomeDir()
	if u, err := user.Current(); err == nil {
		e.User, e.UID = u.Username, u.Uid
	}
	e.TempDirs = []string{os.TempDir()}
	if e.SystemRoot != "" {
		e.TempDirs = append(e.TempDirs, filepath.Join(e.SystemRoot, "Temp"))
	}
	e.TempFile = func(content []byte) (string, func(), error) {
		f, err := os.CreateTemp("", "holocron-task-*.xml")
		if err != nil {
			return "", nil, err
		}
		name := f.Name()
		_, err = f.Write(content)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(name)
			return "", nil, err
		}
		return name, func() { os.Remove(name) }, nil
	}
	return e
}
