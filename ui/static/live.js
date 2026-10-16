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
//   - A tile that is not playing says why, in plain words (signalling result,
//     ICE states, how many candidates the browser and the server offered and of
//     what kind) with a "Copy diagnostics" button. Only counts, types and an
//     address CLASS (loopback, private, ...) are shown: never an address.
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
    local: null, answer: null, pair: null });

  const seconds = (diag) => Math.round((performance.now() - diag.t0) / 1000);

  // One line for pasting into a bug report. No address, no camera name.
  function diagLine(tile) {
    const d = tile.diag;
    if (!d) return "live-diag/1 browser=" + browserFamily() + " no attempt yet";
    return ["live-diag/1", "browser=" + browserFamily(), "stream=" + tile.stream, "http=" + (d.http === null ? "none" : d.http),
      "gather=" + d.gather + (d.gatherMs === null ? "" : "(" + d.gatherMs + "ms)"), "ice=" + d.ice, "conn=" + d.conn,
      "local=[" + summary(d.local) + "]", "answer=[" + summary(d.answer) + "]", d.pair ? "pair=" + d.pair : "pair=none",
      "elapsed=" + seconds(d) + "s", "verdict=" + (tile.lastVerdict || tile.detail || tile.state)].join(" ");
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
        '<div class="lover"><div class="lh"></div><div class="ld"></div><button type="button" class="lcopy hide">Copy diagnostics</button></div>' +
        '<div class="lbar"><span class="lname">' + esc(cam.name) + '</span><span class="lchip"></span>' +
        '<button type="button" class="lmute" aria-pressed="false" title="Sound is muted. Click to unmute.">Unmute</button></div>';
      const tile = { name: cam.name, root, video: root.querySelector("video"), pc: null, session: null, gen: 0,
        stream: "sub", state: "idle", detail: "", fails: 0, retry: null, watchdog: null, tick: null, diag: null, lastVerdict: "", lastDetail: "", cam };
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

  function closeTile(tile) {
    tile.gen++;
    clearTimeout(tile.retry);
    clearTimeout(tile.watchdog);
    clearInterval(tile.tick);
    tile.retry = tile.watchdog = tile.tick = null;
    const pc = tile.pc;
    tile.pc = null;
    if (pc) { pc.onconnectionstatechange = null; pc.oniceconnectionstatechange = null; pc.onicegatheringstatechange = null; pc.ontrack = null; try { pc.close(); } catch (err) { /* already closed */ } }
    dropSession(tile.session);
    tile.session = null;
    tile.video.srcObject = null;
  }

  function fail(tile, gen, state, detail, delay) {
    if (tile.gen !== gen) return;
    tile.lastVerdict = detail;
    const was = tile.state;
    tile.state = state;
    tile.lastDetail = explain(tile)[1];
    tile.state = was;
    if (tile.diag) tile.diag.conn = tile.pc ? tile.pc.connectionState : tile.diag.conn;
    console.info("live:", tile.name, diagLine(tile));
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
    const diag = tile.diag = newDiag();
    setState(tile, "connecting");

    let pc;
    try {
      pc = new RTCPeerConnection({ iceServers: [] });
      tile.pc = pc;
      pc.addTransceiver("video", { direction: "recvonly" });
      pc.addTransceiver("audio", { direction: "recvonly" });
      pc.ontrack = (event) => { if (tile.video.srcObject !== event.streams[0]) tile.video.srcObject = event.streams[0]; };
      pc.onicegatheringstatechange = () => { diag.gather = pc.iceGatheringState; paint(tile); };
      pc.oniceconnectionstatechange = () => { diag.ice = pc.iceConnectionState; paint(tile); };
      let lost = null;
      pc.onconnectionstatechange = () => {
        if (tile.gen !== gen) return;
        const now = pc.connectionState;
        diag.conn = now;
        if (now === "connected") {
          selectedPair(pc).then((pair) => { diag.pair = pair || diag.pair; if (!pair) setTimeout(() => selectedPair(pc).then((again) => { diag.pair = again || diag.pair; }), 1500); });
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
      diag.gather = pc.iceGatheringState;
      diag.gatherMs = Math.round(performance.now() - diag.t0);
      diag.local = candidates(pc.localDescription.sdp);

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
      diag.http = response.status;
      if (response.status !== 201) return failedAnswer(tile, gen, response);

      const location = response.headers.get("Location");
      tile.session = location ? location.replace(/^\//, "") : null;
      const answer = await response.text();
      diag.answer = candidates(answer);
      await pc.setRemoteDescription({ type: "answer", sdp: answer });
      if (tile.gen !== gen) return;
      paint(tile);
      tile.watchdog = setTimeout(() => fail(tile, gen, "error", "No media after " + Math.round(CONNECT_MS / 1000) + " s"), CONNECT_MS);
    } catch (err) {
      if (tile.gen === gen) fail(tile, gen, "error", "Could not connect");
    }
  }

  async function failedAnswer(tile, gen, response) {
    let text = "";
    try { text = (await response.json()).error || ""; } catch (err) { /* a body is not guaranteed */ }
    if (tile.gen !== gen) return;
    tile.diag.httpText = text;
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
    if (event.target.closest(".lcopy")) {
      const button = event.target.closest(".lcopy");
      copyText(diagLine(tile)).then((ok) => {
        if (!ok) window.prompt("Copy this line:", diagLine(tile));
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
        focus: tile.name === view.focus,
        diag: tile.diag ? { http: tile.diag.http, gather: tile.diag.gather, ice: tile.diag.ice, conn: tile.diag.conn, local: summary(tile.diag.local),
          answer: summary(tile.diag.answer), answerClasses: (tile.diag.answer || []).map((c) => c.cls), pair: tile.diag.pair, line: diagLine(tile) } : null,
        explain: explain(tile).slice(0, 2), rect: { x: Math.round(rect.x), y: Math.round(rect.y), w: Math.round(rect.width), h: Math.round(rect.height) } };
      if (tile.pc) {
        if (tile.diag && !tile.diag.pair && tile.pc.connectionState === "connected") {
          tile.diag.pair = await selectedPair(tile.pc);
          row.diag.pair = tile.diag.pair;
          row.diag.line = diagLine(tile);
        }
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
