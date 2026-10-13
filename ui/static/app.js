"use strict";

// The control UI. Two screens: the dashboard polls GET /api/status, and the
// settings screen reads and writes /api/cameras and /api/config.
//
// Three things here are not incidental:
//
//   - Whether the recorder is up comes from payload.supervisor.running, which
//     the server decides from the supervisor's PROCESS, never from
//     status.json's State field. Closing the NVR window runs no cleanup, so the
//     file can say "running" with a pid that has gone.
//   - Every action takes seconds. The button that was pressed stays visible and
//     busy, the others are disabled, and polling carries on, so the page can
//     never look frozen or let a second action race the first.
//   - Anything that stops footage being recorded asks first.

const POLL_MS = 5000;

const el = (id) => document.getElementById(id);
const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
const num = (value, places = 1) => (typeof value === "number" && isFinite(value) ? value.toFixed(places) : "-");
const gb = (bytes) => (typeof bytes === "number" ? bytes / 1073741824 : null);

function age(seconds) {
  if (seconds == null || !isFinite(seconds)) return "never";
  if (seconds < 90) return Math.round(seconds) + "s ago";
  if (seconds < 5400) return Math.round(seconds / 60) + " min ago";
  if (seconds < 172800) return Math.round(seconds / 3600) + " h ago";
  return Math.round(seconds / 86400) + " d ago";
}

function shortAge(seconds) {
  if (seconds == null || !isFinite(seconds)) return "";
  if (seconds < 90) return Math.round(seconds) + "s";
  if (seconds < 5400) return Math.round(seconds / 60) + " min";
  if (seconds < 172800) return Math.round(seconds / 3600) + " h";
  return Math.round(seconds / 86400) + " d";
}

// eventTime pulls HH:MM:SS out of either timestamp shape the recorder writes.
function eventTime(at) {
  const match = /(\d{2}:\d{2}:\d{2})/.exec(String(at || ""));
  return match ? match[1] : String(at || "");
}

// since turns a recorder timestamp into "09:12 (1 h 5 min)". Used for
// DownSince and for the last unexpected stop, where both the clock time and how
// long ago it was matter.
function since(at) {
  const when = new Date(String(at || ""));
  if (isNaN(when.getTime())) return esc(String(at || ""));
  const clock = when.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  const elapsed = shortAge((Date.now() - when.getTime()) / 1000);
  return esc(clock) + (elapsed ? " (" + esc(elapsed) + " ago)" : "");
}

function chip(cls, text) {
  return '<span class="chip ' + cls + '"><i class="dot"></i>' + esc(text) + "</span>";
}

// ---------------------------------------------------------------------------
// Pending actions
//
// Held here rather than in the DOM, because the dashboard re-renders on every
// poll and would otherwise wipe the state of an action still running.
// ---------------------------------------------------------------------------

let busyControl = null;        // {scope, action} while cam.ps1 is running
const busyCameras = new Set(); // camera names whose record toggle is in flight

const anyBusy = () => busyControl !== null || busyCameras.size > 0;

function renderControls() {
  for (const button of el("controls").querySelectorAll("button")) {
    const mine = busyControl && button.dataset.scope === busyControl.scope && button.dataset.action === busyControl.action;
    if (mine) {
      button.setAttribute("aria-busy", "true");
      button.textContent = busyControl.action === "start" ? "Starting…" : "Stopping…";
      button.disabled = true;
    } else {
      button.removeAttribute("aria-busy");
      button.textContent = button.dataset.label;
      button.disabled = anyBusy();
    }
  }
}

// ---------------------------------------------------------------------------
// Dashboard
// ---------------------------------------------------------------------------

function cameraChip(cam, live) {
  if (busyCameras.has(cam.Name)) return chip("warn", cam.Record ? "Stopping…" : "Starting…");
  if (!live) return chip("off", "Stopped");
  if (!cam.Record) return chip("off", "Not recording");
  if (!cam.Alive) return chip("crit", "Camera offline");
  if (cam.RestartsLastHour > 0) return chip("ser", "Recovering");
  return chip("good", "Recording");
}

function inputChip(cam, live) {
  if (!live || !cam.InputMode) return '<span class="muted">&mdash;</span>';
  if (cam.InputMode === "relay") return chip("good", "Relay");
  return chip("warn", "Direct");
}

// cameraDetail is the second line of a camera's cell. DownSince wins over the
// segment count and over LastError: a camera that has written nothing since a
// known time is the thing the Operator has to see.
function cameraDetail(cam, live) {
  if (live && cam.DownSince) {
    return '<div class="err">No footage since ' + since(cam.DownSince) + "</div>";
  }
  if (live && cam.LastError) {
    return '<div class="err" title="' + esc(cam.LastError) + '">' + esc(cam.LastError) + "</div>";
  }
  return '<div class="cam-meta">' + (cam.ArchiveFiles || 0) + " segments &middot; " + num(gb(cam.ArchiveBytes)) + " GB</div>";
}

function toggle(cam, live) {
  const pending = busyCameras.has(cam.Name);
  return '<div class="tglwrap"><button class="tgl" aria-checked="' + !!cam.Record +
    '" aria-label="Record ' + esc(cam.Name) + '" data-camera="' + esc(cam.Name) + '"' +
    (pending || anyBusy() ? " disabled" : "") + "><i></i></button><span>" +
    (pending ? "…" : cam.Record ? "On" : "Off") + "</span></div>";
}

function render(payload, fetchError) {
  const status = payload && payload.status;
  const file = (payload && payload.statusFile) || {};
  const supervisor = (payload && payload.supervisor) || {};
  const cams = (status && Array.isArray(status.Cameras) && status.Cameras) || [];
  const disk = (status && status.Disk) || {};

  // The one liveness test. Not status.State.
  const live = !!supervisor.running;
  const stale = !!status && supervisor.state === "running" && !live;

  // ---- feed indicator ----
  if (fetchError) {
    el("feed").innerHTML = chip("crit", "Server unreachable");
    el("feedWhen").textContent = fetchError;
  } else if (!file.present) {
    el("feed").innerHTML = chip("crit", "No status file");
    el("feedWhen").textContent = file.error || "";
  } else if (stale) {
    el("feed").innerHTML = chip("crit", "Status not trustworthy");
    el("feedWhen").textContent = supervisor.reason || "";
  } else {
    el("feed").innerHTML = chip(live ? "good" : "off", live ? "Live" : "Stopped");
    el("feedWhen").textContent = "updated " + shortAge(file.ageSeconds) + " ago";
  }

  // ---- banner ----
  const enabled = cams.filter((c) => c.Record).length;
  const recording = live ? cams.filter((c) => c.Record && c.Alive).length : 0;
  const unreachable = live ? cams.filter((c) => c.Record && !c.Alive) : [];
  const reserve = typeof disk.ReserveGB === "number" ? disk.ReserveGB : 0;
  const atReserve = typeof disk.FreeGB === "number" && disk.FreeGB <= reserve + (disk.PruneHysteresisGB || 0);

  let banner = "";
  if (!status) {
    banner = bannerHTML("crit", "No status to show.",
      file.error || "The recorder has not written a status file.");
  } else if (stale) {
    // The case State alone gets wrong, said out loud.
    banner = bannerHTML("crit", "Nothing is recording.",
      "The status file still says the recorder is running, but " + (supervisor.reason || "its process is gone") +
      ". Nothing has been captured since it stopped.");
  } else if (!live) {
    banner = bannerHTML("crit", "Nothing is recording.",
      "The supervisor is stopped, so no footage is being captured.");
  } else if (atReserve) {
    banner = bannerHTML("warn", "Disk is at its reserve.",
      "The pruner is deleting the oldest footage to keep " + num(reserve, 0) + " GB free.");
  } else if (unreachable.length) {
    banner = bannerHTML("warn",
      unreachable.length + (unreachable.length === 1 ? " camera is" : " cameras are") + " not reachable.",
      esc(unreachable.map((c) => c.Name).join(", ")) + ". They keep retrying; the others are unaffected.");
  }
  el("banner").innerHTML = banner;

  // ---- the last unexpected stop, shown whether or not it is current ----
  // A crash the UI does not mention is a crash nobody finds out about.
  const unexpected = status && status.LastUnexpectedStop;
  const kind = (status && status.StopKind) || "";
  let notice = "";
  if (unexpected && unexpected.At) {
    const current = !live && (kind === "unexpected" || kind === "interrupted");
    notice = bannerHTML(current ? "crit" : "warn",
      current ? "The recorder stopped on its own." : "The recorder stopped on its own earlier.",
      "At " + since(unexpected.At) + ": " + esc(unexpected.Reason || "no reason recorded") +
      (unexpected.DetectedBy ? " Detected by the " + esc(unexpected.DetectedBy) + "." : "") +
      (current ? "" : " It is running again now."));
  } else if (!live && kind && kind !== "requested") {
    notice = bannerHTML("crit", "The recorder stopped on its own.",
      "Its last status was " + esc(kind) + ". " + esc(supervisor.reason || ""));
  }
  el("stopNotice").innerHTML = notice;

  // ---- stat tiles: exactly ONE hero figure ----
  const days = disk.ProjectedDaysRemaining;
  const free = disk.FreeGB;
  const archive = disk.ArchiveGB;
  const usable = (free || 0) + (archive || 0) - reserve;
  const pct = usable > 0 ? Math.max(2, Math.min(100, Math.round(((free - reserve) / usable) * 100))) : 2;
  const meter = atReserve ? "meter crit" : typeof days === "number" && days < 4 ? "meter warn" : "meter";

  el("stats").innerHTML =
    '<div class="tile hero"><div class="lab">Footage headroom</div>' +
    '<div class="val">' + (typeof days === "number" && days < 1 ? days.toFixed(2) : num(days)) +
    '<span class="unit"> days</span></div>' +
    '<div class="sub">at ' + num(disk.MeasuredGBPerDay, 2) + " GB/day from " + recording +
    " recording camera" + (recording === 1 ? "" : "s") + "</div>" +
    '<div class="' + meter + '"><i style="width:' + pct + '%"></i></div></div>' +

    '<div class="tile"><div class="lab">Free space</div><div class="val">' + num(free) +
    '<span class="unit"> GB</span></div><div class="sub">' + num(reserve, 0) + " GB reserved</div></div>" +

    '<div class="tile"><div class="lab">Archive</div><div class="val">' + num(archive) +
    '<span class="unit"> GB</span></div><div class="sub">' + (disk.ArchiveFiles || 0) + " segments</div></div>" +

    '<div class="tile"><div class="lab">Recording</div><div class="val">' + recording + " / " + enabled +
    '</div><div class="sub">' + enabled + " of " + cams.length + " cameras enabled</div></div>";

  el("recCount").textContent = recording + " of " + enabled + " recording";

  // ---- layout A: table ----
  el("tbody").innerHTML = cams.map((cam) =>
    "<tr>" +
    '<td><div class="cam-name">' + esc(cam.Name) + "</div>" + cameraDetail(cam, live) + "</td>" +
    "<td>" + cameraChip(cam, live) + "</td>" +
    "<td>" + inputChip(cam, live) + "</td>" +
    '<td class="num">' + (live ? age(cam.SecondsSinceLastClosedSegment) : "&mdash;") + "</td>" +
    '<td class="num">' + (cam.RestartsLastHour ? chip("ser", String(cam.RestartsLastHour)) : "0") + "</td>" +
    "<td>" + toggle(cam, live) + "</td></tr>").join("");

  // ---- layout C: compact rows ----
  el("camsC").innerHTML = cams.map((cam) => {
    const sub = !live
      ? "Stopped"
      : cam.DownSince
        ? "No footage since " + since(cam.DownSince)
        : cam.LastError
          ? esc(cam.LastError)
          : esc(cam.InputMode) + " &middot; " + age(cam.SecondsSinceLastClosedSegment) + " &middot; " + num(gb(cam.ArchiveBytes)) + " GB";
    return '<div class="crow"><div class="main"><div class="nm">' + esc(cam.Name) + "</div>" +
      '<div class="sb">' + sub + "</div></div>" + cameraChip(cam, live) + toggle(cam, live) + "</div>";
  }).join("");

  // ---- events ----
  const events = (status && Array.isArray(status.Events) && status.Events) || [];
  el("events").innerHTML = events.length
    ? events.map((e) => "<li><time>" + esc(eventTime(e.At)) + '</time><span class="t">' + esc(e.Text) + "</span></li>").join("")
    : '<li><span class="t muted">No events recorded.</span></li>';
  el("eventCount").textContent = events.length ? events.length + " most recent" : "";

  // ---- services ----
  const tasks = (payload && payload.tasks) || {};
  el("tasks").innerHTML = ["recorder", "dashboard"].map((key) => {
    const task = tasks[key] || {};
    const label = task.name || key;
    let state;
    if (!task.exists) state = chip("crit", task.error || "not registered");
    else if (task.status === "Running") state = chip("good", "Running");
    else if (task.enabled === "Disabled") state = chip("off", "Disabled");
    else state = chip("off", task.status || "Ready");
    const last = task.exists && task.lastResult ? "last result " + esc(task.lastResult) : "";
    return '<li><time style="min-width:17ch">' + esc(label) + "</time>" + state +
      '<span class="t muted" style="margin-left:12px">' + last + "</span></li>";
  }).join("");

  // The supervisor's own verdict, in its own words, below the services.
  if (supervisor.reason) {
    el("tasks").innerHTML +=
      '<li><time style="min-width:17ch">supervisor</time>' +
      chip(live ? "good" : stale ? "crit" : "off", live ? "Running" : "Not running") +
      '<span class="t muted" style="margin-left:12px">' + esc(supervisor.reason) + "</span></li>";
  }

  renderControls();
}

function bannerHTML(cls, title, body) {
  return '<div class="banner ' + cls + '">' + chip(cls, "") + "<div><b>" + esc(title) + "</b> " + body + "</div></div>";
}

// ---------------------------------------------------------------------------
// Toast: what the last action did
// ---------------------------------------------------------------------------

function toast(cls, message, lines) {
  const output = lines && lines.length
    ? "<pre>" + esc(lines.join("\n")) + "</pre>"
    : "";
  el("toast").innerHTML =
    '<div class="toast ' + cls + '"><div class="body"><b>' + esc(message) + "</b>" + output +
    '</div><button class="x" id="toastX" aria-label="Dismiss">&times;</button></div>';
  el("toastX").onclick = () => (el("toast").innerHTML = "");
}

// ---------------------------------------------------------------------------
// Confirmation
//
// Asked for anything that stops footage being recorded, and for nothing else:
// a confirmation on a harmless action trains the Operator to dismiss them.
// ---------------------------------------------------------------------------

function confirmAction({ title, body, go }) {
  return new Promise((resolve) => {
    const dialog = el("confirm");
    el("confirmTitle").textContent = title;
    el("confirmBody").textContent = body;
    el("confirmGo").textContent = go;
    dialog.onclose = () => resolve(dialog.returnValue === "go");
    dialog.showModal();
  });
}

// ---------------------------------------------------------------------------
// Actions
// ---------------------------------------------------------------------------

async function sendJSON(method, path, body) {
  const response = await fetch(path, {
    method,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
    cache: "no-store",
  });
  let payload = {};
  try { payload = await response.json(); } catch (err) { /* a body is not guaranteed */ }
  return { ok: response.ok, status: response.status, payload };
}

const SCOPE_LABEL = { all: "everything", recorder: "the recorder", dashboard: "the viewers" };

async function runControl(scope, action) {
  if (anyBusy()) return;

  // Stopping the recorder, or everything, stops footage being written.
  // Stopping the viewers does not.
  if (action === "stop" && (scope === "all" || scope === "recorder")) {
    const ok = await confirmAction({
      title: scope === "all" ? "Stop everything?" : "Stop the recorder?",
      body: scope === "all"
        ? "The recorder, the relay and the viewers all stop. No footage is captured from any camera until you start it again."
        : "The recorder and the relay stop. No footage is captured from any camera until you start it again.",
      go: scope === "all" ? "Stop everything" : "Stop the recorder",
    });
    if (!ok) return;
  }

  busyControl = { scope, action };
  renderControls();
  toast("warn", (action === "start" ? "Starting " : "Stopping ") + SCOPE_LABEL[scope] + "… this takes a few seconds.");

  try {
    const { ok, payload } = await sendJSON("POST", "api/control", { scope, action });
    const verb = action === "start" ? "Start" : "Stop";
    if (!ok || !payload.ok) {
      toast("crit", verb + " " + SCOPE_LABEL[scope] + " failed: " + (payload.error || "unknown error"), payload.output);
      return;
    }

    // An exit code of zero is not proof that the state changed. cam.ps1 -Start
    // can return 0 having reported that the supervisor never came up, and
    // reporting that as success is how a UI tells the Operator he is recording
    // when he is not. The outcome is read back from the status endpoint, which
    // decides liveness from the supervisor's own process.
    await poll();
    const took = (payload.durationMs / 1000).toFixed(1) + "s";
    const running = !!(last && last.supervisor && last.supervisor.running);
    const touchesRecorder = scope === "all" || scope === "recorder";
    if (touchesRecorder && action === "start" && !running) {
      toast("warn", verb + " " + SCOPE_LABEL[scope] + " ran for " + took +
        " and returned success, but the recorder is still not running: " +
        ((last && last.supervisor && last.supervisor.reason) || "no status") +
        ". Nothing is being captured.", payload.output);
    } else if (touchesRecorder && action === "stop" && running) {
      toast("warn", verb + " " + SCOPE_LABEL[scope] + " ran for " + took +
        " and returned success, but the recorder is still running.", payload.output);
    } else {
      toast("good", verb + " " + SCOPE_LABEL[scope] + " finished in " + took + ".", payload.output);
    }
  } catch (err) {
    toast("crit", "The server could not be reached: " + (err.message || err));
  } finally {
    busyControl = null;
    renderControls();
    await poll();
  }
}

async function toggleRecord(name, enable) {
  if (anyBusy()) return;

  if (!enable) {
    const ok = await confirmAction({
      title: "Stop recording " + name + "?",
      body: "No footage is captured from this camera until you turn it back on. The other cameras are unaffected.",
      go: "Stop recording",
    });
    if (!ok) return;
  }

  busyCameras.add(name);
  render(last, null);
  try {
    const { ok, payload } = await sendJSON("POST", "api/cameras/" + encodeURIComponent(name) + "/record", { enabled: enable });
    if (ok && payload.ok) {
      toast("good", "Recording " + (enable ? "enabled" : "disabled") + " for " + name + ".", payload.output);
    } else {
      toast("crit", "Could not change recording for " + name + ": " + (payload.error || "unknown error"), payload.output);
    }
  } catch (err) {
    toast("crit", "The server could not be reached: " + (err.message || err));
  } finally {
    busyCameras.delete(name);
    poll();
  }
}

// ---------------------------------------------------------------------------
// Settings: cameras
// ---------------------------------------------------------------------------

// rows is the edit buffer. originalName is what identifies the row in the file,
// so a rename keeps the fields camui never sees attached to the right camera.
let rows = null;
let rowsLoaded = null;
let rowErrors = [];

const STREAMS = ["stream1", "stream2"];

function camerasDirty() {
  return rows !== null && JSON.stringify(rows) !== JSON.stringify(rowsLoaded);
}

async function loadCameras() {
  try {
    const response = await fetch("api/cameras", { cache: "no-store" });
    const payload = await response.json();
    if (!response.ok) throw new Error(payload.error || "HTTP " + response.status);
    rows = payload.cameras.map((c) => ({ originalName: c.name, name: c.name, ip: c.ip, quality: c.quality, record: c.record }));
    rowsLoaded = JSON.parse(JSON.stringify(rows));
    rowErrors = [];
    renderCameraRows();
    note("camNote", "", "");
  } catch (err) {
    rows = null;
    el("setRows").innerHTML = '<div class="empty">Could not read the camera list: ' + esc(err.message || err) + "</div>";
  }
}

function renderCameraRows() {
  if (!rows) return;

  const problems = (index, field) => rowErrors.filter((e) => e.row === index && e.field === field);
  el("setRows").innerHTML = rows.map((row, i) => {
    const streams = STREAMS.includes(row.quality) ? STREAMS : STREAMS.concat([row.quality]);
    const bad = (field) => (problems(i, field).length ? " bad" : "");
    const messages = rowErrors.filter((e) => e.row === i);
    return '<div class="row">' +
      '<label data-label="Name"><input class="' + bad("name").trim() + '" value="' + esc(row.name) +
        '" data-row="' + i + '" data-key="name" aria-label="Camera name"></label>' +
      '<label data-label="Address"><input class="' + bad("ip").trim() + '" value="' + esc(row.ip) +
        '" data-row="' + i + '" data-key="ip" aria-label="Camera address" autocomplete="off"></label>' +
      '<label data-label="Stream"><select class="' + bad("quality").trim() + '" data-row="' + i + '" data-key="quality" aria-label="Stream">' +
        streams.map((s) => '<option value="' + esc(s) + '"' + (s === row.quality ? " selected" : "") + ">" + esc(s) + "</option>").join("") +
      "</select></label>" +
      '<div data-label="Record" class="tglwrap"><button class="tgl" aria-checked="' + !!row.record +
        '" aria-label="Record ' + esc(row.name) + '" data-row="' + i + '" data-key="record"><i></i></button>' +
        "<span>" + (row.record ? "On" : "Off") + "</span></div>" +
      '<div><button class="linkbtn" data-remove="' + i + '">Remove</button></div>' +
      (messages.length ? '<div class="rowerr">' + messages.map((m) => esc(m.message)).join(" · ") + "</div>" : "") +
      "</div>";
  }).join("");

  el("camCount").textContent = rows.length + (rows.length === 1 ? " camera" : " cameras") +
    (camerasDirty() ? " · unsaved changes" : "");
  el("saveCams").disabled = !camerasDirty();
  el("discardCams").disabled = !camerasDirty();
}

async function saveCameras() {
  if (!rows) return;

  // Confirmed here rather than per keystroke: this is the moment the change
  // reaches the file.
  const stopping = rows.filter((row) => {
    const before = rowsLoaded.find((r) => r.originalName === row.originalName);
    return before && before.record && !row.record;
  }).map((row) => row.name);
  const removed = rowsLoaded.filter((before) => !rows.some((row) => row.originalName === before.originalName))
    .map((before) => before.name);

  if (stopping.length || removed.length) {
    const parts = [];
    if (stopping.length) parts.push("Recording stops for " + stopping.join(", ") + ".");
    if (removed.length) parts.push(removed.join(", ") + (removed.length === 1 ? " is" : " are") + " removed from the list and stops recording.");
    const ok = await confirmAction({
      title: "Save these changes?",
      body: parts.join(" ") + " No footage is captured from " +
        (stopping.length + removed.length === 1 ? "it" : "them") + " until you turn it back on.",
      go: "Save changes",
    });
    if (!ok) return;
  }

  el("saveCams").disabled = true;
  el("saveCams").setAttribute("aria-busy", "true");
  note("camNote", "", "Saving…");
  try {
    const { ok, payload } = await sendJSON("PUT", "api/cameras", { cameras: rows });
    rowErrors = payload.invalid || [];
    if (!ok) {
      note("camNote", "bad", payload.error || "HTTP error");
      renderCameraRows();
      return;
    }
    rows = payload.cameras.map((c) => ({ originalName: c.name, name: c.name, ip: c.ip, quality: c.quality, record: c.record }));
    rowsLoaded = JSON.parse(JSON.stringify(rows));
    renderCameraRows();
    note("camNote", "good", payload.message || "Saved.");
    poll();
  } catch (err) {
    note("camNote", "bad", "The server could not be reached: " + (err.message || err));
  } finally {
    el("saveCams").removeAttribute("aria-busy");
    renderCameraRows();
  }
}

// ---------------------------------------------------------------------------
// Settings: the recorder config
// ---------------------------------------------------------------------------

let config = null;
let configLoaded = null;

const CONFIG_FIELDS = ["SegmentSeconds", "ReserveGB", "PruneHysteresisGB", "MaxRetentionDays"];

async function loadConfig() {
  try {
    const response = await fetch("api/config", { cache: "no-store" });
    const payload = await response.json();
    if (!response.ok) throw new Error(payload.error || "HTTP " + response.status);
    configLoaded = payload.config;
    config = Object.assign({}, payload.config);
    for (const key of CONFIG_FIELDS) {
      const input = el(key);
      input.value = config[key] === null || config[key] === undefined ? "" : String(config[key]);
      clearFieldError(input.closest(".field"));
    }
    // The archive root is shown, never edited: moving it needs the recorder
    // stopped, and it comes from the status document rather than this endpoint.
    el("archiveRoot").value = (last && last.status && last.status.RootPath) || "";
    note("configNote", "", "");
    configChanged();
  } catch (err) {
    note("configNote", "bad", "Could not read the configuration: " + (err.message || err));
  }
}

// readConfigForm turns the form into the request body. A blank retention is
// null, which is what "no age limit" is in the file; a blank anywhere else is
// left for the server to refuse, with the message it gives.
function readConfigForm() {
  const body = {};
  for (const key of CONFIG_FIELDS) {
    const raw = el(key).value.trim();
    if (key === "MaxRetentionDays" && raw === "") {
      body[key] = null;
      continue;
    }
    body[key] = raw === "" ? "" : Number(raw);
    if (typeof body[key] === "number" && !isFinite(body[key])) body[key] = raw;
  }
  return body;
}

function configChanged() {
  if (!configLoaded) return;
  const current = readConfigForm();
  const dirty = CONFIG_FIELDS.some((key) => {
    const was = configLoaded[key] === undefined ? null : configLoaded[key];
    const now = current[key];
    if (was === null && now === null) return false;
    return String(was) !== String(now);
  });
  el("saveConfig").disabled = !dirty;
  el("discardConfig").disabled = !dirty;
}

async function saveConfig() {
  el("saveConfig").disabled = true;
  el("saveConfig").setAttribute("aria-busy", "true");
  note("configNote", "", "Saving…");
  for (const key of CONFIG_FIELDS) clearFieldError(el(key).closest(".field"));

  try {
    const { ok, payload } = await sendJSON("PUT", "api/config", { config: readConfigForm() });
    if (!ok) {
      for (const problem of payload.invalid || []) {
        setFieldError(document.querySelector('.field[data-field="' + problem.field + '"]'), problem.message);
      }
      note("configNote", "bad", payload.error || "HTTP error");
      return;
    }
    configLoaded = payload.config;
    await loadConfig();
    note("configNote", "good", payload.message || "Saved.");
  } catch (err) {
    note("configNote", "bad", "The server could not be reached: " + (err.message || err));
  } finally {
    el("saveConfig").removeAttribute("aria-busy");
    configChanged();
  }
}

// A field's validation message replaces its hint, so the hint is kept where it
// can be put back - otherwise one rejected save costs the explanation for good.
function clearFieldError(field) {
  if (!field) return;
  field.classList.remove("bad");
  const hint = field.querySelector(".hint");
  if (hint && hint.dataset.hint !== undefined) hint.textContent = hint.dataset.hint;
}

function setFieldError(field, message) {
  if (!field) return;
  field.classList.add("bad");
  const hint = field.querySelector(".hint");
  if (!hint) return;
  if (hint.dataset.hint === undefined) hint.dataset.hint = hint.textContent;
  hint.textContent = message;
}

function note(id, cls, text) {
  const target = el(id);
  target.className = "formnote" + (cls ? " " + cls : "");
  target.textContent = text;
}

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

for (const button of el("controls").querySelectorAll("button")) {
  button.dataset.label = button.textContent;
  button.onclick = () => runControl(button.dataset.scope, button.dataset.action);
}

// Both camera layouts and both screens are rendered from innerHTML, so the
// handlers are delegated rather than attached per element.
document.addEventListener("click", (event) => {
  const dashToggle = event.target.closest("#camsA .tgl, #camsC .tgl");
  if (dashToggle && !dashToggle.disabled) {
    const name = dashToggle.dataset.camera;
    toggleRecord(name, dashToggle.getAttribute("aria-checked") !== "true");
    return;
  }

  const rowToggle = event.target.closest('#setRows .tgl');
  if (rowToggle) {
    const row = rows[Number(rowToggle.dataset.row)];
    row.record = !row.record;
    renderCameraRows();
    return;
  }

  const remove = event.target.closest("#setRows .linkbtn");
  if (remove) {
    rows.splice(Number(remove.dataset.remove), 1);
    rowErrors = [];
    renderCameraRows();
  }
});

el("setRows").addEventListener("input", (event) => {
  const field = event.target.closest("input, select");
  if (!field || field.dataset.row === undefined) return;
  rows[Number(field.dataset.row)][field.dataset.key] = field.value;
  el("camCount").textContent = rows.length + (rows.length === 1 ? " camera" : " cameras") +
    (camerasDirty() ? " · unsaved changes" : "");
  el("saveCams").disabled = !camerasDirty();
  el("discardCams").disabled = !camerasDirty();
});

el("addCam").onclick = () => {
  rows.push({ originalName: "", name: "", ip: "", quality: "stream1", record: false });
  rowErrors = [];
  renderCameraRows();
};
el("saveCams").onclick = saveCameras;
el("discardCams").onclick = loadCameras;
el("saveConfig").onclick = saveConfig;
el("discardConfig").onclick = loadConfig;
for (const key of CONFIG_FIELDS) el(key).addEventListener("input", configChanged);

el("tabs").onclick = (event) => {
  const button = event.target.closest("button");
  if (!button) return;
  for (const tab of el("tabs").children) tab.removeAttribute("aria-current");
  button.setAttribute("aria-current", "page");
  for (const section of document.querySelectorAll("section[data-tab]")) {
    section.classList.toggle("hide", section.dataset.tab !== button.dataset.t);
  }
  if (button.dataset.t === "set" && !camerasDirty()) {
    loadCameras();
    loadConfig();
  }
};

// ---------------------------------------------------------------------------
// Polling
// ---------------------------------------------------------------------------

let last = null;

async function poll() {
  try {
    const response = await fetch("api/status", { cache: "no-store" });
    if (!response.ok) throw new Error("HTTP " + response.status);
    last = await response.json();
    render(last, null);
  } catch (err) {
    render(last, err.message || String(err));
  }
}

poll();
setInterval(poll, POLL_MS);
document.addEventListener("visibilitychange", () => {
  if (!document.hidden) poll();
});
