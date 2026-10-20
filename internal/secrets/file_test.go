package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"holocron/internal/osx"
)

func TestFileSuite(t *testing.T) {
	suite(t, func(t *testing.T) Provider { return newTestFile(t, t.TempDir(), noEnv) })
}

func TestFilePersistsAcrossOpens(t *testing.T) {
	dir := t.TempDir()
	must(t, newTestFile(t, dir, noEnv).Put("camera.user", "admin"))
	if got, err := newTestFile(t, dir, noEnv).Get("camera.user"); err != nil || got != "admin" {
		t.Fatalf("second open Get = %q, %v", got, err)
	}
}

func TestFileConcurrent(t *testing.T) {
	p := newTestFile(t, t.TempDir(), noEnv)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			own := fmt.Sprintf("own-%d", g)
			for i := 0; i < 15; i++ {
				v := fmt.Sprintf("value-%d-%d", g, i)
				if err := p.Put(own, v); err != nil {
					t.Error(err)
					return
				}
				if got, err := p.Get(own); err != nil || got != v {
					t.Errorf("Get(%s) = %q, %v; want %q", own, got, err, v)
				}
				_ = p.Put("shared", v)
				if _, err := p.Get("shared"); err != nil {
					t.Error(err)
				}
				if _, err := p.List(); err != nil {
					t.Error(err)
				}
			}
		}(g)
	}
	wg.Wait()
	if names, _ := p.List(); len(names) != 9 {
		t.Fatalf("names = %v", names)
	}
}

// A temp file left by a crashed writer, even a half-written one, neither corrupts nor blocks the store.
func TestFileLeftoverTempIsHarmless(t *testing.T) {
	dir := t.TempDir()
	p := newTestFile(t, dir, noEnv)
	must(t, p.Put("camera.user", "admin"))
	stale := filepath.Join(dir, "secrets.enc.tmp-deadbeef0000")
	must(t, os.WriteFile(stale, []byte(`{"version":1,"che`), 0o600))
	if got, err := p.Get("camera.user"); err != nil || got != "admin" {
		t.Fatalf("Get with a stale temp = %q, %v", got, err)
	}
	must(t, p.Put("camera.password", "secret-pw"))
	if got, _ := newTestFile(t, dir, noEnv).Get("camera.password"); got != "secret-pw" {
		t.Fatalf("Put with a stale temp lost data: %q", got)
	}
	// Control: a corrupt store file itself is reported, not silently emptied.
	must(t, os.WriteFile(filepath.Join(dir, "secrets.enc"), []byte(`{"version":1,"che`), 0o600))
	if _, err := p.Get("camera.user"); !errors.Is(err, ErrFormat) {
		t.Fatalf("truncated store: %v", err)
	}
}

func readDoc(t *testing.T, path string) fileDoc {
	t.Helper()
	b, err := os.ReadFile(path)
	must(t, err)
	var d fileDoc
	must(t, json.Unmarshal(b, &d))
	return d
}

func writeDoc(t *testing.T, path string, d fileDoc) {
	t.Helper()
	b, err := json.Marshal(d)
	must(t, err)
	must(t, os.WriteFile(path, b, 0o600))
}

func TestFileTamper(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.enc")
	p := newTestFile(t, dir, noEnv)
	want := map[string]string{"camera.user": "fakeuser-1", "camera.password": "fakepass-9f8e7d", "push.token": "tok-abcdef"}
	for n, v := range want {
		must(t, p.Put(n, v))
	}
	orig, err := os.ReadFile(path)
	must(t, err)

	// control: the untouched file reads back
	for n, v := range want {
		if got, err := p.Get(n); err != nil || got != v {
			t.Fatalf("control Get(%s) = %q, %v", n, got, err)
		}
	}

	t.Run("flip every byte: an error, or exactly the right data, never anything else", func(t *testing.T) {
		failures := 0
		for i := range orig {
			bad := append([]byte(nil), orig...)
			bad[i] ^= 0x01
			must(t, os.WriteFile(path, bad, 0o600))
			for n, v := range want {
				got, err := p.Get(n)
				switch {
				case err != nil:
					failures++
					if got != "" {
						t.Fatalf("byte %d: partial data %q returned with error %v", i, got, err)
					}
				case got != v:
					t.Fatalf("byte %d: Get(%s) returned wrong data %q", i, n, got)
				}
			}
		}
		if failures < len(orig) { // nearly every flip must be caught on every name
			t.Fatalf("only %d failures over %d flips x %d names", failures, len(orig), len(want))
		}
	})

	t.Run("a flipped ciphertext byte fails authentication with a clear error", func(t *testing.T) {
		must(t, os.WriteFile(path, orig, 0o600))
		d := readDoc(t, path)
		e := d.Entries["camera.password"]
		e.Data = flipB64(t, e.Data)
		d.Entries["camera.password"] = e
		writeDoc(t, path, d)
		got, err := p.Get("camera.password")
		if !errors.Is(err, ErrAuth) || got != "" {
			t.Fatalf("Get = %q, %v", got, err)
		}
		if !strings.Contains(err.Error(), "tampered") || strings.Contains(err.Error(), "fakepass") {
			t.Fatalf("unclear or leaking error: %v", err)
		}
		if _, err := p.List(); !errors.Is(err, ErrAuth) {
			t.Fatalf("List must refuse a tampered store too: %v", err)
		}
	})

	t.Run("swapping two entries is caught (name is bound as AAD)", func(t *testing.T) {
		must(t, os.WriteFile(path, orig, 0o600))
		d := readDoc(t, path)
		d.Entries["camera.user"], d.Entries["camera.password"] = d.Entries["camera.password"], d.Entries["camera.user"]
		writeDoc(t, path, d)
		if v, err := p.Get("camera.user"); !errors.Is(err, ErrAuth) || v != "" {
			t.Fatalf("swapped Get = %q, %v", v, err)
		}
	})

	t.Run("dropping or renaming an entry is caught", func(t *testing.T) {
		must(t, os.WriteFile(path, orig, 0o600))
		d := readDoc(t, path)
		delete(d.Entries, "push.token")
		writeDoc(t, path, d)
		if _, err := p.Get("camera.user"); !errors.Is(err, ErrAuth) {
			t.Fatalf("dropped entry: %v", err)
		}
		must(t, os.WriteFile(path, orig, 0o600))
		d = readDoc(t, path)
		d.Entries["renamed"] = d.Entries["push.token"]
		delete(d.Entries, "push.token")
		writeDoc(t, path, d)
		if _, err := p.Get("renamed"); !errors.Is(err, ErrAuth) {
			t.Fatalf("renamed entry: %v", err)
		}
	})

	t.Run("writes on a tampered store are refused and change nothing", func(t *testing.T) {
		must(t, os.WriteFile(path, orig, 0o600))
		d := readDoc(t, path)
		e := d.Entries["camera.user"]
		e.Data = flipB64(t, e.Data)
		d.Entries["camera.user"] = e
		writeDoc(t, path, d)
		before, _ := os.ReadFile(path)
		if err := p.Put("other", "value-x"); !errors.Is(err, ErrAuth) {
			t.Fatalf("Put on a tampered store: %v", err)
		}
		if err := p.Delete("push.token"); !errors.Is(err, ErrAuth) {
			t.Fatalf("Delete on a tampered store: %v", err)
		}
		if after, _ := os.ReadFile(path); string(after) != string(before) {
			t.Fatal("a refused write changed the file")
		}
	})

	t.Run("unsupported version", func(t *testing.T) {
		must(t, os.WriteFile(path, orig, 0o600))
		d := readDoc(t, path)
		d.Version = 2
		writeDoc(t, path, d)
		if _, err := p.Get("camera.user"); !errors.Is(err, ErrFormat) {
			t.Fatalf("version 2: %v", err)
		}
	})
}

func flipB64(t *testing.T, s string) string {
	t.Helper()
	b := []byte(s)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}

func TestFileWrongKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.enc")
	good := newTestFile(t, dir, envWithKey(keyA))
	must(t, good.Put("camera.password", "fakepass-1234"))
	before, _ := os.ReadFile(path)

	bad := newTestFile(t, dir, envWithKey(keyB))
	v, err := bad.Get("camera.password")
	if !errors.Is(err, ErrAuth) || v != "" {
		t.Fatalf("wrong key Get = %q, %v", v, err)
	}
	if _, err := bad.List(); !errors.Is(err, ErrAuth) {
		t.Fatalf("wrong key List: %v", err)
	}
	if err := bad.Put("x", "value-x"); !errors.Is(err, ErrAuth) {
		t.Fatalf("wrong key Put: %v", err)
	}
	if err := bad.Delete("camera.password"); !errors.Is(err, ErrAuth) {
		t.Fatalf("wrong key Delete: %v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("a wrong-key operation changed the store")
	}
	// control: the right key still works
	if got, err := newTestFile(t, dir, envWithKey(keyA)).Get("camera.password"); err != nil || got != "fakepass-1234" {
		t.Fatalf("right key = %q, %v", got, err)
	}
	// the store is gone-key: file present, no key at all
	if _, err := newTestFile(t, dir, noEnv).Get("camera.password"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("no key: %v", err)
	}
}

func TestEmptyStoreWithWrongKeyIsCaught(t *testing.T) {
	dir := t.TempDir()
	a := newTestFile(t, dir, envWithKey(keyA))
	must(t, a.Put("n", "value-x"))
	must(t, a.Delete("n"))
	if _, err := newTestFile(t, dir, envWithKey(keyB)).List(); !errors.Is(err, ErrAuth) {
		t.Fatalf("empty store, wrong key: %v", err)
	}
}

func TestKeyFromEnvVersusFile(t *testing.T) {
	t.Run("env key: no key file is created", func(t *testing.T) {
		dir := t.TempDir()
		p := newTestFile(t, dir, envWithKey(keyA))
		must(t, p.Put("n", "value-x"))
		if _, err := os.Stat(filepath.Join(dir, "secrets.key")); !os.IsNotExist(err) {
			t.Fatalf("key file exists: %v", err)
		}
	})
	t.Run("key file: created on first write, not on read, reused later", func(t *testing.T) {
		dir := t.TempDir()
		keyPath := filepath.Join(dir, "secrets.key")
		p := newTestFile(t, dir, noEnv)
		if _, err := p.Get("n"); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
			t.Fatal("a read created the key file")
		}
		must(t, p.Put("n", "value-x"))
		k1, err := os.ReadFile(keyPath)
		must(t, err)
		if len(strings.TrimSpace(string(k1))) != 64 {
			t.Fatalf("key file = %q", k1)
		}
		must(t, newTestFile(t, dir, noEnv).Put("m", "value-y"))
		k2, _ := os.ReadFile(keyPath)
		if string(k1) != string(k2) {
			t.Fatal("the key changed on reuse")
		}
	})
	t.Run("env wins over the key file", func(t *testing.T) {
		dir := t.TempDir()
		must(t, newTestFile(t, dir, noEnv).Put("n", "value-x")) // under the file key
		if _, err := newTestFile(t, dir, envWithKey(keyA)).Get("n"); !errors.Is(err, ErrAuth) {
			t.Fatalf("env key must override the file key: %v", err)
		}
	})
	t.Run("a malformed env key is refused and never echoed", func(t *testing.T) {
		for _, k := range []string{"short", keyA[:62], strings.Repeat("zz", 32)} {
			err := newTestFile(t, t.TempDir(), envWithKey(k)).Put("n", "value-x")
			if err == nil || strings.Contains(err.Error(), k) {
				t.Errorf("key %q: %v", k, err)
			}
		}
	})
	t.Run("a malformed key file is refused", func(t *testing.T) {
		dir := t.TempDir()
		must(t, os.WriteFile(filepath.Join(dir, "secrets.key"), []byte("not a key\n"), 0o600))
		if err := newTestFile(t, dir, noEnv).Put("n", "value-x"); err == nil || strings.Contains(err.Error(), "not a key") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestKeyFileAccessibleToOthersIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits")
	}
	dir := t.TempDir()
	p := newTestFile(t, dir, noEnv)
	must(t, p.Put("n", "value-x"))
	must(t, os.Chmod(filepath.Join(dir, "secrets.key"), 0o644))
	if _, err := p.Get("n"); err == nil || !strings.Contains(err.Error(), "accessible to others") {
		t.Fatalf("got %v", err)
	}
}

func TestOwnerOnlyUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("covered by the Windows ACL test")
	}
	base := t.TempDir()
	dir := filepath.Join(base, "auth") // created by the store
	p, err := newFile(Config{File: filepath.Join(dir, "secrets.enc"), KeyFile: filepath.Join(dir, "secrets.key"),
		Platform: osx.Current(), Environ: noEnv})
	must(t, err)
	must(t, p.Put("n", "value-x"))
	must(t, p.Put("n", "value-y")) // the rewrite keeps the mode too
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "secrets.enc"): 0o600, filepath.Join(dir, "secrets.key"): 0o600} {
		fi, err := os.Stat(path)
		must(t, err)
		if fi.Mode().Perm() != want {
			t.Errorf("%s mode %o, want %o", filepath.Base(path), fi.Mode().Perm(), want)
		}
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

// Fail closed: when the file cannot be made owner-only nothing is written or left behind.
func TestOwnerOnlyFailureFailsClosed(t *testing.T) {
	dir := t.TempDir()
	p, err := newFile(Config{File: filepath.Join(dir, "secrets.enc"), KeyFile: filepath.Join(dir, "secrets.key"),
		Platform: &osx.Fake{OwnerErr: errors.New("acl broke")}, Environ: noEnv})
	must(t, err)
	if err := p.Put("n", "value-x"); err == nil || !strings.Contains(err.Error(), "owner-only") {
		t.Fatalf("Put = %v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("left behind: %v", ents)
	}
	// with a key from the environment the temp file is the one that fails
	p, _ = newFile(Config{File: filepath.Join(dir, "secrets.enc"), KeyFile: filepath.Join(dir, "secrets.key"),
		Platform: &osx.Fake{OwnerErr: errors.New("acl broke")}, Environ: envWithKey(keyA)})
	if err := p.Put("n", "value-x"); err == nil {
		t.Fatal("Put must fail")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("left behind: %v", ents)
	}
}

func TestFileLimits(t *testing.T) {
	p := newTestFile(t, t.TempDir(), noEnv)
	must(t, p.Put("big", strings.Repeat("x", MaxValueLen))) // exactly 4 KB is allowed
	for i := 0; i < maxEntries-1; i++ {
		must(t, p.Put(fmt.Sprintf("n%d", i), "v-value"))
	}
	if err := p.Put("one-too-many", "v-value"); err == nil {
		t.Fatal("the entry limit was not enforced")
	}
	must(t, p.Put("big", "overwrite is still fine"))
}
