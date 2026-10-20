package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"holocron/internal/cli"
	"holocron/internal/config"
	"holocron/internal/logx"
	"holocron/internal/osx"
)

// EditableKeys is the allow-list of `holocron config set|unset` (the Editable column of
// internal/config/KEYS.md; a test keeps the two equal). "logging.levels.*" stands for one
// component.
var EditableKeys = []string{
	"SegmentSeconds", "ReserveGB", "PruneHysteresisGB", "MaxRetentionDays", "MaxAgePruneFraction",
	"CameraDownAlertMinutes", "CameraDownRepeatHours", "StopTimeoutSeconds",
	"LiveViewEnabled", "LiveUdpAddress", "LiveAdvertiseHosts", "LiveSubStream", "LiveCloseAfterSeconds",
	"logging.level", "logging.levels.*",
	"alerts.queueSize", "alerts.timeoutSeconds", "alerts.minRepeatSeconds", "alerts.retryBackoffSeconds",
	"alerts.webhook.enabled", "alerts.webhook.url", "alerts.webhook.secretName", "alerts.webhook.secretHeader",
	"alerts.push.enabled", "alerts.push.server", "alerts.push.topic", "alerts.push.tokenName",
	"alerts.desktop.enabled", "alerts.eventlog.enabled",
	"secrets.backend",
	"web.enabled",
}

// CanonicalKey matches key against the allow-list (case-insensitive) and returns its spelling;
// a refusal names the editable keys.
func CanonicalKey(key string) (string, error) {
	for _, k := range EditableKeys {
		if strings.EqualFold(k, key) {
			return k, nil
		}
		if strings.HasSuffix(k, ".*") {
			prefix := strings.TrimSuffix(k, "*")
			if len(key) > len(prefix) && strings.EqualFold(key[:len(prefix)], prefix) && !strings.Contains(key[len(prefix):], ".") {
				return prefix + strings.ToLower(key[len(prefix):]), nil
			}
		}
	}
	return "", cli.Refuse("%q is not a key `holocron config set` may change; the editable keys are: %s", key, strings.Join(EditableKeys, ", "))
}

// ParseValue turns a command-line value into JSON: anything that is valid JSON is taken as JSON
// (true, 30, null, [1,2], "text"), anything else as a string.
func ParseValue(s string) json.RawMessage {
	t := strings.TrimSpace(s)
	if t != "" && json.Valid([]byte(t)) {
		return json.RawMessage(t)
	}
	b, _ := json.Marshal(s)
	return b
}

// member and object keep the order of the document's keys.
type member struct {
	Key   string
	Value json.RawMessage
}
type object []member

func parseObject(raw []byte) (object, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var o object
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o = append(o, member{Key: kt.(string), Value: v})
	}
	return o, nil
}

func (o object) find(key string) int {
	for i, m := range o {
		if strings.EqualFold(m.Key, key) {
			return i
		}
	}
	return -1
}

func (o object) compact() []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(m.Key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(m.Value)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// edit sets (value != nil) or removes (value == nil) the dotted path in o.
func (o object) edit(path []string, value json.RawMessage) (object, error) {
	i := o.find(path[0])
	if len(path) == 1 {
		switch {
		case value == nil && i >= 0:
			return append(o[:i:i], o[i+1:]...), nil
		case value == nil:
			return o, nil
		case i >= 0:
			o[i].Value = value
			return o, nil
		}
		return append(o, member{Key: path[0], Value: value}), nil
	}
	var sub object
	if i >= 0 {
		var err error
		if sub, err = parseObject(o[i].Value); err != nil {
			return nil, fmt.Errorf("%s: %w", path[0], err)
		}
	}
	sub, err := sub.edit(path[1:], value)
	if err != nil {
		return nil, err
	}
	if i >= 0 {
		o[i].Value = sub.compact()
		return o, nil
	}
	if value == nil {
		return o, nil
	}
	return append(o, member{Key: path[0], Value: sub.compact()}), nil
}

var bom = []byte("\xef\xbb\xbf")

// ConfigEdit is the outcome of an edit.
type ConfigEdit struct {
	Key      string          `json:"key"`
	Old, New json.RawMessage `json:"-"`
	Changed  bool            `json:"changed"`
}

// EditConfig sets (value != nil) or unsets one allow-listed key in the configuration file, in
// place: every other key (unknown and dropped ones included), their order and the file's BOM stay.
// The edited document is validated with config.Parse first; a refusal leaves the file untouched.
// The write is atomic (temp in the same folder, sync, rename).
func EditConfig(path, key string, value json.RawMessage, plat osx.Platform) (ConfigEdit, error) {
	canon, err := CanonicalKey(key)
	if err != nil {
		return ConfigEdit{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return ConfigEdit{}, err
	}
	hasBOM := bytes.HasPrefix(raw, bom)
	body := bytes.TrimPrefix(raw, bom)
	var doc object
	if len(bytes.TrimSpace(body)) > 0 {
		if doc, err = parseObject(body); err != nil {
			return ConfigEdit{}, fmt.Errorf("%s: %w", path, err)
		}
	}
	before, _ := lookup(body, canon)
	next, err := doc.edit(strings.Split(canon, "."), value)
	if err != nil {
		return ConfigEdit{}, err
	}
	var flat, out bytes.Buffer
	if err := json.Compact(&flat, next.compact()); err != nil {
		return ConfigEdit{}, err
	}
	if err := json.Indent(&out, flat.Bytes(), "", "    "); err != nil {
		return ConfigEdit{}, err
	}
	out.WriteString("\n")
	if _, _, err := config.Parse(out.Bytes(), plat); err != nil {
		return ConfigEdit{}, cli.Refuse("%s=%s: %v", canon, valueText(value), err)
	}
	after, _ := lookup(out.Bytes(), canon)
	ed := ConfigEdit{Key: canon, Old: before, New: after, Changed: !bytes.Equal(compactJSON(before), compactJSON(after))}
	if !ed.Changed {
		return ed, nil
	}
	data := out.Bytes()
	if hasBOM {
		data = append(append([]byte{}, bom...), data...)
	}
	return ed, writeAtomic(path, data)
}

func valueText(v json.RawMessage) string {
	if v == nil {
		return "(unset)"
	}
	return string(v)
}

func compactJSON(v json.RawMessage) []byte {
	if v == nil {
		return nil
	}
	var b bytes.Buffer
	if json.Compact(&b, v) != nil {
		return v
	}
	return b.Bytes()
}

// lookup finds a dotted key (case-insensitive) in a JSON document.
func lookup(doc []byte, key string) (json.RawMessage, bool) {
	cur := json.RawMessage(bytes.TrimPrefix(doc, bom))
	for _, part := range strings.Split(key, ".") {
		o, err := parseObject(cur)
		if err != nil {
			return nil, false
		}
		i := o.find(part)
		if i < 0 {
			return nil, false
		}
		cur = o[i].Value
	}
	return cur, true
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error { tmp.Close(); os.Remove(name); return err }
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	if err := os.Chmod(name, mode); err != nil {
		return fail(err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// EffectiveConfig is the configuration as the supervisor would run it, as a JSON object with the
// file's key names; URLs with a login are masked (secrets never printed).
func EffectiveConfig(cfg config.Config) (map[string]any, error) {
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	b = []byte(logx.NewRedactor().String(string(b)))
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	for _, k := range []string{"Logging", "Alerts", "Secrets", "Web"} {
		if v, ok := m[k]; ok {
			delete(m, k)
			m[strings.ToLower(k)] = v
		}
	}
	return m, nil
}

// EffectiveValue is one dotted key of EffectiveConfig (case-insensitive).
func EffectiveValue(cfg config.Config, key string) (any, error) {
	var cur any
	m, err := EffectiveConfig(cfg)
	if err != nil {
		return nil, err
	}
	cur = m
	for _, part := range strings.Split(key, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, cli.Refuse("%q: no such key", key)
		}
		found := false
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if strings.EqualFold(k, part) {
				cur, found = obj[k], true
				break
			}
		}
		if !found {
			return nil, cli.Refuse("%q: no such key", key)
		}
	}
	return cur, nil
}
