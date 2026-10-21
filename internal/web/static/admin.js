"use strict";

// The Admin tab. This file and its markup are served to admins only (GET /admin.js is an
// admin route, access.go), and every call below goes to /api/admin/*, which the server refuses
// to anyone else whatever this page does. It uses app.js's helpers: el, esc, getJSON, sendJSON,
// toast, confirmAction.
//
// Rules kept here: a password or a camera login is typed into a password field, sent once in a
// POST body and cleared from the form; nothing secret is ever read back. Every destructive action
// asks first. What stays on the command line says so where it would be.

(function () {
  const TEMPLATE = `
  <div class="panel apanel" id="aUsersPanel">
    <header><h2>Users</h2><span class="muted" id="aUsersCount"></span></header>
    <div class="ascroll"><table class="ausers">
      <thead><tr><th>Name</th><th>Role</th><th>Created</th><th>Locked out</th><th></th></tr></thead>
      <tbody id="aUsers"></tbody>
    </table></div>
    <form class="form aform" id="aAddUser" autocomplete="off">
      <h3>Add a user</h3>
      <div class="field"><label for="aNewName">Name</label><input id="aNewName" autocomplete="off" spellcheck="false" required></div>
      <div class="field"><label for="aNewPass">Password</label><input id="aNewPass" type="password" autocomplete="new-password" required>
        <span class="hint">12 to 128 characters, not the name.</span></div>
      <div class="field"><label for="aNewRole">Role</label><select id="aNewRole"><option value="viewer">viewer: watches live and footage</option><option value="admin">admin: manages everything</option></select></div>
      <div class="actions"><button class="btn primary" type="submit">Add user</button><span class="formnote" id="aAddNote"></span></div>
    </form>
    <form class="form aform" id="aSetPass" autocomplete="off">
      <h3>Set a password</h3>
      <div class="field"><label for="aPassUser">User</label><select id="aPassUser"></select></div>
      <div class="field"><label for="aPassNew">New password</label><input id="aPassNew" type="password" autocomplete="new-password" required>
        <span class="hint">Ends that user's other sessions; yours stays signed in.</span></div>
      <div class="actions"><button class="btn" type="submit">Set password</button><span class="formnote" id="aPassNote"></span></div>
    </form>
    <p class="hint apad">There is always at least one admin: the last one can be neither removed nor made a viewer. A role change ends that user's other sessions and applies at once.</p>
  </div>

  <div class="panel apanel">
    <header><h2>Camera login</h2><span class="muted" id="aCamStore"></span></header>
    <div class="form">
      <p id="aCamState" class="hint"></p>
      <form id="aCamForm" class="aform" autocomplete="off">
        <div class="field"><label for="aCamUser">Camera user name</label><input id="aCamUser" autocomplete="off" spellcheck="false">
          <span class="hint">Leave empty to keep the stored one.</span></div>
        <div class="field"><label for="aCamPass">Camera password</label><input id="aCamPass" type="password" autocomplete="new-password">
          <span class="hint">Leave empty to keep the stored one. It is never shown again, here or anywhere.</span></div>
        <div class="actions"><button class="btn primary" type="submit">Store the camera login</button><span class="formnote" id="aCamNote"></span></div>
      </form>
      <p class="hint">Importing the old <code>camera_creds.xml</code> stays on the command line (<code>holocron secret import</code>): this page never names files on the PC.</p>
    </div>
  </div>

  <div class="panel apanel">
    <header><h2>Settings keys</h2><span class="muted" id="aKeysPath"></span></header>
    <p class="hint apad">The keys <code>holocron config set</code> may change. A value is read as the command line reads it: JSON when it is valid JSON (<code>true</code>, <code>30</code>, <code>null</code>, <code>["a"]</code>), text otherwise. Every change is checked first; nothing is written when it would not be valid.</p>
    <div class="ascroll"><table class="akeys">
      <thead><tr><th>Key</th><th>Value</th><th>Default</th><th>Rule</th><th></th></tr></thead>
      <tbody id="aKeys"></tbody>
    </table></div>
  </div>

  <div class="panel apanel">
    <header><h2>Certificate authority (HTTPS on the LAN)</h2></header>
    <div class="form">
      <p id="aTlsState" class="hint"></p>
      <p><code id="aTlsFp" class="abreak"></code></p>
      <div class="actions"><a class="btn" id="aTlsDownload" href="api/admin/tls/ca.crt" download="ipcctv-local-ca.crt">Download the CA certificate</a></div>
      <p class="hint">The file is the CA certificate only; its key never leaves the PC. Install it once on each device and check the fingerprint above first.
        iPhone: open the file, install it under Settings &gt; General &gt; VPN &amp; Device Management, then turn it on under Settings &gt; General &gt; About &gt; Certificate Trust Settings.
        Android: Settings &gt; Security &gt; Install a certificate &gt; CA certificate. Windows: double-click &gt; Install &gt; Trusted Root Certification Authorities.</p>
      <form id="aRotate" class="aform" autocomplete="off">
        <div class="field"><label for="aRotateWord">Rotate: type ROTATE to confirm</label><input id="aRotateWord" autocomplete="off" spellcheck="false">
          <span class="hint">Deletes the CA and the server certificate; a new pair is made at the next start of the web app, and EVERY device must install the new CA.</span></div>
        <div class="actions"><button class="btn danger" type="submit">Rotate the CA</button><span class="formnote" id="aRotateNote"></span></div>
      </form>
    </div>
  </div>

  <div class="panel apanel">
    <header><h2>Maintenance</h2></header>
    <div class="form">
      <div class="actions"><button class="btn" id="aPrune" type="button">Preview the pruner</button><span class="formnote" id="aPruneNote"></span></div>
      <div id="aPruneOut"></div>
      <p class="hint">A preview deletes nothing. Running the pruner by hand stays on the command line (<code>holocron prune --now --yes</code>, only while no recorder runs); the running recorder prunes on its own.</p>
      <h3>Archive</h3>
      <div class="ascroll"><table>
        <thead><tr><th>Folder</th><th>Camera</th><th class="num">Segments</th><th class="num">GB</th><th>Oldest</th><th>Newest</th></tr></thead>
        <tbody id="aArchive"></tbody>
      </table></div>
      <p class="hint" id="aArchiveSum"></p>
      <h3>Log levels</h3>
      <p class="hint" id="aLevels"></p>
      <form id="aLevelForm" class="aform" autocomplete="off">
        <div class="field"><label for="aLevelComp">Component</label><input id="aLevelComp" list="aComps" value="all" spellcheck="false">
          <datalist id="aComps"><option value="all"><option value="supervisor"><option value="ffmpeg"><option value="relay"><option value="live"><option value="prune"><option value="alerts"><option value="ctlapi"><option value="host"><option value="web"><option value="auth"><option value="cli"></datalist></div>
        <div class="field"><label for="aLevelValue">Level</label><select id="aLevelValue"><option>error</option><option>warn</option><option selected>info</option><option>debug</option><option>trace</option></select></div>
        <label class="acheck"><input type="checkbox" id="aLevelSave"> also save it in the configuration</label>
        <div class="actions"><button class="btn" type="submit">Change the level</button><span class="formnote" id="aLevelNote"></span></div>
      </form>
    </div>
  </div>

  <div class="panel apanel">
    <header><h2>Autostart</h2></header>
    <div class="form">
      <p id="aAuto" class="hint"></p>
      <ul class="events" id="aAutoOld"></ul>
      <label class="acheck hide" id="aAutoOkWrap"><input type="checkbox" id="aAutoOk"> I know a PowerShell system task is enabled; switch holocron's autostart on anyway</label>
      <div class="actions">
        <button class="btn" id="aAutoOn" type="button">Enable</button>
        <button class="btn danger" id="aAutoOff" type="button">Disable</button>
        <span class="formnote" id="aAutoNote"></span>
      </div>
      <p class="hint">Creating or removing the autostart entry stays on the command line (<code>holocron install</code>, <code>holocron uninstall</code>): they create operating-system entries, which a browser should not.</p>
    </div>
  </div>`;

  let built = false;
  let users = { users: [], self: "" };

  const text = (v) => (v === undefined ? "" : typeof v === "string" ? v : JSON.stringify(v));
  const day = (iso) => (iso ? String(iso).slice(0, 10) : "-");

  function say(id, cls, message) {
    const target = el(id);
    target.className = "formnote" + (cls ? " " + cls : "");
    target.textContent = message || "";
  }

  // post sends one change and reports it in the note; it returns the payload when it worked.
  async function post(path, body, noteId) {
    say(noteId, "", "Working…");
    try {
      const { ok, payload } = await sendJSON("POST", path, body);
      if (!ok) {
        say(noteId, "bad", payload.error || "refused");
        return null;
      }
      say(noteId, "good", payload.message || "Done.");
      return payload;
    } catch (err) {
      say(noteId, "bad", "The server could not be reached: " + (err.message || err));
      return null;
    }
  }

  // ---------------------------------------------------------------- users
  async function loadUsers() {
    try {
      users = await getJSON("api/admin/users");
    } catch (err) {
      el("aUsers").innerHTML = '<tr><td colspan="5">' + esc(err.message || err) + "</td></tr>";
      return;
    }
    el("aUsersCount").textContent = users.users.length + " user" + (users.users.length === 1 ? "" : "s");
    el("aUsers").innerHTML = users.users.map((u) => {
      const other = u.role === "admin" ? "viewer" : "admin";
      const me = u.username === users.self ? ' <span class="muted">(you)</span>' : "";
      const lock = users.lockoutsKnown ? (u.locked ? "locked" : "-") : "unknown here";
      return "<tr><td>" + esc(u.username) + me + '</td><td data-label="Role">' + esc(u.role) + '</td><td data-label="Created">' + esc(day(u.createdAt)) + '</td><td data-label="Locked out">' + esc(lock) + "</td>" +
        '<td class="abtns"><button class="btn small" data-u="' + esc(u.username) + '" data-act="role" data-to="' + other + '">Make ' + other + "</button>" +
        '<button class="btn small" data-u="' + esc(u.username) + '" data-act="unlock">Unlock</button>' +
        '<button class="btn small danger" data-u="' + esc(u.username) + '" data-act="remove">Remove</button></td></tr>';
    }).join("");
    el("aPassUser").innerHTML = users.users.map((u) => '<option value="' + esc(u.username) + '">' + esc(u.username) + "</option>").join("");
  }

  async function userAction(button) {
    const name = button.dataset.u;
    const act = button.dataset.act;
    if (act === "remove") {
      const go = await confirmAction({ title: "Remove " + name + "?", body: "The account and every session of it end now. Its recordings are not affected.", go: "Remove" });
      if (!go) return;
      if (await post("api/admin/users/remove", { username: name }, "aAddNote")) loadUsers();
    } else if (act === "role") {
      const to = button.dataset.to;
      if (to === "viewer") {
        const self = name === users.self;
        const go = await confirmAction({ title: "Make " + name + " a viewer?", body: self ? "You lose the Admin tab at once." : name + " can then only watch; other sessions of " + name + " end.", go: "Make viewer" });
        if (!go) return;
      }
      if (await post("api/admin/users/role", { username: name, role: to }, "aAddNote")) {
        if (name === users.self && to === "viewer") location.reload();
        else loadUsers();
      }
    } else if (act === "unlock") {
      if (await post("api/admin/users/unlock", { username: name }, "aAddNote")) loadUsers();
    }
  }

  // ---------------------------------------------------------------- camera login
  async function loadCamera() {
    try {
      const st = (await getJSON("api/admin/camera-login")).cameraLogin;
      const word = (b) => (b ? "present" : "not set");
      el("aCamStore").textContent = st.backend ? st.backend + " store" : "";
      let line = "User name: " + word(st.user) + ". Password: " + word(st.password) + ".";
      if (st.fromEnvironment) line += " HOLOCRON_CAMERA_USER and HOLOCRON_CAMERA_PASSWORD are set for this process and win over the store.";
      if (st.readOnly) line += " The env store is read only: nothing can be stored from here.";
      if (st.storeError) line += " The store cannot be opened: " + st.storeError;
      el("aCamState").textContent = line;
      el("aCamForm").dataset.present = st.user || st.password ? "1" : "";
    } catch (err) {
      el("aCamState").textContent = err.message || String(err);
    }
  }

  async function saveCamera(event) {
    event.preventDefault();
    const user = el("aCamUser").value;
    const password = el("aCamPass").value;
    if (!user && !password) { say("aCamNote", "bad", "Type the user name, the password, or both."); return; }
    if (el("aCamForm").dataset.present) {
      const go = await confirmAction({ title: "Replace the camera login?", body: "The stored value is overwritten; the old one cannot be shown or restored.", go: "Replace" });
      if (!go) return;
    }
    const done = await post("api/admin/camera-login", { user, password }, "aCamNote");
    el("aCamPass").value = "";
    if (done) { el("aCamUser").value = ""; loadCamera(); }
  }

  // ---------------------------------------------------------------- settings keys
  async function loadKeys() {
    let payload;
    try {
      payload = await getJSON("api/admin/config");
    } catch (err) {
      el("aKeys").innerHTML = '<tr><td colspan="5">' + esc(err.message || err) + "</td></tr>";
      return;
    }
    el("aKeysPath").textContent = payload.path || "";
    el("aKeys").innerHTML = payload.keys.map((k, i) => {
      const id = "aK" + i;
      const keyCell = k.pattern
        ? "<code>logging.levels.&lt;component&gt;</code>" + '<input id="' + id + 'c" class="akeyin" placeholder="component" aria-label="Component for logging.levels" spellcheck="false">'
        : "<code>" + esc(k.key) + "</code>";
      const notes = (k.restart ? '<span class="hint">applies at the next start</span>' : '<span class="hint">applied at once when running</span>') +
        (k.warning ? '<span class="hint awarn">' + esc(k.warning) + "</span>" : "");
      return '<tr data-key="' + esc(k.key) + '" data-i="' + i + '"' + (k.warning ? ' data-warn="' + esc(k.warning) + '"' : "") + ">" +
        "<td>" + keyCell + "</td>" +
        '<td><input id="' + id + '" class="akeyin" aria-label="Value of ' + esc(k.key) + '" spellcheck="false" value="' + esc(k.pattern ? "" : text(k.value)) + '">' +
        '<span class="formnote" id="' + id + 'n"></span></td>' +
        '<td data-label="Default"><code>' + esc(text(k.default)) + '</code></td><td data-label="Rule">' + esc(k.rule) + notes + "</td>" +
        '<td class="abtns"><button class="btn small" data-kact="set">Save</button>' +
        (k.set ? '<button class="btn small" data-kact="unset">Default</button>' : "") + "</td></tr>";
    }).join("");
  }

  async function keyAction(button) {
    const row = button.closest("tr");
    const i = row.dataset.i;
    let key = row.dataset.key;
    if (key.endsWith(".*")) {
      const comp = el("aK" + i + "c").value.trim();
      if (!comp) { say("aK" + i + "n", "bad", "Name the component."); return; }
      key = key.slice(0, -1) + comp;
    }
    if (row.dataset.warn) {
      const go = await confirmAction({ title: "Change " + key + "?", body: row.dataset.warn, go: "Change it" });
      if (!go) return;
    }
    const set = button.dataset.kact === "set";
    const body = set ? { key, value: el("aK" + i).value } : { key };
    say("aK" + i + "n", "", "Saving…");
    const { ok, payload } = await sendJSON("POST", set ? "api/admin/config/set" : "api/admin/config/unset", body);
    if (!ok) {
      const lines = (payload.invalid || []).map((p) => (p.field ? p.field + ": " : "") + p.message);
      say("aK" + i + "n", "bad", lines.length ? lines.join("; ") : payload.error || "refused");
      return;
    }
    await loadKeys();
    toast("good", payload.message || "Saved.");
  }

  // ---------------------------------------------------------------- TLS
  async function loadTls() {
    try {
      const t = await getJSON("api/admin/tls");
      el("aTlsDownload").classList.toggle("hide", !t.present);
      el("aTlsFp").parentElement.classList.toggle("hide", !t.present);
      if (!t.present) {
        el("aTlsState").textContent = "There is no local CA yet: it is made when the web app starts with web.tlsListen set (or by holocron tls ca).";
        el("aTlsFp").textContent = "";
        return;
      }
      el("aTlsState").textContent = t.name + ", valid until " + t.notAfter + ". SHA-256 fingerprint:" +
        (t.lanListener ? "" : " (no LAN listener is configured: web.tlsListen is empty)");
      el("aTlsFp").textContent = t.sha256;
    } catch (err) {
      el("aTlsState").textContent = err.message || String(err);
    }
  }

  async function rotate(event) {
    event.preventDefault();
    if (el("aRotateWord").value !== "ROTATE") { say("aRotateNote", "bad", "Type ROTATE (capitals) to confirm."); return; }
    const go = await confirmAction({ title: "Rotate the certificate authority?", body: "Every phone and PC that trusts it must install the new CA after the next start of the web app.", go: "Rotate" });
    if (!go) return;
    await post("api/admin/tls/rotate", { confirm: el("aRotateWord").value }, "aRotateNote");
    el("aRotateWord").value = "";
    loadTls();
  }

  // ---------------------------------------------------------------- maintenance
  async function prunePreview() {
    say("aPruneNote", "", "Reading the archive…");
    try {
      const p = (await getJSON("api/admin/prune")).prune;
      say("aPruneNote", "", "");
      const gbOf = (b) => (b / 1073741824).toFixed(2);
      let html = "";
      if (p.refused) html += '<p class="formnote bad">Refused at the ' + esc(p.gate) + " gate: " + esc(p.refused) + "</p>";
      html += "<p>Free " + esc(p.freeGB.toFixed(2)) + " GB, reserve " + esc(p.reserveGB.toFixed(2)) + " GB, target " + esc(p.targetGB.toFixed(2)) +
        " GB. Would delete " + p.deletions.length + " file(s), " + gbOf(p.freedBytes) + " GB; free after " + esc(p.freeGBAfter.toFixed(2)) + " GB.</p>";
      if (p.blocked) html += '<p class="formnote bad">Blocked: below the reserve with nothing deletable left.</p>';
      if (p.breakerHeld) html += '<p class="formnote bad">Age rule held by the circuit breaker: ' + esc(p.breakerHeld) + "</p>";
      if (p.deletions.length) {
        html += '<ul class="events">' + p.deletions.slice(0, 20).map((d) => "<li><span class=\"t\">" + esc(d.path) + " (" + gbOf(d.bytes) + " GB, " + esc(d.reason) + ")</span></li>").join("") +
          (p.deletions.length > 20 ? "<li><span class=\"t\">… and " + (p.deletions.length - 20) + " more</span></li>" : "") + "</ul>";
      }
      if (p.note) html += '<p class="hint">' + esc(p.note) + "</p>";
      html += '<p class="hint">' + esc(p.stopLine || "") + "</p>";
      el("aPruneOut").innerHTML = html;
    } catch (err) {
      say("aPruneNote", "bad", err.message || String(err));
    }
  }

  async function loadArchive() {
    try {
      const a = (await getJSON("api/admin/archive")).archive;
      el("aArchive").innerHTML = a.cameras.map((c) => "<tr><td>" + esc(c.folder) + "</td><td>" + esc(c.camera || "-") + '</td><td class="num">' + c.segments +
        '</td><td class="num">' + (c.bytes / 1073741824).toFixed(2) + "</td><td>" + esc(c.oldest ? c.oldest.replace("T", " ").slice(0, 19) : "-") + "</td><td>" +
        esc(c.newest ? c.newest.replace("T", " ").slice(0, 19) : "-") + "</td></tr>").join("") || '<tr><td colspan="6">No camera folders yet.</td></tr>';
      let sum = "Total " + a.segments + " segment(s), " + (a.bytes / 1073741824).toFixed(2) + " GB; free " + a.freeGB.toFixed(2) + " GB, reserve " + a.reserveGB.toFixed(2) + " GB.";
      if (a.measuredGBPerDay !== null && a.measuredGBPerDay !== undefined) sum += " Measured " + a.measuredGBPerDay.toFixed(2) + " GB a day";
      if (a.projectedDaysRemaining !== null && a.projectedDaysRemaining !== undefined) sum += ", about " + a.projectedDaysRemaining.toFixed(1) + " days until the reserve";
      if (a.note) sum += " " + a.note;
      el("aArchiveSum").textContent = sum;
    } catch (err) {
      el("aArchiveSum").textContent = err.message || String(err);
    }
  }

  async function loadLevels() {
    try {
      const l = (await getJSON("api/admin/log")).levels;
      const comps = (m) => Object.keys(m || {}).sort().map((k) => k + "=" + m[k]).join(", ");
      let line = "Configured: " + (l.configured || "info") + (comps(l.configuredComponents) ? " (" + comps(l.configuredComponents) + ")" : "") + ". ";
      line += l.running ? "Running supervisor: " + l.default + (comps(l.components) ? " (" + comps(l.components) + ")" : "") + "." : "No supervisor answers: a change can only be saved in the configuration.";
      el("aLevels").textContent = line;
    } catch (err) {
      el("aLevels").textContent = err.message || String(err);
    }
  }

  async function setLevel(event) {
    event.preventDefault();
    const body = { component: el("aLevelComp").value.trim(), level: el("aLevelValue").value, save: el("aLevelSave").checked };
    if (await post("api/admin/log/level", body, "aLevelNote")) { loadLevels(); loadKeys(); }
  }

  // ---------------------------------------------------------------- autostart
  async function loadAutostart() {
    try {
      const rep = (await getJSON("api/admin/autostart")).autostart;
      const s = rep.autostart;
      let line;
      if (!s.installed) {
        line = "Not installed (" + s.mode + " " + s.name + "): holocron install creates it.";
      } else {
        line = s.mode + " " + s.name + ": " + (s.enabled ? "enabled" : "disabled") + (s.window ? ", window " + s.window : "") +
          (s.running ? ", " + s.running : "") + ". Runs " + (s.exe || "?") + (s.exeExists ? "" : " - THAT FILE DOES NOT EXIST") + "." +
          (s.recorded ? "" : " Not recorded by holocron install.");
      }
      if (s.detail) line += " " + s.detail;
      el("aAuto").textContent = line;
      const olds = rep.oldTasks || [];
      el("aAutoOld").innerHTML = olds.map((o) => "<li><span class=\"t\">PowerShell system task " + esc(o.name) + ": " +
        esc(!o.found ? "not found" : o.detail ? "unreadable (" + o.detail + ")" : o.enabled ? "ENABLED - both systems would start at logon" : "disabled") + "</span></li>").join("");
      el("aAutoOn").disabled = !s.installed;
      el("aAutoOff").disabled = !s.installed;
    } catch (err) {
      el("aAuto").textContent = err.message || String(err);
    }
  }

  async function setAutostart(on) {
    if (!on) {
      const go = await confirmAction({ title: "Disable autostart?", body: "holocron will not start at the next logon or boot until it is enabled again. What runs now keeps running.", go: "Disable" });
      if (!go) return;
    }
    const done = await post("api/admin/autostart", { enabled: on, oldTasksOk: el("aAutoOk").checked }, "aAutoNote");
    if (!done && on && /PowerShell/.test(el("aAutoNote").textContent)) el("aAutoOkWrap").classList.remove("hide");
    loadAutostart();
  }

  // ---------------------------------------------------------------- wiring
  function build() {
    const section = document.querySelector('section[data-tab="admin"]');
    section.innerHTML = TEMPLATE;
    el("aUsers").onclick = (event) => { const b = event.target.closest("button[data-act]"); if (b) userAction(b); };
    el("aAddUser").onsubmit = async (event) => {
      event.preventDefault();
      const body = { username: el("aNewName").value.trim(), password: el("aNewPass").value, role: el("aNewRole").value };
      const done = await post("api/admin/users/add", body, "aAddNote");
      el("aNewPass").value = "";
      if (done) { el("aNewName").value = ""; loadUsers(); }
    };
    el("aSetPass").onsubmit = async (event) => {
      event.preventDefault();
      const body = { username: el("aPassUser").value, password: el("aPassNew").value };
      await post("api/admin/users/password", body, "aPassNote");
      el("aPassNew").value = "";
    };
    el("aCamForm").onsubmit = saveCamera;
    el("aKeys").onclick = (event) => { const b = event.target.closest("button[data-kact]"); if (b) keyAction(b); };
    el("aRotate").onsubmit = rotate;
    el("aPrune").onclick = prunePreview;
    el("aLevelForm").onsubmit = setLevel;
    el("aAutoOn").onclick = () => setAutostart(true);
    el("aAutoOff").onclick = () => setAutostart(false);
    built = true;
  }

  function show() {
    if (!built) build();
    loadUsers();
    loadCamera();
    loadKeys();
    loadTls();
    loadArchive();
    loadLevels();
    loadAutostart();
  }

  window.Admin = { show };
})();
