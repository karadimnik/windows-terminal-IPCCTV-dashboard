// Package secrets is a portable store for the camera login and any other named secret
// (LLD 2.8, decision D12). Three backends sit behind Provider: keyring (Windows Credential
// Manager), file (AES-256-GCM) and env (read-only). See README.md.
package secrets
