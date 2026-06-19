// enroll-pin.js — "Show enrollment PIN" affordance for owner-access pages.
//
// First-passkey enrollment over the relay needs the LAN-minted PIN
// (GET /api/owner-access/enroll-pin → {"pin":"123456","expires_in_s":600}).
// That endpoint is LAN-only: it returns 403 to any relay-tunnelled request
// (see go/internal/api/api_owner_access.go handleOwnerEnrollPin). Before this
// button the PIN was only reachable by curling the raw endpoint or reading the
// Pi's logs — a dead-end for non-experts. This module renders the 6 digits big
// (mono, tabular), a copy button, and a live countdown of expires_in_s.
//
// Each GET mints a FRESH PIN server-side, so "mint a new PIN" is just another
// fetch. On a relay page (apiBase() returns a /me/<id> prefix) we never even
// hit the endpoint — we show the LAN-only notice up front.

import { apiBase } from "./webauthn.js";

// True when the page was loaded through the relay tunnel. apiBase() returns a
// non-empty "/me/<site_id>" prefix in that case; on the LAN it is "".
function isRemote() {
  return apiBase() !== "";
}

function fmtCountdown(s) {
  if (s <= 0) return "0:00";
  const m = Math.floor(s / 60);
  const sec = s % 60;
  return m + ":" + String(sec).padStart(2, "0");
}

async function copyText(text) {
  if (typeof navigator !== "undefined" && navigator.clipboard && navigator.clipboard.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch (_) { /* fall through to the selection fallback */ }
  }
  if (typeof document === "undefined" || !document.createElement || !document.body) return false;
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.setAttribute("readonly", "");
  ta.style.position = "fixed";
  ta.style.top = "-1000px";
  ta.style.left = "-1000px";
  document.body.appendChild(ta);
  ta.focus();
  ta.select();
  let ok = false;
  try {
    ok = !!(document.execCommand && document.execCommand("copy"));
  } catch (_) {
    ok = false;
  }
  ta.remove();
  return ok;
}

function selectText(el) {
  if (!el || typeof window === "undefined" || typeof document === "undefined" || !window.getSelection || !document.createRange) return;
  const range = document.createRange();
  range.selectNodeContents(el);
  const sel = window.getSelection();
  sel.removeAllRanges();
  sel.addRange(range);
}

// mountEnrollPin wires a host element. It injects the toggle button + a hidden
// result region, and manages a single live countdown timer at a time.
//
//   host      — element to render into (button + result region appended).
//   label     — text for the toggle button (default "Show enrollment PIN").
export function mountEnrollPin(host, label) {
  if (!host) return;

  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "pin-show-btn";
  btn.textContent = label || "Show enrollment PIN";

  const out = document.createElement("div");
  out.className = "pin-result";
  out.hidden = true;

  host.appendChild(btn);
  host.appendChild(out);

  let timer = null;
  function stopTimer() {
    if (timer) { clearInterval(timer); timer = null; }
  }

  function renderRemote() {
    stopTimer();
    out.hidden = false;
    out.innerHTML =
      '<p class="pin-note">The enrollment PIN is available on your local network only. ' +
      'Open this page from a device on the same Wi‑Fi as your Pi to read it.</p>';
  }

  function renderError(msg) {
    stopTimer();
    out.hidden = false;
    out.innerHTML = '<p class="pin-err"></p>';
    out.querySelector(".pin-err").textContent = msg;
  }

  function renderPin(pin, expiresInS) {
    stopTimer();
    out.hidden = false;
    out.innerHTML =
      '<div class="pin-digits" aria-label="enrollment PIN"></div>' +
      '<div class="pin-meta">' +
        '<button type="button" class="pin-copy-btn">Copy</button>' +
        '<span class="pin-countdown" aria-live="polite"></span>' +
      '</div>';

    out.querySelector(".pin-digits").textContent = pin;

    const copyBtn = out.querySelector(".pin-copy-btn");
    copyBtn.onclick = async () => {
      if (await copyText(pin)) {
        copyBtn.textContent = "Copied";
        setTimeout(() => { copyBtn.textContent = "Copy"; }, 1500);
      } else {
        selectText(out.querySelector(".pin-digits"));
        copyBtn.textContent = "Select PIN";
        setTimeout(() => { copyBtn.textContent = "Copy"; }, 2000);
      }
    };

    const cd = out.querySelector(".pin-countdown");
    let remaining = Math.max(0, Math.floor(expiresInS));
    function tick() {
      if (remaining <= 0) {
        stopTimer();
        cd.textContent = "";
        // Replace the whole result with an expired prompt that re-mints.
        out.innerHTML = '<button type="button" class="pin-show-btn pin-expired">Expired — tap to mint a new PIN</button>';
        out.querySelector(".pin-expired").onclick = fetchPin;
        return;
      }
      cd.textContent = "expires in " + fmtCountdown(remaining);
      remaining -= 1;
    }
    tick();
    timer = setInterval(tick, 1000);
  }

  async function fetchPin() {
    if (isRemote()) { renderRemote(); return; }
    out.hidden = false;
    out.innerHTML = '<p class="pin-note">Minting…</p>';
    try {
      const r = await fetch(apiBase() + "/api/owner-access/enroll-pin", {
        credentials: "same-origin",
      });
      if (r.status === 403) { renderRemote(); return; }
      if (!r.ok) { renderError("Couldn't read the PIN (" + r.status + ")."); return; }
      const body = await r.json();
      if (!body || !body.pin) { renderError("Server returned no PIN."); return; }
      renderPin(String(body.pin), Number(body.expires_in_s) || 0);
    } catch (e) {
      renderError("Network error: " + e.message);
    }
  }

  btn.onclick = fetchPin;
}
