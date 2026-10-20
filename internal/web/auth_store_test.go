package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestUserStore(t *testing.T) (*fileUserStore, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "auth")
	return newFileUserStore(dir, time.Now), dir
}

func TestUserStoreCreateGetListDelete(t *testing.T) {
	store, dir := newTestUserStore(t)
	if users, err := store.List(); err != nil || len(users) != 0 {
		t.Fatalf("an empty store: %v %v", users, err)
	}
	if _, ok, err := store.Get("nobody"); ok || err != nil {
		t.Fatalf("Get on empty: ok=%v err=%v", ok, err)
	}
	a := User{ID: "id-a", Username: "Anna", Hash: "h1", Role: "admin", CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	if err := store.Put(a); err != nil {
		t.Fatal(err)
	}
	// Case-insensitive lookup returns the stored spelling.
	for _, name := range []string{"Anna", "anna", "ANNA"} {
		got, ok, err := store.Get(name)
		if err != nil || !ok || got != a {
			t.Errorf("Get(%q) = %+v %v %v", name, got, ok, err)
		}
	}
	if got, ok, _ := store.GetByID("id-a"); !ok || got.Username != "Anna" {
		t.Errorf("GetByID = %+v %v", got, ok)
	}
	// Update by id keeps one record.
	a.Hash = "h2"
	if err := store.Put(a); err != nil {
		t.Fatal(err)
	}
	if users, _ := store.List(); len(users) != 1 || users[0].Hash != "h2" {
		t.Errorf("update duplicated or lost: %+v", users)
	}
	// A restart (a new store over the same folder) sees it.
	again := newFileUserStore(dir, time.Now)
	if got, ok, _ := again.Get("anna"); !ok || got.Hash != "h2" {
		t.Errorf("not persisted: %+v %v", got, ok)
	}
	if err := store.Delete("id-a"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := again.Get("anna"); ok {
		t.Error("a second store instance (the CLI) did not see the delete")
	}
	if err := store.Delete("id-a"); err != nil {
		t.Errorf("deleting twice must not fail: %v", err)
	}
}

func TestUserStoreRejectsBadRecords(t *testing.T) {
	store, _ := newTestUserStore(t)
	good := User{ID: "1", Username: "anna", Hash: "h", Role: "admin"}
	if err := store.Put(good); err != nil {
		t.Fatal(err)
	}
	cases := map[string]User{
		"duplicate name, other case":  {ID: "2", Username: "ANNA", Hash: "h", Role: "admin"},
		"empty name":                  {ID: "3", Username: "", Hash: "h", Role: "admin"},
		"65 characters":               {ID: "4", Username: strings.Repeat("a", 65), Hash: "h", Role: "admin"},
		"space":                       {ID: "5", Username: "an na", Hash: "h", Role: "admin"},
		"slash":                       {ID: "6", Username: "a/b", Hash: "h", Role: "admin"},
		"unicode":                     {ID: "7", Username: "an\u00e1", Hash: "h", Role: "admin"},
		"newline":                     {ID: "8", Username: "an\nna", Hash: "h", Role: "admin"},
		"no id":                       {Username: "bob", Hash: "h", Role: "admin"},
		"no role":                     {ID: "9", Username: "bob", Hash: "h"},
		"no hash":                     {ID: "10", Username: "bob", Role: "admin"},
		"rename onto an existing one": {ID: "1", Username: "anna", Hash: "h", Role: "admin"}, // same id is fine, see below
	}
	for name, u := range cases {
		err := store.Put(u)
		if name == "rename onto an existing one" {
			if err != nil {
				t.Errorf("%s: updating one's own record must work: %v", name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if users, _ := store.List(); len(users) != 1 {
		t.Errorf("rejected records were stored: %+v", users)
	}
	ok := User{ID: "11", Username: strings.Repeat("a", 63) + ".", Hash: "h", Role: "admin"}
	if err := store.Put(ok); err != nil {
		t.Errorf("64 characters with a dot: %v", err)
	}
	if err := store.Put(User{ID: "12", Username: "A_b-c.9", Hash: "h", Role: "admin"}); err != nil {
		t.Errorf("allowed punctuation: %v", err)
	}
}

func TestUserStoreFileShapeAndAtomicWrite(t *testing.T) {
	store, dir := newTestUserStore(t)
	created := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	if err := store.Put(User{ID: "x", Username: "anna", Hash: "pbkdf2-sha256$1$a$b", Role: "admin", CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Version int `json:"version"`
		Users   []map[string]any
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != 1 || len(doc.Users) != 1 {
		t.Fatalf("schema: %s", raw)
	}
	for _, key := range []string{"id", "username", "hash", "role", "createdAt"} {
		if _, ok := doc.Users[0][key]; !ok {
			t.Errorf("missing %q in %s", key, raw)
		}
	}
	if len(doc.Users[0]) != 5 {
		t.Errorf("unexpected fields: %v", doc.Users[0])
	}
	// No temp file is left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
	assertOwnerOnly(t, filepath.Join(dir, "users.json"), false)
	assertOwnerOnly(t, dir, true)
}

func TestUserStoreCorruptFileFailsClosed(t *testing.T) {
	for name, content := range map[string]string{
		"not json":        "{{{",
		"future version":  `{"version":2,"users":[]}`,
		"a bad user":      `{"version":1,"users":[{"id":"1","username":"a b","hash":"h","role":"admin"}]}`,
		"duplicate names": `{"version":1,"users":[{"id":"1","username":"a","hash":"h","role":"admin"},{"id":"2","username":"A","hash":"h","role":"admin"}]}`,
	} {
		store, dir := newTestUserStore(t)
		os.MkdirAll(dir, 0o700)
		os.WriteFile(filepath.Join(dir, "users.json"), []byte(content), 0o600)
		if _, _, err := store.Get("a"); err == nil {
			t.Errorf("%s: read as if it were empty", name)
		}
		if err := store.Put(User{ID: "9", Username: "zed", Hash: "h", Role: "admin"}); err == nil {
			t.Errorf("%s: a write over a file we cannot read", name)
		}
	}
}

func TestUserStoreSeesChangesMadeByAnotherProcess(t *testing.T) {
	running, dir := newTestUserStore(t)
	cli := newFileUserStore(dir, time.Now)
	if err := cli.Put(User{ID: "1", Username: "anna", Hash: "old", Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	if u, ok, _ := running.Get("anna"); !ok || u.Hash != "old" {
		t.Fatalf("running store missed the new user: %+v", u)
	}
	if err := cli.Put(User{ID: "1", Username: "anna", Hash: "new-and-longer", Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	if u, _, _ := running.Get("anna"); u.Hash != "new-and-longer" {
		t.Errorf("running store kept the old hash: %+v", u)
	}
}

func TestNewUserIDIsRandomAndHex(t *testing.T) {
	a, b := newID(), newID()
	if a == b || len(a) != 32 {
		t.Errorf("ids: %q %q", a, b)
	}
}
