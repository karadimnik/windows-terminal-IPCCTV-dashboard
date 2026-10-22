package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"holocron/internal/app"
	"holocron/internal/cli"
	"holocron/internal/config"
	"holocron/internal/secrets"
	"holocron/internal/web"
)

// webAdmin is the Admin tab's AdminTools in the holocron web role: each call runs the function the
// matching command runs (configEdit, storeSecret, pruneDecide, collectStats, setLogLevel,
// autostartSet, autostartReportOf, buildDoctor) on a copy of the web role's env with the
// configuration read again (the web app outlives edits) and the command's output captured as the
// page's message. health shares and caches the Health runs; nil runs each call alone.
type webAdmin struct {
	e      *env
	health *healthCache
}

func newWebAdmin(e *env) webAdmin { return webAdmin{e: e, health: newHealthCache()} }

// fresh is the env of one call: the configuration as it is on disk now, output into out.
func (w webAdmin) fresh(out *bytes.Buffer) *env {
	e := *w.e
	e.out, e.errw, e.args, e.json = out, out, nil, false
	paths, cfg, warns, err := app.Resolve(e.opts.Root, e.opts.ConfigPath, e.opts.Platform)
	if err == nil {
		e.paths, e.cfg, e.warns = paths, cfg, warns
	}
	e.loadErr = err
	return &e
}

// adminErr turns a command's error into what the page is told: a usage fault is the caller's input
// (422), a refusal stays a refusal (409), anything else a failure.
func adminErr(err error) error {
	var ee *exitError
	if errors.As(err, &ee) && ee.code == exitUsage {
		return web.BadInput("%v", ee.err)
	}
	return err
}

func (w webAdmin) ConfigKeys(ctx context.Context) ([]web.ConfigKey, error) {
	var out bytes.Buffer
	e := w.fresh(&out)
	if err := e.needConfig(); err != nil {
		return nil, err
	}
	infos, err := app.EditableKeyInfos(e.paths.Config, e.cfg, e.opts.Platform)
	if err != nil {
		return nil, err
	}
	keys := make([]web.ConfigKey, len(infos))
	for i, k := range infos {
		keys[i] = web.ConfigKey{Key: k.Key, Value: k.Value, Default: k.Default, Set: k.Set, Rule: k.Rule, Restart: k.Restart, Pattern: k.Pattern, Warning: k.Warning}
	}
	return keys, nil
}

func (w webAdmin) SetConfigKey(ctx context.Context, key string, value *string) (string, []web.FieldError, error) {
	var out bytes.Buffer
	e := w.fresh(&out)
	var raw json.RawMessage
	if value != nil {
		raw = app.ParseValue(*value)
	}
	err := configEdit(ctx, e, key, raw)
	var ve *config.ValidationError
	if errors.As(err, &ve) {
		var invalid []web.FieldError
		for _, p := range ve.Problems {
			invalid = append(invalid, web.FieldError{Field: p.Path, Message: p.Message})
		}
		return "", invalid, nil
	}
	if err != nil {
		return "", nil, adminErr(err)
	}
	return out.String(), nil, nil
}

func (w webAdmin) CameraLogin(ctx context.Context) (web.CameraLoginState, error) {
	var out bytes.Buffer
	e := w.fresh(&out)
	st := web.CameraLoginState{Backend: e.cfg.Secrets.Backend,
		FromEnv: e.opts.Getenv(app.EnvCameraUser) != "" && e.opts.Getenv(app.EnvCameraPassword) != ""}
	p, err := openSecrets(e)
	if err != nil {
		st.StoreError = err.Error()
		return st, nil
	}
	st.Backend, st.ReadOnly = p.Backend(), p.Backend() == "env"
	names, err := p.List() // names only: a value is never read here
	if err != nil {
		st.StoreError = err.Error()
		return st, nil
	}
	for _, n := range names {
		st.User = st.User || n == secrets.CameraUserName
		st.Password = st.Password || n == secrets.CameraPasswordName
	}
	return st, nil
}

func (w webAdmin) SetCameraLogin(ctx context.Context, user, password string) (string, error) {
	var out bytes.Buffer
	e := w.fresh(&out)
	for _, s := range []struct{ name, value string }{{secrets.CameraUserName, user}, {secrets.CameraPasswordName, password}} {
		if s.value == "" {
			continue
		}
		if err := storeSecret(e, s.name, s.value); err != nil {
			return out.String(), adminErr(err)
		}
	}
	msg := out.String()
	if _, ok := dialRunning(e); ok {
		msg += "the recorder is running: holocron restart makes every camera use the new login\n"
	}
	return msg, nil
}

func (w webAdmin) PrunePreview(ctx context.Context) (any, error) {
	var out bytes.Buffer
	e := w.fresh(&out)
	if err := e.needConfig(); err != nil {
		return nil, err
	}
	rep, _, err := pruneDecide(ctx, e, false)
	return rep, adminErr(err)
}

func (w webAdmin) ArchiveStats(ctx context.Context) (any, error) {
	var out bytes.Buffer
	e := w.fresh(&out)
	if err := e.needConfig(); err != nil {
		return nil, err
	}
	st, err := collectStats(e)
	if err != nil {
		return nil, failed("%v", err)
	}
	return st, nil
}

// adminLevels is the log-level section: the running supervisor's levels (if one answers) and the
// configured ones.
type adminLevels struct {
	Running    bool              `json:"running"`
	Default    string            `json:"default,omitempty"`
	Components map[string]string `json:"components,omitempty"`
	Configured string            `json:"configured"`
	ConfigComp map[string]string `json:"configuredComponents"`
}

func (w webAdmin) LogLevels(ctx context.Context) (any, error) {
	var out bytes.Buffer
	e := w.fresh(&out)
	lv := adminLevels{Configured: e.cfg.Logging.Level, ConfigComp: e.cfg.Logging.Levels}
	if c, ok := dialRunning(e); ok {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if got, err := c.Levels(cctx); err == nil {
			lv.Running, lv.Default, lv.Components = true, got.Default, got.Components
		}
	}
	return lv, nil
}

func (w webAdmin) SetLogLevel(ctx context.Context, component, level string, save bool) (string, error) {
	var out bytes.Buffer
	e := w.fresh(&out)
	err := setLogLevel(ctx, e, component, level, save)
	return strings.TrimSpace(out.String()), adminErr(err)
}

func (w webAdmin) Autostart(ctx context.Context) (any, error) {
	var out bytes.Buffer
	return autostartReportOf(w.fresh(&out)), nil
}

func (w webAdmin) SetAutostart(ctx context.Context, on, oldTasksOK bool) (string, error) {
	var out bytes.Buffer
	err := autostartSet(w.fresh(&out), on, false, oldTasksOK, "")
	if err != nil && errors.Is(err, cli.ErrRefused) {
		return "", cli.Refuse("%s\n%s", strings.TrimPrefix(err.Error(), cli.ErrRefused.Error()+": "), strings.TrimSpace(out.String()))
	}
	return out.String(), adminErr(err)
}

// Health is `holocron doctor --json` run inside the web role (doctor_health.go): the same check
// table and document, with the differences drun.inWeb documents.
func (w webAdmin) Health(ctx context.Context, groups []string) (any, time.Time, error) {
	sel, key, err := healthSelection(groups)
	if err != nil {
		return nil, time.Time{}, err
	}
	run := func(ctx context.Context) doctorReport {
		var out bytes.Buffer
		return runHealth(ctx, w.fresh(&out), sel)
	}
	if w.health == nil {
		return run(ctx), time.Now(), nil
	}
	rep, at, err := w.health.get(ctx, key, run)
	return rep, at, err
}
