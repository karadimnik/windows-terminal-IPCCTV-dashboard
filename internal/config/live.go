package config

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	whepRE    = regexp.MustCompile(`^127\.0\.0\.1:([0-9]{1,5})$`)
	udpRE     = regexp.MustCompile(`^([0-9.]+):([0-9]{1,5})$`)
	subStream = regexp.MustCompile(`^[A-Za-z0-9_\-]{1,32}$`)
	quadRE    = regexp.MustCompile(`^(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})$`)
)

// privateIPv4 is Test-CamPrivateIPv4: a strict dotted quad in RFC 1918 space, no leading zeros.
func privateIPv4(s string) bool {
	m := quadRE.FindStringSubmatch(s)
	if m == nil {
		return false
	}
	var o [4]int
	for i := 0; i < 4; i++ {
		o[i], _ = strconv.Atoi(m[i+1])
		if o[i] > 255 {
			return false
		}
	}
	return o[0] == 10 || (o[0] == 172 && o[1] >= 16 && o[1] <= 31) || (o[0] == 192 && o[1] == 168)
}

func port(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 65535
}

// live ports Assert-CamLiveConfig: checked whether or not live view is on, so a typo cannot sit
// unnoticed until the day it is switched on.
func (d *decoder) live(c *Config) {
	d.boolean("LiveViewEnabled", &c.LiveViewEnabled)
	d.str("LiveExePath", &c.LiveExePath)
	d.str("LiveConfigPath", &c.LiveConfigPath)
	d.str("LiveSecretPath", &c.LiveSecretPath)
	d.str("LiveWhepAddress", &c.LiveWhepAddress)
	d.str("LiveUdpAddress", &c.LiveUdpAddress)
	d.str("LiveSubStream", &c.LiveSubStream)
	d.integer("LiveCloseAfterSeconds", &c.LiveCloseAfterSeconds, 1)
	if _, ok := d.raw["LiveCloseAfterSeconds"]; ok && !d.lastProblemOn("LiveCloseAfterSeconds") && c.LiveCloseAfterSeconds > 3600 {
		d.problem("LiveCloseAfterSeconds", "must be a whole number from 1 to 3600, got %d", c.LiveCloseAfterSeconds)
	}
	if raw, ok := d.raw["LiveAdvertiseHosts"]; ok {
		if jsonKind(raw) == "null" {
			c.LiveAdvertiseHosts = []string{}
		} else {
			var hosts []string
			d.subList("LiveAdvertiseHosts", raw, &hosts)
			c.LiveAdvertiseHosts = hosts
			if hosts == nil {
				c.LiveAdvertiseHosts = []string{}
			}
		}
	}
	if d.hasProblemUnder("Live") { // do not judge relationships between values that failed to decode
		return
	}

	for _, k := range []struct{ key, val string }{{"LiveExePath", c.LiveExePath}, {"LiveConfigPath", c.LiveConfigPath}, {"LiveSecretPath", c.LiveSecretPath}} {
		if strings.HasPrefix(k.val, `\\`) {
			d.problem(k.key, "'%s' is a network or WSL share path. MediaMTX cannot watch its config there and a share cannot hold the secret's access control. Use a local path.", k.val)
		}
	}
	if strings.TrimSpace(c.LiveConfigPath) == "" {
		d.problem("LiveConfigPath", "must not be empty")
	}

	if m := whepRE.FindStringSubmatch(c.LiveWhepAddress); m == nil || !port(m[1]) {
		d.problem("LiveWhepAddress", "must be 127.0.0.1 and a port such as 127.0.0.1:8889, got '%s'. Signaling is loopback only: browsers reach it through camui.", c.LiveWhepAddress)
	}
	udpHost := ""
	if m := udpRE.FindStringSubmatch(c.LiveUdpAddress); m == nil || !port(m[2]) {
		d.problem("LiveUdpAddress", "must be an IPv4 address and a port such as 127.0.0.1:8189, got '%s'.", c.LiveUdpAddress)
	} else if udpHost = m[1]; udpHost != "127.0.0.1" && udpHost != "0.0.0.0" && !privateIPv4(udpHost) {
		d.problem("LiveUdpAddress", "host must be 127.0.0.1, 0.0.0.0 or a private (10.x, 172.16-31.x, 192.168.x) IPv4 address of this PC, got '%s'. Names, IPv6 and public addresses are refused.", udpHost)
		udpHost = ""
	}

	adv := c.LiveAdvertiseHosts
	if len(adv) > 4 {
		d.problem("LiveAdvertiseHosts", "may list at most 4 addresses")
	}
	seen := map[string]bool{}
	hostsOK := true
	for i, h := range adv {
		if !privateIPv4(h) {
			d.problem("LiveAdvertiseHosts["+strconv.Itoa(i)+"]", "must be a private (10.x, 172.16-31.x, 192.168.x) IPv4 literal such as 192.168.1.50 - no names, IPv6 or public addresses - got '%s'", h)
			hostsOK = false
		}
		if seen[h] {
			d.problem("LiveAdvertiseHosts", "lists the same address twice")
			hostsOK = false
		}
		seen[h] = true
	}
	if udpHost != "" && hostsOK && len(adv) <= 4 {
		switch {
		case len(adv) == 0 && udpHost != "127.0.0.1":
			d.problem("LiveAdvertiseHosts", "LiveUdpAddress '%s' listens beyond loopback but LiveAdvertiseHosts is empty, so no browser would be told to use it. List the address(es) browsers reach this PC by, or set LiveUdpAddress back to 127.0.0.1.", c.LiveUdpAddress)
		case len(adv) > 0 && udpHost == "127.0.0.1":
			d.problem("LiveAdvertiseHosts", "is set but LiveUdpAddress is '%s', which only loopback can reach. Use 0.0.0.0:<port> or this PC's private address.", c.LiveUdpAddress)
		case len(adv) > 0 && udpHost != "0.0.0.0":
			for _, h := range adv {
				if h != udpHost {
					d.problem("LiveAdvertiseHosts", "LiveUdpAddress is bound to %s only, so LiveAdvertiseHosts may list only %s. Use 0.0.0.0:<port> to serve several addresses.", udpHost, udpHost)
					break
				}
			}
		}
	}

	if !subStream.MatchString(c.LiveSubStream) {
		d.problem("LiveSubStream", "must be a camera stream name such as stream2 (letters, digits, - and _), got '%s'", c.LiveSubStream)
	}

	// the live instance must not take an address the relay owns
	for _, k := range []struct{ key, url string }{{"RelayBase", c.RelayBase}, {"RelayApiBase", c.RelayApiBase}} {
		if k.url == "" {
			continue
		}
		if hp, err := HostPort(k.url); err == nil && hp == c.LiveWhepAddress {
			d.problem("LiveWhepAddress", "'%s' is the address of the recording relay (%s)", c.LiveWhepAddress, k.key)
		}
	}
}

func (d *decoder) lastProblemOn(key string) bool {
	for _, p := range d.problems {
		if p.Path == key {
			return true
		}
	}
	return false
}
