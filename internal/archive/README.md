# archive

Naming, scanning and root-safety rules. The pruner planner/executor (W3) builds on this; nothing here deletes anything.

    // names
    func SafeName(name string) (string, error)       // == Get-CamSafeName; *NameError{Name, Kind, Stem} (KindNoLegalCharacters, KindReservedDevice); messages equal PowerShell's
    func FolderKey(safe string) string               // equal keys = one folder on a case-insensitive filesystem (not NFC/NFD-aware, no normaliser in the stdlib)

    // segments: <safe>_YYYYMMDD_HHMMSS+HHMM<ext>
    func FormatSegmentPattern(safe string) string    // strftime pattern WITHOUT extension; '%' in safe is doubled (see below)
    func FormatSegmentName(safe string, start time.Time, ext string) string
    func ParseSegment(name string) (camera string, start time.Time, ok bool)   // start keeps the written offset; any 1-8 char alnum extension
    func IsSegmentName(name, container string) bool  // shape + extension; container is ".mkv" (dot optional)
    func IsSegmentOf(name, safe, container string) bool   // == Test-CamSegmentName: also the camera prefix
    type Matcher struct{ FoldCase bool }             // same methods; FoldCase = ASCII case-insensitive extension and prefix
    var DefaultMatcher Matcher                       // FoldCase only on Windows (NTFS); Linux/macOS are case-sensitive
    func MatcherFor(p osx.Platform) Matcher

    // scan
    func Scan(dir, container string, now time.Time, segmentSeconds int) (DirScan, error)   // NOTE extra `container` parameter
    func (Matcher) Scan(...)                         // same, with an injected matcher
    type DirScan struct { Dir string; ScannedAt time.Time; Files int; Bytes int64; OldestWrite, NewestWrite, NewestClosedWrite time.Time
                          Newest string; NewestMTime time.Time; NewestBytes int64; Recent, Anomaly bool }
    func NewTracker() *Tracker; (*Tracker).Observe(DirScan) time.Time /* first-seen of Newest */; (*Tracker).Forget(dir string)

    // root guards and ownership
    func CheckRoot(path string, p osx.Platform) (problem string)
    func IsReparse(fs.FileInfo) bool                 // symlink, junction, any reparse point
    func MarkerPath(root string) string              // <root>/_state/archive-owner.json
    func MarkerValid(root string, p osx.Platform) (ok bool, why string)
    func ClaimProblem(root string, p osx.Platform, container string) string
    func ClaimRoot(root string, p osx.Platform, container string, now time.Time) (ClaimResult, error)
    func VerifyRoot(root string, p osx.Platform, container string, dryRun bool) string   // == Get-CamPruneRootRefusal

## Decisions and deliberate differences from PowerShell

- **Strict allowlist** (advisor decision): ASCII digits only, anchored at both ends (no trailing newline), digits must form a real
  date/time and an offset within +/-14:00. The 24 golden `trap` rows are asserted REJECTED. Case (advisor update): the extension and the camera
  prefix match case-insensitively (ASCII) on Windows and exactly on Linux/macOS (`Matcher.FoldCase`, defaulted from the OS, tested both ways).
- **Newest / Recent** (HLD 6 as amended by spike SP4): `Newest` is the newest strict-pattern file (configured container, prefix = the directory
  name) ordered by mtime, ties by later name; name order is wrong across a fall-back. The PRUNER always protects `Newest`. `Recent` = its mtime is
  within segmentSeconds+60 s of now (a future mtime counts as recent); only the stall watchdog and the down alert use it. `Anomaly` = mtime more than
  60 s in the future (clock skew). `Files`/`Bytes` count every non-directory entry, as `Get-CamDirScan`; sizes come from os.Stat (no
  `Get-CamLiveFileLength` port). A missing directory is an empty scan.
- **Marker**: `MarkerValid` also compares the `RootPath` inside the file with the root (PowerShell only checks `Owner`, so a copied marker
  satisfied it). A copied/moved archive is re-claimed by `ClaimRoot` when it holds only recorder files. The marker is written atomically
  (temp in `_state`, `Sync`, owner-only via `Platform.SetOwnerOnly`, rename); a failed owner-only restriction fails the claim (fail closed).
- **`%` in a camera name**: `FormatSegmentPattern` doubles it so strftime writes it literally (PowerShell splices the offset into the prefix).
  Verified by W5 with a real ffmpeg 8.x (Windows) and a static Linux build: `%%` in the FILE NAME part is written literally (`Cam%Y_...`).
  `-strftime 1` also expands the directory part, so the recorder MUST build the output pattern with `ffmpeg.SegmentPattern(root, safe, ext)`
  and never `archive.FormatSegmentPattern` plus `filepath.Join`. Existing PowerShell-written files for such a camera stay "foreign".
- `CheckRoot`: lexical checks use the `osx.Platform`'s syntax (`Name()`), so the Windows table is tested on Linux; symlink/reparse and
  symlinked-ancestor checks run only when the platform is the host. A root that does not exist yet passes (claiming checks existence).
  A folder inside a protected one is accepted (the marker decides), as in PowerShell.
- Not here: the per-file prune predicate (`Get-CamPruneRefusal`: reparse, inside-root, parent equals root, no `_` folder, `IsSegmentOf`) - W3 composes it from these parts.
