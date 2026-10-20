package status

import (
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// A small copy of the redaction ideas of internal/logx/redact.go, kept here so
// the packages stay independent.

const mask = "***"

var userinfoRE = regexp.MustCompile(`(?i)([a-z][a-z0-9+.\-]*://)[^/\s@]+@`)

// Redactor masks registered secrets (raw, URL-escaped and JSON-escaped forms;
// shorter than 3 chars are ignored) and scheme://user:pass@ userinfo. Safe for
// concurrent use.
type Redactor struct {
	mu       sync.RWMutex
	secrets  map[string]struct{}
	replacer *strings.Replacer
}

func NewRedactor() *Redactor { return &Redactor{secrets: map[string]struct{}{}} }

// Register adds a secret to mask everywhere.
func (r *Redactor) Register(secret string) {
	if len(secret) < 3 {
		return
	}
	forms := []string{secret, url.QueryEscape(secret), url.PathEscape(secret)}
	if j, err := json.Marshal(secret); err == nil {
		forms = append(forms, string(j[1:len(j)-1]))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range forms {
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
