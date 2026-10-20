//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The point of the change (SP6): a reader that holds status.json open must not
// stop the writer from replacing or deleting it. Deleting is the observable
// proxy for FILE_SHARE_DELETE; a plain os.Open handle (the old os.ReadFile)
// blocks it, which is the negative control.
func TestStatusReaderHandleDoesNotBlockDelete(t *testing.T) {
	dir := t.TempDir()

	plain := filepath.Join(dir, "plain.json")
	os.WriteFile(plain, []byte("{}"), 0o600)
	f, err := os.Open(plain)
	if err != nil {
		t.Fatal(err)
	}
	blocked := os.Remove(plain)
	f.Close()
	if blocked == nil {
		t.Fatal("negative control: an os.Open handle did not block the delete")
	}

	shared := filepath.Join(dir, "shared.json")
	os.WriteFile(shared, []byte("{}"), 0o600)
	h, err := openShared(shared)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err := os.Remove(shared); err != nil {
		t.Fatalf("share-delete handle blocked the delete: %v", err)
	}
}
