package logx

import (
	"context"
	"log/slog"
	"time"
)

// shared is the state common to every component logger of one Logger.
type shared struct {
	ctl   *LevelController
	sink  Sink
	audit Sink // may be nil
	json  bool
	now   func() time.Time // nil: use the record time
	loc   *time.Location
}

type op struct {
	group string
	attrs []slog.Attr
}

// core formats records and writes them to the sinks. It performs no redaction itself: it is always
// wrapped (see Logger.For) by redactHandler, and the sinks by redactSink.
type core struct {
	s         *shared
	component string
	ops       []op
}

// Enabled is permissive when an audit sink exists: whether a record is audit=true is known only once
// its attributes are read, and an audit record bypasses the level filter. Handle filters the main sink.
func (h core) Enabled(_ context.Context, l slog.Level) bool {
	return h.s.audit != nil || l >= h.s.ctl.Level(h.component)
}

func (h core) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	h.ops = append(h.ops[:len(h.ops):len(h.ops)], op{attrs: attrs})
	return h
}

func (h core) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	h.ops = append(h.ops[:len(h.ops):len(h.ops)], op{group: name})
	return h
}

func (h core) Handle(_ context.Context, rec slog.Record) (err error) {
	defer func() { _ = recover() }()
	root := &node{}
	cur := root
	depth := 0
	audit := false
	addAttr := func(a slog.Attr) {
		if depth == 0 && a.Key == "audit" && a.Value.Kind() == slog.KindBool && a.Value.Bool() {
			audit = true
		}
		cur.add(a)
	}
	for _, o := range h.ops {
		if o.group != "" {
			child := &node{}
			cur.entries = append(cur.entries, entry{key: o.group, child: child})
			cur = child
			depth++
			continue
		}
		for _, a := range o.attrs {
			addAttr(a)
		}
	}
	rec.Attrs(func(a slog.Attr) bool { addAttr(a); return true })

	mainOn := rec.Level >= h.s.ctl.Level(h.component)
	if !mainOn && !(audit && h.s.audit != nil) {
		return nil
	}

	t := rec.Time
	if h.s.now != nil {
		t = h.s.now()
	} else if t.IsZero() {
		t = time.Now()
	}
	line := []byte(formatLine(h.s.json, t.In(h.s.loc), rec.Level, h.component, rec.Message, root))
	if mainOn {
		_, _ = h.s.sink.Write(line)
	}
	if audit && h.s.audit != nil {
		_, _ = h.s.audit.Write(line)
	}
	return nil
}
