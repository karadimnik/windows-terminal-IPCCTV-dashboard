# logx and the action contract

`internal/logx` is the logging layer (rule R2, LLD 2.13); `internal/cli` holds the Action registry and
Runner that make "every action is logged" enforceable. Standard library only.

## Sample records

Text (default), at TRACE, with a secret masked:

    2026-10-17T13:44:05.123456+03:00 INFO  [supervisor] child started action=camera.add actor=cli:nik target=cam1 result=ok duration_ms=42 pid=4242
    2026-10-17T13:44:05.123456+03:00 TRACE [supervisor] ffmpeg: frame=1 fps=25 password=***
    2026-10-17T13:44:05.123456+03:00 WARN  [supervisor] login failed err="bad password for rtsp://***:***@h/"

JSON option: `{"time":"2026-10-17T13:44:05.123456+03:00","level":"INFO","component":"supervisor","msg":"child started","action":"camera.add",...}`

## Behaviour notes

- Levels ERROR, WARN, INFO (default), DEBUG, TRACE. `HOLOCRON_LOG=debug` or `supervisor=debug,web=info` overrides
  `Config`; invalid entries are returned once by `New` and ignored. A component `ffmpeg:cam1` falls back to an
  override named `ffmpeg`. Component names are case-insensitive.
- `Logger.For(c)` loggers read the `LevelController` on every call: `Levels().SetComponent(...)` applies at once.
- Redaction runs in the handler (message, string attrs, errors, Stringers, groups) and again on every formatted line
  before any sink. Attributes whose key contains password|passwd|secret|token|authorization|cookie|hash|apikey|api_key
  are masked. Non-string, non-error, non-Stringer `Any` values are flattened to a string so they can be redacted.
- Records with top-level `audit=true` go to the main sink and the audit sink. They obey the level filter like any record.
- `RotatingFile` opens, appends and closes on every write and looks at the on-disk size each time (it tolerates other
  processes; no cross-process lock). Rotation: size >= MaxBytes before append, one `.1` generation. Failures never reach
  the caller. A blocked rotation produces one notice (in the file, next to the line) per episode, rearmed after a
  successful rotation. A failed append drops the line, writes one notice to `NoticeOut` (stderr), and the next successful
  write is preceded by "N line(s) dropped". Repeat collapse (off unless `CollapseAfter > 0`): after K same-shape lines
  (digits and hex runs stripped) the rest are held and the latest is written as `msg [+N more similar]` when the run ends,
  on `Close`, or after `CollapseFlush`.
- Files are created 0600 and directories 0700 on Unix. On Windows the mode bits are ignored and the folder's default ACL applies.

## Exported API (signatures)

logx:

    const LevelTrace slog.Level; LevelError, LevelWarn, LevelInfo, LevelDebug; EnvVar = "HOLOCRON_LOG"; DefaultMaxBytes
    const ResultOK, ResultFail, ResultRefused
    func ParseLevel(s string) (slog.Level, error)
    func LevelName(l slog.Level) string
    func ParseSpec(spec string) (def *slog.Level, comps map[string]slog.Level, errs []error)
    type LevelController; func NewLevelController(def slog.Level) *LevelController
      (c) SetDefault(l); SetComponent(name, l); Clear(name); Level(component) slog.Level; Snapshot() LevelSnapshot
    type LevelSnapshot struct{ Default slog.Level; Components map[string]slog.Level }; (s) String() string
    type Config struct{ Level string; Levels map[string]string }
    type Options struct{ Config Config; Getenv func(string) string; JSON bool; Sink, Audit Sink; Redactor *Redactor; Now func() time.Time; Location *time.Location }
    func New(o Options) (*Logger, []error)
    (l *Logger) For(component string) *slog.Logger; Levels() *LevelController; Redactor() *Redactor; Close() error
    type Redactor; func NewRedactor() *Redactor; (r) Register(secret string); String(s string) string
    type Sink interface{ Write([]byte) (int, error); Close() error }
    func NewMemorySink() *MemorySink  (Lines() []string; String() string)
    func NewWriterSink(w io.Writer) *WriterSink; func StderrSink() *WriterSink; func FanOut(sinks ...Sink) Sink
    type FileOptions struct{ Path string; MaxBytes int64; CollapseAfter int; CollapseFlush time.Duration; JSON bool; NoticeOut io.Writer; Now func() time.Time }
    func NewRotatingFile(o FileOptions) *RotatingFile   (Write, Close; never fail)
    func Action(string), Actor(string), Target(string), Result(string), ActionID(string) slog.Attr; PID(int), DurationMS(time.Duration), Audit(), Err(error) slog.Attr

cli:

    type Input struct{ ActionID, Target string; Args map[string]any }
    type Output struct{ Target string; Data any }
    type Action struct{ Name, Component string; Mutating, Audit bool; Run func(context.Context, Input) (Output, error) }
    var ErrRefused error; func Refuse(format string, args ...any) error; var ErrUnknownAction error
    type Registry; func NewRegistry() *Registry; var Default *Registry; func Register(Action)
      (r) Register(Action) (panics on duplicate/invalid); All() []Action; Lookup(name string) (Action, bool)
    type Runner struct{ Registry *Registry; Logs LogSource; Now func() time.Time }; func NewRunner(reg *Registry, logs LogSource) *Runner
      (r) Do(ctx, name string, in Input) (Output, error)      // the only way to run an action
    func NewActionID() string
    func CLIActor() string; WebActor(name string) string; WithActor(ctx, actor) ctx; ActorFrom(ctx) string; WithActionID(ctx, id) ctx; ActionIDFrom(ctx) string
    const ActorHost, ActorSupervisor, ActorSystem
    type Exposed []string; type TB interface{ Helper(); Errorf(string, ...any) }
    func AssertAllExposedAreActions(t TB, exposed Exposed); func AssertExposedIn(t TB, r *Registry, exposed Exposed)

Result record levels: INFO mutating success, DEBUG read-only success, WARN refused (`errors.Is(err, ErrRefused)`), ERROR otherwise
(panics included). Begin is DEBUG. Both carry action, actor, target, action_id, pid (+ duration_ms, result, err on the result; audit=true for Audit actions).
