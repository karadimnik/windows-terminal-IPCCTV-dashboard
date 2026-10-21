package osx

import "strings"

// Env is the input of the pure default-path and protected-set builders, so every OS's table can be
// tested on any host.
type Env struct {
	Getenv func(string) string
	Home   string   // the user's home / profile directory
	ExeDir string   // directory of the running program
	Mounts []string // mount points (unix); empty on Windows
}

func (e Env) get(k string) string {
	if e.Getenv == nil {
		return ""
	}
	return e.Getenv(k)
}

func sep(goos string) string {
	if goos == "windows" {
		return `\`
	}
	return "/"
}

func join(goos string, parts ...string) string {
	s := sep(goos)
	out := strings.TrimRight(parts[0], s)
	for _, p := range parts[1:] {
		out += s + strings.Trim(p, s)
	}
	return out
}

func absolute(goos, p string) bool {
	if goos == "windows" {
		return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') || strings.HasPrefix(p, `\\`)
	}
	return strings.HasPrefix(p, "/")
}

// DefaultDirs is the table of LLD section 4 for one OS. On Windows the config folder is the
// program's own folder (where recorder.config.json and cameras.json live today) and the auth and
// TLS folders are the ones camui already uses, so the shadow phases share them.
func DefaultDirs(goos string, e Env) Dirs {
	d := Dirs{}
	var base string // state/auth/TLS base
	switch goos {
	case "windows":
		d.Root = `E:\cameras`
		local := e.get("LOCALAPPDATA")
		if local == "" && e.Home != "" {
			local = join(goos, e.Home, "AppData", "Local")
		}
		d.Config = e.ExeDir
		if d.Config == "" && local != "" {
			d.Config = join(goos, local, "holocron")
		}
		if local != "" {
			d.Auth = join(goos, local, "IPCCTV", "camui", "auth")
			d.TLS = join(goos, local, "IPCCTV", "tls")
		}
	case "darwin":
		d.Root = join(goos, e.Home, "Movies", "cameras")
		base = join(goos, e.Home, "Library", "Application Support", "holocron")
		d.Config = base
	default:
		d.Root = "/srv/cameras"
		d.Config = xdg(goos, e, "XDG_CONFIG_HOME", ".config")
		base = xdg(goos, e, "XDG_STATE_HOME", join(goos, ".local", "state"))
	}
	if base != "" {
		d.Auth = join(goos, base, "auth")
		d.TLS = join(goos, base, "tls")
	}
	d.State = join(goos, d.Root, "_state")
	d.Logs = join(goos, d.Root, "_logs")
	return d
}

func xdg(goos string, e Env, key, fallback string) string {
	if v := e.get(key); v != "" && absolute(goos, v) {
		return join(goos, v, "holocron")
	}
	return join(goos, e.Home, fallback, "holocron")
}

// ProtectedFor builds the protected set of one OS (IR section 5 and 10 item 6): system folders, the
// user's profile and the folder holding profiles, the program's own folder, and on unix the mount
// points. Drive roots, UNC share roots and "/" are judged structurally by the archive guard.
func ProtectedFor(goos string, e Env) []Protected {
	var out []Protected
	add := func(path, why string) {
		if strings.TrimSpace(path) != "" {
			out = append(out, Protected{Path: path, Why: why})
		}
	}
	switch goos {
	case "windows":
		add(firstOf(e.get("SystemRoot"), e.get("windir")), "the Windows directory")
		add(e.get("ProgramW6432"), "Program Files")
		add(e.get("ProgramFiles"), "Program Files")
		add(e.get("ProgramFiles(x86)"), "Program Files (x86)")
		profile := firstOf(e.get("USERPROFILE"), e.Home)
		add(profile, "the user profile")
		add(parentOf(goos, profile), "the folder holding the user profiles")
	case "darwin":
		for _, p := range []string{"/System", "/Library", "/Applications", "/Users", "/Volumes", "/private", "/usr", "/bin", "/sbin", "/etc", "/var", "/opt", "/cores", "/dev", "/tmp"} {
			add(p, "a system folder")
		}
		addHome(goos, e, add)
	default:
		for _, p := range []string{"/bin", "/boot", "/dev", "/etc", "/home", "/lib", "/lib32", "/lib64", "/media", "/mnt", "/opt", "/proc", "/root", "/run", "/sbin", "/srv", "/sys", "/tmp", "/usr", "/var"} {
			add(p, "a system folder")
		}
		addHome(goos, e, add)
	}
	for _, m := range e.Mounts {
		add(m, WhyMount)
	}
	add(e.ExeDir, "the program's own directory")
	return out
}

func addHome(goos string, e Env, add func(string, string)) {
	if e.Home == "" {
		return
	}
	add(e.Home, "the home directory")
	add(parentOf(goos, e.Home), "the folder holding the home directories")
}

func firstOf(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

func parentOf(goos, p string) string {
	s := sep(goos)
	p = strings.TrimRight(p, s)
	i := strings.LastIndex(p, s)
	if i <= 0 {
		return ""
	}
	if goos == "windows" && i == 2 && p[1] == ':' { // C:\Users -> C:\
		return p[:3]
	}
	return p[:i]
}
