# app

Production wiring of holocron, shared by the command line (`cmd/holocron`) and the control API backend.

    type Paths struct{ Root, Config, Cameras string }   // State(), Logs(), StatusFile(), StopFile(), HostFile(), LockFile()
    func Resolve(root, configPath string, plat) (Paths, config.Config, []config.Warning, error) // --root overrides RootPath; sandbox guard
    func CheckSandboxRoot(root string) error            // HOLOCRON_SANDBOX_ONLY=1: drive/fs root, a "cameras" folder, PowerShell recorder files -> ErrSandbox
    func NewLogger(LogOptions) (*logx.Logger, []error)  // holocron.log + audit.log (rotating), stderr mirror, HOLOCRON_LOG, -v/-vv
    type Credentials interface{ CameraLogin() (user, password string, err error) }
    func CredentialsFor(cfg, getenv) / CredentialsWith(cfg, plat, getenv, redactor)   // HOLOCRON_CAMERA_USER + _PASSWORD when both set (they win), else secrets.Open(cfg.Secrets) camera.user / camera.password;
                                                         // a login in neither is a ConfigFault naming both places; var CredentialProvider overrides (tests)
    func OpenSecrets(cfg, plat, redactor) (secrets.Provider, error); SecretsConfig(cfg, plat, redactor)   // secrets.backend, secrets.file, secrets.keyFile
    func NewSupervisor(Options) (*SupervisorRole, error) // config, logger + Events ring, alerts dispatcher, credentials (missing = ConfigFault 21),
                                                         // ctlapi server, supervise.NewProduction (lock, scope, pruner + root claim, status, relay)
    (*SupervisorRole) Run(ctx) int
    func RunHost(ctx, HostOptions) int                   // osx scope, host.json, ExecSpawner(<exe> <flags> supervisor), alerts 100-103; the web role as host.Aux (web.go) unless web.enabled is false or NoWeb
    func Detach(exe, args) (pid, note, err)              // Windows DETACHED_PROCESS|NEW_PROCESS_GROUP|NO_WINDOW (+breakaway); Unix Setsid
    func EditConfig(path, key, value json.RawMessage, plat) (ConfigEdit, error) // allow-list, validate first, in place, BOM, atomic
    var EditableKeys; CanonicalKey; ParseValue; EffectiveConfig; EffectiveValue
    func EditConfigMany(path, []KeyValue, plat) ([]ConfigEdit, error)  // several keys, one validation, one write (the Settings page); a document problem wraps *config.ValidationError
    func SetRecord / AddCamera / RemoveCamera / SetCamera / FindCamera / LoadCameras // cameras.json edits
    func PutCameras(path, []CameraRow) (changed int, err error)     // the Settings save: replace the list in order, rows keep unknown fields via OriginalName, *InvalidRows (row, field, message) refuses all
    func TakeCensus(Paths, osx.Inspector, statusInterval) Census   // host.json, instance lock / heartbeat, children.json and webchild.json (the web child); identity, never names
    func Down(ctx, Paths, cfg, dial, inspector) DownResult         // API stop or stop.request, then proved; with no supervisor it first reaps provable orphans (ReapOrphans, reported in DownResult.Reaped)
    func ReapOrphans(Paths, inspector, log, withAux) ([]osx.ReapVerdict, error) // osx.Reap over children.json (and webchild.json when no host is alive), holding the instance lock
    Paths.AuxFile() = <state>/webchild.json (the host's own children: the web role), Paths.WebStopFile() = <state>/web.stop; HostOptions.NoWeb
    func Running / WaitRunning                                     // status.Verdict; a fresh heartbeat after the start (G10)

The camera login: the two environment variables when both are set, else the secrets store (`secrets.CameraLogin`); the store is
opened on first use and every value read is registered with the logger's redactor.
With `RelayExternal` (the shadow, docs/shadow.md) the login is still required at start but never used: every camera with a relay
path reads the external relay (`TestShadowRigSurvivesARelayThatIsGoneAndRecovers` proves the vector, the backoff and the recovery).
