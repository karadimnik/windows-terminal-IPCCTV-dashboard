package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"holocron/internal/logx"
	"holocron/internal/osx"
)

// droppedKeys are keys of the PowerShell recorder.config.json that the Go core does not read. They
// are accepted and ignored (no warning) so an existing file loads unchanged. See KEYS.md.
var droppedKeys = []string{
	"HealthStripHeightPx", "HealthStripMinPx", "GridGutterPx", "VideoAspectWidth", "VideoAspectHeight",
	"HealthCellWidthPx", "HealthCellHeightPx", "HealthSections", "HealthEventRows",
	"RelayReadyPollMs", "ViewerPromoteCheckSeconds",
}

var liveKeys = []string{"LiveViewEnabled", "LiveExePath", "LiveConfigPath", "LiveWhepAddress", "LiveUdpAddress", "LiveAdvertiseHosts", "LiveSubStream", "LiveCloseAfterSeconds", "LiveSecretPath"}

// keptKeys is every key Parse reads, in the order problems are reported.
var keptKeys = []string{
	"RootPath", "FfmpegPath", "FfprobePath", "SegmentSeconds", "Container", "ContainerProfiles", "RtspTransport", "TimeoutMicroseconds",
	"ReserveGB", "PruneHysteresisGB", "PruneIntervalSeconds", "MaxRetentionDays", "MaxAgePruneFraction",
	"ReconcileSeconds", "StatusIntervalSeconds", "ArchiveScanSeconds", "CameraDownAlertMinutes", "CameraDownRepeatHours",
	"StopTimeoutSeconds", "BackoffStartSeconds", "BackoffMaxSeconds", "HealthyResetSeconds", "StallFactor", "LogMaxBytes",
	"LogRepeatSuppressSeconds", "StatusEventCount",
	"RestreamerEnabled", "RelayExternal", "RestreamerExePath", "RestreamerConfigPath", "RelayBase", "RelayApiBase", "RelayProbeTimeoutMs",
	"RelayStartupWaitSeconds", "RelayReadyWaitSeconds", "StartStaggerMs",
	"LiveViewEnabled", "LiveExePath", "LiveConfigPath", "LiveWhepAddress", "LiveUdpAddress", "LiveAdvertiseHosts", "LiveSubStream", "LiveCloseAfterSeconds", "LiveSecretPath",
	"logging", "alerts", "secrets", "web",
}

// KeptKeys returns the keys the Go core reads (copy).
func KeptKeys() []string { return append([]string(nil), keptKeys...) }

// DroppedKeys returns the PowerShell keys that are accepted and ignored (copy).
func DroppedKeys() []string { return append([]string(nil), droppedKeys...) }

// Load reads a holocron.config.json. A missing or whitespace-only file yields the defaults, as
// Get-CamConfig does. The error is a *ValidationError for a bad document, or an I/O error.
func Load(path string, p osx.Platform) (Config, []Warning, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Defaults(p), nil, nil
	}
	if err != nil {
		return Config{}, nil, err
	}
	return Parse(raw, p)
}

// Parse overlays a document on the defaults and validates the result. Every problem is returned in
// one *ValidationError. The Config is only meaningful when err is nil.
func Parse(data []byte, p osx.Platform) (Config, []Warning, error) {
	c := Defaults(p)
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if len(bytes.TrimSpace(data)) == 0 {
		return c, nil, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return Config{}, nil, &ValidationError{Problems: []Problem{{Message: "the document is not a JSON object: " + errText(err)}}}
	}
	d := newDecoder(doc)
	d.run(&c)
	if len(d.problems) > 0 {
		return Config{}, d.warnings, &ValidationError{Problems: d.problems}
	}
	return c, d.warnings, nil
}

func errText(err error) string {
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return fmt.Sprintf("%v (at byte %d)", err, se.Offset)
	}
	return err.Error()
}

type decoder struct {
	raw      map[string]json.RawMessage // by canonical key
	problems []Problem
	warnings []Warning
}

func newDecoder(doc map[string]json.RawMessage) *decoder {
	d := &decoder{raw: map[string]json.RawMessage{}}
	canon := map[string]string{}
	for _, k := range keptKeys {
		canon[strings.ToLower(k)] = k
	}
	for _, k := range droppedKeys {
		canon[strings.ToLower(k)] = ""
	}
	keys := make([]string, 0, len(doc))
	for k := range doc {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	seen := map[string]string{}
	for _, k := range keys {
		lk := strings.ToLower(k)
		if first, dup := seen[lk]; dup {
			d.problem(k, "appears twice with different capitals (also %q); keys are case-insensitive", first)
			continue
		}
		seen[lk] = k
		c, known := canon[lk]
		switch {
		case !known && strings.HasPrefix(lk, "live"):
			d.problem(k, "unknown live-view setting. The known ones are: %s", strings.Join(liveKeys, ", "))
		case !known:
			d.warnings = append(d.warnings, Warning{Path: k, Message: "unknown key, ignored"})
		case c == "":
			// dropped key: accepted and ignored
		default:
			d.raw[c] = doc[k]
		}
	}
	return d
}

func (d *decoder) problem(path, format string, args ...any) {
	d.problems = append(d.problems, Problem{Path: path, Message: fmt.Sprintf(format, args...)})
}

func (d *decoder) warn(path, format string, args ...any) {
	d.warnings = append(d.warnings, Warning{Path: path, Message: fmt.Sprintf(format, args...)})
}

func jsonKind(raw json.RawMessage) string {
	s := bytes.TrimSpace(raw)
	if len(s) == 0 {
		return "nothing"
	}
	switch s[0] {
	case '"':
		return "a string"
	case '[':
		return "an array"
	case '{':
		return "an object"
	case 't', 'f':
		return "a boolean"
	case 'n':
		return "null"
	}
	return "a number"
}

func num(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

func (d *decoder) str(key string, dst *string) {
	raw, ok := d.raw[key]
	if !ok {
		return
	}
	if err := json.Unmarshal(raw, dst); err != nil || jsonKind(raw) != "a string" {
		d.problem(key, "must be a string, got %s", jsonKind(raw))
	}
}

func (d *decoder) boolean(key string, dst *bool) {
	raw, ok := d.raw[key]
	if !ok {
		return
	}
	if jsonKind(raw) != "a boolean" {
		d.problem(key, "must be true or false (a JSON boolean), got %s %s", jsonKind(raw), strings.TrimSpace(string(raw)))
		return
	}
	_ = json.Unmarshal(raw, dst)
}

// float reads a JSON number with an inclusive-or-exclusive lower bound.
func (d *decoder) float(key string, dst *float64, min float64, strict bool) {
	raw, ok := d.raw[key]
	if !ok {
		return
	}
	v, good := d.number(key, raw)
	if !good {
		return
	}
	if v < min || (strict && v == min) {
		if strict {
			d.problem(key, "must be greater than %s, got %s", num(min), num(v))
		} else {
			d.problem(key, "must be at least %s, got %s", num(min), num(v))
		}
		return
	}
	*dst = v
}

func (d *decoder) number(key string, raw json.RawMessage) (float64, bool) {
	var v float64
	if jsonKind(raw) != "a number" || json.Unmarshal(raw, &v) != nil || math.IsInf(v, 0) || math.IsNaN(v) {
		d.problem(key, "must be a number, got %s", jsonKind(raw))
		return 0, false
	}
	return v, true
}

// integer reads a whole JSON number with a lower bound.
func (d *decoder) integer(key string, dst *int, min int) {
	raw, ok := d.raw[key]
	if !ok {
		return
	}
	v, good := d.number(key, raw)
	if !good {
		return
	}
	if v != math.Trunc(v) || math.Abs(v) > 1<<53 {
		d.problem(key, "must be a whole number, got %s", num(v))
		return
	}
	if v < float64(min) {
		d.problem(key, "must be at least %d, got %s", min, num(v))
		return
	}
	*dst = int(v)
}

// clamped reads a whole number and raises a value below floor to floor with a warning, like the
// PowerShell consumers (which clamp at use).
func (d *decoder) clamped(key string, dst *int, floor int) {
	raw, ok := d.raw[key]
	if !ok {
		return
	}
	v, good := d.number(key, raw)
	if !good {
		return
	}
	if v != math.Trunc(v) || math.Abs(v) > 1<<31 {
		d.problem(key, "must be a whole number, got %s", num(v))
		return
	}
	n := int(v)
	if n < floor {
		d.warn(key, "%d is below the floor %d, using %d", n, floor, floor)
		n = floor
	}
	*dst = n
}

func (d *decoder) run(c *Config) {
	d.str("RootPath", &c.RootPath)
	d.str("FfmpegPath", &c.FfmpegPath)
	d.str("FfprobePath", &c.FfprobePath)
	d.integer("SegmentSeconds", &c.SegmentSeconds, 1)
	d.str("Container", &c.Container)
	d.profiles(c)
	d.str("RtspTransport", &c.RtspTransport)
	d.timeoutMicroseconds(c)

	d.float("ReserveGB", &c.ReserveGB, 0, false)
	d.float("PruneHysteresisGB", &c.PruneHysteresisGB, 0, false)
	d.float("PruneIntervalSeconds", &c.PruneIntervalSeconds, 0, true)
	d.retention(c)
	d.fraction(c)

	d.float("ReconcileSeconds", &c.ReconcileSeconds, 0, true)
	d.float("StatusIntervalSeconds", &c.StatusIntervalSeconds, 0, true)
	d.float("ArchiveScanSeconds", &c.ArchiveScanSeconds, 0, true)
	d.float("CameraDownAlertMinutes", &c.CameraDownAlertMinutes, 0, true)
	d.float("CameraDownRepeatHours", &c.CameraDownRepeatHours, 0, true)
	d.float("StopTimeoutSeconds", &c.StopTimeoutSeconds, 0, true)
	d.integer("BackoffStartSeconds", &c.BackoffStartSeconds, 1)
	d.integer("BackoffMaxSeconds", &c.BackoffMaxSeconds, 1)
	d.integer("HealthyResetSeconds", &c.HealthyResetSeconds, 1)
	d.float("StallFactor", &c.StallFactor, 1, true)
	d.integer("LogMaxBytes", &c.LogMaxBytes, 1)
	d.float("LogRepeatSuppressSeconds", &c.LogRepeatSuppressSeconds, 0, false)
	d.integer("StatusEventCount", &c.StatusEventCount, 0)

	d.boolean("RestreamerEnabled", &c.RestreamerEnabled)
	d.boolean("RelayExternal", &c.RelayExternal)
	d.str("RestreamerExePath", &c.RestreamerExePath)
	d.str("RestreamerConfigPath", &c.RestreamerConfigPath)
	d.str("RelayBase", &c.RelayBase)
	d.str("RelayApiBase", &c.RelayApiBase)
	d.clamped("RelayProbeTimeoutMs", &c.RelayProbeTimeoutMs, 100)
	d.integer("RelayStartupWaitSeconds", &c.RelayStartupWaitSeconds, 0)
	d.clamped("RelayReadyWaitSeconds", &c.RelayReadyWaitSeconds, 0)
	d.clamped("StartStaggerMs", &c.StartStaggerMs, 0)

	d.live(c)
	d.logging(c)
	d.alerts(c)
	d.secrets(c)
	d.web(c)
	d.crossChecks(c)
}

func (d *decoder) timeoutMicroseconds(c *Config) {
	raw, ok := d.raw["TimeoutMicroseconds"]
	if !ok {
		return
	}
	v, good := d.number("TimeoutMicroseconds", raw)
	if !good {
		return
	}
	if v != math.Trunc(v) || v < 1 || v > 1<<53 {
		d.problem("TimeoutMicroseconds", "must be a whole number of microseconds, at least 1, got %s", num(v))
		return
	}
	c.TimeoutMicroseconds = int64(v)
}

func (d *decoder) retention(c *Config) {
	raw, ok := d.raw["MaxRetentionDays"]
	if !ok {
		return
	}
	if jsonKind(raw) == "null" {
		c.MaxRetentionDays = nil
		return
	}
	v, good := d.number("MaxRetentionDays", raw)
	if !good {
		return
	}
	if v <= 0 {
		d.problem("MaxRetentionDays", "must be null (age rule off) or greater than 0, got %s", num(v))
		return
	}
	c.MaxRetentionDays = &v
}

// fraction: Get-CamConfig accepts the string form too (TryParse of the text); the Go reader wants a number.
func (d *decoder) fraction(c *Config) {
	raw, ok := d.raw["MaxAgePruneFraction"]
	if !ok {
		return
	}
	v, good := d.number("MaxAgePruneFraction", raw)
	if !good {
		return
	}
	if !(v >= 0.01 && v <= 1.0) {
		d.problem("MaxAgePruneFraction", "must be a number from 0.01 to 1.0 - the largest share of the archive one age-rule run may delete, 1.0 to switch the brake off - got '%s'", num(v))
		return
	}
	c.MaxAgePruneFraction = v
}

var (
	extensionRE = regexp.MustCompile(`^\.[A-Za-z0-9]{1,8}$`)
	transports  = map[string]bool{"tcp": true, "udp": true, "udp_multicast": true, "http": true, "https": true}
)

func (d *decoder) profiles(c *Config) {
	raw, ok := d.raw["ContainerProfiles"]
	if !ok {
		return
	}
	if jsonKind(raw) != "an object" {
		d.problem("ContainerProfiles", "must be an object of named profiles, got %s", jsonKind(raw))
		return
	}
	var named map[string]json.RawMessage
	_ = json.Unmarshal(raw, &named)
	names := make([]string, 0, len(named))
	for n := range named {
		names = append(names, n)
	}
	sort.Strings(names)
	// the file's map REPLACES the defaults (shallow overlay, as in PowerShell)
	out := map[string]ContainerProfile{}
	for _, n := range names {
		path := "ContainerProfiles." + n
		if jsonKind(named[n]) != "an object" {
			d.problem(path, "must be an object, got %s", jsonKind(named[n]))
			continue
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(named[n], &fields)
		var p ContainerProfile
		for k, v := range fields {
			sub := path + "." + k
			switch strings.ToLower(k) {
			case "extension":
				d.subStr(sub, v, &p.Extension)
			case "segmentformat":
				d.subStr(sub, v, &p.SegmentFormat)
			case "segmentformatoptions":
				if jsonKind(v) != "null" {
					d.subStr(sub, v, &p.SegmentFormatOptions)
				}
			case "audioargs":
				d.subList(sub, v, &p.AudioArgs)
			case "extraargs":
				d.subList(sub, v, &p.ExtraArgs)
			default:
				d.warn(sub, "unknown key, ignored")
			}
		}
		if strings.TrimSpace(p.Extension) == "" {
			d.problem(path+".Extension", "is required (for example \".mkv\")")
		} else if !extensionRE.MatchString(p.Extension) {
			d.problem(path+".Extension", "must be a dot and 1-8 letters or digits such as \".mkv\" (it decides which files the pruner may delete), got %q", p.Extension)
		}
		if strings.TrimSpace(p.SegmentFormat) == "" {
			d.problem(path+".SegmentFormat", "is required (the ffmpeg segment_format, for example \"matroska\")")
		}
		if p.AudioArgs == nil {
			p.AudioArgs = []string{}
		}
		if p.ExtraArgs == nil {
			p.ExtraArgs = []string{}
		}
		out[n] = p
	}
	c.ContainerProfiles = out
}

func (d *decoder) subStr(path string, raw json.RawMessage, dst *string) {
	if jsonKind(raw) != "a string" {
		d.problem(path, "must be a string, got %s", jsonKind(raw))
		return
	}
	_ = json.Unmarshal(raw, dst)
}

func (d *decoder) subList(path string, raw json.RawMessage, dst *[]string) {
	if jsonKind(raw) == "null" {
		return
	}
	var items []json.RawMessage
	if jsonKind(raw) != "an array" || json.Unmarshal(raw, &items) != nil {
		d.problem(path, "must be an array of strings, got %s", jsonKind(raw))
		return
	}
	out := make([]string, 0, len(items))
	for i, it := range items {
		if jsonKind(it) != "a string" {
			d.problem(fmt.Sprintf("%s[%d]", path, i), "must be a string, got %s", jsonKind(it))
			continue
		}
		var s string
		_ = json.Unmarshal(it, &s)
		out = append(out, s)
	}
	*dst = out
}

func (d *decoder) logging(c *Config) {
	raw, ok := d.raw["logging"]
	if !ok {
		return
	}
	if jsonKind(raw) != "an object" {
		d.problem("logging", "must be an object, got %s", jsonKind(raw))
		return
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := fields[k]
		switch strings.ToLower(k) {
		case "level":
			var s string
			d.subStr("logging."+k, v, &s)
			if s != "" {
				if _, err := logx.ParseLevel(s); err != nil {
					d.problem("logging."+k, "%v", err)
				}
			}
			c.Logging.Level = s
		case "levels":
			if jsonKind(v) != "an object" {
				d.problem("logging."+k, "must be an object of component to level, got %s", jsonKind(v))
				continue
			}
			var m map[string]json.RawMessage
			_ = json.Unmarshal(v, &m)
			levels := map[string]string{}
			comps := make([]string, 0, len(m))
			for comp := range m {
				comps = append(comps, comp)
			}
			sort.Strings(comps)
			for _, comp := range comps {
				var s string
				d.subStr("logging."+k+"."+comp, m[comp], &s)
				if _, err := logx.ParseLevel(s); err != nil {
					d.problem("logging."+k+"."+comp, "%v", err)
				}
				levels[comp] = s
			}
			c.Logging.Levels = levels
		default:
			d.warn("logging."+k, "unknown key, ignored")
		}
	}
}

func (d *decoder) crossChecks(c *Config) {
	if _, ok := d.raw["RootPath"]; ok && strings.TrimSpace(c.RootPath) == "" {
		d.problem("RootPath", "must not be empty")
	}
	if strings.Contains(c.RootPath, "%") {
		d.problem("RootPath", "must not contain '%%': ffmpeg's -strftime 1 expands '%%' in the folder part of the output path too (a folder 'Cam%%Y' would be written as 'Cam2026')")
	}
	if _, ok := d.raw["RtspTransport"]; ok && !transports[c.RtspTransport] {
		d.problem("RtspTransport", "must be one of tcp, udp, udp_multicast, http, https, got %q", c.RtspTransport)
	}
	_, hasC := d.raw["Container"]
	_, hasP := d.raw["ContainerProfiles"]
	if (hasC || hasP) && !d.hasProblemUnder("Container") && !d.hasProblemUnder("ContainerProfiles") {
		if _, ok := c.Profile(); !ok {
			d.problem("Container", "profile %q is not defined in ContainerProfiles", c.Container)
		}
	}
	if c.BackoffMaxSeconds < c.BackoffStartSeconds && !d.hasProblemUnder("Backoff") {
		d.problem("BackoffMaxSeconds", "must not be less than BackoffStartSeconds (%d), got %d", c.BackoffStartSeconds, c.BackoffMaxSeconds)
	}
	if c.RelayExternal && c.RestreamerEnabled && !d.hasProblemUnder("RestreamerEnabled") {
		d.problem("RelayExternal", "needs RestreamerEnabled false: with an external relay Holocron starts no MediaMTX of its own (RestreamerEnabled defaults to true, so set it to false)")
	}
	for _, k := range []string{"RelayBase", "RelayApiBase"} {
		if v := c.value(k); strings.TrimSpace(v) != "" {
			if _, err := HostPort(v); err != nil {
				d.problem(k, "%v", err)
			}
		}
	}
}

func (d *decoder) hasProblemUnder(prefix string) bool {
	for _, p := range d.problems {
		if strings.HasPrefix(p.Path, prefix) {
			return true
		}
	}
	return false
}

func (c Config) value(k string) string {
	if k == "RelayBase" {
		return c.RelayBase
	}
	return c.RelayApiBase
}

var schemeRE = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.\-]*://`)

// HostPort strips a URL down to host:port, the form MediaMTX and the live address check expect: the
// scheme and trailing slashes are removed (Get-CamRelayHostPort). It fails when nothing is left.
func HostPort(url string) (string, error) {
	text := strings.TrimRight(schemeRE.ReplaceAllString(url, ""), "/")
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("'%s' has no host:port", url)
	}
	return text, nil
}
