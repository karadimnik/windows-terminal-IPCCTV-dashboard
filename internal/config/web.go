package config

import (
	"net"
	"strconv"
	"strings"
)

// DefaultWebListen is the web app's loopback address (camui's own default).
const DefaultWebListen = "127.0.0.1:8787"

// Web is the "web" object: the web role that `holocron up` runs beside the supervisor.
type Web struct {
	Enabled   bool   `json:"enabled"`   // `up` starts the web role
	Listen    string `json:"listen"`    // loopback listener; a non-loopback address needs CAMUI_TOKEN (the web app refuses otherwise)
	TLSListen string `json:"tlsListen"` // private IPv4:port of the LAN HTTPS listener; "" = off
	TLSDir    string `json:"tlsDir"`    // local CA and server certificate; "" = the web app's default folder
	AuthDir   string `json:"authDir"`   // users.json, sessions.json, auth.log; "" = the web app's default folder
}

func defaultWeb() Web { return Web{Enabled: true, Listen: DefaultWebListen} }

func validHostPort(s string) bool {
	_, port, err := net.SplitHostPort(s)
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535 && strings.TrimLeft(port, "0123456789") == ""
}

func (d *decoder) web(c *Config) {
	raw, ok := d.raw["web"]
	if !ok {
		return
	}
	keys, fields, ok := d.object("web", raw)
	if !ok {
		return
	}
	w := &c.Web
	for _, k := range keys {
		v, p := fields[k], "web."+k
		switch strings.ToLower(k) {
		case "enabled":
			d.subBool(p, v, &w.Enabled)
		case "listen", "tlslisten":
			var s string
			d.subStr(p, v, &s)
			if d.lastProblemOn(p) {
				continue
			}
			s = strings.TrimSpace(s)
			switch {
			case strings.ToLower(k) == "listen" && !validHostPort(s):
				d.problem(p, "must be host:port with a port from 1 to 65535, got %q", s)
			case strings.ToLower(k) == "tlslisten" && s != "" && !validHostPort(s):
				d.problem(p, "must be empty (off) or IPv4:port with a port from 1 to 65535, got %q", s)
			case strings.ToLower(k) == "listen":
				w.Listen = s
			default:
				w.TLSListen = s
			}
		case "tlsdir":
			d.subStr(p, v, &w.TLSDir)
		case "authdir":
			d.subStr(p, v, &w.AuthDir)
		default:
			d.warn(p, "unknown key, ignored")
		}
	}
}
