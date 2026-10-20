package secrets

import (
	"strings"
)

const (
	envPrefix       = "HOLOCRON_SECRET_"
	envKeyName      = "HOLOCRON_SECRET_KEY" // the file backend's key, never a secret of this store
	legacyUserVar   = "HOLOCRON_CAMERA_USER"
	legacyPasswdVar = "HOLOCRON_CAMERA_PASSWORD"
)

// envProvider is read-only. Secret NAME is HOLOCRON_SECRET_<NAME>, upper-cased with "." and "-"
// turned into "_"; camera.user and camera.password also read the two legacy variables.
type envProvider struct{ environ func() []string }

func newEnv(environ func() []string) Provider { return envProvider{environ} }

func (envProvider) Backend() string { return BackendEnv }

func envVar(name string) string {
	return envPrefix + strings.ToUpper(strings.NewReplacer(".", "_", "-", "_").Replace(name))
}

func (e envProvider) vars() map[string]string {
	m := map[string]string{}
	for _, kv := range e.environ() {
		if i := strings.IndexByte(kv, '='); i > 0 && kv[i+1:] != "" {
			k := kv[:i]
			if strings.HasPrefix(strings.ToUpper(k), "HOLOCRON_") { // Windows names are case-insensitive
				m[strings.ToUpper(k)] = kv[i+1:]
			}
		}
	}
	return m
}

func legacyVar(name string) string {
	switch name {
	case CameraUserName:
		return legacyUserVar
	case CameraPasswordName:
		return legacyPasswdVar
	}
	return ""
}

func (e envProvider) lookup(vars map[string]string, name string) (string, bool) {
	v := envVar(name)
	if v == envKeyName {
		return "", false
	}
	if val, ok := vars[v]; ok {
		return val, true
	}
	if l := legacyVar(name); l != "" {
		val, ok := vars[l]
		return val, ok
	}
	return "", false
}

func (e envProvider) Get(name string) (string, error) {
	if err := checkName(name); err != nil {
		return "", err
	}
	if v, ok := e.lookup(e.vars(), name); ok {
		return v, nil
	}
	return "", notFound(name)
}

func (envProvider) Put(name, value string) error {
	if err := checkName(name); err != nil {
		return err
	}
	return ErrReadOnly
}

func (envProvider) Delete(name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	return ErrReadOnly
}

// List returns the names of the set variables. The variable form cannot tell "." from "_", so a
// variable lists as its lower-cased name with "_" (camera_user), except the two camera names.
func (e envProvider) List() ([]string, error) {
	seen := map[string]bool{}
	for k := range e.vars() {
		switch {
		case k == legacyUserVar:
			seen[CameraUserName] = true
		case k == legacyPasswdVar:
			seen[CameraPasswordName] = true
		case strings.HasPrefix(k, envPrefix) && k != envKeyName:
			n := strings.ToLower(k[len(envPrefix):])
			switch n {
			case "camera_user":
				n = CameraUserName
			case "camera_password":
				n = CameraPasswordName
			}
			if ValidName(n) {
				seen[n] = true
			}
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	return sorted(names), nil
}
