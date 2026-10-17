// The first-run page (this PC only, while no user exists). External script for the same CSP reason as login.js.
(() => {
  "use strict";

  const form = document.getElementById("form");
  const msg = document.getElementById("msg");
  const go = document.getElementById("go");

  function show(text, good) {
    msg.textContent = text;
    msg.hidden = !text;
    msg.className = "formnote " + (good ? "good" : "bad");
  }

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    show("");
    if (form.password.value !== form.again.value) {
      show("The two passwords differ.");
      return;
    }
    go.disabled = true;
    try {
      const response = await fetch("/api/auth/setup", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username: form.username.value, password: form.password.value }),
      });
      let body = {};
      try { body = await response.json(); } catch (err) { /* no body */ }
      form.password.value = "";
      form.again.value = "";
      if (response.status === 201) {
        form.hidden = true;
        show("User " + body.username + " created. Next: add \"tlsListen\" to camui.config.json (or start camui with -tls-listen), restart camui, and sign in from your phone or PC.", true);
        form.after(msg);
        msg.hidden = false;
        return;
      }
      go.disabled = false;
      show(response.status === 404 ? "A user already exists." : (body.error || "Could not create the user (HTTP " + response.status + ")."));
    } catch (err) {
      go.disabled = false;
      show("Cannot reach the server.");
    }
  });
})();
