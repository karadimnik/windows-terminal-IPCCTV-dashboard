//go:build !windows

package secrets

// newKeyring: the Windows Credential Manager only. macOS Keychain (the security CLI) is a
// follow-up; Linux has no Secret Service without a dependency.
func newKeyring(prefix string) (Provider, error) { return nil, ErrUnsupported }
