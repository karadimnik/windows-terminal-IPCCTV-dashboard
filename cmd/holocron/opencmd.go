package main

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"time"

	"holocron/internal/app"
)

// liveURL is the Live grid on the loopback listener (web.listen): #live, or #live/<camera>.
func liveURL(listen, camera string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if err != nil {
		port = "8787"
	}
	u := "http://" + net.JoinHostPort(host, port) + "/#live"
	if camera != "" {
		u += "/" + url.PathEscape(camera)
	}
	return u
}

// browserCommand is the OS's way to open a URL in the default browser.
func browserCommand(goos, u string) *exec.Cmd {
	switch goos {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	case "darwin":
		return exec.Command("open", u)
	}
	return exec.Command("xdg-open", u)
}

func openBrowser(goos, u string) error {
	cmd := browserCommand(goos, u)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func cmdOpen(ctx context.Context, e *env) error {
	fs := newFlags(e, "open")
	printOnly := fs.Bool("print", false, "only print the URL, open nothing")
	pos, err := parseFlags(fs, e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("open", pos, 0, 1); err != nil {
		return err
	}
	name := ""
	if len(pos) == 1 {
		cam, err := app.FindCamera(e.paths.Cameras, pos[0])
		if err != nil {
			return err
		}
		name = cam.Name
	}
	u := liveURL(e.cfg.Web.Listen, name)
	if *printOnly {
		fmt.Fprintln(e.out, u)
		return nil
	}
	if !e.cfg.Web.Enabled {
		return failed("the web app is switched off (web.enabled is false), so there is nothing to open")
	}
	c, err := net.DialTimeout("tcp", e.cfg.Web.Listen, 2*time.Second)
	if err != nil {
		return failed("the web app is not answering on %s: start it with holocron up (or holocron web) and run this again; holocron open --print shows the address", e.cfg.Web.Listen)
	}
	c.Close()
	opener := e.opener
	if opener == nil {
		opener = func(u string) error { return openBrowser(e.opts.Platform.Name(), u) }
	}
	if err := opener(u); err != nil {
		return failed("cannot open a browser (%v); open %s by hand", err, u)
	}
	fmt.Fprintf(e.out, "opened %s in the default browser\n", u)
	return nil
}
