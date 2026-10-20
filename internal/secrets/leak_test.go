package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"holocron/internal/logx"
	"holocron/internal/osx"
)

// exerciseEverything drives every operation, success and failure, and returns every error text.
// Secrets must not appear in any of them.
func exerciseEverything(t *testing.T, dir string, p Provider, secretsIn map[string]string) (errs []string) {
	t.Helper()
	note := func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	for n, v := range secretsIn {
		note(p.Put(n, v))
		_, err := p.Get(n)
		note(err)
	}
	_, err := p.Get("absent")
	note(err)
	note(p.Delete("absent"))
	note(p.Put("BAD NAME", "v-value"))
	_, err = p.Get("BAD NAME")
	note(err)
	note(p.Put("n", ""))
	_, err = p.List()
	note(err)
	// wrong key and tampered file, on a second store in the same tree
	sub := filepath.Join(dir, "other")
	must(t, os.MkdirAll(sub, 0o700))
	a := newTestFile(t, sub, envWithKey(keyA))
	for n, v := range secretsIn {
		note(a.Put(n, v))
	}
	_, err = newTestFile(t, sub, envWithKey(keyB)).Get(CameraPasswordName)
	note(err)
	note(newTestFile(t, sub, envWithKey("not-hex-"+keyA[:8])).Put("n", "v-value"))
	b, rerr := os.ReadFile(filepath.Join(sub, "secrets.enc"))
	must(t, rerr)
	b[len(b)/2] ^= 1
	must(t, os.WriteFile(filepath.Join(sub, "secrets.enc"), b, 0o600))
	_, err = a.Get(CameraPasswordName)
	note(err)
	return errs
}

func TestLeakScan(t *testing.T) {
	const pw, user = "fakepass-7c1e5a90", "fakeuser"
	vals := map[string]string{CameraUserName: user, CameraPasswordName: pw, "push.token": "tok-3b9d2f64"}
	needles := []string{pw, "tok-3b9d2f64"} // the user name is not a secret value worth masking
	dir := t.TempDir()

	sink := logx.NewMemorySink()
	lg, _ := logx.New(logx.Options{Sink: sink, Getenv: func(string) string { return "trace" }})
	log := lg.For("secrets")
	p, err := Open(Config{Backend: "file", Platform: &osx.Fake{DirsValue: osx.Dirs{Auth: dir}}, Environ: noEnv, Redactor: lg.Redactor()})
	must(t, err)

	errs := exerciseEverything(t, dir, p, vals)
	for n := range vals { // a careless caller logs what it read: the redactor must catch it
		v, err := p.Get(n)
		must(t, err)
		log.Info("read secret", "name", n, "value", v, "text", "login "+v+" ok")
	}
	for _, e := range errs {
		log.Warn("operation failed", "err", e)
	}
	if !strings.Contains(sink.String(), "***") {
		t.Fatal("test is vacuous: nothing was masked")
	}

	// 1. no log record
	for _, l := range sink.Lines() {
		for _, n := range needles {
			if strings.Contains(l, n) {
				t.Errorf("log record leaks %q: %s", n, l)
			}
		}
	}
	// 2. no error string
	if len(errs) < 8 {
		t.Fatalf("only %d error strings were collected", len(errs))
	}
	for _, e := range errs {
		for _, n := range needles {
			if strings.Contains(e, n) {
				t.Errorf("error leaks %q: %s", n, e)
			}
		}
	}
	// 3. no file at all, ciphertext and key file included, holds a plaintext value
	if leaks := scanTree(t, dir, needles); len(leaks) != 0 {
		t.Errorf("files holding a secret: %v", leaks)
	}

	// Positive controls: the detectors do fire.
	t.Run("positive control", func(t *testing.T) {
		raw, _ := logx.New(logx.Options{Sink: sink, Getenv: func(string) string { return "trace" }}) // no registration
		before := len(sink.Lines())
		raw.For("control").Info("unregistered", "text", "login "+pw)
		if l := sink.Lines()[before]; !strings.Contains(l, pw) {
			t.Fatal("the log check cannot see an unmasked value")
		}
		decoy := filepath.Join(dir, "decoy.txt")
		must(t, os.WriteFile(decoy, []byte("password="+pw), 0o600))
		if leaks := scanTree(t, dir, needles); len(leaks) != 1 || leaks[0] != "decoy.txt" {
			t.Fatalf("scanTree missed the decoy: %v", leaks)
		}
		must(t, os.Remove(decoy))
	})
}
