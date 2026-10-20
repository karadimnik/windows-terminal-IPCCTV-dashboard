# internal/relay

The two MediaMTX configurations (recording relay, live view), the per-camera source environment, the
secret files, the control-API client with its ready set, and the start specification of the MediaMTX
children. Port of `Get-CamRelayConfigText`, `Get-CamLiveConfigText`, `Get-CamRelayApiCredential`,
`Invoke-CamRelayApi`, `Get-CamRelayReadyPaths`, `Test-CamRelayConfigCurrent`, `Get-CamLivePathMap`,
`Get-CamLiveCredential` (LLD 2.6, IS section 4). The generated text is **byte-identical** to the
PowerShell output for every case in `testdata/golden/relay` and `live`.

## API

```go
// names and paths
func PathName(cameraName string) (string, error)               // safe name, ASCII lowercase, [a-z0-9] only
func RelayPaths(cams []config.Camera) ([]RelayPath, error)     // collision = error
func LivePaths(cams []config.Camera, cfg config.Config) ([]LivePath, error) // <p>sub, <p>main; Quality==LiveSubStream: main only
func SourceEnvName(path string) string                         // MTX_PATHS_<UPPER>_SOURCE

// generators (exact bytes, CRLF, no credential: the login appears as its sha256 only)
type RelayInputs struct{ Config config.Config; Cameras []config.Camera; API Login }
type LiveInputs  struct{ Config config.Config; Cameras []config.Camera; Login Login }
func RelayConfig(RelayInputs) ([]byte, error)                  // mediamtx.template.yml (embedded copy) + 5 replacements
func LiveConfig(LiveInputs) ([]byte, error)
func WriteConfigIfChanged(path string, text []byte) (bool, error) // temp + rename, UTF-8 no BOM, no-op when identical

// environment (credentials only here)
type CredentialSource interface{ CameraCredential(cam config.Camera) (user, password string, err error) }
func SourceEnv(cams []config.Camera, s CredentialSource) ([]string, error)                       // relay
func LiveSourceEnv(cams []config.Camera, cfg config.Config, s CredentialSource) ([]string, error) // live sub paths
func ChildEnv(parent, source []string) []string                // parent env without MTX_*, plus source entries

// secrets on disk (64 lowercase hex, no newline, owner-only, temp + rename)
type Login struct{ User, Secret string }                       // prints as the user only; PassHash() = "sha256:"+base64(sha256(secret))
func LoadAPICredential(p osx.Platform, path string, create bool) (Login, error) // user camrelayapi
func LoadLiveSecret(p osx.Platform, path string, create bool) (Login, error)    // user camlive
func DefaultAPICredentialPath(p) string; func LiveSecretPath(p, cfg) string

// control API
func NewAPI(base string, login Login, timeout time.Duration) *API
func (a *API) Ready(ctx) (ReadySet, error)                     // ONE GET /v3/paths/list per cycle
func (a *API) WaitStarted(ctx, ReadinessPolicy, exited <-chan struct{}) (ReadySet, bool)
type ReadySet struct{ State State; ... }                       // Has(path), Names()
State: OK | Denied | Unavailable                               // errors.Is(err, ErrUnavailable / ErrMalformed)
type LoginWatch ...  Observe(State) Transition                 // LoginRefused once per episode (alert 130), LoginAccepted
func ConfigCurrent(path string, in RelayInputs) CurrentResult  // paths set + auth block; NeedsRewrite(); RewriteMessage(path)

// child start (the process start itself belongs to the supervisor)
func RelayChildSpec(p, cfg, logPath, parentEnv, sourceEnv) (ChildSpec, error)
func LiveChildSpec(...)  (ChildSpec, error)
func NewReadinessPolicy(cfg) ReadinessPolicy                   // StartupWait 20 s, ReadyWait 15 s, Poll 500 ms, ProbeTimeout 1.5 s
func (ReadinessPolicy) HoldChild(now, relayStartedAt, relayAlive, pathServing) bool
```

## Semantics worth knowing

* **Denied is not down.** HTTP 401/403, or a credentialed request that runs out of time and is then
  refused at once when asked again without a login (MediaMTX delays a WRONG login by up to ~4 s and
  refuses a MISSING one immediately), gives `State == Denied`, `err == nil`, and `Has(any path) == true`:
  the relay is serving RTSP, so no consumer is sent to a direct camera input. Everything else
  (refused, timed out twice, not 2xx, redirect, malformed JSON) is `Unavailable` with `ErrUnavailable`
  (and `ErrMalformed` for a 2xx that is not a path list). A cancelled ctx returns `ctx.Err()`.
  Measured against MediaMTX v1.21.2: wrong login answered after 1.2-3.1 s (varies per request), no login at once.
* **Camera credentials never reach a file.** Each path keeps `source: publisher`; the URL is in
  `MTX_PATHS_<NAME>_SOURCE` of the child. `ChildEnv` removes inherited `MTX_*` variables, which would
  override the generated file. The user and password in a source URL are percent-escaped.
* **Working directory** is the config file's folder, local and absolute; a UNC or `\\wsl` path is refused.
* **Secret files**: a file that does not hold 64 hex characters is rejected, never rewritten (delete it
  to get a new one), as in PowerShell. A file readable by others is repaired (Unix: chmod 0600;
  Windows: `icacls /reset` then `osx.SetOwnerOnly`) before the secret is used.
* `ConfigCurrent` compares the SET of path names and the authentication block (case-sensitive), not the
  text: a camera's new DHCP address only changes a comment.
* `mediamtx.template.yml` is embedded from a copy in this folder; a test keeps it byte-equal to the
  repository's template and checks its SHA-256 against `relay-live-inputs.json`.

## Deliberate differences from PowerShell

* `RelayConfig` and `LiveConfig` return an error as well (name and collision errors, an address that
  could not sit in a YAML scalar, a login user with odd characters).
* A line break or other control character in a camera name or address is written as a space in the
  comment line, so a name cannot write configuration; identical output for every normal name.
* The relay and the live child start in the config file's folder (PowerShell used the executable's
  folder for the relay: the same in the default install).
* The API login is a plain owner-only file (PowerShell: DPAPI Clixml); Go cannot read Clixml.
* One list request replaces the per-path `v3/paths/get` probe of `Test-CamRelayPathReady`.

## Tests

`go test ./internal/relay` runs the goldens, the leak scan (with positive controls), the API table
tests against a fake HTTP server and the file tests. The throwaway MediaMTX check is skipped unless
`HOLOCRON_IT_MEDIAMTX` and `HOLOCRON_IT_FFMPEG` are set; it copies the executable into its own temp
folder, uses ports 18560-18599 only with every unneeded listener off, asserts the listener set per
pid with `netstat -ano`, and kills only the processes it started. Build it as a Windows binary and run
it from `/mnt/c/Windows/Temp` (see `tests/holocron/` for the pattern):

    GOOS=windows go test -c -o /mnt/c/Windows/Temp/w6/relay.test.exe ./internal/relay
    cd /mnt/c/Windows/Temp/w6 && WSLENV=HOLOCRON_IT_MEDIAMTX:HOLOCRON_IT_FFMPEG \
      HOLOCRON_IT_MEDIAMTX='C:\mediamtx\mediamtx.exe' HOLOCRON_IT_FFMPEG='C:\ffmpeg\bin\ffmpeg.exe' \
      ./relay.test.exe -test.run 'TestThrowawayMediaMTX|TestSecretFileAcl' -test.v
