// Settings → FTW app tab: pair a phone with this box.
//
// One button and a square. The code is single-use and short-lived, so it is
// minted when someone asks rather than sitting on screen — a QR left visible
// for a week is a QR anyone who walked past has photographed.
//
// Everything the app needs travels in the URL fragment: the box's static key,
// the rendezvous secret, the pairing code. A fragment is never sent in an HTTP
// request, so none of it reaches Sourceful. That is what lets the app trust
// this box specifically rather than whatever the relay introduces it to.
//
// The payload is never written into innerHTML and never offered as text to
// copy. It is a credential; the only path it should take is the camera.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  // The tab context, kept so the callbacks below can read the live config
  // without threading it through every handler.
  var pairingCtx = null;

  function setStatus(text) {
    var el = document.getElementById("app-link-status");
    if (el) el.textContent = text;
  }

  function pairedText(n) {
    if (n === 0) return "No phones paired yet.";
    if (n === 1) return "One phone paired.";
    return n + " phones paired.";
  }

  // What the checkbox says and what the box is doing are two different
  // things, because the uplink is connected at startup. Between saving and
  // restarting they disagree, and that gap is the whole reason this says
  // anything at all: a checkbox that goes quiet after Save leaves someone
  // pressing a pairing button that cannot work yet.
  //
  // `saved` is the checkbox, read from the config the Save button will post.
  // `running` is /api/app-link/status, which reports the process.
  function describe(saved, running) {
    if (running) {
      if (!saved) return "Running. It stops at the next restart.";
      return null; // The caller shows the pairing count instead.
    }
    if (saved) return "Saved. Press Save, then restart, to finish turning it on.";
    return "Off. Turn it on above, save, and restart.";
  }

  function agoText(ms) {
    if (!ms) return "never seen";
    var d = Date.now() - ms;
    if (d < 90 * 1000) return "just now";
    if (d < 90 * 60 * 1000) return Math.round(d / 60000) + " min ago";
    if (d < 36 * 3600 * 1000) return Math.round(d / 3600000) + " h ago";
    return Math.round(d / 86400000) + " d ago";
  }

  // The device list is what makes "remove" possible at all. Rows carry a
  // short key prefix and two timestamps — the phone in daily use shows a
  // fresh "last seen" and floats to the top; a key that paired once and
  // vanished (a test run, a mistake, a stranger) sinks and is the one to
  // remove. Removal is immediate: the box drops any live session too.
  function refreshDevices() {
    var list = document.getElementById("app-link-devices");
    if (!list) return;
    fetch("/api/app-link/devices")
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (body) {
        list.textContent = "";
        var devices = (body && body.devices) || [];
        if (devices.length === 0) return;

        devices.forEach(function (d) {
          var row = document.createElement("div");
          row.className = "app-device-row";

          var name = document.createElement("span");
          name.className = "mono";
          name.textContent = "Phone " + d.id;
          row.appendChild(name);

          var seen = document.createElement("span");
          seen.className = "hint";
          seen.textContent = d.last_seen_ms ? "seen " + agoText(d.last_seen_ms) : "never connected";
          row.appendChild(seen);

          var btn = document.createElement("button");
          btn.type = "button";
          btn.textContent = "Remove";
          btn.addEventListener("click", function () {
            if (!confirm("Remove this phone? It loses access immediately and must scan a new code to come back.")) return;
            btn.disabled = true;
            fetch("/api/app-link/devices/" + encodeURIComponent(d.id), { method: "DELETE" })
              .then(function () { refreshDevices(); refreshStatus(pairingCtx); })
              .catch(function () { btn.disabled = false; });
          });
          row.appendChild(btn);

          list.appendChild(row);
        });
      })
      .catch(function () {});
  }

  function refreshStatus(ctx) {
    var saved = !!(ctx && ctx.config.app_link && ctx.config.app_link.enabled);

    fetch("/api/app-link/status")
      .then(function (r) {
        return r.ok ? r.json() : null;
      })
      .then(function (s) {
        var button = document.getElementById("app-link-pair");
        if (!s) {
          setStatus("Pairing is available on your local network only.");
          if (button) button.disabled = true;
          return;
        }
        var note = describe(saved, s.enabled);
        setStatus(note === null ? pairedText(s.paired_devices) : note);
        if (button) button.disabled = !s.enabled;
        if (s.enabled) refreshDevices();
      })
      .catch(function () {
        setStatus("Could not reach the box.");
      });
  }

  // drawQR paints qrMatrix(text) into a canvas. Same shape as the calendar
  // tab's, because /vendor/qrcode.js hands back a boolean matrix and leaves
  // the painting to the caller — there is no constructor to call.
  function drawQR(qrMatrix, text, target) {
    var matrix = qrMatrix(text);
    var n = matrix.length, quiet = 4, total = n + 2 * quiet;
    var px = Math.max(2, Math.floor((target || 260) / total));
    var size = total * px;
    var canvas = document.createElement("canvas");
    canvas.width = size; canvas.height = size;
    var ctx = canvas.getContext("2d");
    // Dark on light whatever the page theme. An inverted QR is one many phone
    // cameras will not read.
    ctx.fillStyle = "#ffffff"; ctx.fillRect(0, 0, size, size);
    ctx.fillStyle = "#000000";
    for (var r = 0; r < n; r++) {
      for (var c = 0; c < n; c++) {
        if (matrix[r][c]) ctx.fillRect((c + quiet) * px, (r + quiet) * px, px, px);
      }
    }
    return canvas;
  }

  function showCode(pairing) {
    var slot = document.getElementById("app-link-slot");
    if (!slot) return;
    slot.textContent = "";

    // Loaded on demand: this tab is opened once per phone, and every other
    // page would otherwise carry the encoder for nothing.
    import("/vendor/qrcode.js")
      .then(function (m) {
        slot.appendChild(drawQR(m.qrMatrix, pairing.url, 260));

        var note = document.createElement("p");
        note.className = "hint";
        var minutes = Math.max(1, Math.round((pairing.expires_at_ms - Date.now()) / 60000));
        note.textContent = "Works once, for about " + minutes + " more minutes.";
        slot.appendChild(note);
      })
      .catch(function () {
        var err = document.createElement("p");
        err.className = "hint";
        err.textContent = "Could not draw the code. Reload the page and try again.";
        slot.appendChild(err);
      });
  }

  function requestCode() {
    var button = document.getElementById("app-link-pair");
    var slot = document.getElementById("app-link-slot");
    if (!button || !slot) return;

    button.disabled = true;
    button.textContent = "Making a code…";
    slot.textContent = "";

    fetch("/api/app-link/pairing", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    })
      .then(function (r) {
        return r.json().then(function (body) {
          if (!r.ok) throw new Error(body.error || "could not make a code");
          return body;
        });
      })
      .then(showCode)
      .catch(function (e) {
        var err = document.createElement("p");
        err.className = "hint";
        err.textContent = e.message || String(e);
        slot.appendChild(err);
      })
      .then(function () {
        button.disabled = false;
        button.textContent = "Show a new code";
        refreshStatus(pairingCtx);
      });
  }

  S.tabs.app = {
    render: function (ctx) {
      if (!ctx.config.app_link) ctx.config.app_link = { enabled: false };
      var enabled = !!ctx.config.app_link.enabled;
      pairingCtx = ctx;

      // Wired after this string becomes the DOM.
      setTimeout(function () {
        var button = document.getElementById("app-link-pair");
        if (button) button.addEventListener("click", requestCode);

        // The shared data-checkbox-path handler writes the config; this only
        // repaints the line underneath, so the wording follows the checkbox
        // before anyone presses Save.
        var toggle = document.getElementById("app-link-enabled");
        if (toggle) toggle.addEventListener("change", function () { refreshStatus(ctx); });

        refreshStatus(ctx);
      }, 0);

      return (
        "<fieldset><legend>The FTW app</legend>" +
        '<p class="hint">The FTW app talks to this box directly. Turning this on ' +
        "lets it reach you when you are away from home; nothing readable passes " +
        "through Sourceful either way.</p>" +
        '<label><input type="checkbox" id="app-link-enabled" ' +
        'data-checkbox-path="app_link.enabled"' + (enabled ? " checked" : "") +
        "> Let the FTW app connect to this box</label>" +
        '<p class="hint">Takes effect after a restart — the box offers one when ' +
        "you save.</p>" +
        '<p id="app-link-status" class="hint">checking…</p>' +
        '<div id="app-link-devices"></div>' +
        '<button type="button" id="app-link-pair" disabled>Show pairing code</button>' +
        '<p class="hint">Scan the code with the FTW app to add a phone. It works ' +
        "once and expires in a few minutes, so ask for a new one when you need it. " +
        "Everything the app needs is in the code itself, which is why the app can " +
        "be sure it is talking to this box.</p>" +
        '<div id="app-link-slot"></div>' +
        "</fieldset>"
      );
    },
  };
})();
