package logx

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
)

// LevelTrace is the custom level below DEBUG (raw detail: ffmpeg lines, status documents).
const LevelTrace slog.Level = -8

// The five levels, highest severity first.
const (
	LevelError = slog.LevelError
	LevelWarn  = slog.LevelWarn
	LevelInfo  = slog.LevelInfo
	LevelDebug = slog.LevelDebug
)

// ParseLevel parses ERROR, WARN, INFO, DEBUG or TRACE, case-insensitively.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "ERROR":
		return LevelError, nil
	case "WARN":
		return LevelWarn, nil
	case "INFO":
		return LevelInfo, nil
	case "DEBUG":
		return LevelDebug, nil
	case "TRACE":
		return LevelTrace, nil
	}
	return 0, fmt.Errorf("unknown log level %q (want error, warn, info, debug or trace)", s)
}

// LevelName prints a level as one of the five names; levels between names print as the next lower name.
func LevelName(l slog.Level) string {
	switch {
	case l >= LevelError:
		return "ERROR"
	case l >= LevelWarn:
		return "WARN"
	case l >= LevelInfo:
		return "INFO"
	case l >= LevelDebug:
		return "DEBUG"
	}
	return "TRACE"
}

// LevelSnapshot is a copy of the controller state.
type LevelSnapshot struct {
	Default    slog.Level
	Components map[string]slog.Level
}

// LevelController holds the default level and the per-component overrides. It is safe for
// concurrent use, and every logger made by Logger.For consults it on each call, so a change takes
// effect immediately on existing loggers. Component names are case-insensitive. A component
// "ffmpeg:cam1" with no override of its own falls back to an override named "ffmpeg".
type LevelController struct {
	mu    sync.RWMutex
	def   slog.Level
	comps map[string]slog.Level
}

// NewLevelController returns a controller with the given default level.
func NewLevelController(def slog.Level) *LevelController {
	return &LevelController{def: def, comps: map[string]slog.Level{}}
}

func normComponent(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// SetDefault changes the default level.
func (c *LevelController) SetDefault(l slog.Level) {
	c.mu.Lock()
	c.def = l
	c.mu.Unlock()
}

// SetComponent sets the override for one component.
func (c *LevelController) SetComponent(name string, l slog.Level) {
	c.mu.Lock()
	c.comps[normComponent(name)] = l
	c.mu.Unlock()
}

// Clear removes the override of one component.
func (c *LevelController) Clear(name string) {
	c.mu.Lock()
	delete(c.comps, normComponent(name))
	c.mu.Unlock()
}

// Level is the effective level of a component: its override if present, else the default.
func (c *LevelController) Level(component string) slog.Level {
	name := normComponent(component)
	c.mu.RLock()
	defer c.mu.RUnlock()
	if l, ok := c.comps[name]; ok {
		return l
	}
	if i := strings.IndexByte(name, ':'); i > 0 {
		if l, ok := c.comps[name[:i]]; ok {
			return l
		}
	}
	return c.def
}

// Snapshot returns a copy of the current state.
func (c *LevelController) Snapshot() LevelSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := LevelSnapshot{Default: c.def, Components: make(map[string]slog.Level, len(c.comps))}
	for k, v := range c.comps {
		s.Components[k] = v
	}
	return s
}

// String renders the snapshot as "info supervisor=debug web=info" (components sorted).
func (s LevelSnapshot) String() string {
	names := make([]string, 0, len(s.Components))
	for n := range s.Components {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := []string{strings.ToLower(LevelName(s.Default))}
	for _, n := range names {
		parts = append(parts, n+"="+strings.ToLower(LevelName(s.Components[n])))
	}
	return strings.Join(parts, " ")
}

// ParseSpec parses the HOLOCRON_LOG form: "debug" or "supervisor=debug,web=info". A bare level is
// the default, name=level an override. Invalid entries are returned in errs and skipped.
func ParseSpec(spec string) (def *slog.Level, comps map[string]slog.Level, errs []error) {
	comps = map[string]slog.Level{}
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, lvl, found := strings.Cut(entry, "=")
		if !found {
			l, err := ParseLevel(entry)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			def = &l
			continue
		}
		name = normComponent(name)
		l, err := ParseLevel(lvl)
		if name == "" {
			err = fmt.Errorf("log level entry %q has no component name", entry)
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		comps[name] = l
	}
	return def, comps, errs
}
