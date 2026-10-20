# app

Production wiring of holocron, shared by the command line (`cmd/holocron`) and the control API backend.

    type Paths struct{ Root, Config, Cameras string }   // State(), Logs(), StatusFile(), StopFile(), HostFile(), LockFile()
    func Resolve(root, configPath string, plat) (Paths, config.Config, []config.Warning, error) // --root overrides RootPath; sandbox guard
    func CheckSandboxRoot(root string) error            // HOLOCRON_SANDBOX_ONLY=1: drive/fs root, a "cameras" folder, PowerShell recorder files -> ErrSandbox
    func NewLogger(LogOptions) (*logx.Logger, []error)  // holocron.log + audit.log (rotating), stderr mirror, HOLOCRON_LOG, -v/-vv
    type Credentials interface{ CameraLogin() (user, password string, err error) }
    type EnvCredentials; var CredentialProvider func(config.Config) (Credentials, error)  // the secrets seam (nil: environment)
    func NewSupervisor(Options) (*SupervisorRole, error) // config, logger + Events ring, alerts dispatcher, credentials (missing = ConfigFault 21),
                                                         // ctlapi server, supervise.NewProduction (lock, scope, pruner + root claim, status, relay)
    (*SupervisorRole) Run(ctx) int
    func RunHost(ctx, HostOptions) int                   // osx scope, host.json, ExecSpawner(<exe> <flags> supervisor), alerts 100-103
    func Detach(exe, args) (pid, note, err)              // Windows DETACHED_PROCESS|NEW_PROCESS_GROUP|NO_WINDOW (+breakaway); Unix Setsid
    func EditConfig(path, key, value json.RawMessage, plat) (ConfigEdit, error) // allow-list, validate first, in place, BOM, atomic
    var EditableKeys; CanonicalKey; ParseValue; EffectiveConfig; EffectiveValue
    func SetRecord / AddCamera / RemoveCamera / SetCamera / FindCamera / LoadCameras // cameras.json edits
    func TakeCensus(Paths, osx.Inspector, statusInterval) Census   // host.json, instance lock / heartbeat, children.json; identity, never names
    func Down(ctx, Paths, cfg, dial, inspector) DownResult         // API stop or stop.request, then proved; kills nothing
    func Running / WaitRunning                                     // status.Verdict; a fresh heartbeat after the start (G10)

When `internal/secrets` is merged, `CredentialProvider` returns an adapter over `secrets.Open(cfg)` and
`secrets.CameraLogin(p)`; `secrets.CredentialSource(p)` matches `relay.CredentialSource` directly.
