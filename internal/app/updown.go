package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	"holocron/internal/config"
	"holocron/internal/ctlapi"
	"holocron/internal/osx"
	"holocron/internal/status"
)

// Running is the one liveness verdict on status.json (status.Verdict with real processes).
func Running(p Paths, cfg config.Config, now time.Time) (*status.Document, bool, string) {
	doc, err := status.Read(p.StatusFile())
	if err != nil {
		return nil, false, err.Error()
	}
	ok, why := status.Verdict(doc, now, status.OSProcesses{}, status.VerdictConfig{StatusIntervalSeconds: cfg.StatusIntervalSeconds})
	return doc, ok, why
}

// WaitRunning polls until a supervisor reports running with a heartbeat written after `after`
// (G10: a start is honest only when the new supervisor says so), or the timeout ends.
func WaitRunning(ctx context.Context, p Paths, cfg config.Config, after time.Time, timeout, poll time.Duration) (*status.Document, bool, string) {
	deadline := time.Now().Add(timeout)
	why := "no status written yet"
	for {
		doc, ok, reason := Running(p, cfg, time.Now())
		if ok && doc.UpdatedAt.Time().After(after) {
			return doc, true, reason
		}
		if doc != nil {
			why = reason
			if ok {
				why = "the heartbeat is older than this start"
			}
		}
		if time.Now().After(deadline) {
			return doc, false, why
		}
		select {
		case <-ctx.Done():
			return doc, false, ctx.Err().Error()
		case <-time.After(poll):
		}
	}
}

// Stopper is the part of the control client `down` uses (*ctlapi.Client).
type Stopper interface {
	Stop(ctx context.Context, kind string) (ctlapi.StopReport, error)
}

// DownResult is the account of `holocron down`.
type DownResult struct {
	Before, After Census
	Via           string // "api", "stop-file" or "" (nothing was running)
	APIError      string
	Report        *ctlapi.StopReport
	Reaped        []osx.ReapVerdict // orphans found with no supervisor running, each killed or left alone
	Elapsed       time.Duration
}

// OK: nothing of holocron is left for this root.
func (r DownResult) OK() bool { return r.After.Count() == 0 }

// Down is the orderly stop of everything for p, PROVED (G9): census before; with no supervisor
// running, the provable orphans in the registry are reaped (ReapOrphans) and reported; the stop through the
// control API (POST /v1/stop), or the stop-request file when the API is unreachable; then wait up
// to StopTimeoutSeconds until the census is empty. It kills nothing (G7); survivors are reported.
// With nothing running it removes a leftover stop request and succeeds (idempotent).
func Down(ctx context.Context, p Paths, cfg config.Config, dial func(root string) (Stopper, error), in osx.Inspector) DownResult {
	start := time.Now()
	var stuck []osx.ChildEntry
	census := func() Census {
		c := TakeCensus(p, in, cfg.StatusIntervalSeconds)
		for _, e := range stuck {
			if alive, _ := osx.Verify(e, in, runtime.GOOS); alive {
				c.Children = append(c.Children, e)
			}
		}
		return c
	}
	res := DownResult{Before: census()}
	if res.Before.Count() == 0 {
		_ = os.Remove(p.StopFile())
		res.After = res.Before
		res.Elapsed = time.Since(start)
		return res
	}
	timeout := time.Duration(cfg.StopTimeoutSeconds * float64(time.Second))
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if res.Before.Supervisor == 0 && len(res.Before.Children) > 0 {
		// no supervisor holds the lock: what the registry still lists is an orphan or a stranger
		reaped, err := ReapOrphans(p, in, nil, res.Before.Host == nil)
		res.Reaped = reaped
		for _, v := range reaped { // the reap empties the registry: an orphan it could not kill must still count
			if !v.Killed {
				stuck = append(stuck, v.Entry)
			}
		}
		if err != nil {
			res.APIError = "orphan reap: " + err.Error()
		}
		if after := census(); after.Count() == 0 {
			_ = os.Remove(p.StopFile())
			res.After, res.Via = after, "reap"
			res.Elapsed = time.Since(start)
			return res
		}
	}
	if res.Before.Supervisor != 0 {
		var err error
		var c Stopper
		if c, err = dial(p.Root); err == nil {
			sctx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
			var rep ctlapi.StopReport
			rep, err = c.Stop(sctx, status.StopKindRequested)
			cancel()
			if err == nil {
				res.Via, res.Report = "api", &rep
			}
		}
		if err != nil {
			res.APIError = err.Error()
		}
	}
	if res.Via == "" {
		res.Via = "stop-file"
		if err := writeStopRequest(p); err != nil {
			res.APIError += "; the stop request could not be written: " + err.Error()
		}
	}
	deadline := time.Now().Add(timeout)
	for {
		res.After = census()
		if res.After.Count() == 0 || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	res.Elapsed = time.Since(start)
	return res
}

func writeStopRequest(p Paths) error {
	if err := os.MkdirAll(p.State(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p.StopFile(), []byte(fmt.Sprintf("requested by holocron down, pid %d, %s\n", os.Getpid(), time.Now().Format(time.RFC3339))), 0o600)
}

// DialStopper is the production dial for Down.
func DialStopper(root string) (Stopper, error) {
	c, err := ctlapi.Dial(root)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// IsUnreachable: the control endpoint is not there (no supervisor, or one without it).
func IsUnreachable(err error) bool { return errors.Is(err, ctlapi.ErrUnreachable) }
