package secrets

import (
	"errors"
	"fmt"

	"holocron/internal/config"
)

// CameraLogin returns the single login every camera shares.
func CameraLogin(p Provider) (user, password string, err error) {
	user, err = p.Get(CameraUserName)
	if err == nil {
		password, err = p.Get(CameraPasswordName)
	}
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", "", fmt.Errorf("secrets: the camera login is not stored (%s and %s): %w",
				CameraUserName, CameraPasswordName, ErrNotFound)
		}
		return "", "", err
	}
	return user, password, nil
}

// CameraCredentialSource has the method set of relay.CredentialSource (not imported here); every
// camera gets the same login.
type CameraCredentialSource struct{ P Provider }

func (s CameraCredentialSource) CameraCredential(_ config.Camera) (user, password string, err error) {
	return CameraLogin(s.P)
}

// CredentialSource adapts p for the relay package.
func CredentialSource(p Provider) CameraCredentialSource { return CameraCredentialSource{p} }

// Lookup adapts p to alerts.SecretLookup: any failure (absent, invalid name, backend error) is
// "not found".
func Lookup(p Provider) func(name string) (string, bool) {
	return func(name string) (string, bool) {
		v, err := p.Get(name)
		return v, err == nil
	}
}
