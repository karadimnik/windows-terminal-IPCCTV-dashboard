# web

The web app of the recorder (the former `camui`): the loopback page, the HTTPS listener with its login for the LAN, Live (the WHEP
proxy), Footage, Settings, acknowledgements. Moved here from the nested module `ui/` (module `camui`, `package main`) with `git mv`;
the code, the embedded `static/` and the tests are the ones that were there. Security model and behaviour: `docs/ui.md`.

    func Run(ctx, Options) error            // both listeners until ctx ends (or Options.StopFile appears), 5 s shutdown; the guards run first
    func ParseFlags(args) (Options, error)  // every camui flag and config-file key, token env variables; ParseFlagsTo(args, out, base) adds the usage writer and a base Config
    func RunOneShot(Options, stdin, stdout, stderr) (code int, done bool)  // -version, -print-ca, -set-password, -unlock
    func (Options) Validate() error         // TLS settings, a user for the LAN listener, the loopback guard (*Refusal)
    func Main(args) int                     // camui.exe's whole main (cmd/camui): the above, text records on stdout, SIGINT/SIGTERM
    type Options struct{ Config; Logs cli.LogSource; Controller; Launch *HolocronLaunch; GoSystem bool; StopFile string; ... }
    func LoginUsers(authDir) (int, error)   // `holocron status`: does a login exist
    func Users(authDir, *slog.Logger) *UserAdmin   // `holocron user`: Add, SetPassword, Remove, List (no hashes), Unlock (admin.go); ErrUserExists, ErrNoSuchUser, IsInputError
    func CAFor(tlsDir) (CAInfo, error); CACertificatePEM(tlsDir); WriteCAInstructions(w, CAInfo); RotateCA(tlsDir)   // `holocron tls`
    type SettingsWriter interface{ PutCameras(ctx, path, []CameraPut); PutConfig(ctx, path, map[string]any) }  // Options.Settings

`holocron web` calls the same functions with `Logs` set to the process's logx logger (component `web`, request lines at DEBUG;
camui.exe keeps slog text lines on stdout with the request lines at INFO, which the PowerShell harness reads), `GoSystem` true and
`Launch` set. Nothing in the security chain (Host/Origin guard, content-type rule, token on loopback, session gate and
view-only gate on the TLS listener, the WHEP exception, certificate handling) was changed by the move.

## The Controller seam (`controller.go`)

    type Controller interface{ Name(); Status(ctx); Start(ctx, scope); Stop(ctx, scope); SetRecord(ctx, camera, on); TaskStates() }

`PowerShellController` is the previous code (`runPowerShell`, `schtasks`) unchanged. `APIController` uses `ctlapi.Dial(root)`:
`GET /v1/state` (Status, task chips), `POST /v1/stop`, `POST /v1/cameras/{name}/record`, with the request's actor and action id
in the headers. The choice is made per request and cached for 5 s: the API controller when `<root>/_state/ctl.addr` answers within
1 s, in the holocron web role the API controller in its "stopped" form when it does not (Start runs `<holocron> up --detach` when
`Launch` is set, otherwise answers 409 "start it with holocron up"; Stop and the record toggle answer 409), PowerShell otherwise.
`GET /api/status` reports the choice in `controller`. In the Go system `PUT /api/cameras` and `PUT /api/config` go to `Options.Settings` (a `SettingsWriter`: the holocron web role
supplies one over the shared edit helpers of `internal/app` and reloads the supervisor; the page's name and stream rules still run
first, the address check is the writer's so `host:port` is accepted, and a blank stream is `stream1`; a writer's `invalid` rows
answer 422 with the fields, an `ErrRefused` 422, any other error 500, nothing written), and answer 501 only when no writer was
supplied; `camui.exe` keeps the PowerShell script; `Config.CamerasPath` / `Config.ConfigPath` point the readers at `cameras.json` and
`holocron.config.json`.

## Rule R2 (`actions.go`)

Every mutating route is registered with `handleAction(mux, runner, "POST /api/control", ActionControl, handler)`: the handler runs as
a `cli.Action` through `cli.Runner`, which writes `action.begin` (DEBUG) and one `action.result` (INFO ok, WARN for a 4xx, 501 or
503, ERROR for other 5xx or a panic, which also answers 500) with action `web.*`, actor `web:<user>` / `web:loopback` /
`web:anonymous`, target, result, `duration_ms`, `action_id`, `audit=true`. `TestEveryRouteIsClassified` fails for a mutating route
registered without it, and for an action name that is not registered. The older `auth.*` records of the login handlers stay.

## Tests

`go test ./internal/web/...` (about 20 s). New: `controller_test.go` (a fake control endpoint), `actions_test.go` (records, actors,
DEBUG request lines, a panic). The PowerShell checks `uiguard` and `uileak` run against `camui.exe` built by `ui/build.sh`
(`CAMUI_TEST_EXE`); `livetls` opens browsers and is run by hand.
