"use strict";

// The Live screen: one tile per camera, WebRTC straight from the live-view
// MediaMTX through camui's same-origin signalling door (api/live/...).
//
// What is not incidental:
//
//   - The page only ever names a CAMERA and sub|main. It never sees the live
//     instance's address, its login, or a path name; camui adds the login and
//     returns a session address it minted itself.
//   - Tiles play the sub-stream. Focusing one switches that tile alone to the
//     main stream; only one main exists at a time.
//   - Audio is muted until the Operator unmutes that tile.
//   - Every connection is closed while the page is hidden or another tab is
//     showing: a hidden tab keeps decoding at a reduced rate, which costs CPU
//     for nothing, and each open connection holds a camera session.
//   - A deep link opens the screen: #live, or #live/<camera name> to focus one.
//
// getJSON, el and esc are app.js's; they are looked up when called.

const Live = (() => {
  const GATHER_MS = 2000;                        // non-trickle: wait this long for candidates, at most
  const CONNECT_MS = 15000;                      // an answer that never turns into media is a failure
  const DISCONNECT_MS = 4000;                    // a transient 'disconnected' may heal on its own
  const BACKOFF_MS = [1000, 2000, 4000, 8000, 15000];
  const REFRESH_MS = 10000;
  const STAGGER_MS = 150;                        // between tiles opening, so six sources do not start in one burst

  const view = {
    info: null,          // the last GET api/live
    error: null,         // why the last GET failed
    tiles: new Map(),    // camera name -> tile
    focus: null,         // the camera on the main stream
    wantFocus: null,     // a deep link waiting for the list to arrive
    shown: false,        // the Live tab is the one on screen
    hidden: document.hidden,
    timer: null,
  };

  const CHIP = {
    connecting: ["warn", "Connecting"],
    live: ["good", "Live"],
    offline: ["off", "Offline"],
    error: ["crit", "Error"],
    idle: ["off", "Paused"],
  };

  const grid = () => el("lgrid");
  const usable = () => view.shown && !view.hidden && view.info && view.info.enabled && view.info.alive;

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
    const box = el("lnote");
    box.classList.toggle("hide", !note);
    if (note) box.innerHTML = '<div class="lnt">' + esc(note[0]) + '</div><div class="lns">' + esc(note[1]) + "</div>";

    const cameras = view.info && view.info.enabled ? view.info.cameras : [];
    const names = cameras.map((cam) => cam.name);
    if (names.join("\n") !== [...view.tiles.keys()].join("\n")) rebuild(cameras);
    for (const cam of cameras) updateTile(view.tiles.get(cam.name), cam);

    if (view.wantFocus !== null && view.tiles.size) {
      const wanted = [...view.tiles.keys()].find((name) => name.toLowerCase() === view.wantFocus.toLowerCase());
      view.wantFocus = null;
      if (wanted) focusTile(wanted, false);
    }
    el("lcount").textContent = view.info && view.info.enabled ? names.length + (names.length === 1 ? " camera" : " cameras") : "";
    sync();
  }

  function rebuild(cameras) {
    for (const tile of view.tiles.values()) closeTile(tile);
    view.tiles.clear();
    grid().replaceChildren();
    if (view.focus && !cameras.some((cam) => cam.name === view.focus)) view.focus = null;
    for (const cam of cameras) {
      const root = document.createElement("div");
      root.className = "ltile";
      root.tabIndex = 0;
      root.setAttribute("role", "button");
      root.dataset.camera = cam.name;
      root.innerHTML =
        '<video muted autoplay playsinline></video>' +
        '<div class="lover"></div>' +
        '<div class="lbar"><span class="lname">' + esc(cam.name) + '</span><span class="lchip"></span>' +
        '<button type="button" class="lmute" aria-pressed="false" title="Sound is muted. Click to unmute.">Unmute</button></div>';
      const tile = { name: cam.name, root, video: root.querySelector("video"), pc: null, session: null, gen: 0,
        stream: "sub", state: "idle", detail: "", fails: 0, retry: null, watchdog: null, cam };
      tile.video.muted = true;
      view.tiles.set(cam.name, tile);
      grid().append(root);
      paint(tile);
    }
    applyFocusClass();
  }

  function updateTile(tile, cam) {
    if (!tile) return;
    tile.cam = cam;
    paint(tile);
  }

  function paint(tile) {
    const [cls, word] = CHIP[tile.state] || CHIP.idle;
    tile.root.dataset.state = tile.state;
    tile.root.dataset.stream = tile.stream;
    tile.root.querySelector(".lchip").innerHTML = '<span class="chip ' + cls + '"><i class="dot"></i>' + esc(word) +
      (tile.stream === "main" ? " · main" : "") + "</span>";
    const over = tile.root.querySelector(".lover");
    let text = "";
    if (tile.state === "connecting") text = "Connecting";
    else if (tile.state === "offline") text = tile.detail || "No picture from this camera";
    else if (tile.state === "error") text = tile.detail || "Could not connect";
    over.textContent = text;
    over.classList.toggle("hide", !text);
    const hint = tile.cam && tile.cam.lastError ? "Recorder: " + tile.cam.lastError : "";
    tile.root.title = hint;
    const mute = tile.root.querySelector(".lmute");
    mute.textContent = tile.video.muted ? "Unmute" : "Mute";
    mute.setAttribute("aria-pressed", String(!tile.video.muted));
    mute.title = tile.video.muted ? "Sound is muted. Click to unmute." : "Sound is on. Click to mute.";
  }

  // ---- one connection per tile --------------------------------------------

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
    paint(tile);
  }

  function dropSession(url) {
    if (!url) return;
    // keepalive lets the request outlive a page that is unloading. Not
    // sendBeacon: that cannot send a DELETE.
    fetch(url, { method: "DELETE", keepalive: true, cache: "no-store" }).catch(() => {});
  }

  function closeTile(tile) {
    tile.gen++;
    clearTimeout(tile.retry);
    clearTimeout(tile.watchdog);
    tile.retry = tile.watchdog = null;
    const pc = tile.pc;
    tile.pc = null;
    if (pc) { pc.onconnectionstatechange = null; pc.ontrack = null; try { pc.close(); } catch (err) { /* already closed */ } }
    dropSession(tile.session);
    tile.session = null;
    tile.video.srcObject = null;
  }

  function fail(tile, gen, state, detail, delay) {
    if (tile.gen !== gen) return;
    closeTile(tile);
    setState(tile, state, detail);
    tile.fails++;
    const wait = delay || BACKOFF_MS[Math.min(tile.fails - 1, BACKOFF_MS.length - 1)];
    const myGen = tile.gen;
    tile.retry = setTimeout(() => { tile.retry = null; if (tile.gen === myGen && usable()) open(tile); }, wait + Math.random() * 500);
  }

  async function open(tile) {
    closeTile(tile);
    const gen = tile.gen;
    const stream = view.focus === tile.name ? "main" : "sub";
    tile.stream = stream;
    setState(tile, "connecting");

    let pc;
    try {
      pc = new RTCPeerConnection({ iceServers: [] });
      tile.pc = pc;
      pc.addTransceiver("video", { direction: "recvonly" });
      pc.addTransceiver("audio", { direction: "recvonly" });
      pc.ontrack = (event) => { if (tile.video.srcObject !== event.streams[0]) tile.video.srcObject = event.streams[0]; };
      let lost = null;
      pc.onconnectionstatechange = () => {
        if (tile.gen !== gen) return;
        const now = pc.connectionState;
        if (now === "connected") {
          clearTimeout(tile.watchdog);
          clearTimeout(lost);
          tile.fails = 0;
          setState(tile, "live");
        } else if (now === "disconnected") {
          clearTimeout(lost);
          lost = setTimeout(() => fail(tile, gen, "offline", "Connection lost"), DISCONNECT_MS);
        } else if (now === "failed" || now === "closed") {
          clearTimeout(lost);
          fail(tile, gen, "offline", "Connection lost");
        }
      };

      await pc.setLocalDescription(await pc.createOffer());
      await gathered(pc, GATHER_MS);
      if (tile.gen !== gen) return;

      const response = await fetch("api/live/" + encodeURIComponent(tile.name) + "/whep?stream=" + stream, {
        method: "POST",
        headers: { "Content-Type": "application/sdp" },
        body: pc.localDescription.sdp,
        cache: "no-store",
      });
      if (tile.gen !== gen) {
        // Superseded while the answer was on its way: do not leave its session behind.
        const stale = response.headers.get("Location");
        if (response.status === 201 && stale) dropSession(stale.replace(/^\//, ""));
        return;
      }
      if (response.status !== 201) return failedAnswer(tile, gen, response);

      const location = response.headers.get("Location");
      tile.session = location ? location.replace(/^\//, "") : null;
      await pc.setRemoteDescription({ type: "answer", sdp: await response.text() });
      if (tile.gen !== gen) return;
      tile.watchdog = setTimeout(() => fail(tile, gen, "error", "No media arrived"), CONNECT_MS);
    } catch (err) {
      if (tile.gen === gen) fail(tile, gen, "error", "Could not connect");
    }
  }

  async function failedAnswer(tile, gen, response) {
    let text = "";
    try { text = (await response.json()).error || ""; } catch (err) { /* a body is not guaranteed */ }
    if (tile.gen !== gen) return;
    if (response.status === 503 && text === "live view is off") {
      view.info = Object.assign({}, view.info, { enabled: false });
      closeTile(tile);
      return render();
    }
    if (response.status === 503) {
      view.info = Object.assign({}, view.info, { alive: false });
      closeTile(tile);
      return render();
    }
    if (response.status === 404) return fail(tile, gen, "offline", "No picture from this camera");
    if (response.status === 429) return fail(tile, gen, "error", "Too many viewers - retrying", 10000);
    if (response.status === 504) return fail(tile, gen, "error", "The camera is slow to start");
    if (response.status === 502) return fail(tile, gen, "error", text || "Live view error", 15000);
    fail(tile, gen, "error", "Could not connect");
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
      return;
    }
    let slot = 0;
    for (const tile of view.tiles.values()) {
      const want = view.focus === tile.name ? "main" : "sub";
      if ((tile.pc || tile.retry) && tile.stream === want) continue;
      tile.fails = 0;
      clearTimeout(tile.retry);
      tile.retry = setTimeout(() => { tile.retry = null; if (usable()) open(tile); }, slot++ * STAGGER_MS);
    }
  }

  function applyFocusClass() {
    grid().classList.toggle("has-focus", view.focus !== null);
    for (const tile of view.tiles.values()) tile.root.classList.toggle("focus", tile.name === view.focus);
  }

  function focusTile(name, rewriteHash = true) {
    const previous = view.focus;
    view.focus = name;
    applyFocusClass();
    if (rewriteHash) history.replaceState(null, "", name ? "#live/" + encodeURIComponent(name) : "#live");
    // Only the tiles whose stream changed reconnect.
    for (const changed of [previous, name]) {
      const tile = changed && view.tiles.get(changed);
      if (tile && usable()) { tile.fails = 0; open(tile); }
    }
    const focused = name && view.tiles.get(name);
    if (focused) focused.root.scrollIntoView({ block: "nearest" });
  }

  // ---- the page ------------------------------------------------------------

  function onTab(name, arg) {
    const shown = name === "live";
    if (shown && arg) view.wantFocus = arg;
    if (shown === view.shown && !arg) return;
    view.shown = shown;
    clearInterval(view.timer);
    view.timer = null;
    if (shown) {
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
  });
  window.addEventListener("pageshow", (event) => { if (event.persisted && view.shown) refresh(); });

  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape" && view.shown && view.focus) focusTile(null);
  });

  document.addEventListener("click", (event) => {
    const root = event.target.closest("#lgrid .ltile");
    if (!root) return;
    const tile = view.tiles.get(root.dataset.camera);
    if (event.target.closest(".lmute")) {
      tile.video.muted = !tile.video.muted;
      if (!tile.video.muted) { tile.video.volume = 1; tile.video.play().catch(() => {}); }
      paint(tile);
      return;
    }
    focusTile(view.focus === tile.name ? null : tile.name);
  });
  document.addEventListener("keydown", (event) => {
    const root = event.target.closest && event.target.closest("#lgrid .ltile");
    if (root && event.target === root && (event.key === "Enter" || event.key === " ")) {
      event.preventDefault();
      root.click();
    }
  });

  // A read-only snapshot for the rendered test (tests/cam.tests.ps1, livepage).
  window.__liveStats = async () => {
    const columns = grid() ? getComputedStyle(grid()).gridTemplateColumns.split(" ").filter(Boolean).length : 0;
    const tiles = [];
    for (const tile of view.tiles.values()) {
      const rect = tile.root.getBoundingClientRect();
      const row = { name: tile.name, stream: tile.stream, state: tile.state, muted: tile.video.muted, paused: tile.video.paused, volume: tile.video.volume, open: !!tile.pc,
        connection: tile.pc ? tile.pc.connectionState : null, vw: tile.video.videoWidth, vh: tile.video.videoHeight,
        focus: tile.name === view.focus, rect: { x: Math.round(rect.x), y: Math.round(rect.y), w: Math.round(rect.width), h: Math.round(rect.height) } };
      if (tile.pc) {
        try {
          (await tile.pc.getStats()).forEach((r) => {
            if (r.type === "inbound-rtp" && r.kind === "video") row.v = { framesDecoded: r.framesDecoded, framesDropped: r.framesDropped, w: r.frameWidth, h: r.frameHeight };
            if (r.type === "inbound-rtp" && r.kind === "audio") row.a = { energy: r.totalAudioEnergy, packets: r.packetsReceived };
          });
        } catch (err) { row.err = String(err); }
      }
      tiles.push(row);
    }
    return { shown: view.shown, hidden: view.hidden, visibility: document.visibilityState, enabled: view.info ? view.info.enabled : null,
      alive: view.info ? view.info.alive : null, focus: view.focus, columns, innerWidth: window.innerWidth, scrollY: window.scrollY, note: !el("lnote").classList.contains("hide") ? el("lnote").textContent : "", tiles };
  };

  return { onTab };
})();
