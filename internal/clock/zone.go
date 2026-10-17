package clock

import (
	"fmt"
	"strings"
	"time"
	_ "time/tzdata" // embedded zone database: Windows hosts have no zoneinfo files for Go to load
)

// windowsZones maps the Windows zone names that PowerShell reports (TimeZoneInfo.Id) to IANA names.
// Not exhaustive: add a pair when a deployment needs it.
var windowsZones = map[string]string{
	"UTC":                          "UTC",
	"GTB Standard Time":            "Europe/Bucharest",
	"FLE Standard Time":            "Europe/Kyiv",
	"E. Europe Standard Time":      "Europe/Chisinau",
	"W. Europe Standard Time":      "Europe/Berlin",
	"Central Europe Standard Time": "Europe/Budapest",
	"Romance Standard Time":        "Europe/Paris",
	"GMT Standard Time":            "Europe/London",
	"Eastern Standard Time":        "America/New_York",
	"Central Standard Time":        "America/Chicago",
	"Mountain Standard Time":       "America/Denver",
	"Pacific Standard Time":        "America/Los_Angeles",
}

// WindowsZoneToIANA translates a Windows zone name; ok is false for a name not in the table.
func WindowsZoneToIANA(name string) (iana string, ok bool) {
	iana, ok = windowsZones[name]
	return iana, ok
}

// Location resolves a zone by IANA name ("Europe/Bucharest"), by Windows name ("GTB Standard
// Time"), or "" / "Local" for the machine zone.
func Location(name string) (*time.Location, error) {
	if name == "" || strings.EqualFold(name, "Local") {
		return time.Local, nil
	}
	if iana, ok := WindowsZoneToIANA(name); ok {
		name = iana
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unknown time zone %q: %w", name, err)
	}
	return loc, nil
}
