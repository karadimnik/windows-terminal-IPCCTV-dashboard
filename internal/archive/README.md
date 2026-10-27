# archive

Naming, scanning, the archive layout and root-safety rules. The pruner planner/executor (W3) builds on this; nothing here deletes anything.
Every reader lists and resolves a camera's folder and segments through `Layout` (task L1, decision D20): it is the one place that builds a path
under the root, so the day-folder layout of D20 changes this package only. `TestNothingOutsideArchiveListsOrJoinsTheArchive` fails when a
non-test file elsewhere lists a folder or joins a root with a camera (allow-list with reasons in `guard_test.go`).

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
    func LiveSize(path string) (int64, error)       // the size of a file another process is writing (doctor samples growth with it)

    // layout (flat today: <Root>/<camera>/<camera>_<stamp><ext>; the camera folder is the "spool" ffmpeg writes into)
    type Layout struct { Root string; Matcher Matcher; Container string; Cache *Cache }   // a plain value, build one where needed
    func (Layout) CameraDir(camera string) string    // <Root>/<camera>: where ffmpeg writes and the supervisor mkdirs
    func (Layout) Folders() ([]Entry, error)         // root entries not starting with "_", name order, as listed (os.ReadDir's error)
    func (Layout) Cameras() ([]CameraFolder, error)  // Folders checked with os.Lstat: folders, and links (Link true, refused by readers)
    func (Layout) Entries(camera) ([]Entry, error)   // every entry of the camera's folder(s), never cached
    func (Layout) Files(camera) ([]File, error)      // non-directory entries with DirEntry.Info, name order; through Cache when set
    func (Layout) Segments(camera) ([]Segment, error); (Layout) Newest(camera) (Segment, bool, error)   // regular strict segments, newest first (mtime, later name on a tie)
    func (Layout) Scan(camera, now, segmentSeconds) (DirScan, error)   // Matcher.Scan of the camera, through Cache when set
    func (Layout) Resolve(camera, name string) (Entry, error)          // MATCHES name against the listing (exact, then case-folded where Matcher folds); never joins it
    func (Layout) Classify(path string) (camera, name string, ok bool) // lexical: <Root>/<camera>/<segment of camera>
    type Entry struct { fs.DirEntry; Path, Dir, Day string }          // Day: the day folder, "" for the spool (always "" until day folders exist)
    type File struct { Entry; Info fs.FileInfo }; (File) Created() time.Time; type Segment struct { File; Start time.Time }
    func CreatedTime(fs.FileInfo) time.Time          // NTFS creation time; zero elsewhere
    func NewCache() *Cache; (*Cache).Stats() CacheStats; (*Cache).Invalidate(dir); (*Cache).InvalidateAll(); (Layout).Invalidate(camera)

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
  60 s in the future (clock skew). `Files`/`Bytes` count every non-directory entry, as `Get-CamDirScan`; sizes come from the listing, except
  `Newest`, whose size is read through an open handle (`LiveSize`, the `Get-CamLiveFileLength` port: on NTFS the listing shows the open
  segment's size as of its creation until ffmpeg closes it; Windows opens it read-only sharing read, write and delete, elsewhere a stat;
  a refused open keeps the listing's size). `NewestBytes` and `Bytes` use it; closed files are fresh in the listing. Proved on Windows by
  `TestLiveSizeOfAFileAnotherProcessWrites` (a child writer holds the file open: listing 0 bytes, live 1 MiB). A missing directory is an
  empty scan.
- **Marker**: `MarkerValid` also compares the `RootPath` inside the file with the root (PowerShell only checks `Owner`, so a copied marker
  satisfied it). A copied/moved archive is re-claimed by `ClaimRoot` when it holds only recorder files. The marker is written atomically
  (temp in `_state`, `Sync`, owner-only via `Platform.SetOwnerOnly`, rename); a failed owner-only restriction fails the claim (fail closed).
- **`%` in a camera name**: `FormatSegmentPattern` doubles it so strftime writes it literally (PowerShell splices the offset into the prefix).
  Verified by W5 with a real ffmpeg 8.x (Windows) and a static Linux build: `%%` in the FILE NAME part is written literally (`Cam%Y_...`).
  `-strftime 1` also expands the directory part, so the recorder MUST build the output pattern with `ffmpeg.SegmentPattern(root, safe, ext)`
  and never `archive.FormatSegmentPattern` plus `filepath.Join`. Existing PowerShell-written files for such a camera stay "foreign".
- `CheckRoot`: lexical checks use the `osx.Platform`'s syntax (`Name()`), so the Windows table is tested on Linux; symlink/reparse and
  symlinked-ancestor checks run only when the platform is the host. Where the root lands is the system's answer (Windows: the final path
  of an open handle, so junctions and 8.3 short names resolve too; elsewhere `filepath.EvalSymlinks`), and it is compared with the protected
  paths as given and in their real form (mount points as given). A root that does not exist yet passes (claiming checks existence).
  A folder inside a protected one is accepted (the marker decides), as in PowerShell.
- **Layout** (L1): the readers keep their own stat semantics because `Entry` carries the listing's `DirEntry` and the built `Path`: the pruner
  and the empty-segment cleanup still `os.Lstat` every entry, archive compare still `os.Stat`s, the scan, the Footage index, `archive stats`
  and the doctor use the listing's `Info`. Each moved reader is proved equal to its old code (kept as a reference function in the test) over
  the 200-tree prune corpus and the tables, the names goldens and 120 generated trees (spaces, case, foreign files, links and junctions,
  zero-byte, 8.3-style names, +0300 and +0200 in one folder, future-dated files, sub-folders): `*layout_equiv_test.go` in each package.
- **Cache** (L1): every call still lists the folder, so the names are always exact (no ghost file, no hidden new file). File information is
  reused only while the folder is the same folder (`os.SameFile`) with the same mtime and was not changed within 2 s of its last listing
  (any create, delete or rename, also over an existing name, re-reads everything); even then the `Hot` (2) newest by mtime, the 2 last names,
  anything modified within `Fresh` (2 min) of its last read, new names and changed type bits are re-read, and every `FullEvery`-th (30) call
  re-reads all. The one possible staleness is an old file changed in place, for at most 29 calls; recorder segments never are. On Windows
  `os.ReadDir` already returns every entry's size and times (`DirEntry.Info` is no system call), so the cache reuses nothing there and is
  exactly the uncached result. Bounded to `MaxDirs` (256) folders; per process; nothing on disk. The supervisor's status tick and archive
  scan share one Cache, the web role has its own; the pruner, the claim rule, the cleanup, the CLI and the doctor use none. Measured on
  Linux (`TestCacheStatCount`, 360 files, 12 ticks a minute, a roll and a prune every 15 minutes, identical results at every tick): 4332
  entry stats per minute before, 215 with the defaults (95 with `FullEvery` 180).
- Not here: the per-file prune predicate (`Get-CamPruneRefusal`: reparse, inside-root, parent equals root, no `_` folder, `IsSegmentOf`) - W3 composes it from these parts.
