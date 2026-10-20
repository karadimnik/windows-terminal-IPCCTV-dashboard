package logx

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

const mask = "***"

var (
	userinfoRE     = regexp.MustCompile(`(?i)([a-z][a-z0-9+.\-]*://)[^/\s@]+@`)
	sensitiveKeyRE = regexp.MustCompile(`(?i)password|passwd|secret|token|authorization|cookie|hash|apikey|api_key`)
)

// Redactor masks secrets. It masks registered secrets (raw, URL-query-escaped and path-escaped
// forms; values shorter than 3 chars are ignored), then scheme://user:pass@ userinfo, and it blanks
// the value of attributes whose key looks sensitive. Safe for concurrent use.
type Redactor struct {
	mu       sync.RWMutex
	secrets  map[string]struct{}
	replacer *strings.Replacer
}

// NewRedactor returns a Redactor with no registered secrets.
func NewRedactor() *Redactor { return &Redactor{secrets: map[string]struct{}{}} }

// Register adds a secret to mask everywhere.
func (r *Redactor) Register(secret string) {
	if len(secret) < 3 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range []string{secret, url.QueryEscape(secret), url.PathEscape(secret)} {
		r.secrets[f] = struct{}{}
	}
	all := make([]string, 0, len(r.secrets))
	for s := range r.secrets {
		all = append(all, s)
	}
	sort.Slice(all, func(i, j int) bool { // longest first so a secret containing another is masked whole
		if len(all[i]) != len(all[j]) {
			return len(all[i]) > len(all[j])
		}
		return all[i] < all[j]
	})
	pairs := make([]string, 0, 2*len(all))
	for _, s := range all {
		pairs = append(pairs, s, mask)
	}
	r.replacer = strings.NewReplacer(pairs...)
}

// String masks registered secrets and URL userinfo in s.
func (r *Redactor) String(s string) string {
	r.mu.RLock()
	rep := r.replacer
	r.mu.RUnlock()
	if rep != nil {
		s = rep.Replace(s)
	}
	return userinfoRE.ReplaceAllString(s, "${1}***:***@")
}

// maxErrBytes bounds the err attribute (after redaction) so a huge error cannot flood the log.
const maxErrBytes = 1024

func truncateErr(s string) string {
	if len(s) <= maxErrBytes {
		return s
	}
	cut := maxErrBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s...(truncated %d bytes)", s[:cut], len(s)-cut)
}

func (r *Redactor) attr(a slog.Attr) slog.Attr {
	out := r.redactAttr(a)
	if out.Key == "err" && out.Value.Kind() == slog.KindString {
		out.Value = slog.StringValue(truncateErr(out.Value.String()))
	}
	return out
}

func (r *Redactor) redactAttr(a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve()
	if isEmptyAttr(a) {
		return a
	}
	if sensitiveKeyRE.MatchString(a.Key) {
		return slog.String(a.Key, mask)
	}
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, r.String(a.Value.String()))
	case slog.KindGroup:
		in := a.Value.Group()
		out := make([]any, len(in))
		for i, g := range in {
			out[i] = r.attr(g)
		}
		return slog.Group(a.Key, out...)
	case slog.KindAny:
		switch v := a.Value.Any().(type) {
		case nil:
		case error:
			return slog.String(a.Key, r.String(v.Error()))
		case fmt.Stringer:
			return slog.String(a.Key, r.String(v.String()))
		default:
			return slog.String(a.Key, r.String(fmt.Sprintf("%+v", v)))
		}
	}
	return a
}

func (r *Redactor) attrs(in []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, len(in))
	for i, a := range in {
		out[i] = r.attr(a)
	}
	return out
}

// redactHandler redacts the message and every attribute before the wrapped handler sees them.
type redactHandler struct {
	r     *Redactor
	inner slog.Handler
}

func (h redactHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h redactHandler) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, h.r.String(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(h.r.attr(a))
		return true
	})
	return h.inner.Handle(ctx, out)
}

func (h redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return redactHandler{h.r, h.inner.WithAttrs(h.r.attrs(attrs))}
}

func (h redactHandler) WithGroup(name string) slog.Handler {
	return redactHandler{h.r, h.inner.WithGroup(name)}
}

// redactSink is the last line of defence: it masks the final formatted line before any sink sees it.
type redactSink struct {
	r     *Redactor
	inner Sink
}

func (s redactSink) Write(p []byte) (int, error) {
	s.inner.Write([]byte(s.r.String(string(p)))) //nolint:errcheck // sinks never fail the caller
	return len(p), nil
}

func (s redactSink) Close() error { return s.inner.Close() }

func isEmptyAttr(a slog.Attr) bool {
	return a.Key == "" && a.Value.Kind() == slog.KindAny && a.Value.Any() == nil
}
