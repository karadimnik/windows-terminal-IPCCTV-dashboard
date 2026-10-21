package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"

	"holocron/internal/app"
	"holocron/internal/config"
	"holocron/internal/ctlapi"
)

func cmdConfigGet(ctx context.Context, e *env) error {
	pos, err := parseFlags(newFlags(e, "config get"), e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("config get", pos, 1, 1); err != nil {
		return err
	}
	if err := e.needConfig(); err != nil {
		return err
	}
	v, err := app.EffectiveValue(e.cfg, pos[0])
	if err != nil {
		return err
	}
	if s, ok := v.(string); ok && !e.json {
		fmt.Fprintln(e.out, s)
		return nil
	}
	return printJSON(e.out, v)
}

func cmdConfigShow(ctx context.Context, e *env) error {
	pos, err := parseFlags(newFlags(e, "config show"), e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("config show", pos, 0, 0); err != nil {
		return err
	}
	if err := e.needConfig(); err != nil {
		return err
	}
	m, err := app.EffectiveConfig(e.cfg)
	if err != nil {
		return err
	}
	if !e.json {
		fmt.Fprintf(e.out, "# %s (effective values; editable keys: holocron config set)\n", e.paths.Config)
	}
	return printJSON(e.out, m)
}

func cmdConfigSet(ctx context.Context, e *env) error {
	pos, err := parseFlags(flag.NewFlagSet("config set", flag.ContinueOnError), e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("config set", pos, 2, 2); err != nil {
		return err
	}
	return configEdit(ctx, e, pos[0], app.ParseValue(pos[1]))
}

func cmdConfigUnset(ctx context.Context, e *env) error {
	pos, err := parseFlags(flag.NewFlagSet("config unset", flag.ContinueOnError), e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("config unset", pos, 1, 1); err != nil {
		return err
	}
	return configEdit(ctx, e, pos[0], nil)
}

func configEdit(ctx context.Context, e *env, key string, value json.RawMessage) error {
	ed, err := app.EditConfig(e.paths.Config, key, value, e.opts.Platform)
	if err != nil {
		return err
	}
	if !ed.Changed {
		fmt.Fprintf(e.out, "%s: unchanged\n", ed.Key)
		return nil
	}
	fmt.Fprintf(e.out, "%s: %s -> %s (%s)\n", ed.Key, rawText(ed.Old), rawText(ed.New), e.paths.Config)
	if c, ok := dialRunning(e); ok {
		if strings.EqualFold(ed.Key, "LiveViewEnabled") {
			if err := c.Reload(ctx); err == nil {
				fmt.Fprintln(e.out, "the running supervisor applied it")
				return nil
			}
		}
		fmt.Fprintln(e.out, "the running supervisor keeps its values until holocron restart (live view: holocron live enable|disable; levels: holocron log level)")
	}
	return nil
}

func rawText(r json.RawMessage) string {
	if r == nil {
		return "(default)"
	}
	return string(r)
}

func cmdConfigValidate(ctx context.Context, e *env) error {
	if err := wantArgs("config validate", e.args, 0, 0); err != nil {
		return err
	}
	bad := false
	if e.loadErr != nil {
		bad = true
		var ve *config.ValidationError
		if errors.As(e.loadErr, &ve) {
			for _, p := range ve.Problems {
				fmt.Fprintf(e.out, "FAIL %s: %s\n", e.paths.Config, p)
			}
		} else {
			fmt.Fprintf(e.out, "FAIL %v\n", e.loadErr)
		}
	} else {
		fmt.Fprintf(e.out, "ok   %s\n", e.paths.Config)
	}
	for _, w := range e.warns {
		fmt.Fprintf(e.out, "warn %s: %s\n", e.paths.Config, w)
	}
	cams, err := config.LoadCameras(e.paths.Cameras)
	if err == nil {
		err = cams.Validate()
	}
	if err != nil {
		bad = true
		fmt.Fprintf(e.out, "FAIL %s: %v\n", e.paths.Cameras, err)
	} else {
		fmt.Fprintf(e.out, "ok   %s (%d camera(s))\n", e.paths.Cameras, len(cams.List))
	}
	if bad {
		return quietFail("the configuration or the camera list is invalid")
	}
	return nil
}

func cmdLive(ctx context.Context, e *env, on bool) error {
	if err := wantArgs("live", e.args, 0, 0); err != nil {
		return err
	}
	word := map[bool]string{true: "on", false: "off"}[on]
	if c, ok := dialRunning(e); ok {
		ch, err := c.SetLive(ctx, on)
		if err != nil {
			return err
		}
		fmt.Fprintf(e.out, "live view %s (the running supervisor applied it; config changed: %v)\n", word, ch.Changed)
		return nil
	}
	ed, err := app.EditConfig(e.paths.Config, "LiveViewEnabled", app.ParseValue(fmt.Sprint(on)), e.opts.Platform)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.out, "live view %s in %s (changed: %v); the supervisor is not running, it applies at the next start\n", word, e.paths.Config, ed.Changed)
	return nil
}

func cmdLiveStatus(ctx context.Context, e *env) error {
	pos, err := parseFlags(newFlags(e, "live status"), e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("live status", pos, 0, 0); err != nil {
		return err
	}
	type liveReport struct {
		Configured bool              `json:"configured"`
		Whep       string            `json:"whep"`
		Udp        string            `json:"udp"`
		Running    bool              `json:"running"`
		Live       *ctlapi.LiveState `json:"live"`
	}
	r := liveReport{Configured: e.cfg.LiveViewEnabled, Whep: e.cfg.LiveWhepAddress, Udp: e.cfg.LiveUdpAddress}
	if c, ok := dialRunning(e); ok {
		if st, err := c.State(ctx); err == nil {
			r.Running, r.Live = true, st.Live
		}
	}
	if e.json {
		return printJSON(e.out, r)
	}
	fmt.Fprintf(e.out, "configured: %v (WHEP %s, media %s)\n", r.Configured, r.Whep, r.Udp)
	switch {
	case !r.Running:
		fmt.Fprintln(e.out, "process: the supervisor is not running")
	case r.Live == nil || !r.Live.Enabled:
		fmt.Fprintln(e.out, "process: off")
	default:
		fmt.Fprintf(e.out, "process: alive=%v%s%s\n", r.Live.Alive, pidText(r.Live.Pid), errText(r.Live.LastError))
	}
	return nil
}

func cmdLogLevel(ctx context.Context, e *env) error {
	fs := flag.NewFlagSet("log level", flag.ContinueOnError)
	save := fs.Bool("save", false, "also write it to the configuration")
	pos, err := parseFlags(fs, e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("log level", pos, 2, 2); err != nil {
		return err
	}
	return setLogLevel(ctx, e, pos[0], pos[1], *save)
}

// setLogLevel is `log level` once the arguments are parsed (the Admin tab calls it too).
func setLogLevel(ctx context.Context, e *env, comp, level string, save bool) error {
	comp = strings.ToLower(comp)
	applied := false
	if c, ok := dialRunning(e); ok {
		lv, err := c.SetLevel(ctx, comp, level)
		if err != nil {
			return err
		}
		applied = true
		fmt.Fprintf(e.out, "running supervisor: default %s%s\n", lv.Default, componentsText(lv.Components))
	}
	if save {
		key := "logging.levels." + comp
		if comp == "all" {
			key = "logging.level"
		}
		if err := configEdit(ctx, e, key, app.ParseValue(strings.ToLower(level))); err != nil {
			return err
		}
	}
	if !applied && !save {
		return failed("the supervisor is not running (or its control endpoint does not answer): nothing to change; use --save to write it to the configuration")
	}
	return nil
}

func componentsText(m map[string]string) string {
	var parts []string
	for _, k := range sortedKeys(m) {
		parts = append(parts, k+"="+m[k])
	}
	if len(parts) == 0 {
		return ""
	}
	return ", " + strings.Join(parts, ", ")
}
