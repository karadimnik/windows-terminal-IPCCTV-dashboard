package supervise

import (
	"math"
	"time"

	"holocron/internal/archive"
	"holocron/internal/status"
)

// Rows are the status.json Cameras[] rows (IS 1.4, Write-CamStatusFile R:2804-2875): every camera of
// the last Reconcile, in list order, recording or not. The scan is the camera's own (Refresh) when
// it has a child, else archiveScans[SafeName] (the archive-wide scan) when given. The camera's own
// scan reads the spool, so ArchiveFiles and ArchiveBytes add the filed days of the archive-wide
// scan (identical on a flat archive, where nothing is filed). Names and errors
// are not redacted here: the status Writer redacts the whole document.
func (m *Manager) Rows(archiveScans map[string]archive.DirScan) []status.Camera {
	nowWall, nowMono := m.now()
	loc := m.d.Location
	if loc == nil {
		loc = time.Local
	}
	ts := func(t time.Time) status.Time {
		if t.IsZero() {
			return status.Time{}
		}
		return status.NewTime(t.In(loc))
	}
	rows := make([]status.Camera, 0, len(m.cams))
	for i, c := range m.cams {
		row := status.Camera{Name: c.Name, Record: c.Record, ConfigError: m.configErrs[i]}
		safe, err := c.SafeName()
		if err != nil {
			safe = ""
		}
		row.SafeName = safe
		var e *Entry
		if m.configErrs[i] == "" {
			e = m.entries[safe]
		}
		var scan *archive.DirScan
		if e != nil && e.hasScan {
			scan = &e.scan
		} else if s, ok := archiveScans[safe]; ok && safe != "" {
			scan = &s
		}
		if e != nil {
			row.RestartsTotal = e.restartCount
			row.RestartsLastHour = restartsLastHour(e, nowMono)
			if e.lastPid != 0 {
				row.LastPid = ptr(e.lastPid)
			}
			row.LastError = e.lastError
			row.LastErrorAt = ts(e.lastErrorAt)
			row.StartedAt = ts(e.startedWall)
			if e.running {
				row.Pid, row.Alive = ptr(e.pid), true
			} else {
				row.NextStartAt = ts(e.nextWall)
			}
			row.InputMode = string(e.input)
			row.DownSince = ts(e.downSince)
		}
		if scan != nil {
			if scan.Newest != "" {
				row.CurrentSegment, row.CurrentSegmentBytes = ptr(scan.Newest), ptr(scan.NewestBytes)
			}
			if !scan.NewestClosedWrite.IsZero() {
				v := math.RoundToEven(nowWall.Sub(scan.NewestClosedWrite).Seconds()*10) / 10
				row.SecondsSinceLastClosedSegment = &v
			}
			files, bytes := int64(scan.Files), scan.Bytes
			if e != nil && scan == &e.scan { // the status tick reads the spool: the filed days come from the archive scan
				if s, ok := archiveScans[safe]; ok {
					files, bytes = files+int64(s.Filed), bytes+s.FiledBytes
				}
			}
			row.ArchiveFiles, row.ArchiveBytes = ptr(files), ptr(bytes)
		}
		rows = append(rows, row)
	}
	return rows
}

func ptr[T any](v T) *T { return &v }
