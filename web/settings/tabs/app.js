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
// The payload is a credential. A link or explicit copy keeps it in the URL
// fragment; it never goes into innerHTML, a query, a log or remote storage.
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
    if (saved) return "Not running yet. Choose Save and restart FTW to turn it on.";
    return "Off. Turn it on above, then choose Save and restart FTW.";
  }

  // The phone reaches this box through FTW Cloud, so pairing accepts its
  // terms, project statistics included, as it does in the app (Fredrik, 23
  // and 28 November 2026). The page says so in one line by the buttons
  // rather than sending anyone to a separate panel first, and asks nothing
  // more. The box records the answer and is the judge of it: a box whose
  // build names no Cloud service has nothing to accept.
  function cloudNeedsTerms(cloud) {
    return !!cloud && cloud.available === true && !cloud.cloudTermsAccepted;
  }

  // Cloud in use, or still being deleted: the only times its panel has
  // something to show here. Off, pairing is the way in.
  function cloudInUse(cloud) {
    return !!cloud && ((cloud.link === "on" && !cloud.statisticsOnly) ||
      cloud.link === "ending" || cloud.state === "revoking");
  }

  function readJSON(path) {
    return fetch(path)
      .then(function (r) { return r.ok ? r.json() : null; })
      .catch(function () { return null; });
  }

  // Accepts the current terms through the same route as the Cloud panel,
  // POST /api/cloud/grant, and only when the box says they are not yet
  // accepted. A refusal stops the pairing: the line by the button promised
  // that pairing accepts them. So does terms the page could not read, since
  // a code shown then would pair a phone with the terms still unaccepted.
  function acceptCloudTerms() {
    return Promise.all([readJSON("/api/cloud"), readJSON("/api/cloud/stats-policy")])
      .then(function (v) {
        var cloud = v[0], policy = v[1];
        if (!cloudNeedsTerms(cloud)) return;
        if (!policy) throw new Error("Could not read the FTW Cloud terms from the box. Try again.");
        return fetch("/api/cloud/grant", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ scopes: policy.cloud_scopes, policy: policy.cloud_policy }),
        }).then(function (r) {
          return r.json().then(function (body) {
            if (!r.ok) throw new Error((body && body.error) || "Could not accept the FTW Cloud terms.");
            if (window.FTWStatistics) window.FTWStatistics.refresh();
          });
        });
      });
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
    if (role === "watch") return "Can look and charge";
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
  //
  // A watch belongs to the phone that added it. Removing the watch touches
  // nothing else; removing the phone removes its watches too, so the warning
  // says so.
  function removalWarning(d, phones, watches) {
    if (d.parent) {
      return "Remove this watch? It loses access immediately. Its phone can add it again.";
    }
    var also = watches > 0
      ? (watches === 1 ? " Its watch goes with it." : " Its " + watches + " watches go with it.")
      : "";
    if (!d.last_owner) {
      return "Remove this phone? It loses access immediately and must scan a new code to come back." + also;
    }
    if (phones === 1) {
      return "This is the last phone. Remove it and nothing can see or change this " +
        "home until you pair a phone again, with a code from this page." + also + " Remove it?";
    }
    return "This is the only phone that can change things. Remove it and the phones " +
      "left can look but change nothing, until you pair another owner with a code " +
      "from this page." + also + " Remove it?";
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
  //
  // A watch comes right after the phone that added it, set in under it. Its
  // reach follows that phone, so it has no role button of its own.
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
        var phones = devices.filter(function (d) { return !d.parent; }).length;
        var watchesOf = function (id) {
          return devices.filter(function (d) { return d.parent === id; }).length;
        };

        devices.forEach(function (d) {
          var row = document.createElement("div");
          row.className = d.parent ? "app-device-row app-device-watch" : "app-device-row";

          var name = document.createElement("span");
          name.className = "mono";
          name.textContent = (d.parent ? "Watch " : "Phone ") + d.id;
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
          } else if (!d.parent) {
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
            if (!confirm(removalWarning(d, phones, watchesOf(d.id)))) return;
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

  function setHidden(id, hidden) {
    var el = document.getElementById(id);
    if (el) el.hidden = hidden;
  }

  function refreshStatus(ctx) {
    var saved = !!(ctx && ctx.config.app_link && ctx.config.app_link.enabled);

    Promise.all([
      fetch("/api/app-link/status").then(function (r) {
        return r.ok ? r.json() : null;
      }),
      readJSON("/api/cloud"),
    ])
      .then(function (v) {
        var s = v[0], cloud = v[1];
        // Both ways of letting a phone in, enabled together. Sharing is not a
        // separate feature that could still work with the app link off: it
        // mints the same code from the same endpoint.
        var buttons = [
          document.getElementById("app-link-pair"),
          document.getElementById("app-link-code"),
          document.getElementById("app-link-share"),
        ];
        var setEnabled = function (on) {
          buttons.forEach(function (b) { if (b) b.disabled = !on; });
        };
        setHidden("app-cloud-panel", !cloudInUse(cloud));
        if (!s) {
          setStatus("Pairing is available on your local network only.");
          setEnabled(false);
          setHidden("app-link-terms", true);
          return;
        }
        var note = describe(saved, s.enabled);
        setStatus(note === null ? pairedText(s.paired_devices) : note);
        // The switch and its restart are for turning the app link on. While
        // it is on and running, a quiet action below the pairing stops it.
        setHidden("app-link-switch", saved && s.enabled);
        setHidden("app-link-off-row", !(saved && s.enabled));
        var restart = document.getElementById("app-link-restart");
        if (restart) restart.className = saved !== s.enabled ? "btn-send" : "btn-ghost";
        setHidden("app-link-terms", !(s.enabled && cloudNeedsTerms(cloud)));
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
    btn.className = "btn-ghost";
    btn.textContent = label;
    btn.addEventListener("click", function () {
      requestCode({ role: role, kind: "spoken" }, btn, "Making a code…", label);
    });
    return btn;
  }

  // The address the app asks for beside a pairing code: this page's own,
  // which demonstrably works from inside the house. The QR carries it too,
  // so it is shown there as well, and nobody has to look in the router.
  //
  // Not inside Home Assistant: there the page's origin is Home Assistant's
  // ingress URL, and the box's own port is closed, so neither that address
  // nor a short code (which the app trades at that address) can work. The
  // QR and the link go through FTW Cloud and work there as anywhere.
  function inHomeAssistant() {
    return !!window.FTWHomeAssistant;
  }

  function boxAddress() {
    if (inHomeAssistant()) return "";
    return (window.location && window.location.origin) || "";
  }

  function sayBoxAddress(slot) {
    if (!boxAddress()) return;
    var address = document.createElement("p");
    address.textContent = "Box address: " + boxAddress();
    slot.appendChild(address);
  }

  function showCode(pairing) {
    var slot = document.getElementById("app-link-slot");
    if (!slot) return;
    slot.textContent = "";
    sayWhatItLetsIn(slot, pairing.role);

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
      how.textContent = "In FTW on your phone, choose Enter pairing code while on your home Wi-Fi. " +
        "Enter this code and the box address shown below. " + expiryText(pairing) + " Five wrong tries and it " +
        "stops working, so ask for a new one rather than guessing.";
      slot.appendChild(how);
      if (boxAddress()) {
        sayBoxAddress(slot);
        copyButton(slot, "Copy pairing code and address", boxAddress() + "#" + pairing.code);
      }
      if (slot.scrollIntoView) slot.scrollIntoView({ block: "nearest" });
      return;
    }

    // Held so the drawing lands above the fallback button rather than after
    // it, whenever the encoder finishes.
    var picture = document.createElement("div");
    slot.appendChild(picture);
    sayBoxAddress(slot);
    if (/^https:\/\/app\.ftw\.energy\/p#v[23]\./.test(pairing.url)) {
      var open = document.createElement("a");
      open.href = pairing.url;
      open.rel = "noreferrer";
      open.className = "btn-send";
      open.textContent = "Open in FTW on this phone";
      slot.appendChild(open);
      copyButton(slot, "Copy pairing link", pairing.url);
    }
    if (!inHomeAssistant()) slot.appendChild(spokenFallback(pairing.role));

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
        err.textContent = inHomeAssistant()
          ? "Could not draw the code here. Open or copy the pairing link instead."
          : "Could not draw the code here — read one out instead.";
        picture.appendChild(err);
        if (slot.scrollIntoView) slot.scrollIntoView({ block: "nearest" });
      });
  }

  function copyButton(slot, label, text) {
    var button = document.createElement("button");
    button.type = "button";
    button.className = "btn-ghost";
    button.textContent = label;
    button.addEventListener("click", function () {
      var clipboard = window.navigator && window.navigator.clipboard;
      function manualCopy() {
        var field = document.createElement("input");
        field.type = "text";
        field.readOnly = true;
        field.value = text;
        field.setAttribute("aria-label", label);
        slot.appendChild(field);
        field.focus();
        field.select();
        button.textContent = "Select the text above and copy it";
      }
      if (!clipboard) return manualCopy();
      clipboard.writeText(text).then(function () { button.textContent = "Copied"; }).catch(manualCopy);
    });
    slot.appendChild(button);
  }

  // Saves the settings and restarts FTW, which is when the app link starts
  // or stops: it connects at startup. statusId is the line beside the button
  // that was pressed; confirmed says that button already asked; unsaved runs
  // when the save fails, so the page can put back what it changed for it.
  async function restartAppLink(ctx, button, statusId, confirmed, unsaved) {
    if (!confirmed && !confirm("Save settings and restart FTW? Energy control pauses until FTW is back.")) return;
    var status = document.getElementById(statusId || "app-link-restart-status");
    button.disabled = true;
    status.textContent = "Saving settings…";
    resumePairing(true);
    var result;
    try {
      result = await ctx.saveConfig();
    } catch (error) {
      if (unsaved) unsaved();
      resumePairing(false);
      status.textContent = "Could not save settings: " + error.message;
      button.disabled = false;
      return;
    }
    try {
      // Keep the shell's existing dialog and progress when saving opens it.
      var restart = document.getElementById("restart-now");
      if (result && result.restart_required && restart) {
        restart.click();
        button.disabled = false;
        status.textContent = "Follow the restart progress above.";
        return;
      }
      var response = await fetch("/api/restart", { method: "POST" });
      if (!response.ok) {
        var failure = await response.json();
        throw new Error(failure.error || "Restart was refused.");
      }
      status.textContent = "Restarting FTW. Waiting for the box to return…";
      var until = Date.now() + 90000;
      function waitForBox() {
        fetch("/api/health?_=" + Date.now(), { cache: "no-store" })
          .then(function (r) {
            if (!r.ok) throw new Error("not ready");
            status.textContent = "FTW is back.";
            window.location.reload();
          })
          .catch(function () {
            if (Date.now() < until) return setTimeout(waitForBox, 1000);
            status.textContent = "FTW has not returned yet. Check the box, then reload this page.";
            button.disabled = false;
          });
      }
      setTimeout(waitForBox, 1500);
    } catch (error) {
      resumePairing(false);
      status.textContent = "Could not restart FTW: " + error.message;
      button.disabled = false;
    }
  }

  // The quiet way to stop the app link while it runs, so the owner can
  // always stop it at the box. It is the same saved setting and restart that
  // turn it on, so it unticks the same switch.
  function turnOffAppLink(ctx, button) {
    if (!confirm("Turn off the app link? Paired phones cannot reach this box until you turn it on again. " +
      "FTW saves and restarts; energy control pauses until it is back.")) return;
    // The save carries the setting, so it changes here, and changes back if
    // the save fails: a page that says off while the box still has it on
    // would let a later Save elsewhere turn it off unasked.
    var toggle = document.getElementById("app-link-enabled");
    var was = ctx.config.app_link.enabled;
    ctx.config.app_link.enabled = false;
    if (toggle) toggle.checked = false;
    return restartAppLink(ctx, button, "app-link-off-status", true, function () {
      ctx.config.app_link.enabled = was;
      if (toggle) toggle.checked = was;
    });
  }

  function resumePairing(on) {
    try {
      if (on) window.sessionStorage.setItem("ftw.pairing.resume", "1");
      else window.sessionStorage.removeItem("ftw.pairing.resume");
    } catch (_) { /* Pairing still works if the browser disallows storage. */ }
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

    acceptCloudTerms()
      .then(function () {
        return fetch("/api/app-link/pairing", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(opts),
        });
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
        // Restoring the label can wrap the action row on an iPhone. Scroll
        // after that layout change so the code and Copy button stay in view.
        if (slot.scrollIntoView) slot.scrollIntoView({ block: "nearest" });
      });
  }

  function fleetPanel(ctx) {
    var panel = S.fleetPing;
    return panel && panel.render ? panel.render(ctx) : "";
  }

  S.tabs.app = {
    afterSave: function () {
      // These share a page, not a state. The app link has its own restart
      // boundary; the statistics line follows FTW Cloud.
      var panel = S.fleetPing;
      if (panel && panel.afterSave) panel.afterSave();
      refreshStatus(pairingCtx);
    },

    render: function (ctx) {
      if (!ctx.config.app_link) ctx.config.app_link = { enabled: true };
      var enabled = !!ctx.config.app_link.enabled;
      var ha = inHomeAssistant();
      pairingCtx = ctx;

      // Wired after this string becomes the DOM.
      setTimeout(function () {
        var pairButton = document.getElementById("app-link-pair");
        if (pairButton) {
          pairButton.addEventListener("click", function () {
            requestCode({ role: "owner" }, pairButton, "Making a code…", "Show a new code");
          });
        }

        var codeButton = document.getElementById("app-link-code");
        if (codeButton) codeButton.addEventListener("click", function () {
          requestCode({ role: "owner", kind: "spoken" }, codeButton, "Making a code…", "Use a pairing code");
        });
        var restartButton = document.getElementById("app-link-restart");
        if (restartButton) restartButton.addEventListener("click", function () { restartAppLink(ctx, restartButton); });
        var offButton = document.getElementById("app-link-off");
        if (offButton) offButton.addEventListener("click", function () { turnOffAppLink(ctx, offButton); });

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
        if(window.FTWStatistics) {
          var statistics=document.querySelector("[data-project-statistics-settings]");
          if(statistics)window.FTWStatistics.mount(statistics);
        }
      }, 0);

      return (
        '<link rel="stylesheet" href="/settings/app-pairing.css">' +
        '<fieldset class="app-pairing"><legend>The FTW app</legend>' +
        '<p class="hint">This is not the Sourceful (Zap) app. The FTW app needs ' +
        "pairing, at home and away. A browser on your home Wi-Fi needs none.</p>" +
        // Shown only while the app link is off. On a box where it runs, the
        // quiet off action at the foot of this section takes their place.
        '<div id="app-link-switch"' + (enabled ? " hidden" : "") + ">" +
        '<label><input type="checkbox" id="app-link-enabled" ' +
        'data-checkbox-path="app_link.enabled"' + (enabled ? " checked" : "") +
        "> Let the FTW app connect to this box</label>" +
        '<button type="button" class="btn-send" id="app-link-restart">Save and restart FTW…</button>' +
        '<p id="app-link-restart-status" class="hint" role="status"></p>' +
        "</div>" +
        '<p id="app-link-status" class="hint">checking…</p>' +
        '<div id="app-link-devices"></div>' +
        '<div class="app-link-actions">' +
        '<button type="button" class="btn-ghost" id="app-link-pair" disabled>Show pairing code</button>' +
        '<button type="button" class="btn-ghost" id="app-link-code" disabled' + (ha ? " hidden" : "") + ">Use a pairing code</button>" +
        '<button type="button" class="btn-ghost" id="app-link-share" hidden>Let someone see this home</button>' +
        "</div>" +
        '<p id="app-link-terms" class="hint" hidden>The app reaches this box through FTW Cloud, ' +
        "which keeps a readable copy of your energy data and takes project statistics. Pairing accepts its " +
        '<a href="https://ftw.energy/privacy" target="_blank" rel="noopener">terms</a>. ' +
        "The box runs without it.</p>" +
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
        '<p class="hint">Only one phone? Open the pairing link on this phone' +
        (ha ? ". " : ", or choose Use a pairing code and enter it in the FTW app while on your home Wi-Fi. " +
          "If you set a house password, the app asks for it too. ") +
        "You can also choose a screenshot " +
        "of the QR in the FTW app. Only one code works at a time; making a new code cancels the last one.</p>" +
        '<p id="app-link-off-row" class="app-link-off" hidden>' +
        '<button type="button" class="btn-link" id="app-link-off">Turn off the app link…</button>' +
        '<span id="app-link-off-status" class="hint" role="status"></span></p>' +
        "</fieldset>" +
        // FTW Cloud's own panel, below the pairing so it never pushes a code
        // out of view when it appears. Hidden while Cloud is off.
        '<div id="app-cloud-panel" hidden><div data-project-statistics-settings></div></div>' +
        fleetPanel(ctx)
      );
    },
  };

  // A restart reloads the whole page. Return to this step rather than leave
  // the person on the dashboard looking for their unfinished pairing.
  try {
    if (window.sessionStorage.getItem("ftw.pairing.resume") === "1" && S.open) {
      resumePairing(false);
      setTimeout(function () { S.open("app"); }, 0);
    }
  } catch (_) { /* Settings can still be opened by hand. */ }
})();
