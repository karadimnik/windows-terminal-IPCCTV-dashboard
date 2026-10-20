package supervise

import (
	"fmt"
	"math"
	"time"

	"holocron/internal/logx"
)

// DownEvent is one camera-down transition of a Down call.
type DownEvent struct {
	Camera    string
	SafeName  string
	Recovered bool      // "is writing again" (info, no alert)
	Since     time.Time // DownSince: the reference time
	Seconds   float64   // seconds without file progress (down) or down duration (recovered)
	Alerted   bool      // alert 110 was raised by this call
}

// DownAlertSeconds is Get-CamDownAlertSeconds (R:1818-1838): max(CameraDownAlertMinutes x 60,
// SegmentSeconds + StatusIntervalSeconds + 30).
func DownAlertSeconds(s Settings) float64 {
	return math.Max(s.DownAlertMinutes*60, s.SegmentSeconds+s.StatusInterval+30)
}

// FormatAge is Format-CamAgeText (cam.common.ps1:75-84): "Ns" below 120 s, "Nm" below 2 h, else
// "Nh", each with PowerShell's [int] rounding.
func FormatAge(seconds float64) string {
	switch {
	case seconds < 120:
		return fmt.Sprintf("%ds", roundInt(seconds))
	case seconds < 7200:
		return fmt.Sprintf("%dm", roundInt(seconds/60))
	}
	return fmt.Sprintf("%dh", roundInt(seconds/3600))
}

// downReference is Measure-CamCameraDown (R:1842-1868) with the D19 floor: the reference is the
// latest of the supervisor start, this camera's first start by the manager (its creation while it
// has never started), the newest closed write and the open segment's first-seen time. Process
// liveness plays no part (R:4065-4072).
func (m *Manager) downReference(e *Entry) time.Time {
	ref := m.startedWall
	floor := e.firstStart
	if floor.IsZero() {
		floor = e.createdWall
	}
	if floor.After(ref) {
		ref = floor
	}
	if p := e.fileProgress(); p.After(ref) {
		ref = p
	}
	return ref
}

// Down evaluates alert 110 for every recording camera (R:4074-4099); call it on each status tick,
// after Refresh. Above the limit the camera gets DownSince (the reference) and an alert, repeated
// every CameraDownRepeatHours while still down; at or below it a camera that was down logs
// "is writing again after N down" and is cleared. Blind time is not counted.
func (m *Manager) Down() []DownEvent {
	nowWall, nowMono := m.now()
	limit := DownAlertSeconds(m.s)
	repeat := time.Duration(m.s.DownRepeatHours * float64(time.Hour))
	var out []DownEvent
	for _, e := range m.sorted() {
		ref := m.downReference(e)
		secs := (nowWall.Sub(ref) - m.blindWall(ref, nowWall)).Seconds()
		if secs <= limit {
			if !e.downSince.IsZero() {
				d := nowWall.Sub(e.downSince).Seconds()
				m.log.Info(fmt.Sprintf("%s is writing again after %s down", e.cam.Name, FormatAge(d)),
					logx.Action("camera.up"), logx.Target(e.cam.Name), logx.Result("ok"))
				out = append(out, DownEvent{Camera: e.cam.Name, SafeName: e.safe, Recovered: true, Since: e.downSince, Seconds: d})
			}
			e.downSince, e.downAlerted = time.Time{}, false
			continue
		}
		if e.downSince.IsZero() {
			e.downSince = ref
		}
		if e.downAlerted && nowMono.Sub(e.downAlertedMono) < repeat {
			continue
		}
		e.downAlerted, e.downAlertedMono = true, nowMono
		last := e.lastError
		if last == "" {
			last = "none recorded"
		}
		ev := Event{ID: 110, Severity: "Warning", Title: "a camera has stopped recording", Camera: e.cam.Name,
			Message: fmt.Sprintf("%s has written nothing for %s. Last error: %s", e.cam.Name, FormatAge(secs), last),
			Key:     "110:" + e.safe}
		if m.d.Alerts != nil {
			m.d.Alerts.Raise(ev)
		}
		m.log.Warn("ALERT "+ev.Title+" - "+ev.Message, logx.Action("alert.raise"), logx.Target(e.cam.Name), "event_id", 110)
		out = append(out, DownEvent{Camera: e.cam.Name, SafeName: e.safe, Since: e.downSince, Seconds: secs, Alerted: true})
	}
	return out
}
