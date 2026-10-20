// Package config holds the types, defaults and validation of holocron.config.json (the keys of the
// PowerShell recorder.config.json that the Go core reads) and of cameras.json. KEYS.md lists every
// key, its default, and why each dropped key is dropped; README.md has the API.
//
// Strictness: a JSON string is never accepted for a boolean or a number, every problem is reported
// at once with its key path (*ValidationError), unknown keys are warnings, and a leading UTF-8 BOM
// is tolerated. Defaults fill every missing key.
package config
