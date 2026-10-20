# config

    type Config struct { ... }                       // fields are the JSON key names, see KEYS.md; (Config).Profile() (ContainerProfile, bool)
    type ContainerProfile struct{ Extension, SegmentFormat string; AudioArgs, ExtraArgs []string; SegmentFormatOptions string }
    type Logging struct{ Level string; Levels map[string]string }   // default Level "info"
    Config.Alerts alerts.Config                                       // "alerts" object, defaults of internal/alerts/README.md
    type Secrets struct{ Backend, File, KeyFile string }; var SecretBackends   // "secrets" object: backend auto (default) | keyring | file | env; file and keyFile paths ("" = the auth folder)
    func Defaults(p osx.Platform) Config
    func Parse(data []byte, p osx.Platform) (Config, []Warning, error)   // error is *ValidationError (all problems, with key paths)
    func Load(path string, p osx.Platform) (Config, []Warning, error)    // missing or blank file = defaults
    func KeptKeys() []string; func DroppedKeys() []string
    type Problem struct{ Path, Message string }; type ValidationError struct{ Problems []Problem }; type Warning struct{ Path, Message string }

    type Camera struct{ Name, IP, Quality string; Record bool /* + unexported pass-through fields */ }
    func (Camera) SafeName() (string, error); func (Camera) Stream() string      // blank Quality = "stream1"
    type Cameras struct{ List []Camera; BOM bool }
    func LoadCameras(path string) (*Cameras, error); func (*Cameras) Load(path string) error; func (*Cameras) Parse([]byte) error
    func (*Cameras) Save(path string) error          // atomic; BOM follows the file on disk
    func (*Cameras) Find(name string) int; Validate() error; Collisions() []Collision

A load error of type `*ValidationError` still leaves the (partially read) list filled so a caller can report and continue.
Folder collisions use `archive.FolderKey` (case-insensitive; NFC/NFD pairs are not detected, see archive/README.md).
