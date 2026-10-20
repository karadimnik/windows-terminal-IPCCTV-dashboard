package status

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTemp(t *testing.T, content []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "status.json")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadTypedErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.json")
	cases := []struct {
		name string
		path string
		want error
	}{
		{"missing", missing, ErrNoStatus},
		{"empty", writeTemp(t, nil), ErrNoStatus},
		{"whitespace", writeTemp(t, []byte(" \r\n\t")), ErrNoStatus},
		{"bom only", writeTemp(t, utf8BOM), ErrNoStatus},
		{"truncated", writeTemp(t, []byte(`{"Schema": 1, "State": "runn`)), ErrInvalid},
		{"garbage", writeTemp(t, []byte("not json")), ErrInvalid},
		{"array", writeTemp(t, []byte(`[1]`)), ErrInvalid},
		{"null", writeTemp(t, []byte(`null`)), ErrInvalid},
		{"trailing", writeTemp(t, []byte(`{"Schema":1} {"x":1}`)), ErrInvalid},
		{"wrong type", writeTemp(t, []byte(`{"Schema":1,"SupervisorPid":"abc"}`)), ErrInvalid},
	}
	for _, c := range cases {
		doc, err := Read(c.path)
		if doc != nil || !errors.Is(err, c.want) {
			t.Errorf("%s: got (%v, %v), want %v", c.name, doc, err, c.want)
		}
		if _, rerr := ReadRaw(c.path); c.want == ErrNoStatus && !errors.Is(rerr, ErrNoStatus) {
			t.Errorf("%s: ReadRaw %v", c.name, rerr)
		}
	}
}

func TestReadStripsBOMAndToleratesUnknownKeys(t *testing.T) {
	body := append(append([]byte{}, utf8BOM...), []byte(`{"Schema":1,"State":"stopped","StopKind":"requested","SupervisorPid":7,"UpdatedAt":null,"FutureKey":{"a":1}}`)...)
	p := writeTemp(t, body)
	doc, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if doc.State != "stopped" || doc.SupervisorPid != 7 {
		t.Fatalf("decoded wrong: %+v", doc)
	}
	raw, err := ReadRaw(p)
	if err != nil || strings.HasPrefix(string(raw), "\xEF\xBB\xBF") || !strings.Contains(string(raw), "FutureKey") {
		t.Fatalf("raw: %q %v", raw, err)
	}
}

func TestReadFixtures(t *testing.T) {
	for _, f := range positiveFixtures(t) {
		doc, err := Read(f)
		if err != nil || doc.Schema != 1 {
			t.Errorf("%s: %v", filepath.Base(f), err)
		}
	}
}

func TestReadRetriesTransientErrors(t *testing.T) {
	oldOnce, oldTrans, oldSleep := readOnce, isTransient, sleep
	defer func() { readOnce, isTransient, sleep = oldOnce, oldTrans, oldSleep }()
	boom := errors.New("sharing violation")
	isTransient = func(err error) bool { return err == boom }
	var slept []time.Duration
	sleep = func(d time.Duration) { slept = append(slept, d) }

	calls := 0
	readOnce = func(string) ([]byte, error) {
		calls++
		if calls < 3 {
			return nil, boom
		}
		return []byte(`{"Schema":1}`), nil
	}
	if _, err := ReadRaw("x"); err != nil || calls != 3 || len(slept) != 2 || slept[0] != 80*time.Millisecond {
		t.Fatalf("recover: err %v calls %d slept %v", err, calls, slept)
	}

	calls, slept = 0, nil
	readOnce = func(string) ([]byte, error) { calls++; return nil, boom }
	if _, err := ReadRaw("x"); err != boom || calls != 5 || len(slept) != 4 {
		t.Fatalf("give up: err %v calls %d slept %d", err, calls, len(slept))
	}

	calls = 0
	other := errors.New("other")
	readOnce = func(string) ([]byte, error) { calls++; return nil, other }
	if _, err := ReadRaw("x"); err != other || calls != 1 {
		t.Fatalf("non-transient: err %v calls %d", err, calls)
	}
}
