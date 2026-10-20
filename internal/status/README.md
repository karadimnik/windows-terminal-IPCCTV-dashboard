# internal/status

status.json: schema, typed Document, atomic Writer, share-delete Reader, liveness Verdict.
Schema and PowerShell differences: `status.schema.json`, `KNOWN-DIFFERENCES.md`.

## API (signatures)

```go
// read
func Read(path string) (*Document, error)        // ErrNoStatus (missing/empty), ErrInvalid
func ReadRaw(path string) ([]byte, error)        // BOM stripped, must be a JSON object
func Parse(raw []byte) (*Document, error)
// write
func NewWriter(path string, opts Options) *Writer // Options{Redactor *Redactor; RetryDelay time.Duration}
func (w *Writer) Write(doc *Document) error       // redact, temp, Sync, replace; mutex-safe
func (w *Writer) Redactor() *Redactor
func (w *Writer) ConsecutiveFailures() int
func Marshal(doc *Document) ([]byte, error)       // indented, UTF-8 no BOM, LF; no redaction
func NewRedactor() *Redactor
func (r *Redactor) Register(secret string)
func (r *Redactor) String(s string) string
// carry-forward
func (d *Document) CarryFrom(prev *Document)      // LastUnexpectedStop verbatim + LastWindowClose
// liveness
func Verdict(snapshot *Document, now time.Time, procs ProcessInspector, cfg VerdictConfig) (running bool, reason string)
type ProcessInspector interface{ Alive(pid int64) (startTime time.Time, ok bool) }
type OSProcesses struct{}                         // real: /proc (Linux), Win32 (Windows), kill(0) (other Unix)
type FakeProcesses map[int64]time.Time
type VerdictConfig struct{ StatusIntervalSeconds float64; StartTolerance time.Duration }
// heartbeat
type Heartbeater struct{ Writer *Writer; Snapshot func() *Document; Interval time.Duration; OnError func(error); NewTicker func(time.Duration) (<-chan time.Time, func()) }
func (h *Heartbeater) Run(ctx context.Context)
```

## Windows rules (spike SP6)

1. Readers open with `CreateFile(FILE_SHARE_READ|WRITE|DELETE)`, read fully, close at once; `os.Open` blocks the writer.
2. Go's `os.Rename` (MoveFileEx) fails with ACCESS_DENIED whenever any handle is open, whatever the reader's share flags.
3. The writer replaces with `SetFileInformationByHandle(FileRenameInfoEx, REPLACE_IF_EXISTS|POSIX_SEMANTICS)`: no failures against share-delete readers.
4. On error 87 or 50 (`\\wsl.localhost`, 9P) it falls back to `os.Rename`; never `ReplaceFileW` (readers see the file missing).
5. A failed replace (sharing violation) is retried 3 x 100 ms at most, then the cycle is skipped and the error returned; the next heartbeat retries.
6. Readers retry 5 x 80 ms on sharing violation / access denied; Linux and macOS need neither (`os.ReadFile`, `os.Rename`).

## Verdict notes

Running needs heartbeat age <= max(3 x interval, 15 s), a heartbeat not more than that limit in the future (a clock step), the pid alive, and (when both are known) process start within `StartTolerance` (default 30 s, negative disables) of `StartedAt`. After a wall-clock step larger than the tolerance the start-time check reports "pid reused" for a live supervisor, because `StartedAt` is written once.
No `golang.org/x/sys` dependency: everything uses `syscall`.
