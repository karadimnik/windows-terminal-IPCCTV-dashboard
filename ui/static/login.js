// The sign-in page. External on purpose: the page ships a strict
// Content-Security-Policy that allows no inline script.
(() => {
  "use strict";

  const form = document.getElementById("form");
  const msg = document.getElementById("msg");
  const go = document.getElementById("go");
  const params = new URLSearchParams(location.search);
  const next = params.get("next") || "/";

  let countdown = null;

  function show(text) {
    msg.textContent = text;
    msg.hidden = !text;
  }

  // The server decides where "next" may lead (same-origin path only); this page
  // never navigates to a value it has not been handed back.
  function leave(target) {
    target = target || "/";
    // A deep link such as /#live survives the detour through this page.
    if (!target.includes("#")) target += location.hash;
    location.replace(target);
  }

  // A link opened from another site arrives without the SameSite=Strict cookie
  // although the browser is signed in; the page's own request carries it.
  fetch("/api/auth/me?next=" + encodeURIComponent(next), { cache: "no-store" })
    .then((r) => r.json())
    .then((me) => { if (me.authenticated) leave(me.next); })
    .catch(() => { /* stay on the form */ });

  function lockFor(seconds, text) {
    clearInterval(countdown);
    let left = seconds;
    go.disabled = true;
    const tick = () => {
      if (left <= 0) {
        clearInterval(countdown);
        go.disabled = false;
        show("");
        return;
      }
      show(text.replace(/\d+ seconds?/, left + (left === 1 ? " second" : " seconds")));
      left -= 1;
    };
    tick();
    countdown = setInterval(tick, 1000);
  }

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (go.disabled) return;
    show("");
    go.disabled = true;
    try {
      const response = await fetch("/api/auth/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          username: form.username.value,
          password: form.password.value,
          next,
        }),
      });
      let body = {};
      try { body = await response.json(); } catch (err) { /* no body */ }
      if (response.ok) {
        leave(body.next);
        return;
      }
      form.password.value = "";
      if (response.status === 429) {
        lockFor(body.retryAfter || 30, body.error || "Too many attempts. Try again in 30 seconds.");
        return;
      }
      go.disabled = false;
      show(response.status === 401 ? (body.error || "Invalid username or password") : "Could not sign in (HTTP " + response.status + ").");
      form.password.focus();
    } catch (err) {
      go.disabled = false;
      show("Cannot reach the server.");
    }
  });
})();
