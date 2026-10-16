"use strict";

// The Live screen: one tile per camera, WebRTC straight from the live-view
// MediaMTX through camui's same-origin signalling door (api/live/...).
//
// What is not incidental:
//
//   - The page only ever names a CAMERA and sub|main. It never sees the live
//     instance's address, its login, or a path name; camui adds the login and
//     returns a session address it minted itself.
//   - Tiles play the sub-stream and are never touched by a click. A click
//     opens a MODAL for that camera: it shows the tile's own MediaStream at
//     once and, in a second video, the main stream as soon as that has decoded
//     a frame. One main session exists at most, owned by the modal.
//   - The grid fills the window (bestGrid) and can go fullscreen.
//   - Audio is muted until the Operator unmutes that tile (or the modal).
//   - Every connection is closed while the page is hidden or another tab is
//     showing: a hidden tab keeps decoding at a reduced rate, which costs CPU
//     for nothing, and each open connection holds a camera session.
//   - A deep link opens the screen: #live, or #live/<camera name> for the modal.
//
//   - A tile that is not playing says why, in plain words (signalling result,
//     ICE states, how many candidates the browser and the server offered and of
//     what kind, how long each step took) with a "Copy diagnostics" button.
//     Only counts, types and an address CLASS (loopback, private, ...) are
//     shown: never an address.
//
// getJSON and esc are app.js's and are looked up when called (app.js loads
// after this file); $ is the only DOM lookup used while loading.

// <bestGrid
// The column count that gives the biggest 16:9 tiles for n tiles in width x height (CSS px, `gap` between tiles).
// Tile width per column count c is min(W/c, H/rows*16/9); ties go to fewer columns. When even the best is narrower than
// minTileWidth the answer is one column (the page scrolls), as wide as the screen but no taller than it.
function bestGrid(width, height, n, minTileWidth = 300, gap = 0) {
  if (!(n > 0) || !(width > 0) || !(height > 0)) return { cols: 1, rows: Math.max(n, 0), tile: Math.max(Math.floor(width) || 0, 0), scroll: false };
  let best = null;
  for (let cols = 1; cols <= n; cols++) {
    const rows = Math.ceil(n / cols);
    const tile = Math.min((width - gap * (cols - 1)) / cols, ((height - gap * (rows - 1)) / rows) * 16 / 9);
    if (!best || tile > best.tile + 1e-6) best = { cols, rows, tile };
  }
  if (best.tile < minTileWidth) return { cols: 1, rows: n, tile: Math.floor(Math.min(width, height * 16 / 9)), scroll: true };
  return { cols: best.cols, rows: best.rows, tile: Math.floor(best.tile), scroll: false };
}
// bestGrid>

const Live = (() => {
  const GATHER_MS = 2000;                        // non-trickle: wait this long for candidates, at most
  const CONNECT_MS = 15000;                      // an answer that never turns into media is a failure
  const DISCONNECT_MS = 4000;                    // a transient 'disconnected' may heal on its own
  const BACKOFF_MS = [1000, 2000, 4000, 8000, 15000];
  const REFRESH_MS = 10000;
  const GRID_GAP = 4;                            // px between tiles
  const MIN_TILE = 300;                          // narrower than this: one column and the page scrolls
  const STAGGER_MS = 150;                        // between tiles opening, so six sources do not start in one burst
  const WARM_DELAY_MS = 100;                     // pointerdown to the speculative main session: a scroll gesture ends first
  const WARM_SETTLE_MS = 500;                    // after the pointer is released on the tile, the click must follow within this
  const WARM_MAX_MS = 8000;                      // a press held (or lost) longer than this drops the speculative session

  const view = {
    info: null,          // the last GET api/live
    error: null,         // why the last GET failed
    tiles: new Map(),    // camera name -> tile
    modal: null,         // {name, conn, opened, ...}: the camera's modal; opened=false is a speculative (pre-warmed) session
    press: null,         // a pointerdown on a tile that has not become a click yet
    wantModal: null,     // a deep link waiting for the list to arrive
    shown: false,        // the Live tab is the one on screen
    hidden: document.hidden,
    timer: null,
    fit: null,           // the last bestGrid answer applied to the grid
    fitTimer: null,
  };

  const CHIP = {
    connecting: ["warn", "Connecting"],
    live: ["good", "Live"],
    offline: ["off", "Offline"],
    error: ["crit", "Error"],
    idle: ["off", "Paused"],
  };

  const $ = (id) => document.getElementById(id);
  const grid = () => $("lgrid");
  const usable = () => view.shown && !view.hidden && view.info && view.info.enabled && view.info.alive;
  const chipHTML = (cls, word) => '<span class="chip ' + cls + '"><i class="dot"></i>' + esc(word) + "</span>";

  // ---- diagnostics ---------------------------------------------------------

  function addressClass(address) {
    if (!address) return "masked";
    if (/\.local$/i.test(address)) return "mDNS";
    if (/^127\./.test(address) || address === "::1") return "loopback";
    if (/^10\./.test(address) || /^192\.168\./.test(address) || /^172\.(1[6-9]|2\d|3[01])\./.test(address) || /^f[cd][0-9a-f]{2}:/i.test(address)) return "private";
    if (/^169\.254\./.test(address) || /^fe80:/i.test(address)) return "link-local";
    return "other";
  }

  // The candidates of an SDP, component 1 only, as {type, cls}. Addresses are read and dropped here.
  function candidates(sdp) {
    const found = [];
    const seen = new Set();
    for (const line of String(sdp || "").split(/\r?\n/)) {
      const m = /^a=candidate:\S+ (\d+) (\w+) \d+ (\S+) \d+ typ (\w+)/.exec(line);
      if (!m || m[1] !== "1") continue;
      const key = m[2] + " " + m[3] + " " + m[4];
      if (seen.has(key)) continue;
      seen.add(key);
      found.push({ type: m[4], cls: addressClass(m[3]) });
    }
    return found;
  }

  function summary(list) {
    if (!list) return "none yet";
    if (!list.length) return "none";
    const types = {};
    for (const c of list) {
      const t = types[c.type] || (types[c.type] = { n: 0, cls: {} });
      t.n++;
      t.cls[c.cls] = (t.cls[c.cls] || 0) + 1;
    }
    return Object.keys(types).map((type) => types[type].n + " " + type + " (" +
      Object.keys(types[type].cls).map((cls) => (types[type].n > 1 ? types[type].cls[cls] + " " : "") + cls).join(", ") + ")").join(", ");
  }

  function browserFamily() {
    const ua = navigator.userAgent || "";
    let m;
    if ((m = /Firefox\/(\d+)/.exec(ua))) return "Firefox/" + m[1];
    if ((m = /Edg\/(\d+)/.exec(ua))) return "Edge/" + m[1];
    if ((m = /Chrome\/(\d+)/.exec(ua))) return "Chrome/" + m[1];
    if ((m = /Version\/(\d+).*Safari/.exec(ua))) return "Safari/" + m[1];
    return "other";
  }

  const newDiag = () => ({ t0: performance.now(), http: null, httpText: "", gather: "new", gatherMs: null, ice: "new", conn: "new",
    local: null, answer: null, pair: null, postMs: null, connMs: null, frameMs: null, hdMs: null });

  const elapsed = (diag) => Math.round(performance.now() - diag.t0);
  const seconds = (diag) => Math.round((performance.now() - diag.t0) / 1000);

  // One line for pasting into a bug report. No address, no camera name. Every ms is from the start of the attempt
  // (hd: from the click that opened the modal).
  function diagLine(diag, kind, verdict) {
    if (!diag) return "live-diag/2 browser=" + browserFamily() + " no attempt yet";
    const ms = (label, value) => (value === null || value === undefined ? [] : [label + "=" + value + "ms"]);
    return ["live-diag/2", "browser=" + browserFamily(), "stream=" + kind, "http=" + (diag.http === null ? "none" : diag.http),
      "gather=" + diag.gather + (diag.gatherMs === null ? "" : "(" + diag.gatherMs + "ms)"), ...ms("post", diag.postMs),
      "ice=" + diag.ice, "conn=" + diag.conn, ...ms("connected", diag.connMs), ...ms("frame", diag.frameMs), ...ms("hd", diag.hdMs),
      "local=[" + summary(diag.local) + "]", "answer=[" + summary(diag.answer) + "]", diag.pair ? "pair=" + diag.pair : "pair=none",
      "elapsed=" + seconds(diag) + "s", "verdict=" + verdict].join(" ");
  }

  const tileLine = (tile) => diagLine(tile.diag, tile.kind, tile.lastVerdict || tile.detail || tile.state);
  const modalLine = (m) => diagLine(m.conn.diag, "main", m.verdict || m.hd);

  // The first decoded frame of a video element; callers guard against a connection that has moved on.
  function firstFrame(video, done) {
    if (video.requestVideoFrameCallback) video.requestVideoFrameCallback(() => done());
    else video.addEventListener("playing", () => done(), { once: true });
  }

  // [headline, detail, showCopy] for a tile that is not playing.
  function explain(tile) {
    const d = tile.diag;
    if (tile.state === "live" || tile.state === "idle") return ["", "", false];
    let head = tile.state === "connecting" ? "Connecting" : tile.detail || (tile.state === "offline" ? "No picture from this camera" : "Could not connect");
    if (tile.state === "connecting" && tile.fails > 0) head = "Connecting (retry " + tile.fails + ")";
    if (!d || (tile.state === "connecting" && seconds(d) < 3 && !tile.lastVerdict)) return [head, "", false];

    // A retry has nothing of its own to say yet: keep showing why the last attempt failed.
    if (tile.state === "connecting" && d.http === null && tile.lastVerdict && tile.lastDetail) {
      return [head, "last attempt: " + tile.lastVerdict + "\n" + tile.lastDetail, true];
    }
    const parts = [];
    if (d.http === null) parts.push(d.gather === "complete" ? "waiting for the server's answer" : "gathering browser candidates (" + d.gather + ")");
    else parts.push("server answered HTTP " + d.http + (d.httpText ? " (" + d.httpText + ")" : ""));
    if (d.http === 201) parts.push("ICE " + d.ice + ", connection " + d.conn);
    const lines = [parts.join(" · ")];
    if (d.local) lines.push("browser offered " + summary(d.local) + (d.answer ? "; server offered " + summary(d.answer) : ""));
    if (tile.lastVerdict && tile.state === "connecting") lines.push("last attempt: " + tile.lastVerdict);
    if (d.answer && d.answer.length && d.conn !== "connected" && d.answer.every((c) => c.cls === "loopback") && (tile.state !== "connecting" || seconds(d) >= 5)) {
      lines.push("The server offers only loopback addresses, which some browsers (Firefox) refuse. Set LiveAdvertiseHosts, see docs/ui.md.");
    }
    return [head, lines.join("\n"), true];
  }

  async function copyText(text) {
    try { await navigator.clipboard.writeText(text); return true; } catch (err) { /* no clipboard on a plain-http origin */ }
    const box = document.createElement("textarea");
    box.value = text;
    box.style.cssText = "position:fixed;opacity:0";
    document.body.append(box);
    box.select();
    let ok = false;
    try { ok = document.execCommand("copy"); } catch (err) { /* refused */ }
    box.remove();
    return ok;
  }

  // The selected candidate pair as "type/class <-> type/class", never an address.
  async function selectedPair(pc) {
    try {
      const stats = await pc.getStats();
      const byId = new Map();
      stats.forEach((r) => byId.set(r.id, r));
      let pair = null;
      stats.forEach((r) => { if (r.type === "transport" && r.selectedCandidatePairId) pair = byId.get(r.selectedCandidatePairId); });
      if (!pair) stats.forEach((r) => { if (r.type === "candidate-pair" && (r.selected || (r.nominated && r.state === "succeeded"))) pair = r; });
      if (!pair) return null;
      const name = (c) => (c ? c.candidateType + "/" + addressClass(String(c.address || c.ip || "")) : "?");
      return name(byId.get(pair.localCandidateId)) + " <-> " + name(byId.get(pair.remoteCandidateId));
    } catch (err) { return null; }
  }

  // ---- the list ----------------------------------------------------------

  async function refresh() {
    try {
      view.info = await getJSON("api/live");
      view.error = null;
    } catch (err) {
      view.info = null;
      view.error = err.message || String(err);
    }
    render();
  }

  function message() {
    if (view.error) return ["Could not read the live view status", view.error];
    if (!view.info) return ["Loading", ""];
    if (!view.info.enabled) {
      return ["Live view is off",
        'Set "LiveViewEnabled": true in recorder.config.json and restart the recorder. ' +
        "With it off nothing is started and no camera session is used for viewing."];
    }
    if (!view.info.alive) {
      return ["Live view is not running",
        "It is switched on, but its server is not up. Start the recorder: the live view server starts with it."];
    }
    if (!view.info.cameras.length) return ["No cameras", "cameras.json lists no camera for live view."];
    return null;
  }

  function render() {
    const note = message();
    const box = $("lnote");
    box.classList.toggle("hide", !note);
    if (note) box.innerHTML = '<div class="lnt">' + esc(note[0]) + '</div><div class="lns">' + esc(note[1]) + "</div>";

    const cameras = view.info && view.info.enabled ? view.info.cameras : [];
    const names = cameras.map((cam) => cam.name);
    if (names.join("\n") !== [...view.tiles.keys()].join("\n")) rebuild(cameras);
    for (const cam of cameras) updateTile(view.tiles.get(cam.name), cam);

    if (view.wantModal !== null && view.tiles.size) {
      const wanted = [...view.tiles.keys()].find((name) => name.toLowerCase() === view.wantModal.toLowerCase());
      view.wantModal = null;
      if (wanted) openModal(wanted, false);
    }
    $("lcount").textContent = view.info && view.info.enabled ? names.length + (names.length === 1 ? " camera" : " cameras") : "";
    sync();
  }

  function rebuild(cameras) {
    for (const tile of view.tiles.values()) closeTile(tile);
    view.tiles.clear();
    grid().replaceChildren();
    if (view.modal && !cameras.some((cam) => cam.name === view.modal.name)) closeModal({ fromUrl: false });
    for (const cam of cameras) {
      const root = document.createElement("div");
      root.className = "ltile";
      root.tabIndex = 0;
      root.setAttribute("role", "button");
      root.dataset.camera = cam.name;
      root.innerHTML =
        '<video muted autoplay playsinline></video>' +
        '<div class="lover"><div class="lh"></div><div class="ld"></div><button type="button" class="lcopy hide">Copy diagnostics</button></div>' +
        '<div class="lbar"><span class="lname">' + esc(cam.name) + '</span><span class="lchip"></span>' +
        '<button type="button" class="lmute" aria-pressed="false" title="Sound is muted. Click to unmute.">Unmute</button></div>';
      const tile = { name: cam.name, root, video: root.querySelector("video"), pc: null, session: null, gen: 0, media: null,
        kind: "sub", state: "idle", detail: "", fails: 0, retry: null, watchdog: null, lostTimer: null, tick: null, diag: null,
        lastVerdict: "", lastDetail: "", cam };
      tile.video.muted = true;
      view.tiles.set(cam.name, tile);
      grid().append(root);
      paint(tile);
    }
    fit();
  }

  function updateTile(tile, cam) {
    if (!tile) return;
    tile.cam = cam;
    paint(tile);
  }

  function paint(tile) {
    const [cls, word] = CHIP[tile.state] || CHIP.idle;
    tile.root.dataset.state = tile.state;
    tile.root.querySelector(".lchip").innerHTML = chipHTML(cls, word);
    const over = tile.root.querySelector(".lover");
    const [head, detail, copy] = explain(tile);
    over.querySelector(".lh").textContent = head;
    over.querySelector(".ld").textContent = detail;
    over.querySelector(".lcopy").classList.toggle("hide", !copy);
    over.classList.toggle("hide", !head);
    const hint = tile.cam && tile.cam.lastError ? "Recorder: " + tile.cam.lastError : "";
    tile.root.title = hint;
    const mute = tile.root.querySelector(".lmute");
    mute.textContent = tile.video.muted ? "Unmute" : "Mute";
    mute.setAttribute("aria-pressed", String(!tile.video.muted));
    mute.title = tile.video.muted ? "Sound is muted. Click to unmute." : "Sound is on. Click to mute.";
    if (view.modal && view.modal.opened && view.modal.name === tile.name) paintModal();
  }

  // ---- one WebRTC session, for a tile (sub) or for the modal (main) -------------

  const gathered = (pc, ms) => new Promise((resolve) => {
    if (pc.iceGatheringState === "complete") return resolve();
    const timer = setTimeout(done, ms);
    function done() { clearTimeout(timer); pc.removeEventListener("icegatheringstatechange", check); resolve(); }
    function check() { if (pc.iceGatheringState === "complete") done(); }
    pc.addEventListener("icegatheringstatechange", check);
  });

  function setState(tile, state, detail) {
    tile.state = state;
    tile.detail = detail || "";
    // While connecting, the explanation grows with the seconds.
    clearInterval(tile.tick);
    tile.tick = state === "connecting" ? setInterval(() => paint(tile), 1000) : null;
    paint(tile);
  }

  function dropSession(url) {
    if (!url) return;
    // keepalive lets the request outlive a page that is unloading. Not
    // sendBeacon: that cannot send a DELETE.
    fetch(url, { method: "DELETE", keepalive: true, cache: "no-store" }).catch(() => {});
  }

  // Closes a connection (tile or modal) and releases its camera session. Bumping gen makes anything still in flight give up.
  function hangup(conn) {
    conn.gen++;
    clearTimeout(conn.watchdog);
    clearTimeout(conn.lostTimer);
    conn.watchdog = conn.lostTimer = null;
    const pc = conn.pc;
    conn.pc = null;
    if (pc) { pc.onconnectionstatechange = null; pc.oniceconnectionstatechange = null; pc.onicegatheringstatechange = null; pc.ontrack = null; try { pc.close(); } catch (err) { /* already closed */ } }
    dropSession(conn.session);
    conn.session = null;
    conn.media = null;
  }

  // Opens a receive-only session on conn.kind of conn.name and reports through hooks:
  //   track(media) - the MediaStream arrived (called once)   connected()   changed() - ICE or gathering state moved
  //   fail(state, detail, delay) - it will not work          refused(status, text) - the proxy said no
  // Hooks are called only while conn.gen is the one this attempt started with.
  async function dial(conn, hooks) {
    const gen = conn.gen;
    const diag = conn.diag = newDiag();
    const ok = () => conn.gen === gen;
    const call = (name, ...args) => { if (ok() && hooks[name]) hooks[name](...args); };
    try {
      const pc = new RTCPeerConnection({ iceServers: [] });
      conn.pc = pc;
      pc.addTransceiver("video", { direction: "recvonly" });
      pc.addTransceiver("audio", { direction: "recvonly" });
      pc.ontrack = (event) => {
        if (!ok() || conn.media === event.streams[0]) return;
        conn.media = event.streams[0];
        call("track", conn.media);
      };
      pc.onicegatheringstatechange = () => { diag.gather = pc.iceGatheringState; call("changed"); };
      pc.oniceconnectionstatechange = () => { diag.ice = pc.iceConnectionState; call("changed"); };
      pc.onconnectionstatechange = () => {
        if (!ok()) return;
        const now = pc.connectionState;
        diag.conn = now;
        if (now === "connected") {
          if (diag.connMs === null) diag.connMs = elapsed(diag);
          selectedPair(pc).then((pair) => { diag.pair = pair || diag.pair; if (!pair) setTimeout(() => selectedPair(pc).then((again) => { diag.pair = again || diag.pair; }), 1500); });
          clearTimeout(conn.watchdog);
          clearTimeout(conn.lostTimer);
          call("connected");
        } else if (now === "disconnected") {
          clearTimeout(conn.lostTimer);
          conn.lostTimer = setTimeout(() => call("fail", "offline", "Connection lost"), DISCONNECT_MS);
        } else if (now === "failed" || now === "closed") {
          clearTimeout(conn.lostTimer);
          call("fail", "offline", "Connection lost");
        }
      };

      await pc.setLocalDescription(await pc.createOffer());
      await gathered(pc, GATHER_MS);
      if (!ok()) return;
      diag.gather = pc.iceGatheringState;
      diag.gatherMs = elapsed(diag);
      diag.local = candidates(pc.localDescription.sdp);

      const sent = performance.now();
      const response = await fetch("api/live/" + encodeURIComponent(conn.name) + "/whep?stream=" + conn.kind, {
        method: "POST",
        headers: { "Content-Type": "application/sdp" },
        body: pc.localDescription.sdp,
        cache: "no-store",
      });
      if (!ok()) {
        // Superseded while the answer was on its way: do not leave its session behind.
        const stale = response.headers.get("Location");
        if (response.status === 201 && stale) dropSession(stale.replace(/^\//, ""));
        return;
      }
      diag.postMs = Math.round(performance.now() - sent);
      diag.http = response.status;
      if (response.status !== 201) {
        let text = "";
        try { text = (await response.json()).error || ""; } catch (err) { /* a body is not guaranteed */ }
        if (!ok()) return;
        diag.httpText = text;
        return call("refused", response.status, text);
      }

      const location = response.headers.get("Location");
      conn.session = location ? location.replace(/^\//, "") : null;
      const answer = await response.text();
      diag.answer = candidates(answer);
      await pc.setRemoteDescription({ type: "answer", sdp: answer });
      if (!ok()) return;
      call("changed");
      conn.watchdog = setTimeout(() => call("fail", "error", "No media after " + Math.round(CONNECT_MS / 1000) + " s"), CONNECT_MS);
    } catch (err) {
      call("fail", "error", "Could not connect");
    }
  }

  // ---- one connection per tile --------------------------------------------

  function closeTile(tile) {
    clearTimeout(tile.retry);
    clearInterval(tile.tick);
    tile.retry = tile.tick = null;
    hangup(tile);
    tile.video.srcObject = null;
  }

  function failTile(tile, state, detail, delay) {
    tile.lastVerdict = detail;
    const was = tile.state;
    tile.state = state;
    tile.lastDetail = explain(tile)[1];
    tile.state = was;
    if (tile.diag) tile.diag.conn = tile.pc ? tile.pc.connectionState : tile.diag.conn;
    console.info("live:", tile.name, tileLine(tile));
    closeTile(tile);
    setState(tile, state, detail);
    tile.fails++;
    const wait = delay || BACKOFF_MS[Math.min(tile.fails - 1, BACKOFF_MS.length - 1)];
    const myGen = tile.gen;
    tile.retry = setTimeout(() => { tile.retry = null; if (tile.gen === myGen && usable()) open(tile); }, wait + Math.random() * 500);
  }

  function tileHooks(tile) {
    return {
      track(media) {
        if (tile.video.srcObject !== media) tile.video.srcObject = media;
        const gen = tile.gen;
        firstFrame(tile.video, () => {
          if (tile.gen !== gen || !tile.diag || tile.diag.frameMs !== null) return;
          tile.diag.frameMs = elapsed(tile.diag);
          console.info("live:", tile.name, tileLine(tile));
        });
        // The modal of this camera shows this very stream until the main stream is ready.
        const m = view.modal;
        if (m && m.opened && m.name === tile.name && m.shown === "sub") ui.vsub.srcObject = media;
      },
      connected() {
        tile.fails = 0;
        setState(tile, "live");
      },
      changed() { paint(tile); },
      fail(state, detail, delay) { failTile(tile, state, detail, delay); },
      async refused(status, text) {
        if (status === 503 && text === "live view is off") {
          view.info = Object.assign({}, view.info, { enabled: false });
          closeTile(tile);
          return render();
        }
        if (status === 503) {
          view.info = Object.assign({}, view.info, { alive: false });
          closeTile(tile);
          return render();
        }
        if (status === 404) return failTile(tile, "offline", "No picture from this camera");
        if (status === 429) return failTile(tile, "error", "Too many viewers - retrying", 10000);
        if (status === 504) return failTile(tile, "error", "The camera is slow to start");
        if (status === 502) return failTile(tile, "error", text || "Live view error", 15000);
        failTile(tile, "error", "Could not connect");
      },
    };
  }

  function open(tile) {
    closeTile(tile);
    setState(tile, "connecting");
    dial(tile, tileHooks(tile));
  }

  // ---- the modal: one camera, large ---------------------------------------

  const ui = {};

  function initModal() {
    ui.dialog = $("lmodal");
    ui.box = $("lmbox");
    ui.vsub = $("lmsub");
    ui.vmain = $("lmmain");
    ui.name = $("lmname");
    ui.chips = $("lmchips");
    ui.msg = $("lmmsg");
    ui.retry = $("lmretry");
    ui.copy = $("lmcopy");
    ui.mute = $("lmmute");
    ui.fsButton = $("lmfs");
    ui.close = $("lmclose");
  }

  function newModal(name) {
    return { name, conn: { name, kind: "main", gen: 0, pc: null, session: null, media: null, diag: null, watchdog: null, lostTimer: null },
      opened: false, hd: "loading", verdict: "", muted: true, shown: "sub", openedAt: 0, hdMs: null, pushed: false, returnFocus: null, warmTimer: null };
  }

  // Only the picture that is shown may make sound.
  function applyMute(m) {
    ui.vsub.muted = m.muted || m.shown === "main";
    ui.vmain.muted = m.muted || m.shown !== "main";
    if (!m.muted) (m.shown === "main" ? ui.vmain : ui.vsub).play().catch(() => {});
  }

  function showSub(m) {
    const tile = view.tiles.get(m.name);
    m.shown = "sub";
    ui.box.dataset.show = "sub";
    ui.vmain.srcObject = null;
    ui.vsub.srcObject = tile ? tile.video.srcObject : null;
    applyMute(m);
  }

  function paintModal() {
    const m = view.modal;
    if (!m || !m.opened) return;
    const tile = view.tiles.get(m.name);
    ui.name.textContent = m.name;
    const [cls, word] = m.hd === "ready" ? CHIP.live : CHIP[tile ? tile.state : "idle"] || CHIP.idle;
    const hd = m.hd === "ready" ? ["good", "HD"] : m.hd === "unavailable" ? ["crit", "HD unavailable"] : ["warn", "HD loading..."];
    ui.chips.innerHTML = chipHTML(cls, word) + " " + chipHTML(hd[0], hd[1]);
    ui.retry.classList.toggle("hide", m.hd !== "unavailable");
    ui.mute.textContent = m.muted ? "Unmute" : "Mute";
    ui.mute.setAttribute("aria-pressed", String(!m.muted));
    ui.msg.textContent = !ui.vsub.srcObject && m.shown === "sub" && tile ? explain(tile)[0] : "";
    ui.fsButton.textContent = document.fullscreenElement === ui.box ? "Exit fullscreen" : "Fullscreen";
  }

  function mainFailed(m, detail) {
    if (view.modal !== m) return;
    m.verdict = detail;
    m.hd = "unavailable";
    console.info("live:", m.name, modalLine(m));
    hangup(m.conn);
    if (m.opened) { showSub(m); paintModal(); }
  }

  function swapToMain(m) {
    m.hd = "ready";
    m.shown = "main";
    m.hdMs = Math.round(performance.now() - m.openedAt);
    m.conn.diag.hdMs = m.hdMs;
    ui.box.dataset.show = "main";
    ui.vsub.srcObject = null;
    applyMute(m);
    paintModal();
    console.info("live:", m.name, modalLine(m));
  }

  // The main stream is staged invisibly on top of the sub picture and shown when it has rendered a frame.
  function attachMain(m) {
    const media = m.conn.media;
    if (!media || ui.vmain.srcObject === media) return;
    ui.vmain.srcObject = media;
    applyMute(m);
    const gen = m.conn.gen;
    firstFrame(ui.vmain, () => {
      if (view.modal !== m || m.conn.gen !== gen) return;
      if (m.conn.diag && m.conn.diag.frameMs === null) m.conn.diag.frameMs = elapsed(m.conn.diag);
      swapToMain(m);
    });
  }

  function mainHooks(m) {
    return {
      track() { if (view.modal === m && m.opened) attachMain(m); },
      fail(state, detail) { mainFailed(m, detail); },
      refused(status, text) { mainFailed(m, "HTTP " + status + (text ? " (" + text + ")" : "")); },
    };
  }

  function startMain(m) {
    hangup(m.conn);
    m.hd = "loading";
    m.verdict = "";
    if (m.opened) { if (m.shown === "main") showSub(m); paintModal(); }
    dial(m.conn, mainHooks(m));
  }

  // Keeps the modal's main session in step with the page: closed while hidden, open when it is shown again.
  function modalSync() {
    const m = view.modal;
    if (!m || !m.opened) return;
    if (!usable()) {
      if (m.conn.pc || m.conn.session) {
        hangup(m.conn);
        m.hd = "loading";
        if (m.shown === "main") showSub(m);
        paintModal();
      }
      return;
    }
    if (!m.conn.pc && m.hd !== "unavailable") startMain(m);
  }

  function warmStart(name) {
    if (view.modal || !usable() || !view.tiles.has(name)) return;
    const m = view.modal = newModal(name);
    m.warmTimer = setTimeout(dropWarm, WARM_MAX_MS);
    dial(m.conn, mainHooks(m));
  }

  function dropWarm() {
    const m = view.modal;
    if (!m || m.opened) return;
    clearTimeout(m.warmTimer);
    hangup(m.conn);
    view.modal = null;
  }

  function openModal(name, push) {
    if (view.modal && view.modal.opened) return;
    const tile = view.tiles.get(name);
    if (!tile) return;
    if (view.modal && view.modal.name !== name) dropWarm();
    const m = view.modal || (view.modal = newModal(name));
    clearTimeout(m.warmTimer);
    m.opened = true;
    m.openedAt = performance.now();
    m.returnFocus = document.activeElement;
    // A speculative session that failed before the click is tried again now.
    if (m.hd === "unavailable" && usable()) { m.hd = "loading"; m.verdict = ""; }
    m.shown = "sub";
    ui.box.dataset.show = "sub";
    ui.vsub.srcObject = tile.video.srcObject;
    ui.vmain.srcObject = null;
    applyMute(m);
    ui.dialog.showModal();
    ui.close.focus({ preventScroll: true });
    paintModal();
    const url = "#live/" + encodeURIComponent(name);
    if (push) { history.pushState(null, "", url); m.pushed = true; }
    else if (location.hash !== url) history.replaceState(null, "", url);
    if (m.conn.media && m.conn.pc) attachMain(m);
    modalSync();
  }

  function closeModal({ fromUrl = false } = {}) {
    const m = view.modal;
    if (!m) return;
    view.modal = null;
    clearTimeout(m.warmTimer);
    hangup(m.conn);
    if (!m.opened) return;
    if (document.fullscreenElement && ui.dialog.contains(document.fullscreenElement)) document.exitFullscreen().catch(() => {});
    ui.vsub.srcObject = null;
    ui.vmain.srcObject = null;
    if (ui.dialog.open) ui.dialog.close();
    const tile = view.tiles.get(m.name);
    const back = tile ? tile.root : m.returnFocus;
    if (back && back.focus) back.focus({ preventScroll: true });
    if (!fromUrl) {
      if (m.pushed) history.back();
      else history.replaceState(null, "", "#live");
    }
  }

  // ---- what is open, and when ---------------------------------------------

  // Opens what should be open and closes everything else. Safe to call often.
  function sync() {
    if (!usable()) {
      for (const tile of view.tiles.values()) {
        if (tile.pc || tile.retry || tile.state !== "idle") {
          closeTile(tile);
          setState(tile, "idle");
        }
      }
      modalSync();
      return;
    }
    let slot = 0;
    for (const tile of view.tiles.values()) {
      if (tile.pc || tile.retry) continue;
      tile.fails = 0;
      clearTimeout(tile.retry);
      tile.retry = setTimeout(() => { tile.retry = null; if (usable()) open(tile); }, slot++ * STAGGER_MS);
    }
    modalSync();
  }

  // ---- the grid fills the screen -------------------------------------------

  function fit() {
    const g = grid();
    if (!g || !view.shown || !view.tiles.size) return;
    const full = !!document.fullscreenElement;
    const box = g.getBoundingClientRect();
    const top = full ? box.top : box.top + window.scrollY;
    const height = window.innerHeight - top - GRID_GAP - 2;
    view.fit = Object.assign(bestGrid(g.clientWidth, height, view.tiles.size, MIN_TILE, GRID_GAP), { vw: window.innerWidth, vh: window.innerHeight });
    g.style.setProperty("--lcols", view.fit.cols);
    g.style.setProperty("--ltile", view.fit.tile + "px");
    g.classList.toggle("scroll", view.fit.scroll);
  }

  function fitSoon() {
    clearTimeout(view.fitTimer);
    view.fitTimer = setTimeout(fit, 60);
  }

  const fullscreenOk = !!(document.fullscreenEnabled && Element.prototype.requestFullscreen);

  function toggleFullscreen(target) {
    if (document.fullscreenElement === target) return document.exitFullscreen();
    return target.requestFullscreen().catch(() => {});
  }

  function paintFullscreen() {
    const on = document.fullscreenElement === $("lwrap");
    for (const button of [$("lfs"), $("lmfs")]) button.classList.toggle("hide", !fullscreenOk);
    $("lfs").textContent = on ? "Exit fullscreen" : "Fullscreen";
    $("lfs").setAttribute("aria-pressed", String(on));
    paintModal();
  }

  // ---- the page ------------------------------------------------------------

  function onTab(name, arg) {
    const shown = name === "live";
    document.body.classList.toggle("live-tab", shown);
    // #live (or another tab) closes the modal; #live/<camera> opens it once the list is known.
    if (shown && arg) {
      if (view.modal && view.modal.opened && view.modal.name.toLowerCase() !== arg.toLowerCase()) closeModal({ fromUrl: true });
      view.wantModal = arg;
    } else if (view.modal && view.modal.opened) closeModal({ fromUrl: true });
    if (shown === view.shown && !arg) return;
    view.shown = shown;
    clearInterval(view.timer);
    view.timer = null;
    if (shown) {
      fitSoon();
      view.timer = setInterval(() => { if (!view.hidden) refresh(); }, REFRESH_MS);
      refresh();
    } else {
      sync();
    }
  }

  document.addEventListener("visibilitychange", () => {
    view.hidden = document.hidden;
    if (!view.hidden && view.shown) refresh();
    else sync();
  });
  // Tidy up on the way out; a bfcache restore reopens.
  window.addEventListener("pagehide", () => {
    for (const tile of view.tiles.values()) { closeTile(tile); setState(tile, "idle"); }
    if (view.modal) { hangup(view.modal.conn); if (view.modal.opened) view.modal.hd = "loading"; }
  });
  window.addEventListener("pageshow", (event) => { if (event.persisted && view.shown) refresh(); });

  window.addEventListener("resize", fitSoon);
  window.addEventListener("orientationchange", fitSoon);
  document.addEventListener("fullscreenchange", () => { paintFullscreen(); fit(); fitSoon(); });
  if (window.ResizeObserver) {
    const watcher = new ResizeObserver(fitSoon);
    watcher.observe(document.body);
    watcher.observe($("lwrap"));
  }
  initModal();
  paintFullscreen();

  // ---- clicks, and the pre-warm that comes before them ---------------------

  const tileOf = (target) => {
    const root = target.closest && target.closest("#lgrid .ltile");
    return root ? view.tiles.get(root.dataset.camera) : null;
  };
  const OWN_CONTROL = ".lmute, .lcopy, .lchip";

  function endPress(keepWarm) {
    const press = view.press;
    view.press = null;
    if (press) { clearTimeout(press.timer); clearTimeout(press.settle); }
    if (!keepWarm) dropWarm();
  }

  // The speculative main session starts shortly after the press and is dropped unless the press turns into a click on the same tile.
  document.addEventListener("pointerdown", (event) => {
    if (event.pointerType === "mouse" && event.button !== 0) return;
    const tile = tileOf(event.target);
    if (!tile || event.target.closest(OWN_CONTROL)) return endPress(false);
    endPress(false);
    view.press = { name: tile.name, timer: setTimeout(() => warmStart(tile.name), WARM_DELAY_MS), settle: null };
  }, true);
  document.addEventListener("pointerup", (event) => {
    const press = view.press;
    if (!press) return;
    const tile = tileOf(event.target);
    if (!tile || tile.name !== press.name || event.target.closest(OWN_CONTROL)) return endPress(false);
    clearTimeout(press.timer);
    clearTimeout(press.settle);
    press.settle = setTimeout(() => endPress(false), WARM_SETTLE_MS);   // the click follows at once, or it never will
  }, true);
  document.addEventListener("pointercancel", () => endPress(false), true);

  document.addEventListener("click", (event) => {
    if (event.target.closest("#lfs")) return void toggleFullscreen($("lwrap"));
    const tile = tileOf(event.target);
    if (!tile) return;
    if (event.target.closest(".lcopy")) {
      const button = event.target.closest(".lcopy");
      copyText(tileLine(tile)).then((ok) => {
        if (!ok) window.prompt("Copy this line:", tileLine(tile));
        button.textContent = ok ? "Copied" : "Copy diagnostics";
        setTimeout(() => { button.textContent = "Copy diagnostics"; }, 1500);
      });
      return;
    }
    if (event.target.closest(".lmute")) {
      tile.video.muted = !tile.video.muted;
      if (!tile.video.muted) { tile.video.volume = 1; tile.video.play().catch(() => {}); }
      paint(tile);
      return;
    }
    if (event.target.closest(".lchip")) return;
    endPress(true);
    openModal(tile.name, true);
  });
  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape" && view.modal && view.modal.opened) { event.preventDefault(); closeModal(); return; }
    const root = event.target.closest && event.target.closest("#lgrid .ltile");
    if (root && event.target === root && (event.key === "Enter" || event.key === " ")) {
      event.preventDefault();
      root.click();
    }
  });

  ui.dialog.addEventListener("cancel", (event) => { event.preventDefault(); closeModal(); });
  ui.dialog.addEventListener("close", () => { if (view.modal && view.modal.opened) closeModal(); });
  ui.dialog.addEventListener("click", (event) => {
    const m = view.modal;
    if (!m) return;
    if (event.target === ui.dialog) return closeModal();                  // the backdrop
    const button = event.target.closest("button");
    if (!button) return;
    if (button === ui.close) closeModal();
    else if (button === ui.retry) { if (usable()) startMain(m); }
    else if (button === ui.mute) {
      m.muted = !m.muted;
      applyMute(m);
      paintModal();
    } else if (button === ui.fsButton) {
      toggleFullscreen(ui.box).then(paintModal);
    } else if (button === ui.copy) {
      copyText(modalLine(m)).then((ok) => {
        if (!ok) window.prompt("Copy this line:", modalLine(m));
        ui.copy.textContent = ok ? "Copied" : "Copy diagnostics";
        setTimeout(() => { ui.copy.textContent = "Copy diagnostics"; }, 1500);
      });
    }
  });

  // ---- a read-only snapshot for the rendered test (tests/cam.tests.ps1, livepage) ----

  async function connStats(conn, row) {
    if (!conn.pc) return;
    try {
      (await conn.pc.getStats()).forEach((r) => {
        if (r.type === "inbound-rtp" && r.kind === "video") row.v = { framesDecoded: r.framesDecoded, framesDropped: r.framesDropped, w: r.frameWidth, h: r.frameHeight };
        if (r.type === "inbound-rtp" && r.kind === "audio") row.a = { energy: r.totalAudioEnergy, packets: r.packetsReceived };
      });
    } catch (err) { row.err = String(err); }
  }

  const timing = (d) => (d ? { gather: d.gatherMs, post: d.postMs, connected: d.connMs, frame: d.frameMs, hd: d.hdMs } : null);
  const videoRow = (v) => ({ has: !!v.srcObject, vw: v.videoWidth, vh: v.videoHeight, paused: v.paused, muted: v.muted });

  window.__liveStats = async () => {
    const columns = grid() ? getComputedStyle(grid()).gridTemplateColumns.split(" ").filter(Boolean).length : 0;
    const tiles = [];
    for (const tile of view.tiles.values()) {
      const rect = tile.root.getBoundingClientRect();
      const row = { name: tile.name, stream: tile.kind, state: tile.state, muted: tile.video.muted, paused: tile.video.paused, volume: tile.video.volume, open: !!tile.pc,
        connection: tile.pc ? tile.pc.connectionState : null, vw: tile.video.videoWidth, vh: tile.video.videoHeight,
        diag: tile.diag ? { http: tile.diag.http, gather: tile.diag.gather, ice: tile.diag.ice, conn: tile.diag.conn, local: summary(tile.diag.local),
          answer: summary(tile.diag.answer), answerClasses: (tile.diag.answer || []).map((c) => c.cls), pair: tile.diag.pair, ms: timing(tile.diag), line: tileLine(tile) } : null,
        explain: explain(tile).slice(0, 2), rect: { x: Math.round(rect.x), y: Math.round(rect.y), w: Math.round(rect.width), h: Math.round(rect.height) } };
      if (tile.pc) {
        if (tile.diag && !tile.diag.pair && tile.pc.connectionState === "connected") {
          tile.diag.pair = await selectedPair(tile.pc);
          row.diag.pair = tile.diag.pair;
          row.diag.line = tileLine(tile);
        }
        await connStats(tile, row);
      }
      tiles.push(row);
    }
    let modal = null;
    const m = view.modal;
    if (m) {
      modal = { name: m.name, opened: m.opened, hd: m.hd, shown: m.shown, muted: m.muted, hdMs: m.hdMs, verdict: m.verdict, pushed: m.pushed,
        session: !!m.conn.session, connection: m.conn.pc ? m.conn.pc.connectionState : null, ms: timing(m.conn.diag), line: modalLine(m),
        message: ui.msg.textContent, chips: ui.chips.textContent, hash: location.hash, dialogOpen: ui.dialog.open,
        focusInside: ui.dialog.contains(document.activeElement), box: (() => { const b = ui.box.getBoundingClientRect(); return { x: Math.round(b.x), y: Math.round(b.y), w: Math.round(b.width), h: Math.round(b.height) }; })() };
      if (m.opened) { modal.sub = videoRow(ui.vsub); modal.main = videoRow(ui.vmain); }
      await connStats(m.conn, modal);
    }
    const g = grid() ? grid().getBoundingClientRect() : null;
    return { shown: view.shown, hidden: view.hidden, visibility: document.visibilityState, enabled: view.info ? view.info.enabled : null,
      alive: view.info ? view.info.alive : null, modal, columns, fit: view.fit, fullscreen: document.fullscreenElement ? document.fullscreenElement.id : null, fullscreenOk,
      grid: g ? { x: Math.round(g.x), y: Math.round(g.y + window.scrollY), w: Math.round(g.width), h: Math.round(g.height) } : null,
      innerWidth: window.innerWidth, innerHeight: window.innerHeight, scrollY: window.scrollY, scrollHeight: document.documentElement.scrollHeight, scrollWidth: document.documentElement.scrollWidth, clientWidth: document.documentElement.clientWidth,
      hash: location.hash, activeElement: document.activeElement ? (document.activeElement.id || document.activeElement.dataset.camera || document.activeElement.tagName) : null,
      note: !$("lnote").classList.contains("hide") ? $("lnote").textContent : "", tiles };
  };
  window.__liveBestGrid = bestGrid;
  // Synchronous and cheap: what the modal shows right now (the test samples it a frame or two after a click).
  window.__liveProbe = () => {
    const m = view.modal;
    return { open: !!(m && m.opened), dialogOpen: ui.dialog.open, shown: m ? m.shown : null, hd: m ? m.hd : null, subHas: !!ui.vsub.srcObject,
      subVw: ui.vsub.videoWidth, subPaused: ui.vsub.paused, session: !!(m && m.conn.session), http: m && m.conn.diag ? m.conn.diag.http : null, hash: location.hash,
      fitVw: view.fit ? view.fit.vw : null, fitVh: view.fit ? view.fit.vh : null };
  };

  return { onTab };
})();
