package relay

import (
	"fmt"
	"net/url"
	"strings"

	"holocron/internal/config"
)

// CredentialSource hands out a camera's login. The secrets package implements it later; today every
// camera shares one login (camera_creds.xml), so an implementation may ignore the camera.
type CredentialSource interface {
	CameraCredential(cam config.Camera) (user, password string, err error)
}

// SourceURL is Get-CamRtspUrl: rtsp://user:password@ip/stream. It carries the password: it goes into
// a child's environment block and nowhere else. User and password are percent-escaped (unlike
// PowerShell) so a password with @, / or : still parses; for a plain one the text is identical.
func SourceURL(cam config.Camera, stream, user, password string) string {
	u := url.URL{Scheme: "rtsp", User: url.UserPassword(user, password), Host: cam.IP, Path: "/" + stream}
	return u.String()
}

// SourceEnv returns the relay child's source variables, one MTX_PATHS_<NAME>_SOURCE=<url> per
// camera, in path order. It does not include the parent's environment (see ChildEnv).
func SourceEnv(cameras []config.Camera, secrets CredentialSource) ([]string, error) {
	paths, err := RelayPaths(cameras)
	if err != nil {
		return nil, err
	}
	env := make([]string, 0, len(paths))
	for _, p := range paths {
		user, pass, err := secrets.CameraCredential(p.Camera)
		if err != nil {
			return nil, fmt.Errorf("relay: credential for a camera: %w", err)
		}
		env = append(env, SourceEnvName(p.Name)+"="+SourceURL(p.Camera, p.Camera.Stream(), user, pass))
	}
	return env, nil
}

// LiveSourceEnv is the live child's: one variable per sub path, the camera's LiveSubStream.
func LiveSourceEnv(cameras []config.Camera, cfg config.Config, secrets CredentialSource) ([]string, error) {
	paths, err := LivePaths(cameras, cfg)
	if err != nil {
		return nil, err
	}
	var env []string
	for _, p := range paths {
		if p.Kind != LiveSub {
			continue
		}
		user, pass, err := secrets.CameraCredential(p.Camera)
		if err != nil {
			return nil, fmt.Errorf("relay: credential for a camera: %w", err)
		}
		env = append(env, SourceEnvName(p.Name)+"="+SourceURL(p.Camera, cfg.LiveSubStream, user, pass))
	}
	return env, nil
}

// ChildEnv is the environment of a MediaMTX child: the parent's with every MTX_* variable removed,
// then the source entries. MediaMTX maps any MTX_* variable onto its configuration, so one that
// leaked in from the parent would silently override the generated file.
func ChildEnv(parent, source []string) []string {
	out := make([]string, 0, len(parent)+len(source))
	for _, kv := range parent {
		if len(kv) >= 4 && strings.EqualFold(kv[:4], "MTX_") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, source...)
}
