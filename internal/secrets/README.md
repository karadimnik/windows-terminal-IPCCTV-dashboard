# secrets

`internal/secrets` is the portable store for the camera login and any other named secret (LLD 2.8, decision D12).
It replaces `camera_creds.xml` (DPAPI, one Windows user, read through `Import-Clixml`). Standard library only
(`syscall` for the Windows APIs).

## API

```go
type Provider interface {
    Get(name string) (string, error)   // absent: error wrapping ErrNotFound
    Put(name, value string) error
    Delete(name string) error          // absent: error wrapping ErrNotFound
    List() ([]string, error)           // names only, sorted
    Backend() string                   // "keyring" | "file" | "env"
}
func Open(cfg Config) (Provider, error)                    // selects by cfg.Backend, wraps with cfg.Redactor
func WithRedactor(p Provider, r Redactor) Provider         // Redactor = interface{ Register(string) } (*logx.Redactor)
func CameraLogin(p Provider) (user, password string, err error)   // names camera.user / camera.password
func CredentialSource(p Provider) CameraCredentialSource   // method set of relay.CredentialSource (not imported)
func Lookup(p Provider) func(name string) (string, bool)   // alerts.SecretLookup
func ImportLegacyCameraCreds(ctx, path string, dst Provider) error   // Windows; ErrUnsupported elsewhere
```

Names are `[a-z0-9._-]{1,64}`; values are 1 to 4096 bytes of UTF-8. Errors: `ErrNotFound`, `ErrReadOnly`,
`ErrUnsupported`, `ErrInvalidName`, `ErrInvalidValue`, `ErrAuth` (tampered file or wrong key), `ErrFormat`,
`ErrNoKey`. No error text carries a value. Every successful `Get` and `Put` registers the value with
`Config.Redactor`, so a secret that was read is masked in every later log record.

## Config keys (the `secrets` section; the config package does not read them yet)

| Key | Default | Meaning |
|---|---|---|
| `secrets.backend` | `auto` | `auto`, `keyring`, `file`, `env` |
| `secrets.file` | `<auth dir>/secrets.enc` | encrypted store (`file` backend) |
| `secrets.keyFile` | `<auth dir>/secrets.key` | key file when `HOLOCRON_SECRET_KEY` is not set |

`secrets.Config` carries these as `json:"backend"`, `json:"file"`, `json:"keyFile"`; the other fields (`Redactor`,
`Platform`, `Environ`) are code-only. `<auth dir>` is `osx.Dirs().Auth`.

`auto`: Windows uses the keyring when the Credential Manager answers, else the file. Linux and macOS use the
file: there is no Secret Service without a dependency, and macOS Keychain (the `security` CLI) is a follow-up.
An explicit `keyring` on Linux or macOS returns `ErrUnsupported`.

## Backends

**env** (containers, tests): read-only. Name `a.b-c` reads `HOLOCRON_SECRET_A_B_C` (upper-cased, `.` and `-` to
`_`); `camera.user` and `camera.password` also read `HOLOCRON_CAMERA_USER` / `HOLOCRON_CAMERA_PASSWORD`.
`Put`/`Delete` return `ErrReadOnly`. The name `key` is reserved (`HOLOCRON_SECRET_KEY` is the file backend's key and
is never served). `List` cannot tell `.` from `_`, so it reports `push_token` for `HOLOCRON_SECRET_PUSH_TOKEN`.

**file**: one JSON document, AES-256-GCM, written atomically (owner-only temp file, fsync, rename). Per entry: a
random 12-byte nonce and the NAME as additional authenticated data, so entries cannot be swapped. A `check` seal
covers every name and ciphertext, so dropping, adding, renaming or editing anything makes every operation fail with
`ErrAuth`, and a wrong key is caught even on an empty store. A failed read never returns partial data. At most 256
entries and 4 MiB. A temp file left by a crash is ignored (it holds only ciphertext). Files and a newly created
directory are made owner-only (`osx.SetOwnerOnly`); if that fails nothing is written (fail closed). On Unix a key
file that others can read is refused.

The key: `HOLOCRON_SECRET_KEY` (64 hex characters) if set, otherwise `<auth dir>/secrets.key`, created on first
write (not on read) with 32 random bytes from `crypto/rand`, hex-encoded, owner-only.

What the file backend protects against: another OS user on the machine; a copy of `secrets.enc` taken without the
key (a backup, a support bundle, a mistaken commit); an edit of the file by anyone without the key. What it does
NOT protect against: anyone who can read both `secrets.enc` and `secrets.key` as the same user (malware running as
the Operator, an admin or root); rolling the file back to an older, validly sealed copy; two processes writing at
once (the last write wins, there is no cross-process lock). For stronger separation, supply `HOLOCRON_SECRET_KEY`
from the service manager (a systemd credential or unit environment, a Windows service environment) so the key
never sits next to the data.

**keyring** (Windows only): Credential Manager through `advapi32` (`CredWriteW`, `CredReadW`, `CredDeleteW`,
`CredEnumerateW`), GENERIC credentials named `holocron/<name>`, `CRED_PERSIST_LOCAL_MACHINE`; protected by the
user's logon (DPAPI underneath), like `camera_creds.xml`. The constructor probes with a read of a random absent
target and fails (so `auto` falls back to the file) in a session where the Credential Manager is unusable. The
Credential Manager caps a value at 2560 bytes (`ErrInvalidValue` above that). `List` enumerates only `holocron/*`.

## Importer

`ImportLegacyCameraCreds(ctx, path, dst)` never parses the file. It runs `powershell.exe -NoProfile
-NonInteractive -ExecutionPolicy Bypass -OutputFormat Text -EncodedCommand ...` (hidden window, not elevated, 15 s
limit) whose script runs `Import-Clixml -LiteralPath $env:HOLOCRON_IMPORT_PATH` and writes `{"user","password"}`
as UTF-8 JSON to the child's stdout (at most 16 KB), then stores `camera.user` and `camera.password` with
`dst.Put`. The path travels in the child's environment; the login never appears in a command line, the script
text, or an error. The raw output buffer is zeroed after use; Go strings cannot be, so the values live until
collected. Failure (corrupt file, wrong user, not a `PSCredential`, timeout) stores nothing.

## Tests

`go test -race ./internal/secrets`: suite per backend (round trip, not found, overwrite, delete, names only, name
and value validation), file concurrency, crash leftovers, tamper (every byte flipped: an error or exactly the right
data, never anything else; swap, drop, rename, version), wrong key, key from env versus file, Unix modes, fail
closed, `auto` selection, adapters (compile-time assertions against `relay.CredentialSource` and
`alerts.SecretLookup`), and a leak scan with positive controls (log records, error strings, every file in the
scratch tree).

`TestWindows*` are skipped unless `HOLOCRON_W11_REAL=1`: they use a test-only Credential Manager prefix
`holocron/TEST-<random>/` (removed at the end), read back the ACL with `icacls`, create a FAKE `PSCredential` file
with `Export-Clixml` and import it while a PowerShell sampler counts `Win32_Process` command lines containing the
fake password (it logs counts only; a control process that does carry the password proves the sampler sees it).
Run them from a Windows folder (`GOOS=windows go test -c`, then run the `.exe` from `C:\Windows\Temp`).
