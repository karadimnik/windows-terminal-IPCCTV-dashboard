package supervise

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"holocron/internal/clock"
	"holocron/internal/logx"
	"holocron/internal/status"
)

// eventRingSize bounds the ring; StatusEventCount (default 20) is taken from its newest end.
const eventRingSize = 256

// logTimeLayout is logx's record time format.
const logTimeLayout = "2006-01-02T15:04:05.000000-07:00"

// EventRing is a logx.Sink that keeps the newest INFO-and-above records of the supervisor's own
// components as status.json Events (the PowerShell product published the tail of recorder.log).
// Wire it into the supervisor process's logx.Logger (logx.FanOut with the file sink): logx has
// already redacted every line before a sink sees it. Child output (components ffmpeg:*) is not kept.
// Both the text and the JSON record formats are understood. Safe for concurrent use.
type EventRing struct {
	mu   sync.Mutex
	buf  []status.Event
	next int
	full bool
}

// NewEventRing returns an empty ring.
func NewEventRing() *EventRing { return &EventRing{buf: make([]status.Event, eventRingSize)} }

// attrTailRE matches one trailing " key=value" of a text record (value bare or Go-quoted).
var attrTailRE = regexp.MustCompile(`\s[A-Za-z_][A-Za-z0-9_.\-]*=(?:"(?:[^"\\]|\\.)*"|[^\s"]*)$`)

// Write takes one or more formatted records. It never fails.
func (r *EventRing) Write(p []byte) (int, error) {
	for _, line := range strings.Split(string(p), "\n") {
		if ev, ok := parseRecord(strings.TrimRight(line, "\r")); ok {
			r.add(ev)
		}
	}
	return len(p), nil
}

// Close implements logx.Sink.
func (r *EventRing) Close() error { return nil }

func (r *EventRing) add(ev status.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = ev
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
}

// Newest returns up to n events, newest first; an empty (non-nil) slice when n <= 0.
func (r *EventRing) Newest(n int) []status.Event {
	out := []status.Event{}
	if r == nil || n <= 0 {
		return out
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	count := r.next
	if r.full {
		count = len(r.buf)
	}
	for i := 0; i < count && i < n; i++ {
		ev := r.buf[(r.next-1-i+len(r.buf))%len(r.buf)]
		if ev.At != nil {
			at := *ev.At
			ev.At = &at
		}
		out = append(out, ev)
	}
	return out
}

// parseRecord turns a logx record line into an Event: At is the log stamp "yyyy-MM-dd HH:mm:ss
// +03:00" (IS 1.3), Text the message without the attributes. Records below INFO and child output
// are dropped.
func parseRecord(line string) (status.Event, bool) {
	if strings.TrimSpace(line) == "" {
		return status.Event{}, false
	}
	var ts, level, component, msg string
	if strings.HasPrefix(line, "{") {
		var rec struct {
			Time      string `json:"time"`
			Level     string `json:"level"`
			Component string `json:"component"`
			Msg       string `json:"msg"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil {
			return status.Event{}, false
		}
		ts, level, component, msg = rec.Time, rec.Level, rec.Component, rec.Msg
	} else {
		sp := strings.IndexByte(line, ' ')
		if sp < 0 || len(line) < sp+7 {
			return status.Event{Text: line}, true // a notice or foreign line: no stamp
		}
		ts = line[:sp]
		level = strings.TrimSpace(line[sp+1 : sp+6])
		rest := line[sp+7:]
		if strings.HasPrefix(rest, "[") {
			if end := strings.Index(rest, "] "); end > 0 {
				component, rest = rest[1:end], rest[end+2:]
			} else if strings.HasSuffix(rest, "]") {
				component, rest = rest[1:len(rest)-1], ""
			}
		}
		for {
			loc := attrTailRE.FindStringIndex(rest)
			if loc == nil {
				break
			}
			rest = rest[:loc[0]]
		}
		msg = rest
		if strings.HasPrefix(msg, `"`) {
			if uq, err := strconv.Unquote(msg); err == nil {
				msg = uq
			}
		}
	}
	lv, err := logx.ParseLevel(level)
	if err != nil {
		return status.Event{Text: line}, true
	}
	if lv < logx.LevelInfo || strings.HasPrefix(strings.ToLower(component), "ffmpeg") {
		return status.Event{}, false
	}
	ev := status.Event{Text: msg}
	if t, err := time.Parse(logTimeLayout, ts); err == nil {
		at := clock.FormatLogStamp(t)
		ev.At = &at
	} else if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		at := clock.FormatLogStamp(t)
		ev.At = &at
	}
	return ev, true
}
