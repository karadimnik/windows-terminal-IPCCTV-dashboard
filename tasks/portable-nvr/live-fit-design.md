# Live tab: fit the screen, click opens a modal

Branch `portable-nvr-livefit` (front end only: `ui/static/live.js`, `style.css`, `index.html`). Builds on `live-1b-design.md`.

## Why

At 1920x1080 the tiles were 362x204 inside a 1180 px column, and a click made the tile the big "focus" tile: the grid
re-flowed and that tile closed and re-opened its WebRTC connection (a dark gap of seconds).

## Decisions

1. **Fill the screen.** On the Live tab the page has no max-width and a few px of padding; the grid gap is 4 px. Tiles are 16:9
   with `object-fit: contain` (nothing is cropped). `bestGrid(width, height, n, minTileWidth, gap)` picks the column count that
   maximises the tile width (`min(W/c, H/rows*16/9)`), ties to fewer columns; below `minTileWidth` (300 px) it falls back to one
   column, tile = `min(width, height*16/9)`, and the page scrolls. It is recomputed on resize, orientation change,
   fullscreen change and ResizeObserver (debounced) and written to the grid as `--lcols` / `--ltile`.
2. **Fullscreen** button (Fullscreen API on the grid container, hidden when unsupported); in fullscreen the grid uses the whole
   screen and the button stays as a small overlay.
3. **Modal, not focus.** A click (or pointerdown pre-warm) never touches the grid. The modal is a `<dialog>` inside the grid
   container (so it also shows while the grid is fullscreen) with two stacked videos: the first shows the tile's own
   `MediaStream` at once (no new connection), the second receives ONE new WebRTC session on `stream=main`; when the second has
   rendered its first frame it is shown and the first is released. Failure keeps the sub picture ("HD unavailable", Retry).
4. **Pre-warm.** `pointerdown` on a tile starts the main session after 100 ms (so a scroll gesture does not); `pointerup`
   elsewhere, `pointercancel`, or no click within 500 ms of the release drops it, a click adopts it.
5. **One main session**, owned by the modal; closing, hiding the page and `pagehide` hang it up (peer connection closed, the
   session DELETE through `dropSession`). Hiding the page keeps the dialog and re-opens the connection when visible again.
6. **URL.** `#live/<camera>` opens the modal; `#live` closes it. A click pushes `#live/<camera>` (so Back closes it; a plain
   `replaceState` would make Back leave the page), a deep link does not push; closing by Esc, backdrop or button goes back
   one entry (or `replaceState('#live')` when nothing was pushed).
7. **Diagnostics v2.** `live-diag/2` adds `post=` (signalling round trip), `connected=` (ICE+DTLS), `frame=` (first decoded
   frame) in ms from the start of the attempt, and `hd=` (modal: click to HD picture). Same fields in `window.__liveStats` and
   `console.info`. Still no address.
