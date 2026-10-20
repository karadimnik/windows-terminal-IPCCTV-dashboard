package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"holocron/internal/app"
	"holocron/internal/config"
	"holocron/internal/web"
)

// webSettings is the Settings page's writer in the holocron web role: the same edit helpers as
// `holocron cam` and `holocron config` (atomic, BOM and unknown fields kept, validated first), then
// a reload of the running supervisor through the control API.
type webSettings struct{ e *env }

func (w webSettings) PutCameras(ctx context.Context, path string, rows []web.CameraPut) (web.SettingsApplied, []web.FieldError, error) {
	in := make([]app.CameraRow, len(rows))
	for i, r := range rows {
		in[i] = app.CameraRow{OriginalName: r.OriginalName, Name: r.Name, IP: r.IP, Quality: r.Quality, Record: r.Record}
	}
	changed, err := app.PutCameras(path, in)
	var inv *app.InvalidRows
	if errors.As(err, &inv) {
		var out []web.FieldError
		for _, p := range inv.Problems {
			out = append(out, web.FieldError{Row: p.Row, Field: p.Field, Message: p.Message})
		}
		return web.SettingsApplied{}, out, nil
	}
	if err != nil {
		return web.SettingsApplied{}, nil, err
	}
	return web.SettingsApplied{Changed: changed, Message: w.reload(ctx, changed)}, nil, nil
}

func (w webSettings) PutConfig(ctx context.Context, path string, values map[string]any) (web.SettingsApplied, []web.FieldError, error) {
	var edits []app.KeyValue
	for k, v := range values {
		raw, err := json.Marshal(v)
		if err != nil {
			return web.SettingsApplied{}, []web.FieldError{{Field: k, Message: err.Error()}}, nil
		}
		if v == nil {
			raw = nil // null unsets the key (MaxRetentionDays: no age limit)
		}
		edits = append(edits, app.KeyValue{Key: k, Value: raw})
	}
	res, err := app.EditConfigMany(path, edits, w.e.opts.Platform)
	var ve *config.ValidationError
	if errors.As(err, &ve) {
		var out []web.FieldError
		for _, p := range ve.Problems {
			out = append(out, web.FieldError{Field: p.Path, Message: p.Message})
		}
		return web.SettingsApplied{}, out, nil
	}
	if err != nil {
		return web.SettingsApplied{}, nil, err
	}
	changed := 0
	for _, r := range res {
		if r.Changed {
			changed++
		}
	}
	return web.SettingsApplied{Changed: changed, Message: w.reload(ctx, changed)}, nil, nil
}

// reload asks the supervisor to re-read its files and says what happened, for the page.
func (w webSettings) reload(ctx context.Context, changed int) string {
	if changed == 0 {
		return "Nothing changed; nothing was written."
	}
	e := w.e
	if _, ok, _ := app.Running(e.paths, e.cfg, time.Now()); !ok {
		return "Saved. The recorder is not running; the change applies at its next start."
	}
	c, err := e.dial(e.paths.Root)
	if err == nil {
		err = c.Reload(ctx)
	}
	if err != nil {
		return fmt.Sprintf("Saved. The running recorder did not take a reload (%v); it picks the change up on its own within %gs.", err, e.cfg.ReconcileSeconds)
	}
	return "Saved. The running recorder has reloaded."
}
