package logx

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// node is the attribute tree of one record; groups nest.
type node struct{ entries []entry }

type entry struct {
	key   string
	val   slog.Value
	child *node
}

func (n *node) add(a slog.Attr) {
	a.Value = a.Value.Resolve()
	if isEmptyAttr(a) {
		return
	}
	if a.Value.Kind() != slog.KindGroup {
		n.entries = append(n.entries, entry{key: a.Key, val: a.Value})
		return
	}
	if a.Key == "" { // an inline group
		for _, g := range a.Value.Group() {
			n.add(g)
		}
		return
	}
	child := &node{}
	for _, g := range a.Value.Group() {
		child.add(g)
	}
	if !child.empty() {
		n.entries = append(n.entries, entry{key: a.Key, child: child})
	}
}

func (n *node) empty() bool {
	for _, e := range n.entries {
		if e.child == nil || !e.child.empty() {
			return false
		}
	}
	return true
}

func needsQuote(s string) bool {
	if s == "" || !utf8.ValidString(s) {
		return true
	}
	for _, r := range s {
		if r <= ' ' || r == '"' || r == '=' || !unicode.IsPrint(r) {
			return true
		}
	}
	return false
}

func textString(s string) string {
	if needsQuote(s) {
		return strconv.Quote(s)
	}
	return s
}

// msgString keeps spaces in the message readable and quotes it only when it holds control characters.
func msgString(s string) string {
	if !utf8.ValidString(s) {
		return strconv.Quote(s)
	}
	for _, r := range s {
		if r != ' ' && (r < ' ' || !unicode.IsPrint(r)) {
			return strconv.Quote(s)
		}
	}
	return s
}

func textValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return textString(v.String())
	case slog.KindInt64:
		return strconv.FormatInt(v.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(v.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(v.Float64(), 'g', -1, 64)
	case slog.KindBool:
		return strconv.FormatBool(v.Bool())
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().Format(time.RFC3339Nano)
	}
	return textString(fmt.Sprint(v.Any()))
}

func (n *node) writeText(sb *strings.Builder, prefix string) {
	for _, e := range n.entries {
		if e.child != nil {
			e.child.writeText(sb, prefix+e.key+".")
			continue
		}
		sb.WriteByte(' ')
		sb.WriteString(textString(prefix + e.key))
		sb.WriteByte('=')
		sb.WriteString(textValue(e.val))
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func jsonValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return jsonString(v.String())
	case slog.KindInt64:
		return strconv.FormatInt(v.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(v.Uint64(), 10)
	case slog.KindFloat64:
		f := v.Float64()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return jsonString(strconv.FormatFloat(f, 'g', -1, 64))
		}
		return strconv.FormatFloat(f, 'g', -1, 64)
	case slog.KindBool:
		return strconv.FormatBool(v.Bool())
	case slog.KindDuration:
		return jsonString(v.Duration().String())
	case slog.KindTime:
		return jsonString(v.Time().Format(time.RFC3339Nano))
	}
	if b, err := json.Marshal(v.Any()); err == nil {
		return string(b)
	}
	return jsonString(fmt.Sprint(v.Any()))
}

func (n *node) writeJSON(sb *strings.Builder) {
	sb.WriteByte('{')
	first := true
	for _, e := range n.entries {
		if e.child != nil && e.child.empty() {
			continue
		}
		if !first {
			sb.WriteByte(',')
		}
		first = false
		sb.WriteString(jsonString(e.key))
		sb.WriteByte(':')
		if e.child != nil {
			e.child.writeJSON(sb)
		} else {
			sb.WriteString(jsonValue(e.val))
		}
	}
	sb.WriteByte('}')
}

// formatLine renders one record. Text: `TIME LEVEL  [component] msg key=value ...`; JSON: one object
// with time, level, component, msg first and then the attributes.
func formatLine(asJSON bool, t time.Time, level slog.Level, component, msg string, attrs *node) string {
	var sb strings.Builder
	ts := t.Format(timeLayout)
	if asJSON {
		sb.WriteString(`{"time":` + jsonString(ts) + `,"level":` + jsonString(LevelName(level)))
		if component != "" {
			sb.WriteString(`,"component":` + jsonString(component))
		}
		sb.WriteString(`,"msg":` + jsonString(msg))
		var body strings.Builder
		attrs.writeJSON(&body)
		if b := body.String(); b != "{}" {
			sb.WriteByte(',')
			sb.WriteString(b[1:])
		} else {
			sb.WriteByte('}')
		}
		sb.WriteByte('\n')
		return sb.String()
	}
	sb.WriteString(ts)
	sb.WriteByte(' ')
	sb.WriteString(fmt.Sprintf("%-5s", LevelName(level)))
	if component != "" {
		sb.WriteString(" [" + component + "]")
	}
	sb.WriteByte(' ')
	sb.WriteString(msgString(msg))
	attrs.writeText(&sb, "")
	sb.WriteByte('\n')
	return sb.String()
}
