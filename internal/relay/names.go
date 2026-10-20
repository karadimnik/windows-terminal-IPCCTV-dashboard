package relay

import (
	"fmt"
	"strings"

	"holocron/internal/archive"
	"holocron/internal/config"
)

// PathName is Get-CamRelayPathName over Get-CamSafeName: the camera's safe name lowercased with
// every character outside [a-z0-9] removed. The name must stay alphanumeric: MediaMTX reads a
// path's source from MTX_PATHS_<NAME>_SOURCE and cuts the key at the first underscore, so
// workshop_entrance silently becomes a stray "workshop" plus an offline path (mediamtx.template.yml).
//
// Lowercasing is ASCII only, as .NET's ToLowerInvariant is for the characters that matter: it leaves
// U+0130 and the Kelvin sign alone, where Go's unicode.ToLower would turn them into i and k.
func PathName(cameraName string) (string, error) {
	safe, err := archive.SafeName(cameraName)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, r := range safe {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("Camera name '%s' contains no letter or digit, so it cannot name a MediaMTX path. Rename the camera in cameras.json.", safe)
	}
	return b.String(), nil
}

// RelayPath is one path of the recording relay and the camera behind it.
type RelayPath struct {
	Name   string
	Camera config.Camera
}

// RelayPaths is Get-CamRelayPathMap: every camera in the list, in order, not only the recorded ones
// (the dashboard shares the relay). Two cameras mapping to one path is an error, because the quiet
// outcome would be one camera's stream under two names while the other is never pulled.
func RelayPaths(cameras []config.Camera) ([]RelayPath, error) {
	out := make([]RelayPath, 0, len(cameras))
	seen := map[string]int{}
	for i, cam := range cameras {
		name, err := PathName(cam.Name)
		if err != nil {
			return nil, err
		}
		if j, dup := seen[name]; dup {
			return nil, fmt.Errorf("Cameras '%s' and '%s' both map to the MediaMTX path '%s'. Rename one in cameras.json.", cameras[j].Name, cam.Name, name)
		}
		seen[name] = i
		out = append(out, RelayPath{Name: name, Camera: cam})
	}
	return out, nil
}

// LiveKind says where a live path gets its picture.
type LiveKind string

const (
	// LiveSub is pulled from the camera's LiveSubStream on demand; the credentialed URL arrives in the environment.
	LiveSub LiveKind = "sub"
	// LiveMain is the recording relay's own loopback path: no credential, no extra camera session.
	LiveMain LiveKind = "main"
)

// LivePath is one path of the live instance.
type LivePath struct {
	Name      string
	Kind      LiveKind
	RelayPath string
	Camera    config.Camera
}

// LivePaths is Get-CamLivePathMap: <relaypath>sub on demand from the camera's LiveSubStream and
// <relaypath>main from the relay, in that order per camera. A camera whose recording quality already
// is the sub stream gets only main (a second pull of the same stream would spend the camera's second
// session for nothing). A live name equal to a relay path or to another live path is an error.
// The comparison with LiveSubStream is case-insensitive, as PowerShell's -ne is.
func LivePaths(cameras []config.Camera, cfg config.Config) ([]LivePath, error) {
	relay, err := RelayPaths(cameras)
	if err != nil {
		return nil, err
	}
	relayNames := map[string]RelayPath{}
	for _, r := range relay {
		relayNames[r.Name] = r
	}
	liveNames := map[string]LivePath{}
	var out []LivePath
	for _, r := range relay {
		var entries []LivePath
		if !strings.EqualFold(r.Camera.Stream(), cfg.LiveSubStream) {
			entries = append(entries, LivePath{Name: r.Name + "sub", Kind: LiveSub, RelayPath: r.Name, Camera: r.Camera})
		}
		entries = append(entries, LivePath{Name: r.Name + "main", Kind: LiveMain, RelayPath: r.Name, Camera: r.Camera})
		for _, e := range entries {
			if other, ok := relayNames[e.Name]; ok {
				return nil, fmt.Errorf("The live path '%s' for camera '%s' is also the recording relay's path for camera '%s'. Rename a camera in cameras.json.", e.Name, r.Camera.Name, other.Camera.Name)
			}
			if other, ok := liveNames[e.Name]; ok {
				return nil, fmt.Errorf("The live path '%s' for camera '%s' is also the live path of camera '%s'. Rename a camera in cameras.json.", e.Name, r.Camera.Name, other.Camera.Name)
			}
			liveNames[e.Name] = e
			out = append(out, e)
		}
	}
	return out, nil
}

// SourceEnvName is Get-CamRelaySourceEnvName: the variable MediaMTX reads this path's source from.
func SourceEnvName(pathName string) string {
	return "MTX_PATHS_" + strings.ToUpper(pathName) + "_SOURCE"
}
