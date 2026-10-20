package relay

import (
	"errors"
	"reflect"
	"testing"

	"holocron/internal/config"
)

func TestSourceEnv(t *testing.T) {
	cameras := []config.Camera{
		{Name: "Camera One", IP: "192.0.2.11", Quality: "stream1"},
		{Name: "Side_Door", IP: "cam.lan", Quality: ""},
	}
	got, err := SourceEnv(cameras, fixedCreds{"fakeuser", "fakepass"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"MTX_PATHS_CAMERAONE_SOURCE=rtsp://fakeuser:fakepass@192.0.2.11/stream1",
		"MTX_PATHS_SIDEDOOR_SOURCE=rtsp://fakeuser:fakepass@cam.lan/stream1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}

	live, err := LiveSourceEnv(append(cameras, config.Camera{Name: "Gate", IP: "192.0.2.5", Quality: "stream2"}), config.Config{LiveSubStream: "stream2"}, fixedCreds{"fakeuser", "fakepass"})
	if err != nil {
		t.Fatal(err)
	}
	// the sub stream, and no variable for Gate: it records stream2 so it has only a main path
	want = []string{
		"MTX_PATHS_CAMERAONESUB_SOURCE=rtsp://fakeuser:fakepass@192.0.2.11/stream2",
		"MTX_PATHS_SIDEDOORSUB_SOURCE=rtsp://fakeuser:fakepass@cam.lan/stream2",
	}
	if !reflect.DeepEqual(live, want) {
		t.Errorf("live: got %v want %v", live, want)
	}
}

func TestSourceURLEscapesTheLogin(t *testing.T) {
	got := SourceURL(config.Camera{IP: "192.0.2.11"}, "stream1", "us er", "p@ss/w:rd%")
	if got != "rtsp://us%20er:p%40ss%2Fw%3Ard%25@192.0.2.11/stream1" {
		t.Error(got)
	}
}

type failingCreds struct{}

func (failingCreds) CameraCredential(config.Camera) (string, string, error) {
	return "", "", errors.New("vault locked")
}

func TestSourceEnvReportsACredentialFailureWithoutAnyURL(t *testing.T) {
	_, err := SourceEnv(cams("stream1"), failingCreds{})
	if err == nil || err.Error() != "relay: credential for a camera: vault locked" {
		t.Error(err)
	}
	if _, err := SourceEnv([]config.Camera{{Name: "A-B"}, {Name: "AB"}}, fixedCreds{"u", "p"}); err == nil {
		t.Error("a path collision must stop the environment")
	}
}

func TestChildEnvDropsInheritedMTXVariables(t *testing.T) {
	parent := []string{"PATH=/bin", "MTX_PATHS_STALE_SOURCE=rtsp://old", "mtx_api=yes", "MTXX=1", "HOME=/h"}
	got := ChildEnv(parent, []string{"MTX_PATHS_A_SOURCE=rtsp://x"})
	want := []string{"PATH=/bin", "MTXX=1", "HOME=/h", "MTX_PATHS_A_SOURCE=rtsp://x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}
