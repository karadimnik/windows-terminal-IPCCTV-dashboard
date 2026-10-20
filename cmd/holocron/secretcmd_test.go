package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// useFileSecrets points the rig at an encrypted store and key file under its scratch folder.
func (r *rig) useFileSecrets() string {
	r.t.Setenv("HOLOCRON_SECRET_KEY", "")
	file := filepath.Join(r.base(), "secrets", "s.enc")
	key := filepath.Join(r.base(), "secrets", "s.key")
	cfg, _ := os.ReadFile(r.cfg)
	add := `"secrets": {"backend": "file", "file": ` + jsonString(file) + `, "keyFile": ` + jsonString(key) + `}, "web"`
	os.WriteFile(r.cfg, []byte(strings.Replace(string(cfg), `"web"`, add, 1)), 0o644)
	return file
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestSecretSetListDelete(t *testing.T) {
	r := newRig(t)
	r.logSpec = "trace"
	file := r.useFileSecrets()
	const value = "s3cr3t-Value-That-Must-Not-Leak"

	r.stdin = value + "\n"
	out, errw, code := r.run("secret", "set", "push.token")
	if code != 0 || !strings.Contains(out, "stored push.token") || strings.Contains(out+errw, value) {
		t.Fatalf("set: %d %q %q", code, out, errw)
	}
	r.stdin = "u1\n"
	r.run("secret", "set", "camera.user")

	out, _, code = r.run("secret", "list")
	if code != 0 || !strings.Contains(out, "push.token") || !strings.Contains(out, "camera.user") || strings.Contains(out, value) {
		t.Errorf("list: %d %q", code, out)
	}
	out, _, _ = r.run("--json", "secret", "list")
	var l struct {
		Backend string   `json:"backend"`
		Names   []string `json:"names"`
	}
	if err := json.Unmarshal([]byte(out), &l); err != nil || l.Backend != "file" || len(l.Names) != 2 || l.Names[0] != "camera.user" {
		t.Errorf("list --json: %v %q", err, out)
	}

	if out, _, code := r.run("secret", "delete", "push.token"); code != 0 || !strings.Contains(out, "deleted push.token") {
		t.Errorf("delete: %d %q", code, out)
	}
	if _, errw, code := r.run("secret", "delete", "push.token"); code != 1 || !strings.Contains(errw, "not found") {
		t.Errorf("delete again: %d %q", code, errw)
	}

	// the store holds ciphertext; the value is nowhere on disk or in any log (positive control: the scan finds a planted copy)
	if _, err := os.Stat(file); err != nil {
		t.Fatal(err)
	}
	if hits := r.leaks(value); len(hits) > 0 {
		t.Errorf("secret found in %v", hits)
	}
	os.WriteFile(filepath.Join(r.root, "planted"), []byte(value), 0o644)
	if hits := r.leaks(value); len(hits) != 1 {
		t.Errorf("positive control: %v", hits)
	}
	for _, rec := range []string{"secret.set", "secret.delete", "secret.list"} {
		if !strings.Contains(r.log(), "action="+rec) {
			t.Errorf("no record for %s", rec)
		}
	}
}

func TestSecretInputRules(t *testing.T) {
	r := newRig(t)
	r.useFileSecrets()
	for _, c := range []struct {
		name  string
		stdin string
		rd    *failingReader
		args  []string
		code  int
		want  string
	}{
		{"value as an argument", "x\n", nil, []string{"secret", "set", "a.b", "oops"}, 2, "standard input only"},
		{"no name", "x\n", nil, []string{"secret", "set"}, 2, "wrong number of arguments"},
		{"bad name", "x\n", nil, []string{"secret", "set", "Bad Name"}, 2, "not a secret name"},
		{"upper case name", "x\n", nil, []string{"secret", "set", "Camera.User"}, 2, "not a secret name"},
		{"empty stdin", "", nil, []string{"secret", "set", "a.b"}, 2, "no value on standard input"},
		{"closed stdin", "", &failingReader{}, []string{"secret", "set", "a.b"}, 1, "cannot read standard input"},
		{"over-long value", strings.Repeat("v", 5000) + "\n", nil, []string{"secret", "set", "a.b"}, 1, "longer than 4096"},
		{"delete bad name", "", nil, []string{"secret", "delete", "NO"}, 2, "not a secret name"},
		{"import off Windows", "", nil, []string{"secret", "import"}, 1, "Windows only"},
	} {
		if runtime.GOOS == "windows" && c.name == "import off Windows" {
			continue
		}
		r.stdin, r.stdinR = c.stdin, nil
		if c.rd != nil {
			r.stdinR = c.rd
		}
		out, errw, code := r.run(c.args...)
		if code != c.code || !strings.Contains(out+errw, c.want) {
			t.Errorf("%s: exit %d want %d, out %q err %q", c.name, code, c.code, out, errw)
		}
	}
	r.stdinR = nil
	if out, _, _ := r.run("secret", "list"); !strings.Contains(out, "0 secret(s)") {
		t.Errorf("a refused set stored something: %q", out)
	}
	// a 4096-byte value is the largest accepted
	r.stdin = strings.Repeat("v", 4096) + "\n"
	if _, errw, code := r.run("secret", "set", "big.one"); code != 0 {
		t.Errorf("4096 bytes: %d %q", code, errw)
	}
}

func TestSecretImportNeedsAFileAndTellsWhereItLooked(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("secret import runs PowerShell's Import-Clixml: Windows only (the Windows run covers it with a FAKE file)")
	}
	r := newRig(t)
	r.useFileSecrets()
	_, errw, code := r.run("secret", "import", "--from", filepath.Join(r.base(), "missing.xml"))
	if code != 1 || !strings.Contains(errw, "not a readable file") {
		t.Errorf("%d %q", code, errw)
	}
}

// The camera login: the environment first, then the configured store; a missing login names both.
func TestDoctorReadsTheCameraLoginFromTheStore(t *testing.T) {
	r := newRig(t)
	r.useFileSecrets()
	r.noCreds = true
	out, _, _ := r.run("doctor")
	if !strings.Contains(out, "HOLOCRON_CAMERA_USER") || !strings.Contains(out, "holocron secret set camera.user") {
		t.Errorf("a missing login must name both places: %q", out)
	}
	r.stdin = "cam-user\n"
	r.run("secret", "set", "camera.user")
	r.stdin = "cam-pass-9\n"
	r.run("secret", "set", "camera.password")
	out, _, _ = r.run("doctor")
	if !strings.Contains(out, "a camera login is present") || strings.Contains(out, "cam-pass-9") {
		t.Errorf("credentials line: %q", out)
	}
	if hits := r.leaks("cam-pass-9"); len(hits) > 0 {
		t.Errorf("password found in %v", hits)
	}
}
