package app

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"holocron/internal/config"
	"holocron/internal/logx"
)

// LogOptions shape one process's logger.
type LogOptions struct {
	Paths   Paths
	Config  config.Config
	Getenv  func(string) string // HOLOCRON_LOG; default os.Getenv
	Verbose int                 // 1: DEBUG, 2: TRACE for this invocation, mirrored to Stderr
	Stderr  io.Writer           // mirror (attached mode, or -v); nil: none
	Extra   []logx.Sink         // more sinks (the supervisor's status Events ring)
	NoFile  bool                // no file sinks (commands that must not touch a root)
	JSON    bool
}

// NewLogger builds the process logger: <root>/_logs/holocron.log and audit.log (rotating, never
// failing the caller), stderr when asked, levels from the configuration then HOLOCRON_LOG, and -v
// / -vv on top for this invocation.
func NewLogger(o LogOptions) (*logx.Logger, []error) {
	var sinks []logx.Sink
	var audit logx.Sink
	if !o.NoFile && o.Paths.Root != "" {
		max := int64(o.Config.LogMaxBytes)
		sinks = append(sinks, logx.NewRotatingFile(logx.FileOptions{Path: filepath.Join(o.Paths.Logs(), "holocron.log"), MaxBytes: max, JSON: o.JSON, NoticeOut: os.Stderr}))
		audit = logx.NewRotatingFile(logx.FileOptions{Path: filepath.Join(o.Paths.Logs(), "audit.log"), MaxBytes: 4 * max, JSON: o.JSON, NoticeOut: os.Stderr})
	}
	if o.Stderr != nil {
		sinks = append(sinks, logx.NewWriterSink(o.Stderr))
	}
	sinks = append(sinks, o.Extra...)
	var sink logx.Sink = logx.FanOut(sinks...)
	if len(sinks) == 0 {
		sink = logx.NewMemorySink() // nowhere to write: keep the records in memory (dropped at exit)
	}
	lg, errs := logx.New(logx.Options{
		Config: logx.Config{Level: o.Config.Logging.Level, Levels: o.Config.Logging.Levels},
		Getenv: o.Getenv, JSON: o.JSON, Sink: sink, Audit: audit,
	})
	switch {
	case o.Verbose >= 2:
		forceLevel(lg.Levels(), logx.LevelTrace)
	case o.Verbose == 1:
		forceLevel(lg.Levels(), logx.LevelDebug)
	}
	return lg, errs
}

// forceLevel is -v / -vv: this invocation logs every component at l.
func forceLevel(lc *logx.LevelController, l slog.Level) {
	lc.SetDefault(l)
	for c := range lc.Snapshot().Components {
		lc.Clear(c)
	}
}
