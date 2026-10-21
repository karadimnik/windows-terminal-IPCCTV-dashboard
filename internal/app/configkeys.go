package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"

	"holocron/internal/config"
	"holocron/internal/osx"
)

// editableRules is each editable key's rule as internal/config/KEYS.md states it, for the Admin
// tab (TestEveryEditableKeyHasARule keeps it complete).
var editableRules = map[string]string{
	"SegmentSeconds":              "whole number >= 1 (seconds; clock-aligned segments)",
	"ReserveGB":                   "number >= 0: the pruner keeps this much free space",
	"PruneHysteresisGB":           "number >= 0: freed above the reserve before the pruner stops",
	"MaxRetentionDays":            "null (no age limit) or a number > 0",
	"MaxAgePruneFraction":         "number 0.01 to 1.0: the age rule's circuit breaker",
	"CameraDownAlertMinutes":      "number > 0",
	"CameraDownRepeatHours":       "number > 0",
	"StopTimeoutSeconds":          "number > 0",
	"LiveViewEnabled":             "true or false",
	"LiveUdpAddress":              "127.0.0.1, 0.0.0.0 or one private IPv4, with a port 1-65535",
	"LiveAdvertiseHosts":          "a JSON array of at most 4 unique private IPv4 addresses",
	"LiveSubStream":               "1 to 32 characters from A-Z a-z 0-9 _ -",
	"LiveCloseAfterSeconds":       "whole number 1 to 3600",
	"logging.level":               "error, warn, info, debug or trace",
	"logging.levels.*":            "one component's level: error, warn, info, debug or trace",
	"alerts.queueSize":            "whole number >= 1",
	"alerts.timeoutSeconds":       "number > 0",
	"alerts.minRepeatSeconds":     "number > 0",
	"alerts.retryBackoffSeconds":  "a JSON array of numbers > 0; [] = no retries",
	"alerts.webhook.enabled":      "true or false; needs a url",
	"alerts.webhook.url":          "https, or http to a loopback or private address",
	"alerts.webhook.secretName":   "the name of a secret in the store (never the secret)",
	"alerts.webhook.secretHeader": "an HTTP header name",
	"alerts.push.enabled":         "true or false; needs a topic",
	"alerts.push.server":          "https, or http to a loopback or private address",
	"alerts.push.topic":           "1 to 64 characters from A-Z a-z 0-9 _ -",
	"alerts.push.tokenName":       "the name of a secret in the store (never the token)",
	"alerts.desktop.enabled":      "true or false",
	"alerts.eventlog.enabled":     "true or false",
	"secrets.backend":             "auto, keyring, file or env",
	"secrets.file":                "a local path; empty = secrets.enc in the auth folder",
	"secrets.keyFile":             "a local path; empty = secrets.key in the auth folder",
	"web.enabled":                 "true or false: holocron up starts the web app",
}

// storeWarning is said for every key that decides where the camera login is stored.
const storeWarning = "Changing this does not move the stored camera login: the login stays where it is, and with a wrong value the recorder finds none (holocron secret set camera.user and camera.password store it again)."

// KeyInfo is one editable key as the Admin tab lists it.
type KeyInfo struct {
	Key     string
	Value   any  // effective (URLs with a login masked)
	Default any  // the built-in default
	Set     bool // the file names it
	Rule    string
	Restart bool // a running supervisor applies it only at its next start
	Pattern bool // logging.levels.*: stands for one component
	Warning string
}

// EditableKeyInfos lists the allow-list of `holocron config set` for the configuration at path,
// whose effective values are cfg: every key, plus one row per component the file sets in
// logging.levels.
func EditableKeyInfos(path string, cfg config.Config, plat osx.Platform) ([]KeyInfo, error) {
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	body := bytes.TrimPrefix(raw, bom)
	defaults := config.Defaults(plat)
	value := func(c config.Config, key string) any {
		v, err := EffectiveValue(c, key)
		if err != nil {
			return nil
		}
		return v
	}
	row := func(key, rule string) KeyInfo {
		_, set := lookup(body, key)
		info := KeyInfo{Key: key, Value: value(cfg, key), Default: value(defaults, key), Set: set, Rule: rule, Restart: key != "LiveViewEnabled"}
		if strings.HasPrefix(key, "secrets.") {
			info.Warning = storeWarning
		}
		return info
	}
	var out []KeyInfo
	for _, k := range EditableKeys {
		if k != "logging.levels.*" {
			out = append(out, row(k, editableRules[k]))
			continue
		}
		out = append(out, KeyInfo{Key: k, Value: value(cfg, "logging.levels"), Default: value(defaults, "logging.levels"), Rule: editableRules[k], Restart: true, Pattern: true})
		if levels, ok := lookup(body, "logging.levels"); ok {
			if o, err := parseObject(levels); err == nil {
				for _, m := range o {
					var level string
					_ = json.Unmarshal(m.Value, &level)
					out = append(out, KeyInfo{Key: "logging.levels." + strings.ToLower(m.Key), Value: level, Set: true, Rule: editableRules[k], Restart: true})
				}
			}
		}
	}
	return out, nil
}
