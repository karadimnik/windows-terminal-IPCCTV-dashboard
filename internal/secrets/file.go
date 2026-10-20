package secrets

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	fileVersion = 1
	maxFileSize = 4 << 20
	maxEntries  = 256
	// KeyEnv names the environment variable that carries the file backend's key (64 hex chars).
	KeyEnv = envKeyName

	aadEntry = "holocron-secrets/1/entry\x00"
	aadCheck = "holocron-secrets/1/check\x00"
	checkTxt = "holocron-secrets check"
)

type sealed struct {
	Nonce string `json:"nonce"`
	Data  string `json:"data"`
}

// fileDoc is secrets.enc. Every entry is sealed with its NAME as additional authenticated data,
// so entries cannot be swapped; Check is sealed over the sorted list of names, so an entry cannot
// be added, dropped or renamed without the key either, and a wrong key is caught on an empty store.
type fileDoc struct {
	Version int               `json:"version"`
	Check   *sealed           `json:"check"`
	Entries map[string]sealed `json:"entries"`
}

type fileProvider struct {
	mu      sync.Mutex
	path    string
	keyPath string
	plat    interface{ SetOwnerOnly(string) error }
	environ func() []string
}

func newFile(cfg Config) (Provider, error) {
	path, keyPath, err := cfg.paths()
	if err != nil {
		return nil, err
	}
	return &fileProvider{path: path, keyPath: keyPath, plat: cfg.platform(), environ: cfg.environ()}, nil
}

func (*fileProvider) Backend() string { return BackendFile }

func (f *fileProvider) Get(name string) (string, error) {
	if err := checkName(name); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	doc, key, err := f.load(false)
	if err != nil {
		return "", err
	}
	s, ok := doc.Entries[name]
	if !ok {
		return "", notFound(name)
	}
	pt, err := open(key, s, aadEntry+name)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

func (f *fileProvider) Put(name, value string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if err := checkValue(value); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	doc, key, err := f.load(true)
	if err != nil {
		return err
	}
	if _, ok := doc.Entries[name]; !ok && len(doc.Entries) >= maxEntries {
		return fmt.Errorf("secrets: the file store holds at most %d entries", maxEntries)
	}
	s, err := seal(key, []byte(value), aadEntry+name)
	if err != nil {
		return err
	}
	doc.Entries[name] = s
	return f.save(doc, key)
}

func (f *fileProvider) Delete(name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	doc, key, err := f.load(false)
	if err != nil {
		return err
	}
	if _, ok := doc.Entries[name]; !ok {
		return notFound(name)
	}
	delete(doc.Entries, name)
	return f.save(doc, key)
}

func (f *fileProvider) List() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	doc, _, err := f.load(false)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(doc.Entries))
	for n := range doc.Entries {
		names = append(names, n)
	}
	return sorted(names), nil
}

// load reads and verifies the store. A missing file is an empty store; the key is then resolved
// only when create is set (a write), so a read never creates a key file.
func (f *fileProvider) load(create bool) (*fileDoc, []byte, error) {
	raw, err := readBounded(f.path)
	if errors.Is(err, os.ErrNotExist) {
		var key []byte
		if create {
			if key, err = f.key(true); err != nil {
				return nil, nil, err
			}
		}
		return &fileDoc{Version: fileVersion, Entries: map[string]sealed{}}, key, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var doc fileDoc
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil || dec.More() {
		return nil, nil, ErrFormat
	}
	if doc.Version != fileVersion {
		return nil, nil, fmt.Errorf("%w: unsupported version %d", ErrFormat, doc.Version)
	}
	if doc.Check == nil || len(doc.Entries) > maxEntries {
		return nil, nil, ErrFormat
	}
	if doc.Entries == nil {
		doc.Entries = map[string]sealed{}
	}
	for n := range doc.Entries {
		if !ValidName(n) {
			return nil, nil, ErrFormat
		}
	}
	key, err := f.key(false)
	if err != nil {
		return nil, nil, err
	}
	// Verify the entry list (and so the key) up front; entries are opened one at a time on Get.
	names := make([]string, 0, len(doc.Entries))
	for n := range doc.Entries {
		names = append(names, n)
	}
	if pt, err := open(key, *doc.Check, checkAAD(sorted(names))); err != nil || string(pt) != checkTxt {
		return nil, nil, ErrAuth
	}
	return &doc, key, nil
}

func checkAAD(sortedNames []string) string { return aadCheck + strings.Join(sortedNames, "\x00") }

func (f *fileProvider) save(doc *fileDoc, key []byte) error {
	names := make([]string, 0, len(doc.Entries))
	for n := range doc.Entries {
		names = append(names, n)
	}
	chk, err := seal(key, []byte(checkTxt), checkAAD(sorted(names)))
	if err != nil {
		return err
	}
	doc.Version, doc.Check = fileVersion, &chk
	data, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return f.replace(append(data, '\n'))
}

// replace writes data to a temp file in the same directory (owner-only BEFORE any content is
// written, fail closed), syncs it and renames it over the store.
func (f *fileProvider) replace(data []byte) (err error) {
	dir := filepath.Dir(f.path)
	if err := f.ensureDir(dir); err != nil {
		return err
	}
	tmp, err := f.createOwnerOnly(f.path + ".tmp-" + randHex(6))
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(name)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, f.path)
}

// createOwnerOnly creates a new empty file (O_EXCL, mode 0600) and restricts it to its owner
// before the caller writes into it. If the restriction fails the file is removed.
func (f *fileProvider) createOwnerOnly(path string) (*os.File, error) {
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.plat.SetOwnerOnly(path); err != nil {
		fh.Close()
		os.Remove(path)
		return nil, fmt.Errorf("secrets: cannot make %s owner-only: %w", filepath.Base(path), err)
	}
	return fh, nil
}

// ensureDir creates a missing directory owner-only; an existing one is left as it is (it may be
// shared with other files, such as the web login store).
func (f *fileProvider) ensureDir(dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := f.plat.SetOwnerOnly(dir); err != nil {
		return fmt.Errorf("secrets: cannot make %s owner-only: %w", dir, err)
	}
	return nil
}

// key returns the 32-byte key: HOLOCRON_SECRET_KEY when set, else the key file (created on first
// use when create is set).
func (f *fileProvider) key(create bool) ([]byte, error) {
	for _, kv := range f.environ() {
		if i := strings.IndexByte(kv, '='); i > 0 && strings.EqualFold(kv[:i], KeyEnv) && kv[i+1:] != "" {
			k, err := parseKey(kv[i+1:])
			if err != nil {
				return nil, fmt.Errorf("secrets: %s must be 64 hex characters", KeyEnv)
			}
			return k, nil
		}
	}
	k, err := f.readKeyFile()
	if errors.Is(err, os.ErrNotExist) {
		if !create {
			return nil, ErrNoKey
		}
		return f.createKeyFile()
	}
	return k, err
}

func parseKey(s string) ([]byte, error) {
	k, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil || len(k) != 32 {
		return nil, ErrFormat
	}
	return k, nil
}

func (f *fileProvider) readKeyFile() ([]byte, error) {
	var raw []byte
	// a creating process makes the file owner-only first and writes second: wait out that gap
	for try := 0; ; try++ {
		fi, err := os.Stat(f.keyPath)
		if err != nil {
			return nil, err
		}
		if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("secrets: key file %s is accessible to others (mode %o); chmod 600 it", f.keyPath, fi.Mode().Perm())
		}
		if raw, err = readBounded(f.keyPath); err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(raw)) > 0 || try >= 20 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	k, err := parseKey(string(raw))
	if err != nil {
		return nil, fmt.Errorf("secrets: key file %s is not 64 hex characters", f.keyPath)
	}
	return k, nil
}

func (f *fileProvider) createKeyFile() ([]byte, error) {
	if err := f.ensureDir(filepath.Dir(f.keyPath)); err != nil {
		return nil, err
	}
	fh, err := f.createOwnerOnly(f.keyPath)
	if errors.Is(err, os.ErrExist) { // another process won the race
		return f.readKeyFile()
	}
	if err != nil {
		return nil, err
	}
	k := make([]byte, 32)
	if _, err = io.ReadFull(rand.Reader, k); err == nil {
		_, err = fh.WriteString(hex.EncodeToString(k) + "\n")
	}
	if err == nil {
		err = fh.Sync()
	}
	if cerr := fh.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.keyPath)
		return nil, err
	}
	return k, nil
}

func readBounded(path string) ([]byte, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	b, err := io.ReadAll(io.LimitReader(fh, maxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxFileSize {
		return nil, ErrFormat
	}
	return b, nil
}

func gcm(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func seal(key, pt []byte, aad string) (sealed, error) {
	g, err := gcm(key)
	if err != nil {
		return sealed{}, err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return sealed{}, err
	}
	return sealed{
		Nonce: base64.StdEncoding.EncodeToString(nonce),
		Data:  base64.StdEncoding.EncodeToString(g.Seal(nil, nonce, pt, []byte(aad))),
	}, nil
}

func open(key []byte, s sealed, aad string) ([]byte, error) {
	g, err := gcm(key)
	if err != nil {
		return nil, err
	}
	enc := base64.StdEncoding.Strict()
	nonce, err1 := enc.DecodeString(s.Nonce)
	ct, err2 := enc.DecodeString(s.Data)
	if err1 != nil || err2 != nil || len(nonce) != g.NonceSize() {
		return nil, ErrFormat
	}
	pt, err := g.Open(nil, nonce, ct, []byte(aad))
	if err != nil {
		return nil, ErrAuth
	}
	return pt, nil
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
