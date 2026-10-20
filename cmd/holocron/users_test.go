package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodPassword = "correct horse battery staple"

// base is the rig's whole scratch tree.
func (r *rig) base() string { return filepath.Dir(r.root) }

func (r *rig) authDir() string { return filepath.Join(r.base(), "auth") }

// leaks lists the files under the rig's tree that contain needle.
func (r *rig) leaks(needle string) []string {
	var hits []string
	filepath.WalkDir(r.base(), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, rerr := os.ReadFile(path); rerr == nil && strings.Contains(string(b), needle) {
				hits = append(hits, path)
			}
		}
		return nil
	})
	return hits
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("file already closed") }

func TestUserAddListPasswdRemove(t *testing.T) {
	r := newRig(t)
	r.logSpec = "trace"

	r.stdin = goodPassword + "\n"
	out, errw, code := r.run("user", "add", "alice")
	if code != 0 || !strings.Contains(out, "created user alice") {
		t.Fatalf("add: %d %q %q", code, out, errw)
	}
	before, _ := os.ReadFile(filepath.Join(r.authDir(), "users.json"))

	// the same name again (any case) is refused, not turned into a password change
	r.stdin = goodPassword + "\n"
	if _, errw, code := r.run("user", "add", "ALICE"); code != 1 || !strings.Contains(errw, "exists already") {
		t.Errorf("duplicate: %d %q", code, errw)
	}
	// passwd of an unknown user
	r.stdin = goodPassword + "\n"
	if _, errw, code := r.run("user", "passwd", "nobody"); code != 1 || !strings.Contains(errw, "no such user") {
		t.Errorf("unknown: %d %q", code, errw)
	}

	r.stdin = "another very long passphrase\r\n"
	if out, errw, code := r.run("user", "passwd", "alice"); code != 0 || !strings.Contains(out, "session") {
		t.Fatalf("passwd: %d %q %q", code, out, errw)
	}
	after, _ := os.ReadFile(filepath.Join(r.authDir(), "users.json"))
	if string(before) == string(after) {
		t.Error("the stored hash did not change")
	}

	// list: names, role, created - never a hash
	out, _, code = r.run("user", "list")
	if code != 0 || !strings.Contains(out, "alice") || !strings.Contains(out, "admin") || strings.Contains(out, "pbkdf2") {
		t.Errorf("list: %d %q", code, out)
	}
	out, _, code = r.run("--json", "user", "list")
	var users []map[string]any
	if err := json.Unmarshal([]byte(out), &users); err != nil || code != 0 || len(users) != 1 || users[0]["username"] != "alice" || users[0]["role"] != "admin" || users[0]["createdAt"] == nil {
		t.Fatalf("list --json: %v %q", err, out)
	}
	for k := range users[0] {
		if strings.Contains(strings.ToLower(k), "hash") {
			t.Errorf("list --json carries %q", k)
		}
	}

	// remove: the last user may go when no TLS listener is configured
	if out, _, code := r.run("user", "remove", "alice"); code != 0 || !strings.Contains(out, "removed user alice") {
		t.Errorf("remove: %d %q", code, out)
	}
	if _, errw, code := r.run("user", "remove", "alice"); code != 1 || !strings.Contains(errw, "no such user") {
		t.Errorf("remove again: %d %q", code, errw)
	}
	if out, _, _ := r.run("user", "list"); !strings.Contains(out, "no users") {
		t.Errorf("empty list: %q", out)
	}

	// no password, hash or sentinel reached any log, output or file the tool wrote
	for _, secret := range []string{goodPassword, "another very long passphrase"} {
		if hits := r.leaks(secret); len(hits) > 0 {
			t.Errorf("%q found in %v", secret, hits)
		}
	}
	for _, rec := range []string{"user.add", "user.passwd", "user.remove", "user.list"} {
		if !strings.Contains(r.log(), "action="+rec) {
			t.Errorf("no record for %s", rec)
		}
	}
	audit, _ := os.ReadFile(filepath.Join(r.root, "_logs", "audit.log"))
	for _, rec := range []string{"user.add", "user.passwd", "user.remove", "user.list"} {
		if !strings.Contains(string(audit), "action="+rec) {
			t.Errorf("%s is not in the audit stream", rec)
		}
	}
}

// The leak scan itself must be able to find a password: plant one.
func TestLeakScanHasAPositiveControl(t *testing.T) {
	r := newRig(t)
	os.MkdirAll(r.root, 0o755)
	os.WriteFile(filepath.Join(r.root, "planted.txt"), []byte("x "+goodPassword+" y"), 0o644)
	if hits := r.leaks(goodPassword); len(hits) != 1 {
		t.Fatalf("the scan found %v", hits)
	}
}

func TestUserPasswordInput(t *testing.T) {
	r := newRig(t)
	for _, c := range []struct {
		name  string
		stdin string
		rd    io.Reader
		args  []string
		code  int
		want  string
	}{
		{"too short", "short\n", nil, []string{"user", "add", "bob"}, 2, "at least 12"},
		{"too long", strings.Repeat("a", 129) + "\n", nil, []string{"user", "add", "bob"}, 2, "128"},
		{"equals the name", "bobbobbobbobbob\n", nil, []string{"user", "add", "bobbobbobbobbob"}, 2, "username"},
		{"bad name", goodPassword + "\n", nil, []string{"user", "add", "bad name"}, 2, "username"},
		{"empty stdin", "", nil, []string{"user", "add", "bob"}, 2, "no value on standard input"},
		{"blank line", "\n", nil, []string{"user", "add", "bob"}, 2, "no value on standard input"},
		{"closed stdin", "", failingReader{}, []string{"user", "add", "bob"}, 1, "cannot read standard input"},
		{"over-long line", strings.Repeat("p", 5000) + "\n", nil, []string{"user", "add", "bob"}, 1, "longer than"},
		{"no newline at EOF is fine only if valid", goodPassword, nil, []string{"user", "add", "carol"}, 0, "created user carol"},
		{"password as an argument", goodPassword + "\n", nil, []string{"user", "add", "bob", goodPassword}, 2, "wrong number of arguments"},
		{"no name", goodPassword + "\n", nil, []string{"user", "add"}, 2, "wrong number of arguments"},
		{"unknown flag", goodPassword + "\n", nil, []string{"user", "add", "bob", "--password", "x"}, 2, "password"},
	} {
		r.stdin, r.stdinR = c.stdin, c.rd
		out, errw, code := r.run(c.args...)
		if code != c.code || !strings.Contains(out+errw, c.want) {
			t.Errorf("%s: exit %d want %d, out %q err %q", c.name, code, c.code, out, errw)
		}
	}
	r.stdinR = nil
	// only carol exists: the refusals changed nothing
	out, _, _ := r.run("--json", "user", "list")
	var users []map[string]any
	json.Unmarshal([]byte(out), &users)
	if len(users) != 1 || users[0]["username"] != "carol" {
		t.Errorf("users after the refusals: %s", out)
	}
	if hits := r.leaks(goodPassword); len(hits) > 0 {
		t.Errorf("password found in %v", hits)
	}
}

func TestUserRemoveLastUserWithTLSListener(t *testing.T) {
	r := newRig(t)
	cfg, _ := os.ReadFile(r.cfg)
	os.WriteFile(r.cfg, []byte(strings.Replace(string(cfg), `"listen"`, `"tlsListen": "127.0.0.1:18899", "listen"`, 1)), 0o644)
	r.stdin = goodPassword + "\n"
	r.run("user", "add", "alice")
	r.stdin = "a second long passphrase\n"
	r.run("user", "add", "bob")

	if _, _, code := r.run("user", "remove", "alice"); code != 0 {
		t.Fatalf("removing one of two: %d", code)
	}
	_, errw, code := r.run("user", "remove", "bob")
	if code != 1 || !strings.Contains(errw, "last user") {
		t.Errorf("last user with a TLS listener: %d %q", code, errw)
	}
	if out, _, _ := r.run("user", "list"); !strings.Contains(out, "bob") {
		t.Error("bob must still be there")
	}
	if _, _, code := r.run("user", "remove", "bob", "--force"); code != 0 {
		t.Errorf("--force: %d", code)
	}
	if !strings.Contains(r.log(), "result=refused") {
		t.Error("the refusal is not logged as refused")
	}
}

func TestUserUnlockWritesTheMarkerTheServiceConsumes(t *testing.T) {
	r := newRig(t)
	out, errw, code := r.run("user", "unlock", "alice")
	if code != 0 || !strings.Contains(out, "unlock requested") {
		t.Fatalf("%d %q %q", code, out, errw)
	}
	entries, err := os.ReadDir(filepath.Join(r.authDir(), "unlock"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("marker: %v %v", entries, err)
	}
	if _, _, code := r.run("user", "unlock", strings.Repeat("x", 65)); code != 2 {
		t.Errorf("a 65-character name: %d", code)
	}
}

func TestUserAuthDirFlagOverridesConfig(t *testing.T) {
	r := newRig(t)
	other := filepath.Join(r.base(), "elsewhere")
	r.stdin = goodPassword + "\n"
	if _, _, code := r.run("user", "add", "alice", "--auth-dir", other); code != 0 {
		t.Fatal(code)
	}
	if _, err := os.Stat(filepath.Join(other, "users.json")); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(filepath.Join(r.authDir(), "users.json")); err == nil {
		t.Error("the configured folder was used too")
	}
}

func TestTLSCommands(t *testing.T) {
	r := newRig(t)
	dir := filepath.Join(r.base(), "tls")

	if _, errw, code := r.run("tls", "print", "--tls-dir", dir); code != 1 || !strings.Contains(errw, "no local CA yet") {
		t.Errorf("print before ca: %d %q", code, errw)
	}
	out, errw, code := r.run("tls", "ca", "--tls-dir", dir)
	if code != 0 || !strings.Contains(out, "SHA-256:") || !strings.Contains(out, "iPhone") || !strings.Contains(out, filepath.Join(dir, "ca.crt")) {
		t.Fatalf("ca: %d %q %q", code, out, errw)
	}
	fp := out[strings.Index(out, "SHA-256:"):]
	fp = strings.Fields(fp)[1]
	if len(fp) < 64 {
		t.Errorf("fingerprint %q", fp)
	}
	jout, _, _ := r.run("--json", "tls", "ca", "--tls-dir", dir)
	var j map[string]any
	if err := json.Unmarshal([]byte(jout), &j); err != nil || j["sha256"] != fp {
		t.Errorf("ca --json: %v %q", err, jout)
	}
	for _, f := range []string{"ca.crt", "ca.key"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s not created", f)
		}
	}

	// print: the certificate and never a key
	pemOut, _, code := r.run("tls", "print", "--tls-dir", dir)
	if code != 0 || !strings.HasPrefix(pemOut, "-----BEGIN CERTIFICATE-----") || strings.Contains(pemOut, "PRIVATE KEY") {
		t.Fatalf("print: %d %q", code, pemOut)
	}
	file := filepath.Join(r.base(), "ca-copy.crt")
	if out, _, code := r.run("tls", "print", "--tls-dir", dir, "--out", file); code != 0 || strings.Contains(out, "BEGIN") {
		t.Errorf("print --out: %d %q", code, out)
	}
	if b, _ := os.ReadFile(file); string(b) != pemOut {
		t.Error("--out holds something else than stdout")
	}
	key, _ := os.ReadFile(filepath.Join(dir, "ca.key"))
	keyBody := strings.Split(string(key), "\n")[1]
	if hits := r.leaks(keyBody); len(hits) != 1 {
		t.Errorf("the CA key body is in %v (only ca.key may hold it)", hits)
	}

	// rotate: refused without --yes, nothing touched
	if _, errw, code := r.run("tls", "rotate", "--tls-dir", dir); code != 2 || !strings.Contains(errw, "--yes") {
		t.Errorf("rotate without --yes: %d %q", code, errw)
	}
	if _, err := os.Stat(filepath.Join(dir, "ca.crt")); err != nil {
		t.Fatal("rotate without --yes touched the CA")
	}
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("mine"), 0o644)
	out, _, code = r.run("tls", "rotate", "--tls-dir", dir, "--yes")
	if code != 0 || !strings.Contains(out, "EVERY device must install the new CA") || !strings.Contains(out, "left alone") {
		t.Fatalf("rotate: %d %q", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "ca.crt")); err == nil {
		t.Error("ca.crt survived")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "notes.txt")); string(b) != "mine" {
		t.Error("a foreign file was touched")
	}
	out2, _, _ := r.run("tls", "ca", "--tls-dir", dir)
	if strings.Contains(out2, fp) || !strings.Contains(out2, "SHA-256:") {
		t.Errorf("the CA did not change: %q", out2)
	}
	// rotate with no folder
	if out, _, code := r.run("tls", "rotate", "--tls-dir", filepath.Join(r.base(), "none"), "--yes"); code != 0 || !strings.Contains(out, "no CA to delete") {
		t.Errorf("rotate of nothing: %d %q", code, out)
	}
	if !strings.Contains(r.log(), "action=tls.rotate") {
		t.Error("tls.rotate is not logged")
	}
}
