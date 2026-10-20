package status

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrNoStatus: the file is missing or empty (no status has been written yet).
	ErrNoStatus = errors.New("status: no status file")
	// ErrInvalid: the file exists but is not a status document.
	ErrInvalid = errors.New("status: invalid status file")
)

const (
	readAttempts   = 5
	readRetryDelay = 80 * time.Millisecond
)

// Replaceable in tests.
var (
	readOnce    = readFileShared // per-OS: share-delete open on Windows
	isTransient = isSharingError // per-OS: sharing violation / access denied
	sleep       = time.Sleep
)

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// ReadRaw returns the BOM-stripped bytes of status.json. A missing or empty file
// is ErrNoStatus, text that is not a JSON object is ErrInvalid; other I/O errors
// are returned as they are. A sharing violation or access denied (Windows) is
// retried 5 x 80 ms.
func ReadRaw(path string) ([]byte, error) {
	var raw []byte
	var err error
	for attempt := 1; ; attempt++ {
		raw, err = readOnce(path)
		if err == nil || !isTransient(err) || attempt == readAttempts {
			break
		}
		sleep(readRetryDelay)
	}
	if err != nil {
		if errors.Is(err, errNotExist) {
			return nil, fmt.Errorf("%w: %v", ErrNoStatus, err)
		}
		return nil, err
	}
	raw = bytes.TrimPrefix(raw, utf8BOM)
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("%w: empty", ErrNoStatus)
	}
	if trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil, fmt.Errorf("%w: not a JSON object", ErrInvalid)
	}
	return raw, nil
}

// Read reads and decodes status.json. Types must match the schema; unknown keys
// are ignored (forward compatibility). Use ReadRaw to keep the original bytes.
func Read(path string) (*Document, error) {
	raw, err := ReadRaw(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse decodes status.json text (BOM allowed).
func Parse(raw []byte) (*Document, error) {
	raw = bytes.TrimPrefix(raw, utf8BOM)
	var doc Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return &doc, nil
}
