# archive

Naming, scanning and root-safety rules. The pruner planner/executor (W3) builds on this; nothing here deletes anything.

    // names
    func SafeName(name string) (string, error)       // == Get-CamSafeName; *NameError{Name, Kind, Stem} (KindNoLegalCharacters, KindReservedDevice); messages equal PowerShell's
    func FolderKey(safe string) string               // equal keys = one folder on a case-insensitive filesystem (not NFC/NFD-aware, no normaliser in the stdlib)

    // segments: <safe>_YYYYMMDD_HHMMSS+HHMM<ext>
    func FormatSegmentPattern(safe string) string    // strftime pattern WITHOUT extension; '%' in safe is doubled (see below)
    func FormatSegmentName(safe string, start time.Time, ext string) string
    func ParseSegment(name string) (camera string, start time.Time, ok bool)   // start keeps the written offset; any 1-8 char alnum extension
    func IsSegmentName(name, container string) bool  // shape + extension; container is ".mkv" (dot optional), case-insensitive
    func IsSegmentOf(name, safe, container string) bool   // == Test-CamSegmentName: also the camera prefix (ASCII case-insensitive)

    // scan
    func Scan(dir, container string, now time.Time, segmentSeconds int) (DirScan, error)   // NOTE extra `container` parameter
    type DirScan struct { Dir string; ScannedAt time.Time; Files int; Bytes int64; OldestWrite, NewestWrite, NewestClosedWrite time.Time; Open string; OpenMTime time.Time; OpenBytes int64 }
    func NewTracker() *Tracker; (*Tracker).Observe(DirScan) time.Time /* first-seen of the open segment */; (*Tracker).Forget(dir string)

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
  date/time and an offset within +/-14:00. Extension and camera prefix stay case-insensitive (ASCII only). The 24 golden `trap` rows are asserted REJECTED.
- **Open segment** (HLD 6): newest strict-pattern file (configured container, prefix = the directory name) with mtime in
  [now - (segmentSeconds+60 s), now + 60 s]; "newest" = latest start instant parsed from the name (so the repeated hour after a fall-back orders
  right), then later name. `Files`/`Bytes` count every non-directory entry, as `Get-CamDirScan`. A missing directory is an empty scan.
- **Marker**: `MarkerValid` also compares the `RootPath` inside the file with the root (PowerShell only checks `Owner`, so a copied marker
  satisfied it). A copied/moved archive is re-claimed by `ClaimRoot` when it holds only recorder files. The marker is written atomically
  (temp in `_state`, `Sync`, owner-only via `Platform.SetOwnerOnly`, rename); a failed owner-only restriction fails the claim (fail closed).
- **`%` in a camera name**: `FormatSegmentPattern` doubles it so strftime writes it literally (PowerShell splices the offset into the prefix).
  UNVERIFIED against a real ffmpeg; the ffmpeg-runner task must check it. Existing PowerShell-written files for such a camera stay "foreign".
- `CheckRoot`: lexical checks use the `osx.Platform`'s syntax (`Name()`), so the Windows table is tested on Linux; symlink/reparse and
  symlinked-ancestor checks run only when the platform is the host. A root that does not exist yet passes (claiming checks existence).
  A folder inside a protected one is accepted (the marker decides), as in PowerShell.
- Not here: the per-file prune predicate (`Get-CamPruneRefusal`: reparse, inside-root, parent equals root, no `_` folder, `IsSegmentOf`) - W3 composes it from these parts.
