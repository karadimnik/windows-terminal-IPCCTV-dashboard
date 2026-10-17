package logx

import (
	"log/slog"
	"os"
	"sort"
	"time"
)

// EnvVar is the environment variable that overrides the configured levels:
// HOLOCRON_LOG=debug or HOLOCRON_LOG=supervisor=debug,web=info.
const EnvVar = "HOLOCRON_LOG"

// Config is the initial level configuration (logging.level and logging.levels).
type Config struct {
	Level  string            // default level name; empty means INFO
	Levels map[string]string // component to level name
}

// Options configures New.
type Options struct {
	Config Config
	Getenv func(string) string // default os.Getenv; the environment overrides Config
	JSON   bool                // JSON records instead of text

	Sink  Sink // main log; default stderr
	Audit Sink // receives records carrying audit=true, in addition to Sink; may be nil

	Redactor *Redactor        // default: a new one, reachable through Logger.Redactor
	Now      func() time.Time // fake clock for tests; default: the record time
	Location *time.Location   // default time.Local
}

// Logger is the factory of per-component loggers.
type Logger struct {
	s *shared
	r *Redactor
}

// New builds a Logger. Invalid level entries in the configuration or the environment are returned
// in errs (once) and ignored; New never fails.
func New(o Options) (l *Logger, errs []error) {
	ctl := NewLevelController(LevelInfo)

	if o.Config.Level != "" {
		if lv, err := ParseLevel(o.Config.Level); err != nil {
			errs = append(errs, err)
		} else {
			ctl.SetDefault(lv)
		}
	}
	names := make([]string, 0, len(o.Config.Levels))
	for n := range o.Config.Levels {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if lv, err := ParseLevel(o.Config.Levels[n]); err != nil {
			errs = append(errs, err)
		} else {
			ctl.SetComponent(n, lv)
		}
	}

	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	def, comps, envErrs := ParseSpec(getenv(EnvVar))
	errs = append(errs, envErrs...)
	if def != nil {
		ctl.SetDefault(*def)
	}
	for n, lv := range comps {
		ctl.SetComponent(n, lv)
	}

	r := o.Redactor
	if r == nil {
		r = NewRedactor()
	}
	sink := o.Sink
	if sink == nil {
		sink = StderrSink()
	}
	s := &shared{ctl: ctl, sink: redactSink{r, sink}, json: o.JSON, now: o.Now, loc: o.Location}
	if o.Audit != nil {
		s.audit = redactSink{r, o.Audit}
	}
	if s.loc == nil {
		s.loc = time.Local
	}
	return &Logger{s: s, r: r}, errs
}

// For returns the logger of a component (cli, host, supervisor, ffmpeg:<camera>, relay, web, ...).
// Its level is read from the controller on every call, so run-time changes apply immediately.
func (l *Logger) For(component string) *slog.Logger {
	return slog.New(redactHandler{l.r, core{s: l.s, component: component}})
}

// Levels is the run-time level controller.
func (l *Logger) Levels() *LevelController { return l.s.ctl }

// Redactor is the redactor applied to every record; register secrets on it.
func (l *Logger) Redactor() *Redactor { return l.r }

// Close closes the sinks (flushing any pending repeat summary).
func (l *Logger) Close() error {
	err := l.s.sink.Close()
	if l.s.audit != nil {
		if aerr := l.s.audit.Close(); err == nil {
			err = aerr
		}
	}
	return err
}
