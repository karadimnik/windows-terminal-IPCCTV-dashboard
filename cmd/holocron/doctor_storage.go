package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"holocron/internal/archive"
)

var storageTable = []doctorCheck{
	{id: "storage.disk", group: "storage", run: storageDisk},
	{id: "storage.prune", group: "storage", run: storagePrune},
	{id: "storage.marker", group: "storage", run: storageMarker},
	{id: "storage.cameras", group: "storage", run: storageCameras},
}

func init() { doctorTable = append(doctorTable, storageTable...) }

const gb = 1 << 30

// freeGB is the free space of the root now (the OS), else the supervisor's last scan.
func (r *drun) freeGB() (float64, string) {
	if b, err := r.d.platform.DiskFree(r.e.paths.Root); err == nil {
		return float64(b) / gb, "now"
	}
	if r.doc.Disk != nil {
		return r.doc.Disk.FreeGB, "at the supervisor's last scan"
	}
	return -1, ""
}

// pruneRecent: the pruner ran within two of its intervals.
func (r *drun) pruneRecent() bool {
	p := r.doc.Prune
	iv := r.e.cfg.PruneIntervalSeconds
	if iv <= 0 {
		iv = 300
	}
	return p != nil && !p.LastRunAt.IsNull() && r.now.Sub(p.LastRunAt.Time()) <= time.Duration(2*iv*float64(time.Second))
}

// storageDisk: free space against the reserve. This system runs at the reserve edge by design
// (the pruner keeps it there), so near or at the reserve is ok; below it is a warning only while
// the pruner has not run for two intervals.
func storageDisk(_ context.Context, r *drun) []check {
	free, when := r.freeGB()
	reserve := r.e.cfg.ReserveGB
	if free < 0 {
		return []check{mk(lvWarn, "disk", "the free space of %s cannot be read", r.e.paths.Root).fix("holocron archive stats")}
	}
	text := fmt.Sprintf("free %.2f GB %s, reserve %g GB", free, when, reserve)
	data := map[string]any{"freeGB": free, "reserveGB": reserve}
	if k := r.doc.Disk; k != nil {
		if k.ArchiveGB != nil {
			text += fmt.Sprintf(", archive %.2f GB", *k.ArchiveGB)
		}
		if k.MeasuredGBPerDay != nil {
			text += fmt.Sprintf(", %.2f GB/day", *k.MeasuredGBPerDay)
			data["gbPerDay"] = *k.MeasuredGBPerDay
		}
		if k.ProjectedDaysRemaining != nil {
			text += fmt.Sprintf(", about %.1f day(s) of headroom above the reserve", *k.ProjectedDaysRemaining)
			data["daysLeft"] = *k.ProjectedDaysRemaining
		}
	}
	if free < reserve && !r.pruneRecent() {
		return []check{mk(lvWarn, "disk", "%s: BELOW the reserve and the pruner has not run for two intervals (%gs each)", text, r.e.cfg.PruneIntervalSeconds).
			fix("holocron prune --dry-run ; holocron logs --component prune --since 1h").data(data)}
	}
	if free < reserve {
		text += " (below the reserve; the pruner is freeing space)"
	}
	return []check{mk(lvOK, "disk", "%s", text).data(data)}
}

// storagePrune: the pruner's last run - deleted, blocked, the age rule's circuit breaker.
func storagePrune(_ context.Context, r *drun) []check {
	p := r.doc.Prune
	if p == nil {
		return []check{mk(lvWarn, "pruner", "the supervisor reports no pruner state yet").fix("holocron prune --dry-run")}
	}
	data := map[string]any{"lastRunAt": p.LastRunAt.Time(), "deletedTotal": p.DeletedTotal, "freedGBTotal": p.FreedGBTotal, "blocked": p.Blocked}
	text := fmt.Sprintf("last run %s, deleted %d file(s), freed %.2f GB in total%s", stamp(p.LastRunAt), p.DeletedTotal, p.FreedGBTotal, errText(p.LastMessage))
	switch {
	case p.Blocked:
		return []check{mk(lvFail, "pruner", "pruning is BLOCKED (alert 120 or 121): %s", text).
			fix("holocron prune --dry-run (it names the reason) ; free space on the disk or lower the reserve: holocron config set ReserveGB <GB>").data(data)}
	case strings.Contains(p.LastMessage, "circuit breaker"):
		return []check{mk(lvWarn, "pruner", "the age rule is held by the circuit breaker (alert 122): %s", text).
			fix("holocron prune --dry-run ; correct MaxRetentionDays, or raise MaxAgePruneFraction (holocron config set MaxAgePruneFraction <0.01-1>)").data(data)}
	case !r.pruneRecent():
		return []check{mk(lvWarn, "pruner", "has not run for two intervals (%gs each): %s", r.e.cfg.PruneIntervalSeconds, text).fix("holocron logs --component prune --since 1h").data(data)}
	}
	return []check{mk(lvOK, "pruner", "%s", text).data(data)}
}

// storageMarker: a running supervisor without a valid ownership marker prunes nothing.
func storageMarker(_ context.Context, r *drun) []check {
	ok, why := archive.MarkerValid(r.e.paths.Root, r.e.opts.Platform)
	if !ok {
		return []check{mk(lvFail, "marker", "the ownership marker is not valid (%s): pruning is disabled and the disk will fill", why).
			fix("holocron prune --dry-run (it names the gate) ; holocron restart --detach (the supervisor claims the root at start)")}
	}
	return []check{mk(lvOK, "marker", "%s is valid", archive.MarkerPath(r.e.paths.Root))}
}

// storageCameras: segments per camera in the last 24 h; one with far fewer than the others
// (under half the median) has been missing hours.
func storageCameras(_ context.Context, r *drun) []check {
	cams, _ := r.enabled()
	since := r.now.Add(-24 * time.Hour)
	counts := map[string]int{}
	bytes := map[string]int64{}
	var all []int
	for _, cam := range cams {
		for _, s := range r.files(cam).Segs {
			if !s.Start.Before(since) {
				counts[cam.Name]++
				bytes[cam.Name] += s.Size
			}
		}
		all = append(all, counts[cam.Name])
	}
	sort.Ints(all)
	median := 0
	if len(all) > 0 {
		median = all[len(all)/2]
	}
	var cs []check
	for _, cam := range cams {
		n := counts[cam.Name]
		data := map[string]any{"camera": cam.Name, "segments24h": n, "bytes24h": bytes[cam.Name], "median": median}
		text := fmt.Sprintf("%d segment(s), %.2f GB in the last 24 h", n, float64(bytes[cam.Name])/gb)
		if len(cams) > 1 && float64(n) < 0.5*float64(median) {
			safe, _ := archive.SafeName(cam.Name)
			cs = append(cs, mk(lvWarn, cam.Name, "%s, against a median of %d: hours are missing", text, median).fix("holocron doctor --group recording ; %s", camFix(cam, safe)).data(data))
			continue
		}
		cs = append(cs, mk(lvOK, cam.Name, "%s", text).data(data))
	}
	return cs
}
