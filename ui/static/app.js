"use strict";

// The dashboard polls GET /api/status and renders two layouts at once: the
// table (layout A) and the compact rows (layout C). CSS decides which is
// visible at the phone breakpoint, so there is no resize handling here.

const POLL_MS = 5000;

// STALE_SECONDS: status.json is rewritten on every supervisor tick, so a file
// older than this means the supervisor is not reporting even if the last thing
// it wrote said "running".
const STALE_SECONDS = 120;

const el = (id) => document.getElementById(id);
const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));

const num = (value, places = 1) => (typeof value === "number" && isFinite(value) ? value.toFixed(places) : "-");

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
  return Math.round(seconds / 3600) + " h";
}

// eventTime pulls HH:MM:SS out of the recorder's "2026-10-12 19:06:17 +03:00".
function eventTime(at) {
  const match = /(\d{2}:\d{2}:\d{2})/.exec(String(at || ""));
  return match ? match[1] : String(at || "");
}

function chip(cls, text) {
  return '<span class="chip ' + cls + '"><i class="dot"></i>' + esc(text) + "</span>";
}

function cameraChip(cam, live) {
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

function render(payload, fetchError) {
  const status = payload && payload.status;
  const file = (payload && payload.statusFile) || {};
  const cams = (status && Array.isArray(status.Cameras) && status.Cameras) || [];
  const disk = (status && status.Disk) || {};
  const stale = !file.present || file.ageSeconds > STALE_SECONDS;
  const live = !!status && status.State === "running" && !stale;

  // ---- feed indicator ----
  if (fetchError) {
    el("feed").innerHTML = chip("crit", "Server unreachable");
    el("feedWhen").textContent = esc(fetchError);
  } else if (!file.present) {
    el("feed").innerHTML = chip("crit", "No status file");
    el("feedWhen").textContent = file.error || "";
  } else if (stale) {
    el("feed").innerHTML = chip("warn", "Status " + shortAge(file.ageSeconds) + " old");
    el("feedWhen").textContent = "";
  } else {
    el("feed").innerHTML = chip("good", "Live");
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
    banner =
      '<div class="banner crit">' + chip("crit", "") +
      "<div><b>No status to show.</b> " + esc(file.error || "The recorder has not written a status file.") + "</div></div>";
  } else if (stale) {
    banner =
      '<div class="banner crit">' + chip("crit", "") +
      "<div><b>The recorder is not reporting.</b> Its status file was last written " +
      shortAge(file.ageSeconds) + " ago, so what it says cannot be trusted.</div></div>";
  } else if (!live) {
    banner =
      '<div class="banner crit">' + chip("crit", "") +
      "<div><b>Nothing is recording.</b> The supervisor is stopped, so no footage is being captured.</div></div>";
  } else if (atReserve) {
    banner =
      '<div class="banner warn">' + chip("warn", "") +
      "<div><b>Disk is at its reserve.</b> The pruner is deleting the oldest footage to keep " +
      num(reserve, 0) + " GB free.</div></div>";
  } else if (unreachable.length) {
    banner =
      '<div class="banner warn">' + chip("warn", "") +
      "<div><b>" + unreachable.length + (unreachable.length === 1 ? " camera is" : " cameras are") +
      " not reachable.</b> " + esc(unreachable.map((c) => c.Name).join(", ")) +
      ". They keep retrying; the others are unaffected.</div></div>";
  }
  el("banner").innerHTML = banner;

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
  el("tbody").innerHTML = cams.map((cam) => {
    const detail = cam.LastError && live
      ? '<div class="err" title="' + esc(cam.LastError) + '">' + esc(cam.LastError) + "</div>"
      : '<div class="cam-meta">' + (cam.ArchiveFiles || 0) + " segments &middot; " + num(gb(cam.ArchiveBytes)) + " GB</div>";
    return "<tr>" +
      "<td><div class=\"cam-name\">" + esc(cam.Name) + "</div>" + detail + "</td>" +
      "<td>" + cameraChip(cam, live) + "</td>" +
      "<td>" + inputChip(cam, live) + "</td>" +
      '<td class="num">' + (live ? age(cam.SecondsSinceLastClosedSegment) : "&mdash;") + "</td>" +
      '<td class="num">' + (cam.RestartsLastHour ? chip("ser", String(cam.RestartsLastHour)) : "0") + "</td>" +
      "<td>" + toggle(cam) + "</td></tr>";
  }).join("");

  // ---- layout C: compact rows ----
  el("camsC").innerHTML = cams.map((cam) => {
    const sub = !live
      ? "Stopped"
      : cam.LastError
        ? esc(cam.LastError)
        : esc(cam.InputMode) + " &middot; " + age(cam.SecondsSinceLastClosedSegment) + " &middot; " + num(gb(cam.ArchiveBytes)) + " GB";
    return '<div class="crow"><div class="main"><div class="nm">' + esc(cam.Name) + "</div>" +
      '<div class="sb">' + sub + "</div></div>" +
      cameraChip(cam, live) +
      '<button class="tgl" aria-checked="' + !!cam.Record + '" aria-label="Record ' + esc(cam.Name) + '" disabled><i></i></button></div>';
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
}

function toggle(cam) {
  return '<div class="tglwrap"><button class="tgl" aria-checked="' + !!cam.Record +
    '" aria-label="Record ' + esc(cam.Name) + '" disabled><i></i></button><span>' +
    (cam.Record ? "On" : "Off") + "</span></div>";
}

const gb = (bytes) => (typeof bytes === "number" ? bytes / 1073741824 : null);

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
