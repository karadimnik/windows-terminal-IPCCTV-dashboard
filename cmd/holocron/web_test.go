package main

import (
	"testing"

	"holocron/internal/app"
	"holocron/internal/osx"
)

// The web role must look for the live-view secret where the supervisor does: in the platform's auth
// folder (internal/web proves the two resolve alike once it is given that folder).
func TestWebBaseLiveSecretDirIsTheSupervisorsAuthFolder(t *testing.T) {
	plat := &osx.Fake{DirsValue: osx.Dirs{Auth: "/state/auth"}}
	e := &env{opts: app.Options{Platform: plat}}
	if got := webBase(e).LiveSecretDir; got != "/state/auth" {
		t.Errorf("LiveSecretDir = %q", got)
	}
}
