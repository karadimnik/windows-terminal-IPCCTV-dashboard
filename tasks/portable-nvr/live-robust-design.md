# Live robustness A: a wall that stays on

Branch `portable-nvr-liverobust`. Front end plus camui; the PowerShell scripts are untouched.

## Operator decisions (2026-10-16)

1. A hidden window keeps its streams for a grace period, then frees them. Default 300 s, configurable.
2. With the modal open the grid under it keeps playing (unchanged).

## Design

- **Setting.** `liveHiddenGraceSeconds` in `camui.config.json` (env `CAMUI_LIVE_HIDDEN_GRACE` wins), default 300, `0` = free at
  once (the old behaviour), `-1` = never free, clamped to one day. camui returns it as `hiddenGraceSeconds` in `GET /api/live`.
  It is camui's, not the recorder's: nothing in `recorder.config.json` or the scripts changes.
- **Away** is "window hidden OR another camui tab selected". `usable()` no longer looks at either; it looks at `freed`.
  `enterAway()` stamps `Date.now()`, stops painting (see below) and starts a 1 s interval; `checkGrace()` frees everything when
  `Date.now() - hiddenSince >= grace`. The interval is not trusted alone: `leaveAway()`, `resume` (a thawed page) and `online`
  call `checkGrace()` too, and nothing relies on a single long `setTimeout`. Whether the interval really fires in a minimized
  Chrome and Edge page is measured (docs/ui.md, `liverobust`).
- **Not painting while away.** Variants measured: keep, `video.pause()`, `srcObject = null`, `track.enabled = false`, and close
  at once. The chosen one is the cheapest that resumes in about a second; a picture that is sounding (unmuted) is never paused.
- **Stall detection.** Every 2 s the frames decoded by each connected tile (and by the modal's main picture) are read from
  `getStats()`; no advance for 8 s rebuilds that connection. Not while away, and not after a gap in the ticks (a frozen page or a
  sleeping PC), which only reset the clock.
- **Recovery without a reload.** A tile that was live and lost its connection is "Reconnecting" (not "Offline") for its first
  attempts; backoff 1, 2, 4, 8, 15, 30 s with jitter; the backoff restarts only when the rebuilt picture really moves; at most 20
  automatic reconnects per minute for the whole page. A 503 "unavailable" stops every tile and holds new attempts for 5, 10, ... 60 s.
  The list read keeps what it knew when it fails (camui restarting does not touch pictures that are already flowing), and is
  re-read every 5 s while something is wrong (30 s while hidden). ICE restart is NOT used: MediaMTX v1.21.2 has a WHEP PATCH
  (`onWHIPPatch` in `http_server.go`) but camui proxies only POST and DELETE, MediaMTX's own browser reader rebuilds the
  connection on every error, and a killed server or camera loses the session anyway.
- **Off-screen tiles** (a phone stacks six full-width tiles): out of view = a muted tile stops painting at once and its connection
  closes after 60 s; back in view it resumes or reopens.
- **Diagnostics v3.** `live-diag/3` adds `hidden=`, `grace=`, `stall=` and `recoveries=`.
