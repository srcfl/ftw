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

  // What each role may do, in the words the row uses. The box is what
  // enforces these; this only names them.
  function roleText(role) {
    return role === "viewer" ? "Can look" : "Can change things";
  }

  // What removing this phone costs, asked before it happens.
  //
  // The last phone that can change things can be removed from this page — the
  // box allows it here and refuses it from the app, because whoever is reading
  // this is in the house and the code above is the way back in. So the warning
  // has to be honest about what is on the other side of the button, and about
  // what is left: a household with viewers still paired keeps its viewers, and
  // one down to a single phone is left with nothing paired at all.
  function removalWarning(d, total) {
    if (!d.last_owner) {
      return "Remove this phone? It loses access immediately and must scan a new code to come back.";
    }
    if (total === 1) {
      return "This is the last phone. Remove it and nothing can see or change this " +
        "home until you pair a phone again, with a code from this page. Remove it?";
    }
    return "This is the only phone that can change things. Remove it and the phones " +
      "left can look but change nothing, until you pair another owner with a code " +
      "from this page. Remove it?";
  }

  // Reports a refusal the box gave, rather than swallowing it. Without a
  // sentence here a refused button simply does nothing and the household has
  // no idea why.
  function sayWhyNot(response, row) {
    return response.json().then(function (body) {
      var note = document.createElement("p");
      note.className = "hint";
      note.textContent = (body && body.error) || "That did not work.";
      row.appendChild(note);
    });
  }

  // The device list is what makes "remove" possible at all, and it is where
  // sharing lives too. Rows carry a short key prefix, what the phone may do
  // and two timestamps — the phone in daily use shows a fresh "last seen" and
  // floats to the top; a key that paired once and vanished (a test run, a
  // mistake, a stranger) sinks and is the one to remove.
  //
  // Removing a guest and locking a phone out are the same button, because
  // they are the same thing: a guest's phone is a paired phone. Removal is
  // immediate — the box drops any live session too.
  function refreshDevices() {
    var list = document.getElementById("app-link-devices");
    if (!list) return;
    fetch("/api/app-link/devices")
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (body) {
        list.textContent = "";
        var devices = (body && body.devices) || [];
        // Sharing needs somebody to share from. The first phone on a box is
        // its owner whatever code it used, so the button appears once there
        // is one — otherwise it would offer a guest pass that silently makes
        // an owner.
        var shareButton = document.getElementById("app-link-share");
        if (shareButton) shareButton.hidden = devices.length === 0;
        if (devices.length === 0) return;

        devices.forEach(function (d) {
          var row = document.createElement("div");
          row.className = "app-device-row";

          var name = document.createElement("span");
          name.className = "mono";
          name.textContent = "Phone " + d.id;
          row.appendChild(name);

          var what = document.createElement("span");
          what.className = "hint";
          what.textContent = roleText(d.role);
          row.appendChild(what);

          var seen = document.createElement("span");
          seen.className = "hint";
          seen.textContent = d.last_seen_ms ? "seen " + agoText(d.last_seen_ms) : "never connected";
          row.appendChild(seen);

          // The last owner cannot be stepped down at either door, so that
          // button is not drawn for it — a button whose only answer is a
          // refusal is worse than no button. Removing it is a different
          // matter: the box allows that from this page, so the row offers it
          // and the warning above says what it costs.
          if (d.last_owner) {
            var only = document.createElement("span");
            only.className = "hint";
            only.textContent = "The only phone that can change things. Pair another owner to step this one down.";
            row.appendChild(only);
          } else {
            var toRole = d.role === "viewer" ? "owner" : "viewer";
            var change = document.createElement("button");
            change.type = "button";
            change.textContent = d.role === "viewer" ? "Let it change things" : "Make it view only";
            change.addEventListener("click", function () {
              change.disabled = true;
              fetch("/api/app-link/devices/" + encodeURIComponent(d.id), {
                method: "PATCH",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ role: toRole }),
              })
                .then(function (r) {
                  if (!r.ok) return sayWhyNot(r, row);
                  refreshDevices();
                })
                .catch(function () {})
                .then(function () { change.disabled = false; });
            });
            row.appendChild(change);
          }

          var btn = document.createElement("button");
          btn.type = "button";
          btn.textContent = "Remove";
          btn.addEventListener("click", function () {
            if (!confirm(removalWarning(d, devices.length))) return;
            btn.disabled = true;
            fetch("/api/app-link/devices/" + encodeURIComponent(d.id), { method: "DELETE" })
              .then(function (r) {
                if (!r.ok) return sayWhyNot(r, row);
                refreshDevices();
                refreshStatus(pairingCtx);
              })
              .catch(function () {})
              .then(function () { btn.disabled = false; });
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
        // Both ways of letting a phone in, enabled together. Sharing is not a
        // separate feature that could still work with the app link off: it
        // mints the same code from the same endpoint.
        var buttons = [
          document.getElementById("app-link-pair"),
          document.getElementById("app-link-share"),
        ];
        var setEnabled = function (on) {
          buttons.forEach(function (b) { if (b) b.disabled = !on; });
        };
        if (!s) {
          setStatus("Pairing is available on your local network only.");
          setEnabled(false);
          return;
        }
        var note = describe(saved, s.enabled);
        setStatus(note === null ? pairedText(s.paired_devices) : note);
        setEnabled(s.enabled);
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

  // What the code lets in, said in words above it, before anyone reads it
  // out or holds a phone up to it. A code whose power is invisible is the one
  // that gets given to the wrong person.
  function sayWhatItLetsIn(slot, role) {
    var says = document.createElement("p");
    says.className = "hint";
    says.textContent = role === "viewer"
      ? "This lets someone see this home. They cannot change anything."
      : "This adds a phone that can change things here.";
    slot.appendChild(says);
  }

  function expiryText(pairing) {
    var minutes = Math.max(1, Math.round((pairing.expires_at_ms - Date.now()) / 60000));
    return "Works once, for about " + minutes + " more minutes.";
  }

  // The way in for a phone that cannot scan: no camera, a cracked lens, or
  // somebody setting their own phone up again from another house.
  //
  // It carries the role of the code already on screen, so who this is for is
  // chosen once, at the button above, and cannot drift between the sentence
  // the household just read and the code they read out. It is built before
  // the encoder is even asked for, because a QR that will not draw is exactly
  // when somebody needs this most.
  function spokenFallback(role) {
    var label = "Cannot scan? Read a code out instead";
    var btn = document.createElement("button");
    btn.type = "button";
    btn.textContent = label;
    btn.addEventListener("click", function () {
      requestCode({ role: role, kind: "spoken" }, btn, "Making a code…", label);
    });
    return btn;
  }

  function showCode(pairing) {
    var slot = document.getElementById("app-link-slot");
    if (!slot) return;
    slot.textContent = "";
    sayWhatItLetsIn(slot, pairing.role);

    // A box code is meant to be read down a phone, so it is shown as text and
    // nothing else. The QR payload never is: it is a credential, and the only
    // path it should take is a camera.
    if (pairing.code) {
      var spoken = document.createElement("p");
      spoken.className = "mono app-box-code";
      spoken.textContent = pairing.code;
      slot.appendChild(spoken);

      var how = document.createElement("p");
      how.className = "hint";
      // What to do, not what to tap. The button that takes this code lives in
      // the app, in another repository, and a label quoted here would go on
      // being quoted long after the app renamed it.
      how.textContent = "Read this out. On the other phone, choose to type a code " +
        "rather than scan one. " + expiryText(pairing) + " Five wrong tries and it " +
        "stops working, so ask for a new one rather than guessing.";
      slot.appendChild(how);
      return;
    }

    // Held so the drawing lands above the fallback button rather than after
    // it, whenever the encoder finishes.
    var picture = document.createElement("div");
    slot.appendChild(picture);
    slot.appendChild(spokenFallback(pairing.role));

    // Loaded on demand: this tab is opened once per phone, and every other
    // page would otherwise carry the encoder for nothing.
    import("/vendor/qrcode.js")
      .then(function (m) {
        picture.appendChild(drawQR(m.qrMatrix, pairing.url, 260));

        var note = document.createElement("p");
        note.className = "hint";
        note.textContent = expiryText(pairing);
        picture.appendChild(note);
        if (slot.scrollIntoView) slot.scrollIntoView({ block: "nearest" });
      })
      .catch(function () {
        var err = document.createElement("p");
        err.className = "hint app-link-error";
        err.textContent = "Could not draw the code here — read one out instead.";
        picture.appendChild(err);
        if (slot.scrollIntoView) slot.scrollIntoView({ block: "nearest" });
      });
  }

  // One code at a time, whatever kind it is: asking for a guest pass cancels
  // a pairing code still on screen, and the reverse. Every button here goes
  // through this, so the rule is not something a caller has to remember.
  function requestCode(opts, button, busyText, doneText) {
    var slot = document.getElementById("app-link-slot");
    if (!button || !slot) return;

    button.disabled = true;
    button.textContent = busyText;
    slot.textContent = "";

    fetch("/api/app-link/pairing", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(opts),
    })
      .then(function (r) {
        return r.json().then(function (body) {
          if (!r.ok) throw new Error(body.error || "could not make a code");
          return body;
        });
      })
      .then(showCode)
      .catch(function (e) {
        // A refusal is an answer, not background reading: red, and scrolled
        // into view — the #951 gate's "on the box, or after the house
        // password is on" is the expected outcome on every LAN browser.
        var err = document.createElement("p");
        err.className = "hint app-link-error";
        err.textContent = e.message || String(e);
        slot.appendChild(err);
        if (slot.scrollIntoView) slot.scrollIntoView({ block: "nearest" });
      })
      .then(function () {
        button.disabled = false;
        button.textContent = doneText;
        refreshStatus(pairingCtx);
      });
  }

  function fleetPanel(ctx) {
    var panel = S.fleetPing;
    return panel && panel.render ? panel.render(ctx) : "";
  }

  S.tabs.app = {
    afterSave: function () {
      // These two switches share a page, not a state. The app link has its
      // own restart boundary; fleet sharing takes effect as soon as it saves.
      var panel = S.fleetPing;
      if (panel && panel.afterSave) panel.afterSave();
    },

    render: function (ctx) {
      if (!ctx.config.app_link) ctx.config.app_link = { enabled: true };
      var enabled = !!ctx.config.app_link.enabled;
      pairingCtx = ctx;

      // Wired after this string becomes the DOM.
      setTimeout(function () {
        var pairButton = document.getElementById("app-link-pair");
        if (pairButton) {
          pairButton.addEventListener("click", function () {
            requestCode({ role: "owner" }, pairButton, "Making a code…", "Show a new code");
          });
        }

        var shareButton = document.getElementById("app-link-share");
        if (shareButton) {
          shareButton.addEventListener("click", function () {
            requestCode({ role: "viewer" }, shareButton, "Making a code…", "Let someone see this home");
          });
        }

        // The shared data-checkbox-path handler writes the config; this only
        // repaints the line underneath, so the wording follows the checkbox
        // before anyone presses Save.
        var toggle = document.getElementById("app-link-enabled");
        if (toggle) toggle.addEventListener("change", function () { refreshStatus(ctx); });

        refreshStatus(ctx);
      }, 0);

      return (
        "<fieldset><legend>The FTW app</legend>" +
        '<p class="hint">This is not the Sourceful (Zap) app. The FTW app at ' +
        "app.ftw.energy talks to this box when you are away. On the same Wi-Fi you " +
        "can just open this page in a browser — no pairing needed. Readings and " +
        "commands are end-to-end encrypted. Sourceful can see your IP and when the " +
        "box is connected, but cannot read them.</p>" +
        '<label><input type="checkbox" id="app-link-enabled" ' +
        'data-checkbox-path="app_link.enabled"' + (enabled ? " checked" : "") +
        "> Let the FTW app connect to this box</label>" +
        '<p class="hint">Takes effect after a restart — the box offers one when ' +
        "you save.</p>" +
        '<p id="app-link-status" class="hint">checking…</p>' +
        '<div id="app-link-devices"></div>' +
        '<div class="app-link-actions">' +
        '<button type="button" id="app-link-pair" disabled>Show pairing code</button>' +
        '<button type="button" id="app-link-share" hidden>Let someone see this home</button>' +
        "</div>" +
        // The slot sits right under the buttons that fill it. It used to sit
        // after the hint paragraphs below, which put the answer — QR, spoken
        // code, or a refusal — some 300 px under the button and usually below
        // the modal's fold: the operator pressed, the panel "blinked", and
        // nothing appeared where they were looking (field report 2026-08-29,
        // the #951 gate refusal was in the DOM the whole time).
        '<div id="app-link-slot"></div>' +
        '<p class="hint">Scan the code with the FTW app to add a phone. It works ' +
        "once and expires in a few minutes, so ask for a new one when you need it. " +
        "Everything the app needs is in the code itself, which is why the app can " +
        "be sure it is talking to this box.</p>" +
        '<p class="hint">A code can be read out instead of scanned. The offer sits ' +
        "under whichever code is on screen and lets in exactly the same access. It " +
        "is for a phone that has been here before and is being set up again — a " +
        "phone that has never seen this box has to scan, because only the square " +
        "carries what it needs to find this box and be sure it is this one. Only " +
        "one code is live at a time, so asking for any of them stops the last " +
        "one.</p>" +
        "</fieldset>" +
        fleetPanel(ctx)
      );
    },
  };
})();
