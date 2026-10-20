//go:build windows

package secrets

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"errors"

	"holocron/internal/osx"
)

// These tests touch the real Credential Manager (a clearly test-only target, removed at the end)
// and run PowerShell. They run only when HOLOCRON_W11_REAL=1.
func realOnly(t *testing.T) {
	if os.Getenv("HOLOCRON_W11_REAL") != "1" {
		t.Skip("set HOLOCRON_W11_REAL=1 to run the Credential Manager, ACL and importer tests")
	}
}

func ps(t *testing.T, env []string, script string) string {
	t.Helper()
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("powershell: %v: %s", err, out)
	}
	return string(out)
}

func cmdkeyList(t *testing.T, filter string) string {
	t.Helper()
	out, _ := exec.Command("cmdkey.exe", "/list:"+filter).CombinedOutput()
	return string(out)
}

func TestWindowsKeyring(t *testing.T) {
	realOnly(t)
	prefix := "holocron/TEST-" + randHex(6) + "/"
	defer func() {
		if p, err := newKeyring(prefix); err == nil {
			names, _ := p.List()
			for _, n := range names {
				_ = p.Delete(n)
			}
		}
		if out := cmdkeyList(t, prefix+"*"); strings.Contains(out, "Target:") {
			t.Errorf("test credential left behind:\n%s", out)
		}
	}()

	suite(t, func(t *testing.T) Provider {
		p, err := newKeyring(prefix)
		must(t, err)
		names, _ := p.List()
		for _, n := range names {
			must(t, p.Delete(n))
		}
		return p
	})

	p, err := newKeyring(prefix)
	must(t, err)
	t.Run("independent view through cmdkey, and the size limit", func(t *testing.T) {
		must(t, p.Put("camera.user", "fakeuser"))
		out := cmdkeyList(t, prefix+"camera.user")
		for _, want := range []string{"Target: LegacyGeneric:target=" + prefix + "camera.user", "Type: Generic", "Persistence: Local Computer"} {
			if !strings.Contains(out, want) {
				t.Errorf("cmdkey output lacks %q:\n%s", want, out)
			}
		}
		must(t, p.Put("big", strings.Repeat("x", maxCredBlob)))
		if err := p.Put("toobig", strings.Repeat("x", maxCredBlob+1)); !errors.Is(err, ErrInvalidValue) {
			t.Errorf("over the Credential Manager limit: %v", err)
		}
		for _, n := range []string{"camera.user", "big"} {
			must(t, p.Delete(n))
		}
		if out := cmdkeyList(t, prefix+"*"); strings.Contains(out, "Target:") {
			t.Errorf("Delete left an entry:\n%s", out)
		}
	})
	t.Run("List sees only the prefix", func(t *testing.T) {
		must(t, p.Put("mine", "value-x"))
		other, _ := newKeyring(prefix + "sub-")
		names, _ := other.List()
		if len(names) != 0 {
			t.Errorf("other prefix lists %v", names)
		}
		must(t, p.Delete("mine"))
	})
	t.Run("auto and keyring selection on Windows", func(t *testing.T) {
		for _, backend := range []string{"auto", "keyring"} {
			q, err := Open(Config{Backend: backend, Platform: osx.Current(), Environ: noEnv, keyringPrefix: prefix})
			if err != nil || q.Backend() != "keyring" {
				t.Errorf("%s: %v, %v", backend, q, err)
			}
		}
		q, err := Open(Config{Backend: "file", Platform: osx.Current(), File: filepath.Join(t.TempDir(), "s.enc"), KeyFile: filepath.Join(t.TempDir(), "s.key")})
		if err != nil || q.Backend() != "file" {
			t.Errorf("file: %v, %v", q, err)
		}
	})
}

func icacls(t *testing.T, path string) string {
	out, err := exec.Command("icacls.exe", path).CombinedOutput()
	if err != nil {
		t.Fatalf("icacls %s: %v: %s", path, err, out)
	}
	return string(out)
}

func TestWindowsACL(t *testing.T) {
	realOnly(t)
	base := t.TempDir()
	dir := filepath.Join(base, "auth") // created by the store, so the store restricts it
	p, err := newFile(Config{File: filepath.Join(dir, "secrets.enc"), KeyFile: filepath.Join(dir, "secrets.key"),
		Platform: osx.Current(), Environ: noEnv})
	must(t, err)
	must(t, p.Put("n", "value-x"))
	must(t, p.Put("n", "value-y")) // the rewrite keeps the ACL
	for _, path := range []string{dir, filepath.Join(dir, "secrets.enc"), filepath.Join(dir, "secrets.key")} {
		text := icacls(t, path)
		for _, who := range []string{"Everyone", `BUILTIN\Users`, "Authenticated Users", `BUILTIN\Administrators`, `NT AUTHORITY\SYSTEM`} {
			if strings.Contains(text, who) {
				t.Errorf("%s grants %s:\n%s", filepath.Base(path), who, text)
			}
		}
		if strings.Contains(text, "(I)") {
			t.Errorf("%s still inherits:\n%s", filepath.Base(path), text)
		}
		if n := strings.Count(text, ":("); n < 1 || !strings.Contains(text, "(F)") {
			t.Errorf("%s: owner entry missing:\n%s", filepath.Base(path), text)
		}
		t.Logf("%s ACL:\n%s", filepath.Base(path), strings.TrimSpace(text))
	}
	if got, err := p.Get("n"); err != nil || got != "value-y" {
		t.Errorf("owner lost access: %q %v", got, err)
	}
	// negative control: a file without SetOwnerOnly is NOT owner-only here
	plain := filepath.Join(base, "plain.txt")
	must(t, os.WriteFile(plain, []byte("x"), 0o600))
	if text := icacls(t, plain); !strings.Contains(text, `BUILTIN\Administrators`) && !strings.Contains(text, "(I)") {
		t.Errorf("control: a plain file shows no inherited entries, so the ACL check proves nothing:\n%s", text)
	}
}

const exportScript = `$s = ConvertTo-SecureString $env:W11_FAKE_PASS -AsPlainText -Force
New-Object System.Management.Automation.PSCredential($env:W11_FAKE_USER, $s) | Export-Clixml -LiteralPath $env:W11_FAKE_PATH`

// poll runs a PowerShell sampler of Win32_Process while f runs and returns how many samples saw a
// command line containing needle. Only counts leave the sampler; command lines are never written.
func poll(t *testing.T, needle string, f func()) (samples, hits int) {
	t.Helper()
	dir := t.TempDir()
	out, stop, ready := filepath.Join(dir, "out.txt"), filepath.Join(dir, "stop"), filepath.Join(dir, "ready")
	script := `$n=$env:W11_NEEDLE; $s=0; $h=0
New-Item -ItemType File -Path $env:W11_READY | Out-Null
while (-not (Test-Path $env:W11_STOP) -and $s -lt 400) { $s++
  $h += @(Get-CimInstance Win32_Process | Where-Object { $_.CommandLine -and $_.CommandLine.Contains($n) }).Count }
"$s $h" | Set-Content $env:W11_OUT`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.Env = append(os.Environ(), "W11_NEEDLE="+needle, "W11_OUT="+out, "W11_STOP="+stop, "W11_READY="+ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for i := 0; ; i++ {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if i > 200 {
			cmd.Process.Kill()
			t.Fatal("process sampler did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}
	f()
	time.Sleep(600 * time.Millisecond) // let it take a sample after f
	must(t, os.WriteFile(stop, nil, 0o600))
	select {
	case err := <-done:
		must(t, err)
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		t.Fatal("process sampler did not stop")
	}
	b, err := os.ReadFile(out)
	must(t, err)
	f2 := regexp.MustCompile(`\d+`).FindAllString(string(b), -1)
	samples, _ = strconv.Atoi(f2[0])
	hits, _ = strconv.Atoi(f2[1])
	return
}

func TestWindowsImporter(t *testing.T) {
	realOnly(t)
	scratch := t.TempDir()
	fakeUser, fakePass := "fakeuser", "fakepass-"+randHex(5)
	creds := filepath.Join(scratch, "fake_creds.xml")
	ps(t, []string{"W11_FAKE_USER=" + fakeUser, "W11_FAKE_PASS=" + fakePass, "W11_FAKE_PATH=" + creds}, exportScript)
	raw, err := os.ReadFile(creds)
	must(t, err)
	if strings.Contains(string(raw), fakePass) {
		t.Fatal("the fake credential file holds the password in clear")
	}

	newDst := func(name string) (Provider, string) {
		d := filepath.Join(scratch, name)
		p, err := newFile(Config{File: filepath.Join(d, "secrets.enc"), KeyFile: filepath.Join(d, "secrets.key"),
			Platform: osx.Current(), Environ: noEnv})
		must(t, err)
		return p, d
	}

	var errs []string
	t.Run("imports the fake file; no process command line carries the password", func(t *testing.T) {
		dst, _ := newDst("good")
		var impErr error
		var took time.Duration
		samples, hits := poll(t, fakePass, func() {
			t0 := time.Now()
			impErr = ImportLegacyCameraCreds(context.Background(), creds, dst)
			took = time.Since(t0)
		})
		must(t, impErr)
		t.Logf("importer took %s; process sampler: %d samples, %d hits", took, samples, hits)
		if samples < 3 || hits != 0 {
			t.Fatalf("samples=%d hits=%d", samples, hits)
		}
		u, pw, err := CameraLogin(dst)
		if err != nil || u != fakeUser || pw != fakePass {
			t.Fatalf("stored login = %q, %q, %v", u, pw, err)
		}
		names, _ := dst.List()
		if strings.Join(names, ",") != "camera.password,camera.user" {
			t.Fatalf("names = %v", names)
		}
	})

	t.Run("positive control: the sampler sees a command line that carries the password", func(t *testing.T) {
		samples, hits := poll(t, fakePass, func() {
			c := exec.Command("cmd.exe", "/c", "ping -n 4 127.0.0.1 >nul & rem "+fakePass)
			must(t, c.Run())
		})
		t.Logf("control: %d samples, %d hits", samples, hits)
		if hits < 1 {
			t.Fatalf("the sampler cannot see a leaking command line (samples=%d)", samples)
		}
	})

	t.Run("negative controls store nothing and leak nothing", func(t *testing.T) {
		half := filepath.Join(scratch, "truncated.xml")
		must(t, os.WriteFile(half, raw[:len(raw)/2], 0o600))
		mangled := filepath.Join(scratch, "mangled.xml")
		s := string(raw)
		i := strings.Index(s, `N="Password">`)
		if i < 0 {
			t.Fatalf("unexpected Clixml layout:\n%s", s)
		}
		i += len(`N="Password">`) + 40
		must(t, os.WriteFile(mangled, []byte(s[:i]+strings.Repeat("0", 24)+s[i+24:]), 0o600))
		notCred := filepath.Join(scratch, "string.xml")
		ps(t, []string{"W11_FAKE_PATH=" + notCred}, `'hello' | Export-Clixml -LiteralPath $env:W11_FAKE_PATH`)
		empty := filepath.Join(scratch, "empty.xml")
		must(t, os.WriteFile(empty, nil, 0o600))

		for _, c := range []struct{ name, path string }{
			{"truncated", half}, {"mangled DPAPI blob", mangled}, {"not a credential", notCred},
			{"empty file", empty}, {"missing", filepath.Join(scratch, "nope.xml")}, {"a directory", scratch},
		} {
			dst, d := newDst("bad-" + strings.ReplaceAll(c.name, " ", "-"))
			err := ImportLegacyCameraCreds(context.Background(), c.path, dst)
			if err == nil {
				t.Errorf("%s: no error", c.name)
				continue
			}
			errs = append(errs, err.Error())
			t.Logf("%s: %v", c.name, err)
			if names, _ := dst.List(); len(names) != 0 {
				t.Errorf("%s stored %v", c.name, names)
			}
			if _, err := os.Stat(d); err == nil {
				t.Errorf("%s created %s", c.name, d)
			}
		}
		// a read-only destination: the login is read, nothing can be stored, the error holds no secret
		err := ImportLegacyCameraCreds(context.Background(), creds, newEnv(noEnv))
		if !errors.Is(err, ErrReadOnly) {
			t.Errorf("read-only dst: %v", err)
		} else {
			errs = append(errs, err.Error())
		}
		// a cancelled context
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := ImportLegacyCameraCreds(ctx, creds, newEnv(noEnv)); err == nil {
			t.Error("cancelled context: no error")
		} else {
			errs = append(errs, err.Error())
		}
	})

	t.Run("leak scan: errors and every file in the scratch tree", func(t *testing.T) {
		for _, e := range errs {
			if strings.Contains(e, fakePass) {
				t.Errorf("error leaks the password: %s", e)
			}
		}
		if leaks := scanTree(t, scratch, []string{fakePass}); len(leaks) != 0 {
			t.Errorf("files holding the password: %v", leaks)
		}
		decoy := filepath.Join(scratch, "decoy.txt")
		must(t, os.WriteFile(decoy, []byte(fakePass), 0o600))
		if leaks := scanTree(t, scratch, []string{fakePass}); len(leaks) != 1 {
			t.Errorf("positive control failed: %v", leaks)
		}
		must(t, os.Remove(decoy))
	})
}
