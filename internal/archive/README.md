# archive

Naming, scanning, the archive layout and root-safety rules. The pruner planner/executor (W3) builds on this; nothing here deletes anything.
Every reader lists and resolves a camera's folder and segments through `Layout` (task L1, decision D20): it is the one place that builds a path
under the root, so the day-folder layout of D20 is internal to this package (task L2 reads it; nothing here files a segment, task L4 does). `TestNothingOutsideArchiveListsOrJoinsTheArchive` fails when a
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
    type DirScan struct { Dir string; ScannedAt time.Time; Files int; Bytes int64; Filed int; FiledBytes int64
                          OldestWrite, NewestWrite, NewestClosedWrite time.Time; Newest string; NewestMTime time.Time; NewestBytes int64; Recent, Anomaly bool }
    func NewTracker() *Tracker; (*Tracker).Observe(DirScan) time.Time /* first-seen of Newest */; (*Tracker).Forget(dir string)
    func LiveSize(path string) (int64, error)       // the size of a file another process is writing (doctor samples growth with it)

    // layout (D20): <Root>/<camera>/<segment> is the spool ffmpeg writes into; <Root>/<camera>/<YYYY-MM-DD>/<segment> a filed day
    type Layout struct { Root string; Matcher Matcher; Container string; Cache *Cache }   // a plain value, build one where needed
    func (Layout) CameraDir(camera string) string    // <Root>/<camera>: where ffmpeg writes and the supervisor mkdirs
    func (Layout) Folders() ([]Entry, error)         // root entries not starting with "_", name order, as listed (os.ReadDir's error)
    func (Layout) Cameras() ([]CameraFolder, error)  // Folders checked with os.Lstat: folders, and links (Link true, refused by readers)
    func (Layout) Entries(camera) ([]Entry, error)   // every entry of the camera folder (the spool level, day folders included), never cached
    func (Layout) Spool(camera) (files []File, days []Entry, err error)   // ONE listing of the camera folder: its files, its day folders newest first
    func (Layout) DayFiles(day Entry) ([]File, error) // one day folder Spool or Entries listed (Entry.IsDay), Day set; never a caller's path
    func (Layout) Files(camera) ([]File, error)      // the spool's files, then every day folder's (oldest day first), name order in each
    func (Layout) Segments(camera) ([]Segment, error) // footage (Spooled or Filed, regular) across spool and days, newest first (mtime, later name on a tie)
    func (Layout) Newest(camera) (Segment, bool, error) // spool-first: the spool's newest, else the newest day folder's that holds one
    func (Layout) Scan(camera, now, segmentSeconds) (DirScan, error)      // status tick: the spool; day folders only while < 2 segments seen
    func (Layout) ScanAll(camera, now, segmentSeconds) (DirScan, error)   // Scan, with Files/Bytes/write times/Filed over every day folder too
    func (Layout) ScanSpool(camera, now, segmentSeconds) (DirScan, error) // the spool alone, never a day folder (the pruner until task L3)
    func (Layout) Resolve(camera, name string) (Entry, error)          // MATCHES name: the spool's listing, then the listed day folder of the name's date; never joins it
    func (Layout) Classify(path string) Class        // lexical: Class{Camera, Day, Name, Kind}, Kind Foreign | Spooled | Filed | Misfiled; (Class) Footage()
    func IsDayName(name string) bool; func DayName(start time.Time) string   // YYYY-MM-DD and a calendar date; the local date written in a segment name
    type Entry struct { fs.DirEntry; Path, Dir, Day string }; (Entry) IsDay() bool   // Day: the day folder holding it, "" in the spool
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
- **Spool and day folders** (L2, D20). A day folder is a sub-folder of a camera folder whose name is `YYYY-MM-DD` in ASCII digits and a
  calendar date (`2026-02-30`, `2026-13-45`, `2026-1-05` are not) and whose listed type is a plain directory: a symbolic link, a junction or any
  other reparse point named like a day is never entered (on Windows Go reports a junction or symbolic link as no directory and any other reparse
  point as irregular, so the listing's type bits decide; the L1 rule for camera folders, links are never followed). Folders inside a day
  folder are not entered. A file in a day folder is footage (`Filed`) only when it is a regular file, a segment of the camera and the day is
  the local date written in its name (`DayName`: the repeated hour of a clock change stays in one day, with both offsets); a segment of the
  camera under another day is `Misfiled` (left out of the footage, counted by `archive stats` and the doctor); anything else is `Foreign`.
  **What reads what.** The spool alone answers whether a camera records: `Newest` and `Scan` (status tick, `CurrentSegment`, age of the last
  closed segment, alert 110, stall watchdog) list only the camera folder while it holds two or more segments. **Contract for the L4 mover:
  it must always leave the open segment AND the newest closed segment in the spool.** With one segment in the spool `Scan` also lists the
  newest day folder (two listings, the newest closed write stays exact), with none it lists day folders newest first until it has seen two
  segments; `Newest` is then the newest of the newest day folder that holds one. `ScanSpool` is the spool only (the pruner, until L3).
  Everything that shows or counts footage reads every day folder: `Files`, `Segments`, `ScanAll` (archive-wide disk figures), the Footage
  index, `archive stats`, `archive compare`, the doctor. `Resolve` reads the spool and at most the one listed day folder of the name's date.
  Measured (`TestStatusTickListsOnlyTheSpool`, 6 cameras x 30 days x 96 segments, spool of 2, cached and not): a tick is 1 listing per
  camera; `ScanAll` and `Segments` 31. `TestMixedTreesAgreeWithTheWalk` compares every reader with an independent walk (`archivetest.Walk`)
  over 160 generated spool-and-day trees (`archivetest.Mixed`). Claim rule: a camera folder may hold day folders whose files are all the
  camera's segments filed under that day; any other sub-folder, a folder in a day folder and a misfiled or foreign file there block.
- Not here: the per-file prune predicate (`Get-CamPruneRefusal`: reparse, inside-root, parent equals root, no `_` folder, `IsSegmentOf`) - W3 composes it from these parts.
