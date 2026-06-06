// setup-remote.js — the LAN-page "Set up remote access" affordance.
//
// Shown ONLY on a genuine-LAN origin (plain HTTP, same Wi-Fi as the Pi). It GETs
// the enroll-PIN endpoint — which now returns {bootstrap_id, pin, expires_in_s} —
// and renders the COURIER for the high-entropy bootstrap_id:
//
//   1. a QR encoding  https://<rp.id>/owner-access/enroll.html#b=<bootstrap_id>
//   2. the same URL as a clickable link
//   3. the 6-digit PIN, big + amber, with a live countdown + copy (the OPTIONAL
//      manual second factor the Pi validates on enroll)
//
// The bootstrap_id rides in the URL FRAGMENT (#b=), which browsers never send to a
// server, so the secret only ever reaches the destination device. The relay only
// sees hex(sha256(bootstrap_id)). This page is DISPLAY-ONLY: it runs no WebAuthn
// (it's plain HTTP — the secure-context ceremony happens on the home.* destination).
//
// DESIGN.md: mono/tabular digits, one amber accent, 1 px hairlines, no Google
// Fonts / no CDN. The QR encoder is vendored (web/vendor/qrcode.js) so a fresh Pi
// renders it offline.

import { apiBase } from "./webauthn.js";
import { qrMatrix } from "../vendor/qrcode.js";

// resolveRpId returns the WebAuthn RP-ID the destination home.* origin uses. The
// QR/link must point at THAT registrable origin (the secure context where the
// passkey ceremony runs), NOT this LAN IP. Order:
//   1. an explicit <meta name="ftw-rp-id" content="..."> (operator override for a
//      custom-domain deploy),
//   2. the documented default home.fortytwowatts.com (matches the Pi's
//      OwnerAccessRPID fallback in go/internal/api/api_owner_access.go).
export function resolveRpId() {
  try {
    const m = document.querySelector('meta[name="ftw-rp-id"]');
    const v = m && m.getAttribute("content");
    if (v && v.trim()) return v.trim();
  } catch (_) { /* no DOM / no meta */ }
  return "home.fortytwowatts.com";
}

// enrollUrl builds the destination onboarding URL with the bootstrap_id in the
// fragment. The fragment is never transmitted to a server.
export function enrollUrl(rpId, bootstrapId) {
  return "https://" + rpId + "/owner-access/enroll.html#b=" + encodeURIComponent(bootstrapId);
}

function fmtCountdown(s) {
  if (s <= 0) return "0:00";
  const m = Math.floor(s / 60);
  const sec = s % 60;
  return m + ":" + String(sec).padStart(2, "0");
}

// drawQR paints the boolean module matrix into a <canvas>, scaled to fit `target`
// device pixels with a 4-module quiet zone (the QR-spec minimum margin), in the
// page's foreground/background tokens. Returns the canvas element.
export function drawQR(text, target) {
  const matrix = qrMatrix(text);
  const n = matrix.length;
  const quiet = 4; // modules of mandatory margin
  const total = n + quiet * 2;
  const px = Math.max(2, Math.floor((target || 240) / total)); // integer module size
  const size = total * px;
  const canvas = document.createElement("canvas");
  canvas.width = size;
  canvas.height = size;
  canvas.style.width = size + "px";
  canvas.style.height = size + "px";
  const ctx = canvas.getContext("2d");
  // Light background + dark modules. QR scanners need a light quiet zone, so we
  // paint an explicit near-white field rather than relying on the page bg (which
  // may be dark). Dark modules are near-black for max contrast.
  ctx.fillStyle = "#ffffff";
  ctx.fillRect(0, 0, size, size);
  ctx.fillStyle = "#0a0a0a";
  for (let r = 0; r < n; r++) {
    for (let c = 0; c < n; c++) {
      if (matrix[r][c]) {
        ctx.fillRect((c + quiet) * px, (r + quiet) * px, px, px);
      }
    }
  }
  return canvas;
}

// mountSetupRemote wires the host element with a toggle button + a result region.
// On a relay page it shows a LAN-only notice (the bootstrap_id is minted LAN-side).
//
//   host  — element to render into.
//   label — toggle button text (default "Set up remote access").
export function mountSetupRemote(host, label) {
  if (!host) return;

  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "setup-remote-btn";
  btn.textContent = label || "Set up remote access";

  const out = document.createElement("div");
  out.className = "setup-remote-result";
  out.hidden = true;

  host.appendChild(btn);
  host.appendChild(out);

  let timer = null;
  function stopTimer() { if (timer) { clearInterval(timer); timer = null; } }

  function renderRemote() {
    stopTimer();
    out.hidden = false;
    out.innerHTML =
      '<p class="setup-note">Remote-access setup is available on your local network only. ' +
      'Open this page from a device on the same Wi‑Fi as your Pi.</p>';
  }

  function renderError(m) {
    stopTimer();
    out.hidden = false;
    out.innerHTML = '<p class="setup-err"></p>';
    out.querySelector(".setup-err").textContent = m;
  }

  function renderSetup(bootstrapId, pin, expiresInS) {
    stopTimer();
    const url = enrollUrl(resolveRpId(), bootstrapId);
    out.hidden = false;
    out.innerHTML =
      '<p class="eyebrow">Scan to set up another device</p>' +
      '<div class="setup-qr"></div>' +
      '<p class="setup-link-row">or open <a class="setup-link" target="_blank" rel="noopener noreferrer"></a></p>' +
      '<hr class="setup-sep">' +
      '<p class="eyebrow">Optional PIN</p>' +
      '<div class="pin-digits" aria-label="enrollment PIN"></div>' +
      '<div class="pin-meta">' +
        '<button type="button" class="pin-copy-btn">Copy</button>' +
        '<span class="pin-countdown" aria-live="polite"></span>' +
      '</div>' +
      '<p class="setup-note">The QR carries the secure handle. Type this PIN on the new device only if it asks for it.</p>';

    // QR canvas.
    try {
      out.querySelector(".setup-qr").appendChild(drawQR(url, 240));
    } catch (e) {
      out.querySelector(".setup-qr").innerHTML =
        '<p class="setup-err">Couldn\'t render the QR — use the link below.</p>';
    }

    // Clickable link (same URL).
    const a = out.querySelector(".setup-link");
    a.href = url;
    a.textContent = url;

    // PIN digits + copy.
    out.querySelector(".pin-digits").textContent = pin;
    const copyBtn = out.querySelector(".pin-copy-btn");
    copyBtn.onclick = async () => {
      try {
        await navigator.clipboard.writeText(pin);
        copyBtn.textContent = "Copied";
        setTimeout(() => { copyBtn.textContent = "Copy"; }, 1500);
      } catch (e) { copyBtn.textContent = "Copy failed"; }
    };

    // Live countdown; re-mint on expiry (a fresh GET mints a fresh PIN + id).
    const cd = out.querySelector(".pin-countdown");
    let remaining = Math.max(0, Math.floor(expiresInS));
    function tick() {
      if (remaining <= 0) {
        stopTimer();
        out.innerHTML = '<button type="button" class="setup-remote-btn setup-expired">Expired — tap to mint a fresh link</button>';
        out.querySelector(".setup-expired").onclick = fetchSetup;
        return;
      }
      cd.textContent = "expires in " + fmtCountdown(remaining);
      remaining -= 1;
    }
    tick();
    timer = setInterval(tick, 1000);
  }

  async function fetchSetup() {
    // The bootstrap_id is minted LAN-side; on a relay page (apiBase()!="") there is
    // nothing to show.
    if (apiBase() !== "") { renderRemote(); return; }
    out.hidden = false;
    out.innerHTML = '<p class="setup-note">Minting…</p>';
    try {
      const r = await fetch(apiBase() + "/api/owner-access/enroll-pin", { credentials: "same-origin" });
      if (r.status === 403) { renderRemote(); return; }
      if (!r.ok) { renderError("Couldn't start setup (" + r.status + ")."); return; }
      const body = await r.json();
      if (!body || !body.bootstrap_id || !body.pin) { renderError("Server returned an incomplete setup response."); return; }
      renderSetup(String(body.bootstrap_id), String(body.pin), Number(body.expires_in_s) || 0);
    } catch (e) {
      renderError("Network error: " + e.message);
    }
  }

  btn.onclick = fetchSetup;
}

if (typeof window !== "undefined") {
  window.ftwSetupRemote = { mountSetupRemote, resolveRpId, enrollUrl, drawQR };
}
