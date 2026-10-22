package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"holocron/internal/app"
	"holocron/internal/config"
	"holocron/internal/status"
)

type camRow struct {
	Name     string         `json:"name"`
	IP       string         `json:"ip"`
	Quality  string         `json:"quality"`
	Record   bool           `json:"record"`
	SafeName string         `json:"safeName"`
	Problem  string         `json:"problem,omitempty"`
	Recorder *status.Camera `json:"recorder,omitempty"`
}

func camRows(e *env) ([]camRow, error) {
	c, err := app.LoadCameras(e.paths.Cameras)
	if err != nil {
		return nil, err
	}
	var live map[string]status.Camera
	if doc, ok, _ := app.Running(e.paths, e.cfg, time.Now()); ok {
		live = map[string]status.Camera{}
		for _, r := range doc.Cameras {
			live[r.Name] = r
		}
	}
	rows := []camRow{}
	for _, cam := range c.List {
		r := camRow{Name: cam.Name, IP: cam.IP, Quality: cam.Stream(), Record: cam.Record}
		if s, err := cam.SafeName(); err != nil {
			r.Problem = err.Error()
		} else {
			r.SafeName = s
		}
		if st, ok := live[cam.Name]; ok {
			st := st
			r.Recorder = &st
		}
		rows = append(rows, r)
	}
	return rows, nil
}

func cmdCamList(ctx context.Context, e *env) error {
	pos, err := parseFlags(newFlags(e, "cam list"), e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("cam list", pos, 0, 0); err != nil {
		return err
	}
	rows, err := camRows(e)
	if err != nil {
		return err
	}
	if e.json {
		return printJSON(e.out, rows)
	}
	_, running, why := app.Running(e.paths, e.cfg, time.Now())
	fmt.Fprintf(e.out, "%-20s %-7s %-10s %-16s %s\n", "NAME", "RECORD", "QUALITY", "IP", "RECORDER")
	for _, r := range rows {
		rec := "off"
		if r.Record {
			rec = "ON"
		}
		state := "-"
		if r.Recorder != nil {
			state = "alive " + strconv.FormatBool(r.Recorder.Alive)
		}
		if r.Problem != "" {
			state = "PROBLEM: " + r.Problem
		}
		fmt.Fprintf(e.out, "%-20s %-7s %-10s %-16s %s\n", r.Name, rec, r.Quality, r.IP, state)
	}
	if running {
		fmt.Fprintln(e.out, "supervisor: running")
	} else {
		fmt.Fprintf(e.out, "supervisor: not running (%s)\n", why)
	}
	return nil
}

func cmdCamShow(ctx context.Context, e *env) error {
	pos, err := parseFlags(newFlags(e, "cam show"), e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("cam show", pos, 1, 1); err != nil {
		return err
	}
	cam, err := app.FindCamera(e.paths.Cameras, pos[0])
	if err != nil {
		return err
	}
	rows, _ := camRows(e)
	for _, r := range rows {
		if r.Name != cam.Name {
			continue
		}
		if e.json {
			return printJSON(e.out, r)
		}
		fmt.Fprintf(e.out, "name: %s\nfolder: %s\naddress: %s\nstream: %s\nrecord: %v\n", r.Name, orDash(r.SafeName), r.IP, r.Quality, r.Record)
		if r.Problem != "" {
			fmt.Fprintf(e.out, "problem: %s\n", r.Problem)
		}
		if s := r.Recorder; s != nil {
			fmt.Fprintf(e.out, "recorder: alive=%v input=%s%s restarts %d (%d last hour)%s\n", s.Alive, orDash(s.InputMode), pidText(s.Pid), s.RestartsTotal, s.RestartsLastHour, errText(s.LastError))
		} else {
			fmt.Fprintln(e.out, "recorder: not running for this camera")
		}
	}
	return nil
}

func cmdCamAdd(ctx context.Context, e *env) error {
	fs := flag.NewFlagSet("cam add", flag.ContinueOnError)
	name := fs.String("name", "", "camera name (its folder name)")
	ip := fs.String("ip", "", "camera address (IP or hostname)")
	quality := fs.String("quality", "", "stream path (default stream1)")
	record := fs.Bool("record", false, "record it")
	pos, err := parseFlags(fs, e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("cam add", pos, 0, 0); err != nil {
		return err
	}
	if *name == "" || *ip == "" {
		return usageErr("holocron cam add: --name and --ip are required")
	}
	if err := app.AddCamera(e.paths.Cameras, config.Camera{Name: *name, IP: *ip, Quality: *quality, Record: *record}); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "added %s (%s, record %v) to %s\n", *name, *ip, *record, e.paths.Cameras)
	reloadIfRunning(ctx, e)
	return nil
}

func cmdCamRemove(ctx context.Context, e *env) error {
	pos, err := parseFlags(flag.NewFlagSet("cam remove", flag.ContinueOnError), e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("cam remove", pos, 1, 1); err != nil {
		return err
	}
	gone, err := app.RemoveCamera(e.paths.Cameras, pos[0])
	if err != nil {
		return err
	}
	fmt.Fprintf(e.out, "removed %s; its recordings stay on disk\n", gone.Name)
	reloadIfRunning(ctx, e)
	return nil
}

func cmdCamSet(ctx context.Context, e *env) error {
	fs := flag.NewFlagSet("cam set", flag.ContinueOnError)
	var ch app.CameraChange
	fs.Func("name", "new name", func(s string) error { ch.Name = &s; return nil })
	fs.Func("ip", "address", func(s string) error { ch.IP = &s; return nil })
	fs.Func("quality", "stream path", func(s string) error { ch.Quality = &s; return nil })
	fs.Func("record", "true or false", func(s string) error {
		b, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("true or false, got %q", s)
		}
		ch.Record = &b
		return nil
	})
	pos, err := parseFlags(fs, e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("cam set", pos, 1, 1); err != nil {
		return err
	}
	if ch == (app.CameraChange{}) {
		return usageErr("holocron cam set: nothing to change (--name, --ip, --quality, --record)")
	}
	cam, err := app.SetCamera(e.paths.Cameras, pos[0], ch)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.out, "%s: address %s, stream %s, record %v\n", cam.Name, cam.IP, cam.Stream(), cam.Record)
	reloadIfRunning(ctx, e)
	return nil
}

func cmdCamRecord(ctx context.Context, e *env, on bool) error {
	verb := map[bool]string{true: "enable", false: "disable"}[on]
	pos, err := parseFlags(flag.NewFlagSet("cam "+verb, flag.ContinueOnError), e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("cam "+verb, pos, 1, 1); err != nil {
		return err
	}
	changes, err := app.SetRecord(e.paths.Cameras, pos[0], on)
	if err != nil {
		return err
	}
	any := false
	for _, ch := range changes {
		if ch.Changed {
			any = true
			fmt.Fprintf(e.out, "%s: record %s\n", ch.Target, map[bool]string{true: "ON", false: "off"}[on])
		} else {
			fmt.Fprintf(e.out, "%s: already %s\n", ch.Target, map[bool]string{true: "ON", false: "off"}[on])
		}
	}
	if any {
		reloadIfRunning(ctx, e)
	}
	return nil
}

// rtspTarget splits the camera address into host:port (554 unless the address carries a port).
func rtspTarget(cam config.Camera) string {
	if _, _, err := net.SplitHostPort(cam.IP); err == nil {
		return cam.IP
	}
	return net.JoinHostPort(cam.IP, "554")
}

// probeRTSP connects and sends OPTIONS (no login: a 401 is an answer too).
func probeRTSP(ctx context.Context, cam config.Camera, timeout time.Duration) (string, error) {
	return probeRTSPWith(ctx, (&net.Dialer{}).DialContext, cam, timeout)
}

// probeRTSPWith is probeRTSP through dial (doctor's seam).
func probeRTSPWith(ctx context.Context, dial func(ctx context.Context, network, addr string) (net.Conn, error), cam config.Camera, timeout time.Duration) (string, error) {
	addr := rtspTarget(cam)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("TCP %s: %v", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	url := "rtsp://" + addr + "/" + cam.Stream()
	if _, err := fmt.Fprintf(conn, "OPTIONS %s RTSP/1.0\r\nCSeq: 1\r\nUser-Agent: holocron\r\n\r\n", url); err != nil {
		return "", fmt.Errorf("RTSP OPTIONS: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("TCP %s open, but no RTSP answer: %v", addr, err)
	}
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "RTSP/") {
		return "", fmt.Errorf("TCP %s open, but it does not speak RTSP (%q)", addr, line)
	}
	return line, nil
}

func cmdCamTest(ctx context.Context, e *env) error {
	pos, err := parseFlags(flag.NewFlagSet("cam test", flag.ContinueOnError), e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("cam test", pos, 1, 1); err != nil {
		return err
	}
	cam, err := app.FindCamera(e.paths.Cameras, pos[0])
	if err != nil {
		return err
	}
	answer, err := probeRTSP(ctx, cam, 5*time.Second)
	if err != nil {
		fmt.Fprintf(e.out, "%s: unreachable - %v\n", cam.Name, err)
		return quietFail("%s unreachable: %v", cam.Name, err)
	}
	fmt.Fprintf(e.out, "%s: reachable - %s answered %q\n", cam.Name, rtspTarget(cam), answer)
	return nil
}
