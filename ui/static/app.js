"use strict";

// The control UI. The dashboard polls GET /api/status, the settings screen reads
// and writes /api/cameras and /api/config, the footage screen browses the
// archive, and the Live screen (live.js) plays the cameras over WebRTC.
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

// True when this page is served by the HTTPS LAN listener in its default view-only
// mode: api/tls/info (a 404 on the loopback listener) says readOnly. Everything that
// changes state is hidden or inert then, and the server refuses it anyway.
let viewOnly = false;

const el = (id) => document.getElementById(id);

// The LAN listener answers a request without a valid session with 401 and
// X-Login-Required (the session ended, or camui was reset). Go to the sign-in page
// and come back to this screen afterwards. The loopback listener never sends it.
const baseFetch = window.fetch.bind(window);
window.fetch = async (...args) => {
  const response = await baseFetch(...args);
  if (response.status === 401 && response.headers.get("X-Login-Required")) {
    location.assign("/login?next=" + encodeURIComponent(location.pathname + location.search + location.hash));
  }
  return response;
};
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

let busyControl = null;        // {scope, action, startedAt} while cam.ps1 is running
let busyTicker = null;         // the 1 s interval that keeps the elapsed count moving
const busyCameras = new Set(); // camera names whose record toggle is in flight

const anyBusy = () => busyControl !== null || busyCameras.size > 0;

const SCOPE_LABEL = { all: "everything", recorder: "the recorder", dashboard: "the viewers" };

// ACTION_EXPECTATION is what each action actually took when it was measured on
// this system: stop viewers 15.2s, start viewers 12.5s, stop recorder 19.7s,
// start recorder 27.3s, stop everything 17.3s, start everything 52.1s.
//
// Up to about a minute, which is long enough that an indicator which does not
// visibly change reads as a hung page - the Operator reported exactly that. So
// the elapsed seconds tick, on the pressed button and beside it, and the page
// says how long this particular action usually takes.
//
// There is deliberately no progress bar. Nothing here can know what fraction
// of a start is done, and a bar stalled at 80% is a worse lie than an honest
// counter.
const ACTION_EXPECTATION = {
  "start:all": { seconds: 52.1, usual: "usually takes up to a minute" },
  "stop:all": { seconds: 17.3, usual: "usually takes about 20 seconds" },
  "start:recorder": { seconds: 27.3, usual: "usually takes about half a minute" },
  "stop:recorder": { seconds: 19.7, usual: "usually takes about 20 seconds" },
  "start:dashboard": { seconds: 12.5, usual: "usually takes about 15 seconds" },
  "stop:dashboard": { seconds: 15.2, usual: "usually takes about 15 seconds" },
};

// ACTION_CEILING_SECONDS mirrors controlTimeout in ui/control.go. Past it the
// server has given up on the script, so the page says so plainly rather than
// counting upwards for ever.
const ACTION_CEILING_SECONDS = 180;

const busySeconds = () => (busyControl ? Math.floor((Date.now() - busyControl.startedAt) / 1000) : 0);

function renderControls() {
  for (const button of el("controls").querySelectorAll("button")) {
    const mine = busyControl && button.dataset.scope === busyControl.scope && button.dataset.action === busyControl.action;
    if (mine) {
      button.setAttribute("aria-busy", "true");
      button.textContent = (busyControl.action === "start" ? "Starting… " : "Stopping… ") + busySeconds() + "s";
      button.disabled = true;
    } else {
      button.removeAttribute("aria-busy");
      button.textContent = button.dataset.label;
      button.disabled = anyBusy();
    }
  }
}

// renderProgress says what is running, for how long it has been running, and
// how long that normally takes. Called every second while an action is in
// flight, which is what makes it visible movement rather than a still frame.
function renderProgress() {
  const region = el("progress");
  if (!busyControl) {
    region.innerHTML = "";
    return;
  }

  const seconds = busySeconds();
  const expected = ACTION_EXPECTATION[busyControl.action + ":" + busyControl.scope];
  const ceilingMinutes = ACTION_CEILING_SECONDS / 60;

  let detail;
  if (seconds > ACTION_CEILING_SECONDS) {
    detail = "This has passed the " + ceilingMinutes + " minutes camui allows an action. " +
      "It is being given up on, and the result will say what happened.";
  } else if (!expected) {
    detail = "camui gives up at " + ceilingMinutes + " minutes.";
  } else if (seconds > expected.seconds) {
    detail = "Longer than usual - it " + expected.usual + ". " +
      "Still waiting; camui gives up at " + ceilingMinutes + " minutes.";
  } else {
    detail = "This " + expected.usual + ".";
  }

  region.innerHTML = '<div class="toast warn"><div class="body"><b>' +
    esc((busyControl.action === "start" ? "Starting " : "Stopping ") + SCOPE_LABEL[busyControl.scope] + "… ") +
    '<span class="elapsed">' + seconds + 's</span></b><div class="detail">' + esc(detail) + "</div></div></div>";
}

function startBusyTicker() {
  stopBusyTicker();
  busyTicker = setInterval(() => {
    renderControls();
    renderProgress();
  }, 1000);
}

function stopBusyTicker() {
  if (busyTicker !== null) {
    clearInterval(busyTicker);
    busyTicker = null;
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

  // ---- persistent notices, shown whether or not they are current ----
  // A crash the UI does not mention is a crash nobody finds out about, so these
  // records outlive the recovery that followed them. Each can be dismissed, and
  // the dismissal is keyed to the event's own timestamp - see NOTICES.
  const kind = (status && status.StopKind) || "";
  const acknowledged = (payload && payload.acknowledged) || {};
  const notices = [];
  for (const notice of NOTICES) {
    const record = status && status[notice.key];
    if (!record || !record.At) continue;
    if (acknowledged[notice.key] === record.At) continue;
    notices.push(noticeHTML(notice, record, !live && notice.currentKinds.includes(kind)));
  }

  // The fallback: a stop kind the file carries no record for. It cannot be
  // dismissed, because there is no event timestamp to key a dismissal to, and
  // it does not need to be - unlike the records above it describes the CURRENT
  // state and goes of its own accord when the recorder is back.
  const described = NOTICES.some((notice) =>
    notice.currentKinds.includes(kind) && status && status[notice.key] && status[notice.key].At);
  if (!described && !live && kind && kind !== "requested") {
    notices.push(bannerHTML("crit", "The recorder stopped on its own.",
      "Its last status was " + esc(kind) + ". " + esc(supervisor.reason || "")));
  }
  el("stopNotice").innerHTML = notices.join("");

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

  // The archive root is shown, never edited: moving it needs the recorder
  // stopped, and it comes from the status document rather than /api/config. Set
  // on every poll, not when the Settings tab opens: opening that tab before the
  // first poll had returned left the field permanently blank.
  if (status && status.RootPath) el("archiveRoot").value = status.RootPath;

  renderControls();
}

function bannerHTML(cls, title, body, trailing) {
  return '<div class="banner ' + cls + '">' + chip(cls, "") + "<div><b>" + esc(title) + "</b> " + body + "</div>" +
    (trailing || "") + "</div>";
}

// ---------------------------------------------------------------------------
// Persistent notices
//
// status.json keeps the last unexpected stop and the last window close after
// the recorder is running again, deliberately: an outage must not be erased by
// the recovery that followed it. These are therefore the banners that stay, and
// the only way to clear one is to say it has been seen.
//
// That acknowledgement is keyed by the event's OWN timestamp and stored by the
// server in a sidecar beside status.json, not in this browser. So it holds
// after a reload and from a second browser, and a LATER stop - a different
// timestamp - appears again rather than being swallowed by an older dismissal.
// ---------------------------------------------------------------------------

const NOTICES = [
  {
    key: "LastUnexpectedStop",
    current: "The recorder stopped on its own.",
    earlier: "The recorder stopped on its own earlier.",
    currentKinds: ["unexpected", "interrupted"],
  },
  {
    key: "LastWindowClose",
    current: "The NVR window was closed, so recording stopped.",
    earlier: "The NVR window was closed earlier, and recording stopped with it.",
    currentKinds: ["window-closed"],
  },
];

function noticeHTML(notice, record, current) {
  return bannerHTML(current ? "crit" : "warn", current ? notice.current : notice.earlier,
    "At " + since(record.At) + ": " + esc(record.Reason || "no reason recorded") +
    (record.DetectedBy ? " Detected by the " + esc(record.DetectedBy) + "." : "") +
    (current ? "" : " It is running again now."),
    '<button class="btn small" data-ack="' + esc(notice.key) + '" data-at="' + esc(record.At) +
    '" title="Stop showing this one. A later stop appears again.">Dismiss</button>');
}

// acknowledgeNotice records that this event has been seen, keyed by its own
// timestamp. Rendered from the response rather than from the next poll, which
// is up to five seconds away.
async function acknowledgeNotice(button) {
  if (viewOnly) return;
  button.disabled = true;
  try {
    const { ok, payload } = await sendJSON("POST", "api/acknowledge",
      { notice: button.dataset.ack, at: button.dataset.at });
    if (!ok || !payload.ok) {
      button.disabled = false;
      toast("crit", "Could not dismiss that notice: " + (payload.error || "unknown error"));
      return;
    }
    if (last) last.acknowledged = payload.notices;
    render(last, null);
  } catch (err) {
    button.disabled = false;
    toast("crit", "The server could not be reached: " + (err.message || err));
  }
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

async function runControl(scope, action) {
  if (viewOnly) return;
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

  // The progress region replaces the previous toast: the toast said "this
  // takes a few seconds" for an action measured at 52, and never changed once
  // it was on screen.
  busyControl = { scope, action, startedAt: Date.now() };
  el("toast").innerHTML = "";
  renderControls();
  renderProgress();
  startBusyTicker();

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
    stopBusyTicker();
    renderProgress();
    renderControls();
    await poll();
  }
}

async function toggleRecord(name, enable) {
  if (viewOnly) return;
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
  if (viewOnly) return;
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
  if (viewOnly) return;
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
// Footage
//
// Three selections and a player: camera, date, segment. Each selection is
// loaded from /api/footage, which parses the index out of the filenames the
// recorder already wrote, so there is nothing to keep in sync here.
//
// The player is rendered on its own, never as part of a list re-render: a
// <video> whose element is replaced starts again from the beginning, so a
// refresh while watching must not touch it.
// ---------------------------------------------------------------------------

const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

// GAPS_SHOWN caps the banner. A camera that flapped all day produces dozens of
// gaps, and the point is that the Operator sees the day has holes, not that the
// segment list is pushed off the screen.
const GAPS_SHOWN = 4;

const footage = {
  cameras: null,   // null until the first load
  camera: null,
  dates: null,
  date: null,
  segments: null,
  gaps: [],
  seconds: 0,
  bytes: 0,
  playing: null,
  error: null,
};

// clock renders a segment's own length: 15:00 for a full one, 1:11 for one the
// camera cut short, 1:02:30 if the segment length is ever raised past an hour.
function clock(seconds) {
  if (seconds == null || !isFinite(seconds)) return "-";
  const whole = Math.round(seconds);
  const parts = [Math.floor(whole / 60) % 60, whole % 60];
  const hours = Math.floor(whole / 3600);
  const padded = parts.map((n) => String(n).padStart(2, "0"));
  return hours ? hours + ":" + padded.join(":") : Number(padded[0]) + ":" + padded[1];
}

// spell is for durations read as prose rather than as a clock - a gap's length.
function spell(seconds) {
  if (seconds == null || !isFinite(seconds)) return "-";
  const whole = Math.round(seconds);
  if (whole < 60) return whole + " s";
  if (whole < 3600) return Math.floor(whole / 60) + " min";
  const hours = Math.floor(whole / 3600);
  const minutes = Math.round((whole - hours * 3600) / 60);
  return minutes ? hours + " h " + minutes + " min" : hours + " h";
}

function size(bytes) {
  if (typeof bytes !== "number") return "-";
  if (bytes >= 1073741824) return (bytes / 1073741824).toFixed(1) + " GB";
  return Math.round(bytes / 1048576) + " MB";
}

// dayLabel reads an ISO date without constructing a Date, which would reinterpret
// it in the browser's zone. Today and yesterday are named, because "last night's
// footage" is what the Operator is usually after.
function dayLabel(iso) {
  const parts = /^(\d{4})-(\d{2})-(\d{2})$/.exec(String(iso || ""));
  if (!parts) return String(iso || "");
  const now = new Date();
  const today = [now.getFullYear(), String(now.getMonth() + 1).padStart(2, "0"), String(now.getDate()).padStart(2, "0")].join("-");
  const before = new Date(now.getFullYear(), now.getMonth(), now.getDate() - 1);
  const yesterday = [before.getFullYear(), String(before.getMonth() + 1).padStart(2, "0"), String(before.getDate()).padStart(2, "0")].join("-");
  if (iso === today) return "Today";
  if (iso === yesterday) return "Yesterday";
  const label = Number(parts[3]) + " " + MONTHS[Number(parts[2]) - 1];
  return parts[1] === String(now.getFullYear()) ? label : label + " " + parts[1];
}

function gapText(gap) {
  if (gap.open) return "Nothing recorded since " + gap.from + " - " + spell(gap.seconds) + " and counting";
  return "No footage " + gap.from + " to " + gap.to + " - " + spell(gap.seconds);
}

async function getJSON(path) {
  const response = await fetch(path, { cache: "no-store" });
  let payload = null;
  try { payload = await response.json(); } catch (err) { /* a body is not guaranteed */ }
  if (!response.ok) throw new Error((payload && payload.error) || "HTTP " + response.status);
  return payload || {};
}

async function loadFootage() {
  try {
    const payload = await getJSON("api/footage");
    footage.cameras = payload.cameras || [];
    footage.error = null;
  } catch (err) {
    footage.cameras = [];
    footage.error = err.message || String(err);
    renderFootageCameras();
    renderFootageDates();
    renderFootageSegments();
    return;
  }

  renderFootageCameras();
  const held = footage.cameras.some((cam) => cam.name === footage.camera) ? footage.camera : null;
  const first = footage.cameras.find((cam) => cam.segments > 0) || footage.cameras[0];
  const pick = held || (first ? first.name : null);
  if (pick) {
    await selectFootageCamera(pick, pick !== footage.camera);
    return;
  }
  footage.dates = [];
  footage.segments = [];
  footage.gaps = [];
  renderFootageDates();
  renderFootageSegments();
  renderFootagePlayer();
}

async function selectFootageCamera(name, changed) {
  footage.camera = name;
  if (changed) {
    footage.date = null;
    footage.playing = null;
    renderFootagePlayer();
  }
  renderFootageCameras();
  try {
    const payload = await getJSON("api/footage?camera=" + encodeURIComponent(name));
    footage.dates = payload.dates || [];
    footage.error = null;
  } catch (err) {
    footage.dates = [];
    footage.error = err.message || String(err);
  }
  renderFootageDates();

  const held = footage.dates.some((day) => day.date === footage.date) ? footage.date : null;
  const pick = held || (footage.dates[0] ? footage.dates[0].date : null);
  if (pick) {
    await selectFootageDate(pick);
    return;
  }
  footage.segments = [];
  footage.gaps = [];
  renderFootageSegments();
}

async function selectFootageDate(date) {
  if (date !== footage.date) {
    footage.playing = null;
    renderFootagePlayer();
  }
  footage.date = date;
  renderFootageDates();
  try {
    const payload = await getJSON(
      "api/footage?camera=" + encodeURIComponent(footage.camera) + "&date=" + encodeURIComponent(date));
    footage.segments = payload.segments || [];
    footage.gaps = payload.gaps || [];
    footage.seconds = payload.seconds || 0;
    footage.bytes = payload.bytes || 0;
    footage.error = null;
  } catch (err) {
    footage.segments = [];
    footage.gaps = [];
    footage.error = err.message || String(err);
  }
  renderFootageSegments();
  // The segment being watched may have grown, or been pruned away.
  if (footage.playing && !footage.segments.some((s) => s.segment === footage.playing)) {
    footage.playing = null;
    renderFootagePlayer();
  }
}

function playSegment(name) {
  footage.playing = name;
  renderFootagePlayer();
  renderFootageSegments();
}

function renderFootageCameras() {
  const host = el("fCam");
  const head = '<div class="phead">Camera</div>';
  if (footage.cameras === null) {
    host.innerHTML = head + '<div class="pnone">Loading...</div>';
    return;
  }
  if (!footage.cameras.length) {
    host.innerHTML = head + '<div class="pnone">No footage on this machine yet.</div>';
    return;
  }
  host.innerHTML = head + footage.cameras.map((cam) =>
    '<button data-camera="' + esc(cam.name) + '" aria-current="' + (cam.name === footage.camera) + '">' +
    '<span class="cnt">' + cam.segments + "</span>" + esc(cam.name) +
    (cam.known ? "" : '<span class="orphan">no longer in the camera list</span>') +
    "</button>").join("");
}

function renderFootageDates() {
  const host = el("fDay");
  const head = '<div class="phead">Date</div>';
  if (!footage.camera) {
    host.innerHTML = head + '<div class="pnone">Pick a camera.</div>';
    return;
  }
  if (footage.dates === null) {
    host.innerHTML = head + '<div class="pnone">Loading...</div>';
    return;
  }
  if (!footage.dates.length) {
    host.innerHTML = head + '<div class="pnone">No footage for this camera.</div>';
    return;
  }
  host.innerHTML = head + footage.dates.map((day) =>
    '<button data-date="' + esc(day.date) + '" aria-current="' + (day.date === footage.date) + '">' +
    '<span class="cnt">' + day.segments + "</span>" + esc(dayLabel(day.date)) +
    "</button>").join("");
}

function renderFootageSegments() {
  const count = el("fCount");
  const gaps = el("fGaps");
  const list = el("fSegs");

  if (footage.error) {
    count.textContent = "";
    gaps.innerHTML = "";
    list.innerHTML = '<li class="noseg">' + esc(footage.error) + "</li>";
    return;
  }
  if (footage.segments === null) {
    count.textContent = "";
    gaps.innerHTML = "";
    list.innerHTML = '<li class="noseg">Loading...</li>';
    return;
  }
  if (!footage.segments.length) {
    count.textContent = "";
    gaps.innerHTML = "";
    // "on this date" only when there IS one: a camera with nothing at all has
    // no date selected, and saying "on this date" about no date reads as a bug.
    list.innerHTML = '<li class="noseg">' + (!footage.camera ? "Pick a camera."
      : footage.date ? "No footage for " + esc(footage.camera) + " on this date."
      : "No footage for " + esc(footage.camera) + " yet.") + "</li>";
    return;
  }

  count.textContent = footage.segments.length + (footage.segments.length === 1 ? " segment" : " segments") +
    " · " + spell(footage.seconds) + " recorded · " + size(footage.bytes);

  const shown = footage.gaps.slice(0, GAPS_SHOWN).map((gap) =>
    '<div class="gap"><span class="chip warn"><i class="dot"></i></span>' + esc(gapText(gap)) + "</div>").join("");
  const rest = footage.gaps.length - GAPS_SHOWN;
  gaps.innerHTML = shown + (rest > 0
    ? '<div class="gap more">' + rest + (rest === 1 ? " more gap" : " more gaps") + " earlier in the day</div>"
    : "");

  list.innerHTML = footage.segments.map((segment) =>
    "<li><button data-segment=\"" + esc(segment.segment) + '" aria-current="' + (segment.segment === footage.playing) + '">' +
    "<time>" + esc(segment.start) + "</time>" +
    '<span class="t">' + clock(segment.seconds) + " · " + size(segment.bytes) +
    (segment.inProgress ? " " + chip("warn", "recording now") : "") +
    "</span></button></li>").join("");
}

function renderFootagePlayer() {
  const host = el("fPlayer");
  const segment = (footage.segments || []).find((s) => s.segment === footage.playing);

  if (!segment) {
    host.classList.remove("live");
    host.innerHTML = '<div><div class="pbig">&#9205;</div>' +
      '<div class="pnow">' + (footage.camera ? esc(footage.camera) : "Nothing selected") + "</div>" +
      '<div class="psub">Pick a segment below to play it. Footage is remuxed to MP4 when you ask for ' +
      "it - the video is copied and only the camera's own audio is re-encoded - so nothing is " +
      "converted in advance and nothing is stored twice.</div></div>";
    return;
  }

  const source = "api/clip?camera=" + encodeURIComponent(footage.camera) +
    "&segment=" + encodeURIComponent(segment.segment);
  host.classList.add("live");
  host.innerHTML = '<video controls autoplay playsinline preload="auto" src="' + esc(source) + '"></video>';

  // A failed remux arrives here as a media error, because the 502 body is not
  // something a <video> can show. Say so in the page rather than leaving a
  // black rectangle.
  const video = host.querySelector("video");
  video.addEventListener("error", () => {
    host.classList.remove("live");
    host.innerHTML = '<div><div class="pbig">&#9888;</div>' +
      '<div class="pnow">' + esc(segment.start) + " could not be played</div>" +
      '<div class="psub">The browser could not decode what ffmpeg produced for this segment. ' +
      "The camui log records what ffmpeg reported.</div></div>";
  });
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
  const dismiss = event.target.closest("#stopNotice [data-ack]");
  if (dismiss && !dismiss.disabled) {
    acknowledgeNotice(dismiss);
    return;
  }

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

el("fRefresh").onclick = () => loadFootage();

el("fCam").onclick = (event) => {
  const button = event.target.closest("button[data-camera]");
  if (button) selectFootageCamera(button.dataset.camera, button.dataset.camera !== footage.camera);
};

el("fDay").onclick = (event) => {
  const button = event.target.closest("button[data-date]");
  if (button) selectFootageDate(button.dataset.date);
};

el("fSegs").onclick = (event) => {
  const button = event.target.closest("button[data-segment]");
  if (button) playSegment(button.dataset.segment);
};

// showTab is driven by the URL fragment, so a screen can be linked to and
// opened directly - #foot goes straight to the footage browser. An unknown
// fragment falls back to the first tab.
async function detectViewOnly() {
  try {
    viewOnly = (await getJSON("api/tls/info")).readOnly === true;
  } catch (err) {
    viewOnly = false;
  }
  document.body.classList.toggle("viewonly", viewOnly);
  el("viewOnly").classList.toggle("hide", !viewOnly);
  // inert: not focusable, not clickable, hidden from assistive technology as editable.
  for (const panel of document.querySelectorAll('section[data-tab="set"] > .panel:not(#tlsPanel)')) panel.inert = viewOnly;
}

// The "Connect a phone" panel exists only on the HTTPS LAN listener: api/tls/info is
// answered there and is a 404 on the loopback listener, which keeps the panel hidden.
async function loadTlsPanel() {
  try {
    const info = await getJSON("api/tls/info");
    el("tlsFingerprint").textContent = info.caFingerprintSha256;
    el("tlsPanel").classList.remove("hide");
  } catch (err) {
    el("tlsPanel").classList.add("hide");
  }
}

function showTab(fragment) {
  // "live/<camera>" is the Live screen focused on one camera.
  const slash = fragment.indexOf("/");
  const name = slash < 0 ? fragment : fragment.slice(0, slash);
  let arg;
  try { arg = slash < 0 ? undefined : decodeURIComponent(fragment.slice(slash + 1)) || undefined; } catch (err) { arg = undefined; }
  const tabs = [...el("tabs").children];
  const button = tabs.find((tab) => tab.dataset.t === name) || tabs[0];
  for (const tab of tabs) tab.removeAttribute("aria-current");
  button.setAttribute("aria-current", "page");
  for (const section of document.querySelectorAll("section[data-tab]")) {
    section.classList.toggle("hide", section.dataset.tab !== button.dataset.t);
  }
  if (button.dataset.t === "set" && !camerasDirty()) {
    loadCameras();
    loadConfig();
  }
  if (button.dataset.t === "set") loadTlsPanel();
  // Re-read on every visit: a recording camera has new segments by now, and
  // the pruner may have taken the oldest away.
  if (button.dataset.t === "foot") loadFootage();
  Live.onTab(button.dataset.t, arg);
}

el("tabs").onclick = (event) => {
  const button = event.target.closest("button");
  if (!button) return;
  history.replaceState(null, "", "#" + button.dataset.t);
  showTab(button.dataset.t);
};

window.addEventListener("hashchange", () => showTab(location.hash.slice(1)));
showTab(location.hash.slice(1) || "dash");

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

detectViewOnly();

// "Sign out" exists only where there is a login: api/auth/me says loginRequired on
// the HTTPS LAN listener and not on loopback.
async function setupSignOut() {
  try {
    const me = await getJSON("api/auth/me");
    if (!me.loginRequired || !me.authenticated) return;
    el("who").textContent = me.username;
    el("who").classList.remove("hide");
    el("signOut").classList.remove("hide");
  } catch (err) { /* not signed in or not reachable: leave it hidden */ }
}
el("signOut").onclick = async () => {
  try {
    await fetch("api/auth/logout", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" });
  } finally {
    location.assign("/login");
  }
};
setupSignOut();
poll();
setInterval(poll, POLL_MS);
document.addEventListener("visibilitychange", () => {
  if (!document.hidden) poll();
});
