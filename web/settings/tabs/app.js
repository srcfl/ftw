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

  function setStatus(text) {
    var el = document.getElementById("app-link-status");
    if (el) el.textContent = text;
  }

  function pairedText(n) {
    if (n === 0) return "No phones paired yet.";
    if (n === 1) return "One phone paired.";
    return n + " phones paired.";
  }

  function refreshStatus() {
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
        if (!s.enabled) {
          setStatus("The app link is off. Set app_link.enabled in your configuration and restart.");
          if (button) button.disabled = true;
          return;
        }
        setStatus(pairedText(s.paired_devices));
      })
      .catch(function () {
        setStatus("Could not reach the box.");
      });
  }

  function showCode(pairing) {
    var slot = document.getElementById("app-link-slot");
    if (!slot) return;
    slot.textContent = "";

    var box = document.createElement("div");
    box.id = "app-link-qr";
    slot.appendChild(box);

    // Loaded on demand: this tab is opened once per phone, and every other
    // page would otherwise carry the library for nothing.
    import("/vendor/qrcode.js")
      .then(function (m) {
        var QR = m.default || m.QRCode || window.QRCode;
        new QR(box, {
          text: pairing.url,
          width: 260,
          height: 260,
          // Dark modules on light, whatever the page theme. An inverted QR is
          // one many phone cameras will not read.
          colorDark: "#000000",
          colorLight: "#ffffff",
          correctLevel: (QR.CorrectLevel && QR.CorrectLevel.M) || 0,
        });

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
        refreshStatus();
      });
  }

  S.tabs.app = {
    render: function () {
      // Wired after this string becomes the DOM.
      setTimeout(function () {
        var button = document.getElementById("app-link-pair");
        if (button) button.addEventListener("click", requestCode);
        refreshStatus();
      }, 0);

      return (
        "<fieldset><legend>The FTW app</legend>" +
        '<p class="hint">Scan this with the FTW app to add a phone. The code works ' +
        "once and expires in a few minutes, so ask for a new one when you need it. " +
        "Everything the app needs is in the code itself — none of it passes through " +
        "Sourceful, which is why the app can be sure it is talking to this box.</p>" +
        '<p id="app-link-status" class="hint">checking…</p>' +
        '<button type="button" id="app-link-pair">Show pairing code</button>' +
        '<div id="app-link-slot"></div>' +
        "</fieldset>"
      );
    },
  };
})();
