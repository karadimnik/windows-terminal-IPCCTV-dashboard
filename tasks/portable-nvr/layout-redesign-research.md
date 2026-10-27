# Recordings layout redesign: research (27 Oct 2026)

Status: research only, nothing built. Decision D18 ("archive layout and file names unchanged, so both systems can share footage") would be
reversed by any of the options below. Dependency map with file:line for every claim: [spec/layout-dependencies.md](spec/layout-dependencies.md).

## 1. The problem, measured

- Today: `<root>\<Safe_Camera>\<Safe_Camera>_YYYYMMDD_HHMMSS+ZZZZ.mkv`, 15-minute segments, 96 per camera per day. Live numbers (27 Oct): 6 folders with
  269 to 366 files each, 2,075 files, 273 GB, listing one folder takes 134 ms cold. Retention is the free-space watermark (`ReserveGB`), about
  3.7 days at 78 GB/day on a 466 GB drive; `MaxRetentionDays` is unset. So the folders are "only" hundreds of files now; on a bigger drive or a
  longer retention they reach thousands per camera (30 days = 2,880).
- Cost in the software: each recording camera's folder is listed 13.4 times per minute (12 from the 5 s status tick, 1 from the 60 s archive scan,
  0.4 from the 300 s pruner) and every listing stats every entry; no caching. About 3,200 entries per minute per camera at 240 files, 10 times that
  at 10 times the files.
- Cost to the human: Explorer and any tool that lists a camera folder get slower and unreadable as the folder grows; there is no browse-by-day
  structure on disk (the web Footage tab builds one from file names on each click).
- NTFS itself copes (directories are B-trees; lookup is logarithmic); the pain is enumeration and shell/UI work, which grows linearly (web research,
  anecdotal; measure before relying on it).

## 2. Hard facts that constrain any design

1. The segment muxer cannot create directories. Verified 27 Oct on the installed ffmpeg (Windows dev build 2026-07-06): `-strftime_mkdir` is not an option of
   the segment muxer, a date sub-folder in the pattern fails with `No such file or directory` (exit -2), and the same pattern works when the folder
   already exists. The old code says the same (`cam.common.ps1:1927`); a date layout was proposed and DROPPED on 2026-10-14 for this reason
   (`tasks/ipcctv-recording/todo.md` [83]).
2. A segment opens at its boundary (local-midnight multiples of 900 s), so a folder that does not exist at that instant stops recording for that camera
   until the supervisor restarts it. Today a folder is created only at child spawn (`internal/supervise/loop.go:308`).
3. The pruner and every reader are flat and non-recursive: nested files are invisible and would never be pruned (disk fills silently, alert 121); a
   scan that misses a new day folder looks like a dead camera (alert 110, stall kill).
4. The legacy PowerShell system lists only immediate children (`cam.recorder.ps1:2606,3142`). If it is started again over a nested root it writes
   flat files beside the nested days, never prunes them, and its Footage screen no longer shows them. Today's rollback runbook promises the opposite.
5. Nothing in the repository moves or renames archive files today.

## 3. Options

| | Shape | Recorder write path | Readers | Legacy rollback | Verdict |
|---|---|---|---|---|---|
| A | Keep flat; add an incremental scan cache and better views (stats, web day browser) | unchanged | cache only | works | fixes resource use, not the folder |
| B | `<camera>\<YYYY-MM-DD>\file` written directly by ffmpeg | pattern gains the date; supervisor must create today's and tomorrow's folder before every boundary (a miss = recording stops) | all 9 listing sites, pruner, marker claim rule, doctor, compare | breaks unless flattened first | works, but puts recording on a new critical dependency |
| C | `<YYYY-MM-DD>\<camera>\file` | as B | as B plus every camera-keyed assumption (folder name = camera = file prefix) breaks | breaks | hardest; best for whole-day deletion across cameras |
| D | Spool and archive: ffmpeg keeps writing flat into `<camera>\`; a mover files CLOSED segments into `<camera>\<YYYY-MM-DD>\` | unchanged (proven path, no folder dependency at the boundary) | one "list segments of a camera" abstraction reading spool plus day folders | breaks for nested days unless flattened first | recommended |
| E | A SQLite index (Moonfire/Frigate style) | unchanged | queries by index, scan cost near zero | works for flat | heavy; index can drift from disk (Frigate has a sync/repair feature and users hit orphans); not needed for the stated pain |

## 4. Recommended future state (option D, plus A's cache inside the abstraction)

```
<root>\
  _state\ _logs\                      unchanged (system folders start with _)
  Front_Entrance\
    Front_Entrance_20261027_120001+0200.mkv      spool: newest 1 to 2 files, being written or just closed
    2026-10-26\Front_Entrance_20261026_000000+0200.mkv   ... 96 files per day
    2026-10-27\...
```

- Day folder = the local date in the file name (the date part ffmpeg already formats), so the repeated hour of a clock change stays in one folder
  and names keep their offset.
- The mover files a segment only when it is CLOSED (a newer segment of the same camera exists and its write time is older than a margin), by
  `rename` on the same volume (metadata only, effectively instant), idempotent, journaled, never touching the newest file, retry when Windows still
  holds the handle. Failure mode is benign: the file stays in the spool and is still recorded, listed and pruned.
- One shared archive library (`archive.Layout`): list segments of a camera (spool + day folders, newest first, with a cached incremental scan so a
  status tick stats only new files), classify a path, resolve a segment name to its path. The 9 listing sites move onto it; layout-specific code
  lives in one place.
- Pruner: per camera across spool and day folders, oldest first, same gates; deletes empty day folders (never non-empty, marker/sandbox gates); the
  marker claim rule accepts date-named sub-folders inside a camera folder.
- Web Footage: day list comes from folder names (no scan of file names), `/api/clip` keeps its match-never-join traversal guard.
- `holocron archive migrate` (dry-run by default, `--now`, resumable) files today's flat history into day folders; `holocron archive flatten` is its
  inverse and a documented step of the rollback to the PowerShell system. `archive compare` learns both layouts.
- Doctor: gaps/growth/24 h counts read through the abstraction.

## 5. Migration and work breakdown (each a separate commission, Go only, tests first)

1. `archive.Layout` abstraction and cache, flat only, behaviour unchanged (golden and differential tests stay green).
2. Readers read both layouts (spool + day folders) behind the abstraction; Go-only fixtures for nested trees (the PowerShell harness cannot make them).
3. Pruner per camera across folders, empty-day cleanup, marker claim rule, circuit-breaker denominators.
4. Mover + `archive migrate` / `archive flatten`, with a sandbox rig and the shadow-style compare proof on a copy.
5. Web Footage, `archive stats`, `archive compare`, doctor on the abstraction.
6. Docs: how-it-works, configuration, cutover/rollback (flatten step), cheat sheet, agent reference, decision D18 superseded (new D20).
7. Rollout: build, shadow on a copy of the root, then deploy with the usual stop/swap/start (one restart), run `archive migrate --now`, watch.

Rough effort: 7 commissions, the first three are the bulk (about the size of W3 plus W7); no change to the ffmpeg arguments or the goldens.

## 6. Risks

- Reversing D18 makes the PowerShell rollback conditional on `archive flatten` (seconds, but a step). If the PowerShell system is meant to stay a
  long-term fallback, the cost is real; if it is retired after the first good weeks, it disappears.
- Mover races with the muxer (Windows sharing violations): mitigated by "closed" + margin + retry + never the newest file.
- A bug in the unified reader is the silent-disk-fill / false-camera-down class of failure: hence the abstraction first, behaviour-preserving, with
  the existing differential goldens, then the nested fixtures.
- Day-folder date source (local vs UTC): local, matching the name; revisit only if the host changes zone.

## 7. Open decisions for the Operator

1. Accept reversing D18 (shared layout with the PowerShell system), with `archive flatten` as part of rollback?
2. Shape: camera first `<camera>\<date>\` (recommended) or date first `<date>\<camera>\`?
3. Retention plan: stay near 3 to 4 days on this drive, or move to a larger one (days to weeks)? It decides whether the scan cache matters as much as the folders.
4. Is Explorer-level tidiness the main pain, or the resource cost, or both? (Both are covered; the order of work differs.)
